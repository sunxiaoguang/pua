package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

type resolvedResourceAgent struct {
	Binding         app.AgentBinding
	AgentName       string
	ProfileRevision string
	ResolvedProfile string
	ConfigError     string
	InstanceID      string
}

type resourceAgentBindingUnavailableError struct {
	cause error
}

func (e *resourceAgentBindingUnavailableError) Error() string {
	return e.cause.Error()
}

func (e *resourceAgentBindingUnavailableError) Unwrap() error {
	return e.cause
}

func isResourceAgentBindingUnavailable(err error) bool {
	var unavailable *resourceAgentBindingUnavailableError
	return errors.As(err, &unavailable)
}

func generationSourceInstanceID(cfg config, record generationRecord) string {
	if value := strings.TrimSpace(record.SourceInstanceID); value != "" {
		return value
	}
	return strings.TrimSpace(cfg.AgentHubInstanceID)
}

func sourceLookupKey(instanceID, externalID string) string {
	return strings.TrimSpace(instanceID) + "\x00" + strings.TrimSpace(externalID)
}

func (m *agentManager) resourceHasActiveTurn(ctx context.Context, workspace serveWorkspace, resourceID string) (bool, error) {
	records, err := loadCurrentGenerationRecords(workspace.Path)
	if err != nil {
		return false, err
	}
	_, client, configErr := m.agentHubRuntimeConfig()
	for _, record := range records {
		if !generationMatchesResource(record, resourceID) {
			continue
		}
		if record.Status == "running" || record.Status == "waiting_approval" {
			return true, nil
		}
		if !isAgentHubGeneration(record) || strings.TrimSpace(record.AgentHubSessionID) == "" {
			continue
		}
		if configErr != nil {
			return false, fmt.Errorf("verify resource Turn state: %w", configErr)
		}
		session, fetchErr := client.GetSession(ctx, record.AgentHubSessionID)
		if fetchErr != nil {
			return false, fmt.Errorf("verify resource generation %s Turn state: %w", record.GenerationID, fetchErr)
		}
		if session.State == "running" || session.State == "waiting_approval" {
			return true, nil
		}
	}
	return false, nil
}

func (m *agentManager) resolveResourceAgent(workspace serveWorkspace, resourceID string, cfg config) (resolvedResourceAgent, error) {
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return resolvedResourceAgent{}, err
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		return resolvedResourceAgent{}, err
	}
	binding, err := puaWorkspace.ResourceAgentBinding(resourceID)
	if err != nil {
		if app.IsKind(err, "binding") {
			return resolvedResourceAgent{}, &resourceAgentBindingUnavailableError{cause: err}
		}
		return resolvedResourceAgent{}, err
	}
	resolved := resolvedResourceAgent{Binding: binding, InstanceID: runtimeConfig.InstanceID}
	switch binding.Kind {
	case "agent":
		resolved.AgentName = binding.Name
	case "profile":
		requested := strings.ToLower(strings.TrimSpace(binding.Name))
		resolved.ResolvedProfile = requested
		resolved.AgentName = configuredAgentProfileName(cfg.AgentProfiles, requested)
		if strings.TrimSpace(resolved.AgentName) == "" {
			kind, kindErr := resourceAgentKind(puaWorkspace, resourceID)
			if kindErr != nil {
				return resolvedResourceAgent{}, kindErr
			}
			fallback := workspaceResourceDefaultForKind(runtimeConfig, kind)
			if fallback.Kind == "agent" {
				resolved.ResolvedProfile = ""
				resolved.AgentName = fallback.Name
				resolved.ConfigError = fmt.Sprintf("Agent Profile %q cannot be resolved; using fallback Agent %q", requested, fallback.Name)
			} else {
				fallbackAgent := configuredAgentProfileName(cfg.AgentProfiles, fallback.Name)
				if fallbackAgent != "" {
					resolved.ResolvedProfile, resolved.AgentName = fallback.Name, fallbackAgent
				} else if global := configuredAgentProfileName(cfg.AgentProfiles, "default"); global != "" {
					resolved.ResolvedProfile, resolved.AgentName = "default", global
				} else {
					return resolvedAgentError(resolved, requested, fallback.Name)
				}
				resolved.ConfigError = fmt.Sprintf("Agent Profile %q cannot be resolved; using fallback Profile %q", requested, resolved.ResolvedProfile)
			}
		}
		digest := sha256.Sum256([]byte(requested + "\x00" + resolved.ResolvedProfile + "\x00" + resolved.AgentName + "\x00" + resolved.ConfigError))
		resolved.ProfileRevision = hex.EncodeToString(digest[:8])
	default:
		return resolvedResourceAgent{}, &resourceAgentBindingUnavailableError{cause: fmt.Errorf("unsupported resource agent binding kind %q", binding.Kind)}
	}
	return resolved, nil
}

func resourceAgentKind(workspace *app.Workspace, resourceID string) (string, error) {
	if strings.TrimSpace(resourceID) == "" || strings.TrimSpace(resourceID) == "workspace" {
		return "workspace", nil
	}
	if strings.TrimSpace(resourceID) == app.SchedulerResourceID {
		return app.SchedulerResourceID, nil
	}
	value, err := workspace.ResourceValue(resourceID)
	if err != nil {
		return "", err
	}
	if value.Task != nil {
		return "task", nil
	}
	return "project", nil
}

// workspaceResourceDefaultForKind returns the Workspace-configured default
// binding used as the fallback when a resource's Profile binding cannot be
// resolved. Projects and Tasks use their Workspace defaults; the Workspace
// and Scheduler fall back to the always-available default Profile.
func workspaceResourceDefaultForKind(runtimeConfig app.WorkspaceRuntimeConfig, kind string) app.AgentBinding {
	switch kind {
	case "project":
		return runtimeConfig.ResourceDefaults.Project
	case "task":
		return runtimeConfig.ResourceDefaults.Task
	default:
		return app.AgentBinding{Kind: "profile", Name: "default"}
	}
}

func resolvedAgentError(resolved resolvedResourceAgent, requested, fallback string) (resolvedResourceAgent, error) {
	resolved.ConfigError = fmt.Sprintf("Agent Profile %q cannot be resolved; type default %q and global Profile \"default\" are unavailable", requested, fallback)
	return resolved, &resourceAgentBindingUnavailableError{cause: errors.New(resolved.ConfigError + "; configure one of these Profiles before starting a new generation")}
}

func nextResourceGeneration(workspacePath, resourceID string) (int, error) {
	store, err := openGenerationStore(workspacePath, "")
	if err != nil {
		return 0, err
	}
	return store.NextGeneration(resourceID)
}

func resourceGenerationTitle(workspace serveWorkspace, resourceID string, generation int) (string, error) {
	resourceID = normalizedResourceID(resourceID)
	title := strings.TrimSpace(workspace.Name)
	if resourceID == app.SchedulerResourceID {
		title = "Scheduler"
	} else if resourceID != "workspace" {
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			return "", err
		}
		resource, err := puaWorkspace.ResourceValue(resourceID)
		if err != nil {
			return "", err
		}
		switch {
		case resource.Project != nil:
			title = strings.TrimSpace(resource.Project.Title)
		case resource.Task != nil:
			title = strings.TrimSpace(resource.Task.Title)
		}
	}
	if title == "" {
		if resourceID == "workspace" {
			title = workspaceName(workspace.Path)
		} else {
			title = resourceID
		}
	}
	return fmt.Sprintf("%s (gen #%d)", title, generation), nil
}

func currentResourceGeneration(workspacePath, resourceID string) (generationRecord, bool, error) {
	store, err := openGenerationStore(workspacePath, "")
	if err != nil {
		return generationRecord{}, false, err
	}
	storeRecord, found, err := store.Current(resourceID)
	if err != nil || !found {
		return generationRecord{}, found, err
	}
	record, err := fromStoreRecord(storeRecord)
	if err != nil {
		return generationRecord{}, false, err
	}
	if !generationMatchesResource(record, resourceID) || strings.TrimSpace(record.GenerationID) == "" {
		return generationRecord{}, false, nil
	}
	return record, true, nil
}

// deliverPendingResourceMessages retries only messages carrying stable IDs.
// AgentHub's at-least-once capability makes an unknown response safe: PUA
// retains the same stable ID until AgentHub durably accepts retry ownership.
func (rt *agentRuntime) deliverPendingResourceMessages(ctx context.Context, m *agentManager) error {
	record := rt.snapshotGeneration()
	return m.withResourceController(ctx, rt.workspace, record.ResourceID, func() error {
		return m.reconcileResourceMailboxLocked(ctx, rt.workspace, record.ResourceID)
	})
}

// createResourceGeneration creates one durable generation. Callers that need
// resource ordering must invoke it from that resource's controller. Pending
// inputs remain owned by the Workspace mailbox; generation creation never
// transfers or rewrites them.
func (m *agentManager) createResourceGeneration(ctx context.Context, workspace serveWorkspace, resourceID, cwd string, cfg config, client *agentHubClient, resolved resolvedResourceAgent) (generationRecord, error) {
	generation, err := nextResourceGeneration(workspace.Path, resourceID)
	if err != nil {
		return generationRecord{}, err
	}
	title, err := resourceGenerationTitle(workspace, resourceID, generation)
	if err != nil {
		return generationRecord{}, err
	}
	now := time.Now().Format(time.RFC3339Nano)
	providerID, providerName, model := snapshotAgentHubAgent(ctx, client, resolved.AgentName)
	record := generationRecord{
		ID:                 newGenerationRecordID(),
		WorkspaceID:        workspace.ID,
		ResourceID:         strings.TrimSpace(resourceID),
		Generation:         generation,
		GenerationID:       "gen-" + newGenerationRecordID(),
		SourceInstanceID:   resolved.InstanceID,
		BindingKind:        resolved.Binding.Kind,
		BindingName:        resolved.Binding.Name,
		ProfileRevision:    resolved.ProfileRevision,
		ResolvedProfile:    resolved.ResolvedProfile,
		AgentConfigError:   resolved.ConfigError,
		AgentHubAgentName:  resolved.AgentName,
		AgentHubProviderID: providerID, AgentHubProviderName: providerName, AgentHubModel: model,
		Title:     title,
		Cwd:       cwd,
		Status:    "starting",
		CreatedAt: now,
		UpdatedAt: now,
	}
	resourceKey := record.ResourceID
	if resourceKey == "" {
		resourceKey = "workspace"
	}
	record.SourceExternalID = resourceKey + "/" + fmt.Sprint(record.Generation)

	rt := newAgentHubRuntime(m, workspace, record, client)
	persisted := false
	defer func() {
		if !persisted {
			m.removeRuntime(record.ID)
		}
	}()
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		return generationRecord{}, err
	}
	persisted = true
	m.registerRuntime(rt)

	source := agentHubSource{
		App: agentHubSourceApp, InstanceID: record.SourceInstanceID, ExternalID: record.SourceExternalID,
		Metadata: map[string]string{
			"workspaceInstanceId": record.SourceInstanceID, "resourceId": resourceKey,
			"generation": fmt.Sprint(record.Generation), "generationId": record.GenerationID,
			"bindingKind": record.BindingKind, "bindingName": record.BindingName,
			"profileRevision": record.ProfileRevision,
		},
	}
	launchEnvironment := map[string]string{
		"PUA_WORKSPACE_ROOT":        workspace.Path,
		"PUA_WORKSPACE_INSTANCE_ID": record.SourceInstanceID,
		"PUA_RESOURCE_ID":           resourceKey,
	}
	session, err := m.findOrCreateAgentHubSession(ctx, client, source, agentHubCreateSessionRequest{
		Title: record.Title, Cwd: record.Cwd, AgentName: record.AgentHubAgentName,
		Source: &source, IdempotencyKey: record.GenerationID, LaunchEnvironment: launchEnvironment,
	})
	if err != nil {
		rt.setRecoveryError(m, err)
		return rt.snapshotGeneration(), err
	}
	record, err = rt.mutateGeneration(func(record *generationRecord) {
		record.AgentHubSessionID = session.ID
		if strings.TrimSpace(session.AgentName) != "" {
			record.AgentHubAgentName = session.AgentName
		}
		record.CompletionSessionID = session.ID
		record.CompletionCursor = session.LastEventID
	})
	if err != nil {
		rt.setRecoveryError(m, err)
		return rt.snapshotGeneration(), err
	}
	rt.applyAgentHubSessionState(m, session)
	return rt.snapshotGeneration(), nil
}

func (m *agentManager) resourceBindingChanged(ctx context.Context, workspace serveWorkspace, resourceID string, binding app.AgentBinding) error {
	return m.withResourceController(ctx, workspace, resourceID, func() error {
		return m.resourceBindingChangedLocked(ctx, workspace, resourceID, binding)
	})
}

// updateResourceAgentBinding serializes the portable binding mutation with
// generation reconciliation. Scheduler attention has no retry deadline, so a
// changed binding must wake it promptly, but only after both durable mutation
// and generation reconciliation succeed. Once the resource controller starts
// the job, its durable boundary and wake are independent of caller
// cancellation. requestReconcile does not acquire the Scheduler controller,
// so performing it before this job completes cannot invert delivery locks.
func (m *agentManager) updateResourceAgentBinding(ctx context.Context, workspace serveWorkspace, resourceID string, binding app.AgentBinding) (app.AgentBinding, bool, error) {
	outcome := m.runResourceBindingControllerJob(ctx, workspace, resourceID, func(jobCtx context.Context) resourceBindingMutationOutcome {
		return m.updateResourceAgentBindingLocked(jobCtx, workspace, resourceID, binding)
	})
	return outcome.updated, outcome.persisted, outcome.err
}

type resourceBindingMutationOutcome struct {
	updated   app.AgentBinding
	persisted bool
	material  bool
	err       error
}

// runResourceBindingControllerJob separates a request's wait lifetime from a
// binding mutation which has already started. The buffered outcome prevents a
// completed callback from racing result variables owned by a cancelled caller.
func (m *agentManager) runResourceBindingControllerJob(
	ctx context.Context,
	workspace serveWorkspace,
	resourceID string,
	mutation func(context.Context) resourceBindingMutationOutcome,
) resourceBindingMutationOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	result := make(chan resourceBindingMutationOutcome, 1)
	err := m.withResourceController(ctx, workspace, resourceID, func() error {
		outcome := resourceBindingMutationOutcome{}
		if m.server == nil {
			outcome.err = errors.New("resource binding owner Server is unavailable")
		} else if _, ownershipErr := m.server.revalidateWorkspaceMutation(workspace); ownershipErr != nil {
			outcome.err = ownershipErr
		} else if mutation != nil {
			outcome = mutation(context.WithoutCancel(ctx))
		}
		if outcome.err == nil && outcome.material && m.server.ownsWorkspace(workspace.Path) {
			m.requestReconcile(reconcileScheduler)
		}
		result <- outcome
		return outcome.err
	})
	if err != nil {
		// A cancelled waiter must not race a still-running job for its result.
		// If the job was skipped before start, result remains empty; if it was
		// already running, its buffered outcome is intentionally left for GC.
		if ctx.Err() != nil {
			return resourceBindingMutationOutcome{err: err}
		}
		// A controller-owned mutation error is published before its callback
		// returns. Controller lookup failures publish no outcome.
		select {
		case outcome := <-result:
			return outcome
		default:
			return resourceBindingMutationOutcome{err: err}
		}
	}
	return <-result
}

func (m *agentManager) updateResourceAgentBindingLocked(ctx context.Context, workspace serveWorkspace, resourceID string, binding app.AgentBinding) resourceBindingMutationOutcome {
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return resourceBindingMutationOutcome{err: err}
	}
	previous, previousErr := puaWorkspace.ResourceAgentBinding(resourceID)
	updated, err := puaWorkspace.SetResourceAgentBinding(resourceID, binding)
	if err != nil {
		return resourceBindingMutationOutcome{err: err}
	}
	outcome := resourceBindingMutationOutcome{
		updated: updated, persisted: true, material: previousErr != nil || previous != updated,
	}
	if err := m.resourceBindingChangedLocked(ctx, workspace, resourceID, updated); err != nil {
		outcome.err = err
	}
	return outcome
}

func (m *agentManager) resourceBindingChangedLocked(ctx context.Context, workspace serveWorkspace, resourceID string, binding app.AgentBinding) error {
	_ = binding
	record, found, err := currentResourceGeneration(workspace.Path, resourceID)
	if err != nil || !found {
		return err
	}
	// A hand-written or pre-profile legacy projection has no binding to
	// reconcile. Keep it attached to its current generation until an explicit
	// profile binding is persisted; otherwise a startup poll could replace a
	// valid generation merely because its old projection predates profile metadata.
	if strings.TrimSpace(record.BindingKind) == "" && strings.TrimSpace(record.BindingName) == "" &&
		strings.TrimSpace(record.AgentHubAgentName) == "" && strings.TrimSpace(record.ResolvedProfile) == "" {
		return nil
	}
	cfg, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	resolved, err := m.resolveResourceAgent(workspace, resourceID, cfg)
	if err != nil {
		rt := m.runtimeByID(record.ID)
		if rt == nil {
			rt = newAgentHubRuntime(m, workspace, record, client)
			m.registerRuntime(rt)
		}
		_, persistErr := rt.mutateGeneration(func(record *generationRecord) {
			record.AgentConfigError = resolved.ConfigError
			record.ResolvedProfile = ""
			record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		})
		return persistErr
	}
	rt := m.runtimeByID(record.ID)
	if rt == nil {
		rt = newAgentHubRuntime(m, workspace, record, client)
		m.registerRuntime(rt)
	}
	if strings.EqualFold(record.AgentHubAgentName, resolved.AgentName) {
		if record.BindingKind == resolved.Binding.Kind && record.BindingName == resolved.Binding.Name &&
			record.ProfileRevision == resolved.ProfileRevision && record.ResolvedProfile == resolved.ResolvedProfile &&
			record.AgentConfigError == resolved.ConfigError {
			return nil
		}
		_, err := rt.mutateGeneration(func(record *generationRecord) {
			record.BindingKind = resolved.Binding.Kind
			record.BindingName = resolved.Binding.Name
			record.ProfileRevision = resolved.ProfileRevision
			record.ResolvedProfile = resolved.ResolvedProfile
			record.AgentConfigError = resolved.ConfigError
			record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		})
		return err
	}
	rt.mu.Lock()
	if rt.agentHubState == "running" || rt.agentHubState == "waiting_approval" || record.Status == "running" || record.Status == "waiting_approval" {
		rt.mu.Unlock()
		_, err := rt.mutateGeneration(func(record *generationRecord) {
			record.ReplacementPending = true
			record.ResolvedProfile = resolved.ResolvedProfile
			record.AgentConfigError = resolved.ConfigError
			record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		})
		return err
	}
	rt.mu.Unlock()
	if _, err := rt.mutateGeneration(func(record *generationRecord) {
		record.ReplacementPending = true
		record.ResolvedProfile = resolved.ResolvedProfile
		record.AgentConfigError = resolved.ConfigError
		record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	}); err != nil {
		return err
	}
	_ = m.enqueueResourceController(rt.workspace, record.ResourceID, func() error {
		m.retireResourceGenerationLocked(context.WithoutCancel(ctx), rt)
		return nil
	})
	return nil
}

// prepareResourceGenerationForNewTurnLocked evaluates every lazy generation
// boundary immediately before queued input starts a new Turn. Budget rotation
// runs before Profile resolution so simultaneous changes produce one successor,
// whose creation resolves the latest Profile. The caller owns the resource
// controller and must stop mailbox delivery when replaced is true.
func (m *agentManager) prepareResourceGenerationForNewTurnLocked(ctx context.Context, workspace serveWorkspace, record generationRecord, session agentHubSession, rt *agentRuntime, client *agentHubClient) (replaced bool, err error) {
	replaced, err = m.prepareGenerationPolicyForNewTurnLocked(ctx, workspace, record, session, rt, client)
	if err != nil || replaced {
		return replaced, err
	}
	cfg, _, err := m.agentHubRuntimeConfig()
	if err != nil {
		return false, err
	}
	resolved, resolveErr := m.resolveResourceAgent(workspace, record.ResourceID, cfg)
	if resolveErr != nil {
		if !isResourceAgentBindingUnavailable(resolveErr) {
			return false, resolveErr
		}
		if rt != nil {
			_, persistErr := rt.mutateGeneration(func(current *generationRecord) {
				current.AgentConfigError = resolved.ConfigError
				current.ResolvedProfile = ""
				current.UpdatedAt = time.Now().Format(time.RFC3339Nano)
			})
			if persistErr != nil {
				return false, persistErr
			}
		}
		return false, &resourceAPIError{Code: "binding_unavailable", Message: resolveErr.Error()}
	}
	if strings.EqualFold(record.AgentHubAgentName, resolved.AgentName) {
		if record.BindingKind == resolved.Binding.Kind && record.BindingName == resolved.Binding.Name &&
			record.ProfileRevision == resolved.ProfileRevision && record.ResolvedProfile == resolved.ResolvedProfile &&
			record.AgentConfigError == resolved.ConfigError {
			return false, nil
		}
		if rt == nil {
			return false, errors.New("resource generation runtime is unavailable")
		}
		_, err := rt.mutateGeneration(func(current *generationRecord) {
			current.BindingKind = resolved.Binding.Kind
			current.BindingName = resolved.Binding.Name
			current.ProfileRevision = resolved.ProfileRevision
			current.ResolvedProfile = resolved.ResolvedProfile
			current.AgentConfigError = resolved.ConfigError
			current.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		})
		return false, err
	}
	if rt == nil {
		return false, errors.New("resource generation runtime is unavailable")
	}
	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		current.ReplacementPending = true
		current.ResolvedProfile = resolved.ResolvedProfile
		current.AgentConfigError = resolved.ConfigError
		current.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	}); err != nil {
		return false, err
	}
	_ = m.enqueueResourceController(workspace, record.ResourceID, func() error {
		m.retireResourceGenerationLocked(context.WithoutCancel(ctx), rt)
		return nil
	})
	return true, nil
}

func (m *agentManager) retireResourceGeneration(ctx context.Context, rt *agentRuntime) {
	if rt == nil {
		return
	}
	record := rt.snapshotGeneration()
	_ = m.withResourceController(ctx, rt.workspace, record.ResourceID, func() error {
		m.retireResourceGenerationLocked(ctx, rt)
		return nil
	})
}

// retireResourceGenerationLocked runs the Stop -> stopped -> Archive
// lifecycle while its resource controller owns the operation. The name is
// retained to make accidental calls from outside the controller obvious.
func (m *agentManager) retireResourceGenerationLocked(ctx context.Context, rt *agentRuntime) {
	rt.turnActionMu.Lock()
	defer rt.turnActionMu.Unlock()
	rt.mu.Lock()
	rt.lifecycleStopInFlight = true
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		rt.lifecycleStopInFlight = false
		rt.agentHubStopRequested = false
		rt.mu.Unlock()
	}()
	rt.mu.Lock()
	record, client := rt.record, rt.agentHub
	rt.mu.Unlock()
	if record.Retired {
		return
	}
	automaticSleep := record.IdleSleepStopRequested
	manualStop := record.ManualStopRequested
	if client == nil || strings.TrimSpace(record.AgentHubSessionID) == "" {
		return
	}
	cfg, _, cfgErr := m.agentHubRuntimeConfig()
	if cfgErr != nil {
		rt.setRecoveryError(m, fmt.Errorf("inspect retiring resource generation: %w", cfgErr))
		return
	}
	session, err := client.GetSession(ctx, record.AgentHubSessionID)
	if err != nil {
		rt.setRecoveryError(m, fmt.Errorf("inspect retiring resource generation: %w", err))
		return
	}
	if !agentHubSessionMatchesRetirementTarget(cfg, record, session) {
		rt.setRecoveryError(m, fmt.Errorf("retiring AgentHub Session %s does not match generation %s", session.ID, record.GenerationID))
		return
	}
	if automaticSleep && !record.ReplacementPending && !record.ArchivedTaskStopRequested && session.State == "stopped" {
		// Idle Stop is reversible. Keep this exact Session as the current
		// generation for a later mailbox-triggered Resume; it never enters the
		// Archive/retire tail below.
		rt.applyAgentHubSessionState(m, session)
		_, _ = rt.mutateGeneration(func(current *generationRecord) {
			if current.GenerationID == record.GenerationID && current.AgentHubSessionID == record.AgentHubSessionID &&
				current.LifecycleReceipt != nil && current.LifecycleReceipt.Operation == GenerationOperationStopSession {
				receipt := *current.LifecycleReceipt
				receipt.State = GenerationReceiptSucceeded
				current.LifecycleReceipt = &receipt
			}
		})
		return
	}
	mailbox, mailboxErr := loadHotResourceMailbox(rt.workspace.Path, record.ResourceID)
	if mailboxErr != nil {
		rt.setRecoveryError(m, fmt.Errorf("inspect retiring resource mailbox: %w", mailboxErr))
		return
	}
	lifecyclePlan := PlanGeneration(AdaptLegacyGenerationFacts(LegacyGenerationLifecycleInput{
		Generation: record, Session: &session, Mailbox: mailbox, Revision: record.UpdatedAt,
	}))
	switch lifecyclePlan.Operation {
	case GenerationOperationFinalizeArchivedMailbox, GenerationOperationDeliverMessage,
		GenerationOperationInterruptTurn, GenerationOperationWaitForMessageReceipt:
		return
	}
	if session.State == "running" || session.State == "waiting_approval" || len(session.PendingApprovalIDs) > 0 {
		// A message or provider action won the race after the ready snapshot.
		// Never interrupt it for automatic sleep; the next ready boundary gets a
		// fresh deadline.
		_, _ = rt.mutateGeneration(func(record *generationRecord) {
			record.Status = puaStatusForAgentHubState(session.State)
			record.IdleSinceAt = ""
			record.IdleDeadlineAt = ""
			record.IdleSleepStopRequested = false
			record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		})
		return
	}
	if session.State == "starting" {
		_, _ = rt.mutateGeneration(func(record *generationRecord) {
			record.Status = "starting"
			record.IdleSinceAt = ""
			record.IdleDeadlineAt = ""
			record.IdleSleepStopRequested = false
			record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		})
		return
	}
	if session.State == "archived" {
		// Archive may have happened after a successful Stop but before this
		// process observed it. Reuse the existing durable proof path rather than
		// treating an archived projection as proof by itself.
		_ = m.enqueueRuntimeOperation(rt, func() {
			rt.reconcileArchivedAgentHubSession(m, client, session)
		})
		return
	}
	if session.State == "ready" {
		pending, pendingErr := mailboxPendingForResource(rt.workspace.Path, record.ResourceID)
		if pendingErr != nil {
			rt.setRecoveryError(m, fmt.Errorf("inspect retiring resource mailbox: %w", pendingErr))
			return
		}
		if !record.IdleSleepStopRequested && !record.ReplacementPending && pending {
			// No automatic Stop guard was persisted for this attempt and a
			// mailbox item is already available; leave the Session ready so the
			// normal mailbox reconciler can deliver it.
			return
		}
		_, err = rt.mutateRuntime(func(runtime *agentRuntime) {
			runtime.record.Status = "stopping"
			if automaticSleep && !runtime.record.ReplacementPending && !runtime.record.ArchivedTaskStopRequested {
				receipt := GenerationLifecycleReceipt{
					Operation:    GenerationOperationStopSession,
					State:        GenerationReceiptRequested,
					OperationID:  lifecycleOperationID(GenerationLifecyclePlan{Operation: GenerationOperationStopSession, GenerationID: runtime.record.GenerationID, SessionID: runtime.record.AgentHubSessionID}),
					GenerationID: runtime.record.GenerationID,
					SessionID:    runtime.record.AgentHubSessionID,
					Revision:     runtime.record.UpdatedAt,
				}
				runtime.record.LifecycleReceipt = &receipt
			}
			runtime.record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
			runtime.agentHubStopRequested = true
		})
		if err != nil {
			rt.setRecoveryError(m, fmt.Errorf("persist retiring resource generation: %w", err))
			return
		}
		session, err = client.Stop(ctx, record.AgentHubSessionID)
		if err != nil {
			_, _ = rt.mutateGeneration(func(current *generationRecord) {
				if current.LifecycleReceipt != nil && current.LifecycleReceipt.Operation == GenerationOperationStopSession {
					receipt := *current.LifecycleReceipt
					receipt.State = GenerationReceiptUnknown
					current.LifecycleReceipt = &receipt
				}
			})
			rt.setRecoveryError(m, fmt.Errorf("retire resource generation: %w", err))
			return
		}
		if !agentHubSessionMatchesRetirementTarget(cfg, record, session) {
			rt.setRecoveryError(m, fmt.Errorf("Stop response for generation %s did not match its AgentHub source", record.GenerationID))
			return
		}
	}
	if session.State == "stopping" {
		deadline := time.Now().Add(agentHubStopConfirmTimeout)
		for session.State != "stopped" && session.State != "archived" && time.Now().Before(deadline) {
			timer := time.NewTimer(agentHubStopConfirmInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			session, err = client.GetSession(ctx, record.AgentHubSessionID)
			if err != nil {
				rt.setRecoveryError(m, fmt.Errorf("confirm retiring resource generation: %w", err))
				return
			}
			if !agentHubSessionMatchesRetirementTarget(cfg, record, session) {
				rt.setRecoveryError(m, fmt.Errorf("confirmation for generation %s did not match its AgentHub source", record.GenerationID))
				return
			}
		}
	}
	if automaticSleep && !record.ReplacementPending && !record.ArchivedTaskStopRequested {
		if session.State == "stopped" {
			rt.applyAgentHubSessionState(m, session)
			_, _ = rt.mutateGeneration(func(current *generationRecord) {
				if current.LifecycleReceipt != nil && current.LifecycleReceipt.Operation == GenerationOperationStopSession {
					receipt := *current.LifecycleReceipt
					receipt.State = GenerationReceiptSucceeded
					current.LifecycleReceipt = &receipt
				}
			})
			return
		}
		if session.State != "archived" {
			// A temporary Stop confirmation gap is retried by polling. It must
			// not fall through to Archive merely because this legacy helper owns
			// the call site.
			return
		}
	}
	if session.State == "archived" {
		_ = m.enqueueRuntimeOperation(rt, func() {
			rt.reconcileArchivedAgentHubSession(m, client, session)
		})
		return
	}
	if session.State != "stopped" {
		rt.setRecoveryError(m, fmt.Errorf("retiring resource generation %s did not reach durable stopped", record.GenerationID))
		return
	}
	archived, err := client.Archive(ctx, record.AgentHubSessionID)
	if err != nil {
		rt.setRecoveryError(m, fmt.Errorf("archive retired resource generation: %w", err))
		return
	}
	if !agentHubSessionMatchesRetirementTarget(cfg, record, archived) || archived.State != "archived" {
		rt.setRecoveryError(m, fmt.Errorf("Archive response for generation %s was not a matching archived Session", record.GenerationID))
		return
	}
	updated, err := rt.mutateGeneration(func(record *generationRecord) {
		record.Status = "stopped"
		record.AgentHubStoppedObserved = true
		record.ReplacementPending = false
		record.IdleSleepStopRequested = false
		record.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	})
	if err != nil {
		rt.setRecoveryError(m, fmt.Errorf("persist retired generation: %w", err))
		return
	}
	retireReason := strings.TrimSpace(record.RetireReason)
	if retireReason == "" {
		retireReason = "generation_replaced"
	}
	if manualStop {
		retireReason = "manual_generation_stop"
	} else if automaticSleep {
		retireReason = "idle_sleep"
	}
	if err := retireStoredGeneration(rt, updated, retireReason); err != nil {
		rt.setRecoveryError(m, fmt.Errorf("persist retired generation manifest: %w", err))
		return
	}
	pending, pendingErr := mailboxPendingForResource(rt.workspace.Path, updated.ResourceID)
	if pendingErr != nil {
		rt.addPUANotice(m, "warning", "resource/replacement", "Inspect Workspace mailbox: "+pendingErr.Error())
		return
	}
	if !pending {
		return
	}
	cfg, replacementClient, err := m.agentHubRuntimeConfig()
	if err != nil {
		rt.addPUANotice(m, "warning", "resource/replacement", "Queued replacement could not read AgentHub config: "+err.Error())
		return
	}
	resolved, err := m.resolveResourceAgent(rt.workspace, updated.ResourceID, cfg)
	if err == nil {
		resolved.AgentName, err = validateAgentHubGenerationAgent(ctx, replacementClient, resolved.AgentName)
	}
	if err != nil {
		rt.addPUANotice(m, "warning", "resource/replacement", "Queued replacement could not resolve its Agent: "+err.Error())
		return
	}
	replacement, err := m.createResourceGeneration(ctx, rt.workspace, updated.ResourceID, updated.Cwd, cfg, replacementClient, resolved)
	if err != nil {
		rt.addPUANotice(m, "warning", "resource/replacement", "Queued replacement generation failed: "+err.Error())
		return
	}
	replacementRuntime := m.runtimeByID(replacement.ID)
	if replacementRuntime == nil {
		rt.addPUANotice(m, "warning", "resource/replacement", "Replacement runtime disappeared before mailbox delivery")
		return
	}
	if err := m.reconcileResourceMailboxLocked(ctx, rt.workspace, updated.ResourceID); err != nil {
		replacementRuntime.addPUANotice(m, "warning", "resource/message", "Workspace mailbox delivery remains queued: "+err.Error())
	}
	rt.addPUANotice(m, "info", "resource/replacement", "Started replacement resource generation "+replacement.GenerationID)
}
