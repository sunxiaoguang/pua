package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

func (m *agentManager) agentHubRuntimeConfig() (config, *agentHubClient, error) {
	cfg, err := m.server.loadConfig()
	if err != nil {
		return config{}, nil, err
	}
	if cfg.Version < agentHubConfigVersion {
		return config{}, nil, errors.New("PUA chat requires current AgentHub settings; save AgentHub settings before starting a new run")
	}
	if strings.TrimSpace(cfg.AgentHubInstanceID) == "" {
		return config{}, nil, errors.New("AgentHub instance id is not configured")
	}
	endpoint, err := m.server.effectiveAgentHubEndpoint(cfg.AgentHubEndpoint)
	if err != nil {
		return config{}, nil, err
	}
	client, err := newAgentHubClient(endpoint, nil)
	if err != nil {
		return config{}, nil, err
	}
	return cfg, client, nil
}

type agentHubGenerationAgentUnavailableError struct {
	message string
}

func (e *agentHubGenerationAgentUnavailableError) Error() string {
	return e.message
}

// validateAgentHubGenerationAgent runs before PUA creates a session or changes the
// task. AgentHub may reject an unavailable configured target during session
// creation, but validating against the catalog first prevents an unavailable
// selection from leaving a PUA lock behind.
func validateAgentHubGenerationAgent(ctx context.Context, client *agentHubClient, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", &agentHubGenerationAgentUnavailableError{message: "no AgentHub agent is configured"}
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return "", fmt.Errorf("query AgentHub agents: %w", err)
	}
	for _, agent := range catalog.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), requested) {
			continue
		}
		if !agent.Available {
			reason := strings.TrimSpace(agent.UnavailableReason)
			if reason == "" {
				reason = "the AgentHub agent is unavailable"
			}
			return "", &agentHubGenerationAgentUnavailableError{message: fmt.Sprintf("AgentHub agent %q is unavailable: %s", agent.Name, reason)}
		}
		return strings.TrimSpace(agent.Name), nil
	}
	return "", &agentHubGenerationAgentUnavailableError{message: fmt.Sprintf("AgentHub agent %q is unavailable or not present in the catalog", requested)}
}

// snapshotAgentHubAgent captures the catalog facts used to launch a
// generation. A history response must use these persisted values rather than
// looking up the current catalog, which may have changed since the Turn ran.
func snapshotAgentHubAgent(ctx context.Context, client *agentHubClient, requested string) (providerID, providerName, model string) {
	if client == nil || strings.TrimSpace(requested) == "" {
		return "", "", ""
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return "", "", ""
	}
	for _, agent := range catalog.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), strings.TrimSpace(requested)) {
			continue
		}
		providerID = strings.TrimSpace(agent.ProviderID)
		for _, provider := range catalog.Providers {
			if strings.EqualFold(strings.TrimSpace(provider.ID), providerID) {
				providerName = strings.TrimSpace(provider.Name)
				break
			}
		}
		model = strings.TrimSpace(agent.Options["model"])
		if model == "" {
			model = strings.TrimSpace(agent.Options["modelName"])
		}
		return providerID, providerName, model
	}
	return "", "", ""
}

const (
	agentHubDefaultUserName   = "User"
	agentHubUserNameMaxLength = 80
)

func normalizeAgentHubUserName(value string) string {
	name := strings.TrimSpace(value)
	if name == "" {
		return agentHubDefaultUserName
	}
	runes := []rune(name)
	if len(runes) > agentHubUserNameMaxLength {
		name = string(runes[:agentHubUserNameMaxLength])
	}
	return name
}

// agentHubMessageProvenance maps browser-local identity into PUA's opaque
// message payload. AgentHub never interprets the returned values.
func agentHubMessageProvenance(userName string) (string, *agentHubMessageSender) {
	return "user", &agentHubMessageSender{Name: normalizeAgentHubUserName(userName)}
}

func agentHubInitialMessage(text string, userName string) *agentHubInboundMessage {
	if text == "" {
		return nil
	}
	role, sender := agentHubMessageProvenance(userName)
	message := resourceMailboxMessage{Text: text, Role: role, Sender: sender, ActualMode: resourceMessageModeEnqueue}
	input, err := agentHubMailboxMessage(message)
	if err != nil {
		return nil
	}
	return &input
}

func (m *agentManager) findOrCreateAgentHubSession(ctx context.Context, client *agentHubClient, source agentHubSource, request agentHubCreateSessionRequest) (agentHubSession, error) {
	found, err := findAgentHubSourceSessions(ctx, client, source)
	if err != nil {
		return agentHubSession{}, fmt.Errorf("query AgentHub source before create: %w", err)
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
	default:
		return agentHubSession{}, duplicateAgentHubSourceError(source, found)
	}
	created, createErr := client.CreateSession(ctx, request)
	if createErr == nil {
		return created, nil
	}
	// Create is non-idempotent. Any response or transport failure can be
	// ambiguous, so always query the complete source tuple before deciding.
	recovered, queryErr := findAgentHubSourceSessions(context.WithoutCancel(ctx), client, source)
	if queryErr != nil {
		return agentHubSession{}, fmt.Errorf("AgentHub create outcome is unknown (%v); source recovery failed: %w", createErr, queryErr)
	}
	switch len(recovered) {
	case 1:
		return recovered[0], nil
	case 0:
		return agentHubSession{}, fmt.Errorf("create AgentHub session: %w", createErr)
	default:
		return agentHubSession{}, duplicateAgentHubSourceError(source, recovered)
	}
}

func findAgentHubSourceSessions(ctx context.Context, client *agentHubClient, source agentHubSource) ([]agentHubSession, error) {
	sessions, err := client.ListSessions(ctx, agentHubSessionFilter{
		IncludeArchived: true, SourceApp: source.App, SourceInstanceID: source.InstanceID, SourceExternalID: source.ExternalID,
	})
	if err != nil {
		return nil, err
	}
	filtered := sessions[:0]
	for _, session := range sessions {
		if session.Source != nil && session.Source.App == source.App &&
			session.Source.InstanceID == source.InstanceID && session.Source.ExternalID == source.ExternalID {
			filtered = append(filtered, session)
		}
	}
	return filtered, nil
}

func isAgentHubIdempotencyConflict(err error) bool {
	var apiErr *agentHubAPIError
	return errors.As(err, &apiErr) && strings.EqualFold(strings.TrimSpace(apiErr.Code), "idempotency_conflict")
}

func duplicateAgentHubSourceError(source agentHubSource, sessions []agentHubSession) error {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	return fmt.Errorf("multiple AgentHub sessions match source %s/%s/%s: %s; resolve the duplicate source before retrying",
		source.App, source.InstanceID, source.ExternalID, strings.Join(ids, ", "))
}

func newAgentHubRuntime(m *agentManager, workspace serveWorkspace, record generationRecord, client *agentHubClient) *agentRuntime {
	return &agentRuntime{
		workspace: workspace, manager: m, record: record,
		agentHub: client,
	}
}

func puaStatusForAgentHubState(state string) string {
	switch state {
	case "starting":
		return "starting"
	case "ready":
		return "idle"
	case "running":
		return "running"
	case "waiting_approval":
		return "waiting_approval"
	case "stopping":
		return "stopping"
	case "stopped":
		return "stopped"
	case "archived":
		return "recovering"
	case "failed":
		return "recovering"
	default:
		return "recovering"
	}
}

func (rt *agentRuntime) setRecoveryError(m *agentManager, err error) {
	_, _ = rt.mutateGeneration(func(record *generationRecord) {
		if record.Status != "stopped" {
			record.Status = "recovering"
		}
		record.UpdatedAt = time.Now().Format(time.RFC3339)
	})
	if err != nil {
		rt.addPUANotice(m, "error", "agenthub/recovery", err.Error())
	}
}

func (rt *agentRuntime) addPUANotice(m *agentManager, level, method, text string) {
	rt.mu.Lock()
	recordID := rt.record.ID
	rt.mu.Unlock()
	notice := puaNotice{
		Source: "pua",
		Type:   "pua.notice",
		Time:   time.Now().Format(time.RFC3339),
		Data: puaNoticeData{
			Level:  level,
			Method: method,
			Text:   text,
		},
	}
	m.publishNotice(recordID, notice)
}

type interruptGenerationResponse struct {
	Status                        string        `json:"status"`
	PendingSteerPolicy            string        `json:"pendingSteerPolicy"`
	CancelledPendingSteerCount    int           `json:"cancelledPendingSteerCount,omitempty"`
	CancelledPendingSteerIDs      []string      `json:"cancelledPendingSteerIds,omitempty"`
	PendingSteerCancellationError string        `json:"pendingSteerCancellationError,omitempty"`
	TaskState                     app.TaskState `json:"taskState,omitempty"`
	TaskStateError                string        `json:"taskStateError,omitempty"`
}

type interruptGenerationPostActionResult struct {
	CancelledPendingSteers cancelledResourceMessages
	CancellationError      error
	TaskState              app.TaskState
	TaskStateError         error
}

type interruptGenerationPostAction func(serveWorkspace, string) interruptGenerationPostActionResult

func (m *agentManager) interruptGeneration(w http.ResponseWriter, r *http.Request, workspaceID, recordID string) {
	response, err := m.interruptGenerationWithPostAction(r.Context(), workspaceID, recordID, nil)
	if err != nil {
		writeInterruptGenerationError(w, err)
		return
	}
	writeJSON(w, response)
}

func (m *agentManager) interruptGenerationWithPostAction(ctx context.Context, workspaceID, recordID string, postAction interruptGenerationPostAction) (interruptGenerationResponse, error) {
	_, rt, err := m.workspaceRuntime(workspaceID, recordID)
	if err != nil || rt == nil {
		return interruptGenerationResponse{}, errors.New("run is not active")
	}
	record := rt.snapshotGeneration()
	var response interruptGenerationResponse
	err = m.withResourceController(ctx, rt.workspace, record.ResourceID, func() error {
		var err error
		response, err = m.interruptGenerationLocked(ctx, workspaceID, rt)
		if err != nil {
			return err
		}
		response.PendingSteerPolicy = "cancel"
		if postAction != nil {
			result := postAction(rt.workspace, record.ResourceID)
			response.CancelledPendingSteerCount = result.CancelledPendingSteers.Count
			response.CancelledPendingSteerIDs = result.CancelledPendingSteers.IDs
			response.TaskState = result.TaskState
			if result.CancellationError != nil {
				// The interrupt itself succeeded. Keep that durable result visible,
				// but surface a mailbox failure instead of silently allowing a
				// pending steer to affect a later Turn.
				response.PendingSteerCancellationError = result.CancellationError.Error()
			}
			if result.TaskStateError != nil {
				// The Turn is already stopped, so this error cannot make the
				// interrupt fail retroactively. Surface it explicitly and mark
				// this terminal observation handled so an in-progress Task is not
				// restarted after a failed pause write.
				response.TaskStateError = result.TaskStateError.Error()
				current := rt.snapshotGeneration()
				if current.CompletionMarker != "" {
					_ = markTaskTurnCompletionHandled(rt, current.CompletionMarker)
				}
			}
		}
		return nil
	})
	return response, err
}

func writeInterruptGenerationError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if strings.Contains(err.Error(), "run is not active") || strings.Contains(err.Error(), "run is not attached to AgentHub") {
		status = http.StatusBadRequest
	}
	var conflictErr *agentHubTurnConflictError
	if errors.As(err, &conflictErr) {
		status = http.StatusConflict
	}
	writeError(w, err, status)
}

func (m *agentManager) interruptGenerationLocked(ctx context.Context, workspaceID string, rt *agentRuntime) (interruptGenerationResponse, error) {
	// Serialize End Turn with dispatch and Close Session on this Session only;
	// Task desired-state persistence must remain independent.
	rt.turnActionMu.Lock()
	defer rt.turnActionMu.Unlock()
	rt.mu.Lock()
	record, client := rt.record, rt.agentHub
	rt.mu.Unlock()
	if record.WorkspaceID != workspaceID {
		return interruptGenerationResponse{}, errors.New("run belongs to another workspace")
	}
	if client == nil || strings.TrimSpace(record.AgentHubSessionID) == "" {
		return interruptGenerationResponse{}, errors.New("run is not attached to AgentHub")
	}
	currentSession, err := m.interruptibleAgentHubSession(ctx, record, client)
	if err != nil {
		// A failed read leaves the current turn unknown. Retain the PUA and
		// AgentHub sessions and let reconciliation establish the next state;
		// do not guess and send a non-idempotent interrupt.
		recoveryErr := fmt.Errorf("AgentHub turn state could not be confirmed; interrupt was not sent: %w", err)
		rt.setRecoveryError(m, recoveryErr)
		return interruptGenerationResponse{}, recoveryErr
	}
	session, err := client.Interrupt(ctx, currentSession.ID)
	if err != nil {
		// The non-idempotent interrupt result is ambiguous. Keep the Session and let
		// the poller reconcile its state;
		// never retry the interrupt from this path.
		recoveryErr := fmt.Errorf("AgentHub interrupt outcome may be unknown; it was not retried: %w", err)
		rt.setRecoveryError(m, recoveryErr)
		return interruptGenerationResponse{}, recoveryErr
	}
	rt.applyAgentHubSessionState(m, session)
	return interruptGenerationResponse{Status: "interrupted"}, nil
}

func isAgentHubTurnInterruptible(state string) bool {
	switch strings.TrimSpace(state) {
	case "running", "waiting_approval":
		return true
	default:
		return false
	}
}

type agentHubTurnConflictError struct {
	message string
}

func (e *agentHubTurnConflictError) Error() string {
	return e.message
}

// interruptibleAgentHubSession re-reads the AgentHub projection immediately
// before the non-idempotent interrupt. This closes the stale-page window and
// refuses to act on a session that no longer belongs to this PUA generation.
func (m *agentManager) interruptibleAgentHubSession(ctx context.Context, record generationRecord, client *agentHubClient) (agentHubSession, error) {
	cfg, _, err := m.agentHubRuntimeConfig()
	if err != nil {
		return agentHubSession{}, err
	}
	session, err := client.GetSession(ctx, record.AgentHubSessionID)
	if err != nil {
		return agentHubSession{}, fmt.Errorf("read current AgentHub turn state: %w", err)
	}
	source := session.Source
	expectedExternalID := strings.TrimSpace(record.SourceExternalID)
	if source == nil || source.App != agentHubSourceApp || source.InstanceID != generationSourceInstanceID(cfg, record) ||
		expectedExternalID == "" || source.ExternalID != expectedExternalID {
		return agentHubSession{}, &agentHubTurnConflictError{message: "AgentHub session does not belong to the current PUA run"}
	}
	if strings.TrimSpace(session.ID) == "" || session.ID != strings.TrimSpace(record.AgentHubSessionID) {
		return agentHubSession{}, &agentHubTurnConflictError{message: "AgentHub session identity changed before interrupt"}
	}
	if !isAgentHubTurnInterruptible(session.State) {
		return agentHubSession{}, &agentHubTurnConflictError{message: fmt.Sprintf("AgentHub session is not interruptible in %s state", session.State)}
	}
	return session, nil
}

func (m *agentManager) resolveAgentHubApproval(w http.ResponseWriter, r *http.Request, rt *agentRuntime, req agentApprovalRequest) {
	record := rt.snapshotGeneration()
	if err := m.withResourceController(r.Context(), rt.workspace, record.ResourceID, func() error {
		m.resolveAgentHubApprovalLocked(w, r, rt, req)
		return nil
	}); err != nil {
		writeError(w, err, http.StatusBadGateway)
	}
}

func (m *agentManager) resolveAgentHubApprovalLocked(w http.ResponseWriter, r *http.Request, rt *agentRuntime, req agentApprovalRequest) {
	rt.turnActionMu.Lock()
	defer rt.turnActionMu.Unlock()
	if strings.TrimSpace(req.RequestID) == "" {
		writeError(w, errors.New("requestId is required"), http.StatusBadRequest)
		return
	}
	reply, err := normalizeAgentHubApprovalReply(req)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	rt.mu.Lock()
	record, client := rt.record, rt.agentHub
	rt.mu.Unlock()
	session, err := client.Approval(r.Context(), record.AgentHubSessionID, req.RequestID, reply)
	if err != nil {
		writeError(w, fmt.Errorf("AgentHub approval outcome may be unknown; it was not retried: %w", err), http.StatusBadGateway)
		return
	}
	rt.applyAgentHubSessionState(m, session)
	writeJSON(w, map[string]string{"status": "resolved"})
}

func normalizeAgentHubApprovalReply(req agentApprovalRequest) (agentHubApprovalReply, error) {
	reply := agentHubApprovalReply{
		Decision: strings.TrimSpace(req.Decision),
		OptionID: strings.TrimSpace(req.OptionID),
		Text:     strings.TrimSpace(req.Text),
	}
	modes := 0
	if reply.Decision != "" {
		modes++
	}
	if reply.OptionID != "" {
		modes++
	}
	if reply.Text != "" {
		modes++
	}
	if modes != 1 {
		return agentHubApprovalReply{}, errors.New("exactly one of decision, optionId, or text is required")
	}
	if reply.Decision != "" {
		switch reply.Decision {
		case "accept", "acceptForSession", "decline", "cancel":
		default:
			return agentHubApprovalReply{}, errors.New("decision must be accept, acceptForSession, decline, or cancel")
		}
	}
	return reply, nil
}

// recoverAgentHubGenerations rebuilds lightweight runtime projections at startup from
// one AgentHub session list and the local generation indexes. It never reads event
// history and never opens event streams.
func (m *agentManager) recoverAgentHubGenerations(ctx context.Context) error {
	cfg, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	sessions, err := client.ListSessions(ctx, agentHubSessionFilter{
		IncludeArchived: true, SourceApp: agentHubSourceApp,
	})
	if err != nil {
		return err
	}
	byExternalID := make(map[string]agentHubSession, len(sessions))
	byID := make(map[string]agentHubSession, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
		if session.Source != nil && strings.TrimSpace(session.Source.ExternalID) != "" {
			byExternalID[sourceLookupKey(session.Source.InstanceID, session.Source.ExternalID)] = session
		}
	}
	var failures []string
	for _, workspace := range cfg.Workspaces {
		// Recovery and reconciliation only run for owned Workspaces.
		if !m.server.ownsWorkspace(workspace.Path) {
			continue
		}
		records, loadErr := loadCurrentGenerationRecords(workspace.Path)
		if loadErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", workspace.ID, loadErr))
			continue
		}
		for _, record := range records {
			if !isAgentHubGeneration(record) {
				continue
			}
			candidates := []agentHubSession{}
			if session, ok := byExternalID[sourceLookupKey(generationSourceInstanceID(cfg, record), record.SourceExternalID)]; ok {
				candidates = []agentHubSession{session}
			} else if session, ok := byID[strings.TrimSpace(record.AgentHubSessionID)]; ok {
				candidates = []agentHubSession{session}
			}
			if recoverErr := m.recoverAgentHubGeneration(ctx, cfg, client, workspace, record, candidates); recoverErr != nil {
				failures = append(failures, fmt.Sprintf("%s/%s: %v", workspace.ID, record.ID, recoverErr))
			}
		}
	}
	for _, workspace := range cfg.Workspaces {
		if !m.server.ownsWorkspace(workspace.Path) {
			continue
		}
		if _, mailboxErr := rebuildResourceMailboxHotIndex(workspace.Path); mailboxErr != nil {
			failures = append(failures, fmt.Sprintf("%s mailbox hot index: %v", workspace.ID, mailboxErr))
			continue
		}
		if mailboxErr := m.reconcileWorkspaceMailboxes(ctx, workspace); mailboxErr != nil {
			failures = append(failures, fmt.Sprintf("%s mailbox: %v", workspace.ID, mailboxErr))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// recoverAgentHubGeneration rebuilds the lightweight runtime projection for one
// generation without eagerly loading event history or opening an event stream. A persisted
// active -> ready/stopped edge, or a pending completion inspection, may replay
// the bounded durable history needed for the completion marker. candidates
// carries the sessions found by the single instance-wide startup query.
// Live generations may recreate a missing AgentHub session from the source tuple.
func (m *agentManager) recoverAgentHubGeneration(ctx context.Context, cfg config, client *agentHubClient, workspace serveWorkspace, record generationRecord, candidates []agentHubSession) error {
	return m.withResourceController(ctx, workspace, record.ResourceID, func() error {
		return m.recoverAgentHubGenerationLocked(ctx, cfg, client, workspace, record, candidates)
	})
}

func (m *agentManager) recoverAgentHubGenerationLocked(ctx context.Context, cfg config, client *agentHubClient, workspace serveWorkspace, record generationRecord, candidates []agentHubSession) error {
	source := agentHubSource{App: agentHubSourceApp, InstanceID: generationSourceInstanceID(cfg, record), ExternalID: record.SourceExternalID}
	live := isLiveAgentStatus(record.Status)
	if len(candidates) == 0 && strings.TrimSpace(record.AgentHubSessionID) != "" {
		bound, getErr := client.GetSession(ctx, record.AgentHubSessionID)
		if getErr != nil {
			if isMissingAgentHubSessionError(getErr) {
				rt := m.ensureRuntime(workspace, record, client)
				return m.retireUnresumableGenerationLocked(ctx, rt, client,
					fmt.Errorf("AgentHub Session %s is no longer available", record.AgentHubSessionID))
			}
			m.markGenerationRecovering(workspace, record)
			return fmt.Errorf("inspect bound AgentHub Session %s before recovery: %w", record.AgentHubSessionID, getErr)
		}
		if agentHubSourceConflicts(cfg, record, bound) {
			rt := m.ensureRuntime(workspace, record, client)
			return m.retireUnresumableGenerationLocked(ctx, rt, client,
				fmt.Errorf("AgentHub Session %s source is incompatible with generation %s", bound.ID, record.GenerationID))
		}
		candidates = []agentHubSession{bound}
	}
	if len(candidates) == 0 && live {
		request := agentHubCreateSessionRequest{
			Title: record.Title, Cwd: record.Cwd, AgentName: record.AgentHubAgentName,
			Source:         &source,
			InitialMessage: agentHubInitialMessage(record.PendingInitialMessage, ""),
		}
		if record.GenerationID != "" {
			source.Metadata = map[string]string{
				"workspaceInstanceId": record.SourceInstanceID, "resourceId": record.ResourceID,
				"generation": strconv.Itoa(record.Generation), "generationId": record.GenerationID,
				"bindingKind": record.BindingKind, "bindingName": record.BindingName,
				"profileRevision": record.ProfileRevision,
			}
			request.Source = &source
			request.IdempotencyKey = record.GenerationID
			request.InitialMessage = nil
		}
		recovered, createErr := m.findOrCreateAgentHubSession(ctx, client, source, request)
		if createErr != nil {
			if isAgentHubIdempotencyConflict(createErr) && record.GenerationID != "" {
				rt := m.ensureRuntime(workspace, record, client)
				return retireGenerationWithoutSession(rt, "session_create_idempotency_conflict: "+createErr.Error())
			}
			m.markGenerationRecovering(workspace, record)
			return createErr
		}
		candidates = []agentHubSession{recovered}
	}
	if len(candidates) != 1 {
		rt := newAgentHubRuntime(m, workspace, record, client)
		m.registerRuntime(rt)
		if live {
			m.markGenerationRecovering(workspace, rt.snapshotGeneration())
		}
		if len(candidates) > 1 {
			return duplicateAgentHubSourceError(source, candidates)
		}
		return nil
	}
	session := candidates[0]
	previousStatus := record.Status
	record.AgentHubSessionID = session.ID
	if strings.TrimSpace(session.AgentName) != "" {
		record.AgentHubAgentName = session.AgentName
	}
	if record.GenerationID == "" {
		record.PendingInitialMessage = ""
	}
	rt := newAgentHubRuntime(m, workspace, record, client)
	// Let applyAgentHubSessionState compare the recovered state with the
	// persisted projection. This preserves a running -> ready/stopped edge across
	// a PUA restart instead of treating recovery as a fresh idle baseline.
	rt.agentHubState = agentHubStateForPUAStatus(previousStatus)
	m.registerRuntime(rt)
	rt.applyAgentHubSessionState(m, session)
	updated := rt.snapshotGeneration()
	if updated.CompletionMarker != "" && updated.CompletionMarker != updated.TaskStateCompletionMarker {
		m.scheduleTaskTurnCompletion(rt, updated)
	}
	if updated.GenerationID != "" && !resourceIdleSuspensionStable(updated, session) &&
		(session.State == "ready" || (updated.IdleSleepStopRequested && (session.State == "stopping" || session.State == "stopped"))) {
		if err := m.reconcileIdleGenerationLocked(ctx, workspace, updated, session, client); err != nil {
			rt.addPUANotice(m, "warning", "resource/idle-sleep", err.Error())
		}
	}
	if record.ReplacementPending && (session.State == "ready" || session.State == "stopped") {
		_ = m.enqueueResourceController(rt.workspace, record.ResourceID, func() error {
			m.retireResourceGenerationLocked(context.Background(), rt)
			return nil
		})
	}
	if session.State == "archived" {
		// The service missed the stopped edge while it was down. Release the
		// PUA session only when the archived session provably passed
		// through durable stopped; anything else keeps failing closed. Runs
		// asynchronously so a long event replay never blocks startup.
		_ = m.enqueueRuntimeOperation(rt, func() {
			rt.reconcileArchivedAgentHubSession(m, client, session)
		})
	}
	return nil
}

func (m *agentManager) markGenerationRecovering(workspace serveWorkspace, record generationRecord) {
	record.Status = "recovering"
	record.UpdatedAt = time.Now().Format(time.RFC3339)
	if rt := m.runtimeByID(record.ID); rt != nil {
		_, _ = rt.mutateGeneration(func(current *generationRecord) {
			current.Status = record.Status
			current.UpdatedAt = record.UpdatedAt
		})
		return
	}
	_ = saveGenerationRecord(workspace.Path, record)
}
