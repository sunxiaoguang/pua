package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type reconcileRequest uint8

const (
	reconcileAgentHub reconcileRequest = 1 << iota
	reconcileColdAudit
	reconcileMailboxes
	reconcileNotifications
	reconcileScheduler
)

func (m *agentManager) requestReconcile(request reconcileRequest) {
	if m == nil || request == 0 {
		return
	}
	m.mu.Lock()
	m.reconcilePending |= request
	m.mu.Unlock()
	select {
	case m.reconcileWake <- struct{}{}:
	default:
	}
}

func (m *agentManager) takeReconcileRequests() reconcileRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	request := m.reconcilePending
	m.reconcilePending = 0
	return request
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func resetReconcileTimer(timer *time.Timer, deadline time.Time) {
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func earliestReconcileDeadline(deadlines ...time.Time) time.Time {
	var earliest time.Time
	for _, deadline := range deadlines {
		if deadline.IsZero() {
			continue
		}
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest
}

func finishColdAuditRequests(request reconcileRequest, schedulerDeadline, now time.Time) (reconcileRequest, time.Time) {
	request &^= reconcileAgentHub | reconcileMailboxes | reconcileNotifications
	if !schedulerDeadline.IsZero() && !schedulerDeadline.After(now) {
		request |= reconcileScheduler
		schedulerDeadline = time.Time{}
	}
	return request, schedulerDeadline
}

// runReconcileLoop keeps latency-sensitive AgentHub projection separate from
// cold filesystem audits and recovery-only mailbox/notification passes. All
// requests are coalesced here; per-resource mutation remains serialized by the
// existing resource controllers.
func (m *agentManager) runReconcileLoop(ctx context.Context) {
	now := time.Now()
	nextAgentHub := now
	nextColdAudit := now
	nextMailbox := now
	nextNotifications := now
	nextSchedulerFallback := now
	var nextSchedulerDeadline time.Time
	var nextIdleDeadline time.Time
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		now = time.Now()
		request := m.takeReconcileRequests()
		if !now.Before(nextColdAudit) {
			request |= reconcileColdAudit
		}
		if !now.Before(nextAgentHub) {
			request |= reconcileAgentHub
		}
		if !now.Before(nextMailbox) {
			request |= reconcileMailboxes
		}
		if !now.Before(nextNotifications) {
			request |= reconcileNotifications
		}
		if !now.Before(nextSchedulerFallback) || (!nextSchedulerDeadline.IsZero() && !now.Before(nextSchedulerDeadline)) {
			request |= reconcileScheduler
		}
		if !nextIdleDeadline.IsZero() && !now.Before(nextIdleDeadline) {
			request |= reconcileAgentHub
		}

		if request&reconcileColdAudit != 0 {
			if err := m.pollAgentHubSessions(ctx); err != nil {
				log.Printf("audit AgentHub generations: %v", err)
			}
			now = time.Now()
			nextColdAudit = now.Add(durationOr(m.coldAuditInterval, 30*time.Second))
			nextMailbox = now.Add(durationOr(m.mailboxRetryInterval, 10*time.Second))
			nextNotifications = now.Add(durationOr(m.notificationInterval, 30*time.Second))
			nextSchedulerFallback = now.Add(durationOr(m.schedulerFallback, 30*time.Second))
			nextSchedulerDeadline = m.nextSchedulerReconcileDeadline(now)
			request, nextSchedulerDeadline = finishColdAuditRequests(request, nextSchedulerDeadline, now)
		}
		if request&reconcileAgentHub != 0 {
			if err := m.pollFastAgentHubSessions(ctx); err != nil {
				log.Printf("poll active AgentHub sessions: %v", err)
			}
			now = time.Now()
			interval := durationOr(m.stablePollInterval, 10*time.Second)
			if m.agentHubFastWorkPending() {
				interval = durationOr(m.activePollInterval, 2*time.Second)
			}
			nextAgentHub = now.Add(interval)
			nextIdleDeadline = m.nextIdleReconcileDeadline()
			if !nextIdleDeadline.After(now) {
				nextIdleDeadline = nextAgentHub
			}
		}
		if request&reconcileMailboxes != 0 {
			if err := m.reconcileOwnedWorkspaceMailboxes(ctx); err != nil {
				log.Printf("reconcile resource mailboxes: %v", err)
			}
			nextMailbox = time.Now().Add(durationOr(m.mailboxRetryInterval, 10*time.Second))
		}
		if request&reconcileNotifications != 0 {
			if err := m.reconcileOwnedWorkspaceNotifications(ctx); err != nil {
				log.Printf("reconcile resource notifications: %v", err)
			}
			nextNotifications = time.Now().Add(durationOr(m.notificationInterval, 30*time.Second))
		}
		if request&reconcileScheduler != 0 {
			if err := m.reconcileOwnedWorkspaceSchedulers(ctx); err != nil {
				log.Printf("reconcile Schedulers: %v", err)
			}
			now = time.Now()
			nextSchedulerFallback = now.Add(durationOr(m.schedulerFallback, 30*time.Second))
			nextSchedulerDeadline = m.nextSchedulerReconcileDeadline(now)
			if !nextSchedulerDeadline.After(now) {
				nextSchedulerDeadline = time.Time{}
			}
		}

		now = time.Now()
		if !nextAgentHub.After(now) {
			interval := durationOr(m.stablePollInterval, 10*time.Second)
			if m.agentHubFastWorkPending() {
				interval = durationOr(m.activePollInterval, 2*time.Second)
			}
			nextAgentHub = now.Add(interval)
		}
		if nextIdleDeadline.IsZero() {
			nextIdleDeadline = m.nextIdleReconcileDeadline()
			if !nextIdleDeadline.After(now) {
				nextIdleDeadline = nextAgentHub
			}
		}
		deadline := earliestReconcileDeadline(
			nextAgentHub, nextColdAudit, nextMailbox, nextNotifications,
			nextSchedulerFallback, nextSchedulerDeadline, nextIdleDeadline,
		)
		if deadline.IsZero() {
			deadline = now.Add(durationOr(m.stablePollInterval, 10*time.Second))
		}
		resetReconcileTimer(timer, deadline)
		select {
		case <-ctx.Done():
			return
		case <-m.reconcileWake:
		case <-timer.C:
		}
	}
}

func (m *agentManager) runtimeSnapshots() []*agentRuntime {
	m.mu.Lock()
	runtimes := make([]*agentRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		runtimes = append(runtimes, rt)
	}
	m.mu.Unlock()
	return runtimes
}

func lifecycleReceiptNeedsReconcile(receipt *GenerationLifecycleReceipt) bool {
	if receipt == nil {
		return false
	}
	switch receipt.State {
	case GenerationReceiptNone, GenerationReceiptSucceeded, GenerationReceiptTerminal:
		return false
	default:
		return true
	}
}

func generationNeedsFastReconcile(m *agentManager, rt *agentRuntime, record generationRecord, session agentHubSession, found bool) bool {
	if record.Retired {
		return false
	}
	rt.mu.Lock()
	projectedState := rt.agentHubState
	stopInFlight := rt.lifecycleStopInFlight
	rt.mu.Unlock()
	if projectedState == "" {
		projectedState = agentHubStateForPUAStatus(record.Status)
	}
	pending := stopInFlight || record.CompletionPending || record.ReplacementPending || record.ArchivedTaskStopRequested || record.StallWatchdog != nil ||
		strings.TrimSpace(record.ResumeRetryAt) != "" || resourceGenerationLifecyclePending(record)
	pending = pending || lifecycleReceiptNeedsReconcile(record.LifecycleReceipt)
	switch record.Status {
	case "starting", "running", "waiting_approval", "stopping", "recovering":
		pending = true
	}
	if !found {
		return pending || isLiveAgentStatus(record.Status)
	}
	if record.IdleSleepStopRequested && !resourceIdleSuspensionStable(record, session) {
		pending = true
	}
	if record.Status == "idle" && m.idleDeadlineDue(record) {
		pending = true
	}
	switch session.State {
	case "starting", "running", "waiting_approval", "stopping":
		return true
	}
	if pending || projectedState != session.State || record.AgentHubSessionID != session.ID ||
		activeAgentHubTurnID(session) != record.CurrentTurnID || session.LastEventID > record.CompletionCursor {
		return true
	}
	updatedAt := generationTime(session.UpdatedAt)
	return !updatedAt.IsZero() && generationTime(record.UpdatedAt).Before(updatedAt)
}

// pollFastAgentHubSessions projects only in-memory current generations that are
// active, have changed upstream, or own unfinished lifecycle work. The cold
// audit remains responsible for discovering new/out-of-process records and
// checking archive state.
func (m *agentManager) pollFastAgentHubSessions(ctx context.Context) error {
	cfg, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	sessions, err := client.ListSessions(ctx, agentHubSessionFilter{SourceApp: agentHubSourceApp})
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
	for _, rt := range m.runtimeSnapshots() {
		record := rt.snapshotGeneration()
		workspace := rt.workspace
		if !m.server.ownsWorkspace(workspace.Path) || !isAgentHubGeneration(record) {
			continue
		}
		session, found := byExternalID[sourceLookupKey(generationSourceInstanceID(cfg, record), record.SourceExternalID)]
		if !found {
			session, found = byID[strings.TrimSpace(record.AgentHubSessionID)]
		}
		if !generationNeedsFastReconcile(m, rt, record, session, found) {
			continue
		}
		m.reconcileAgentHubGeneration(ctx, cfg, workspace, record, byExternalID, byID, client)
	}
	return nil
}

func (m *agentManager) agentHubFastWorkPending() bool {
	for _, rt := range m.runtimeSnapshots() {
		record := rt.snapshotGeneration()
		rt.mu.Lock()
		state := rt.agentHubState
		stopInFlight := rt.lifecycleStopInFlight
		rt.mu.Unlock()
		if stopInFlight || record.CompletionPending || record.ReplacementPending || record.ArchivedTaskStopRequested || record.StallWatchdog != nil ||
			resourceGenerationLifecyclePending(record) || strings.TrimSpace(record.ResumeRetryAt) != "" ||
			lifecycleReceiptNeedsReconcile(record.LifecycleReceipt) || (record.IdleSleepStopRequested && record.Status != "idle-suspended") {
			return true
		}
		switch firstNonEmpty(state, agentHubStateForPUAStatus(record.Status)) {
		case "starting", "running", "waiting_approval", "stopping":
			return true
		}
	}
	return false
}

func (m *agentManager) nextIdleReconcileDeadline() time.Time {
	var earliest time.Time
	for _, rt := range m.runtimeSnapshots() {
		record := rt.snapshotGeneration()
		if record.Retired || record.Status != "idle" || record.IdleSleepStopRequested || record.ReplacementPending || record.ArchivedTaskStopRequested {
			continue
		}
		deadline := generationTime(record.IdleDeadlineAt)
		if deadline.IsZero() {
			continue
		}
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest
}

func (m *agentManager) reconcileOwnedWorkspaceMailboxes(ctx context.Context) error {
	cfg, _, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	var failures []string
	for _, workspace := range cfg.Workspaces {
		if m.server.ownsWorkspace(workspace.Path) {
			if err := m.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", workspace.ID, err))
			}
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (m *agentManager) reconcileOwnedWorkspaceNotifications(ctx context.Context) error {
	cfg, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	var failures []string
	for _, workspace := range cfg.Workspaces {
		if m.server.ownsWorkspace(workspace.Path) {
			if err := m.reconcileWorkspaceNotifications(ctx, workspace, client); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", workspace.ID, err))
			}
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (m *agentManager) reconcileOwnedWorkspaceSchedulers(ctx context.Context) error {
	cfg, err := m.server.loadConfig()
	if err != nil {
		return err
	}
	var failures []string
	for _, workspace := range cfg.Workspaces {
		if m.server.ownsWorkspace(workspace.Path) {
			if err := m.reconcileSchedulerLocked(ctx, workspace); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", workspace.ID, err))
			}
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// nextSchedulerReconcileDeadline derives the exact earliest native schedule or
// retry deadline. The fallback audit is only for restart, manual file changes,
// and clock-jump recovery.
func (m *agentManager) nextSchedulerReconcileDeadline(now time.Time) time.Time {
	cfg, err := m.server.loadConfig()
	if err != nil {
		return time.Time{}
	}
	var earliest time.Time
	for _, workspace := range cfg.Workspaces {
		if !m.server.ownsWorkspace(workspace.Path) {
			continue
		}
		snapshot, err := newNativeScheduler(m, workspace).Snapshot(now)
		if err != nil || snapshot.NextWakeAt == "" {
			continue
		}
		deadline := generationTime(snapshot.NextWakeAt)
		if deadline.Before(now) {
			deadline = now
		}
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest
}
