package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func idleTestGeneration(workspace serveWorkspace, resourceID, recordID, sessionID string, deadline time.Time) generationRecord {
	boundary := deadline.Add(-30 * time.Minute)
	return generationRecord{
		ID:                recordID,
		WorkspaceID:       workspace.ID,
		ResourceID:        resourceID,
		Generation:        1,
		GenerationID:      "gen-" + recordID,
		SourceInstanceID:  "pua-runtime-test",
		BindingKind:       "profile",
		BindingName:       "default",
		ProfileRevision:   "test-revision",
		AgentHubSessionID: sessionID,
		AgentHubAgentName: "fake-agent",
		SourceExternalID:  resourceID + "/" + recordID,
		Status:            "idle",
		IdleSinceAt:       boundary.Format(time.RFC3339Nano),
		IdleDeadlineAt:    deadline.Format(time.RFC3339Nano),
		CreatedAt:         boundary.Add(-time.Second).Format(time.RFC3339Nano),
		UpdatedAt:         boundary.Format(time.RFC3339Nano),
		Cwd:               workspace.Path,
	}
}

func seedIdleTestGeneration(t *testing.T, fake *runtimeFakeAgentHub, workspace serveWorkspace, record generationRecord, state string) {
	t.Helper()
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: state, AgentName: "fake-agent",
		UpdatedAt: record.IdleSinceAt,
	})
}

func waitForFakeSessionState(t *testing.T, fake *runtimeFakeAgentHub, sessionID, state string) {
	t.Helper()
	waitForRuntimeTest(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.sessions[sessionID].State == state
	})
}

func waitForIdleGenerationSuspended(t *testing.T, manager *agentManager, workspacePath, recordID string) generationRecord {
	t.Helper()
	var stored generationRecord
	waitForRuntimeTest(t, func() bool {
		candidate, err := loadGenerationRecord(workspacePath, recordID)
		if err != nil || candidate.Status != "idle-suspended" || candidate.AgentHubStoppedObserved ||
			!candidate.IdleSleepStopRequested || candidate.ReplacementPending {
			return false
		}
		rt := manager.runtimeByID(recordID)
		if rt != nil {
			rt.mu.Lock()
			lifecycleStopInFlight := rt.lifecycleStopInFlight
			agentHubStopRequested := rt.agentHubStopRequested
			rt.mu.Unlock()
			if lifecycleStopInFlight || agentHubStopRequested {
				return false
			}
		}
		stored = candidate
		return true
	})
	return stored
}

func fakeStopCalls(fake *runtimeFakeAgentHub) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.stopCalls
}

func TestResourceIdleSleepHonorsDeadlineAndSuspendsAllResourceKinds(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline.Add(-time.Second) }
	resources := []string{"workspace", "project1", "project1.task1", app.SchedulerResourceID}
	records := make([]generationRecord, 0, len(resources))
	for index, resourceID := range resources {
		record := idleTestGeneration(workspace, resourceID, "gen-idle-"+string(rune('a'+index)), "ses-idle-"+string(rune('a'+index)), deadline)
		seedIdleTestGeneration(t, fake, workspace, record, "ready")
		records = append(records, record)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fakeStopCalls(fake) != 0 {
		t.Fatalf("generation slept before its deadline: stop calls=%d", fakeStopCalls(fake))
	}
	for _, resourceID := range resources {
		record, found, err := currentResourceGeneration(workspace.Path, resourceID)
		if err != nil || !found || record.Status != "idle" || record.IdleDeadlineAt != deadline.Format(time.RFC3339Nano) {
			t.Fatalf("pre-deadline generation changed for %s: found=%v err=%v run=%#v", resourceID, found, err, record)
		}
	}

	manager.now = func() time.Time { return deadline }
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		waitForFakeSessionState(t, fake, record.AgentHubSessionID, "stopped")
		waitForIdleGenerationSuspended(t, manager, workspace.Path, record.ID)
	}
	if got := fakeStopCalls(fake); got != len(resources) {
		t.Fatalf("each resource kind must be stopped exactly once: got %d, want %d", got, len(resources))
	}
	for _, expected := range records {
		resourceID := expected.ResourceID
		current, found, err := currentResourceGeneration(workspace.Path, resourceID)
		if err != nil || !found || current.GenerationID != expected.GenerationID {
			t.Fatalf("idle generation was not retained current for %s: current=%#v found=%v err=%v", resourceID, current, found, err)
		}
	}
}

func TestResourceIdleSleepDoesNotStopActiveApprovalOrMailboxWork(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	active := idleTestGeneration(workspace, "project1", "gen-active", "ses-active", deadline)
	approval := idleTestGeneration(workspace, "project1.task1", "gen-approval", "ses-approval", deadline)
	seedIdleTestGeneration(t, fake, workspace, active, "running")
	seedIdleTestGeneration(t, fake, workspace, approval, "waiting_approval")
	fake.mu.Lock()
	session := fake.sessions[approval.AgentHubSessionID]
	session.PendingApprovalIDs = []string{"approval-1"}
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	queued := idleTestGeneration(workspace, "project1", "gen-queued", "ses-queued", deadline)
	queued.Generation = 2
	queued.GenerationID = "gen-run-queued"
	queued.SourceExternalID = "project1/run-queued"
	// Give the queued case a distinct resource by using the Workspace mailbox
	// directly; the ready generation still has to remain untouched.
	queued.ResourceID = "workspace"
	seedIdleTestGeneration(t, fake, workspace, queued, "ready")
	if _, err := acceptMailboxMessage(workspace.Path, queued.ResourceID, resourceMessageRequest{Text: "waiting", Mode: resourceMessageModeEnqueue}); err != nil {
		t.Fatal(err)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fakeStopCalls(fake); got != 0 {
		t.Fatalf("active/approval/pending work was stopped: stop calls=%d", got)
	}
	fake.mu.Lock()
	activeState := fake.sessions[active.AgentHubSessionID].State
	approvalState := fake.sessions[approval.AgentHubSessionID].State
	queuedState := fake.sessions[queued.AgentHubSessionID].State
	fake.mu.Unlock()
	if activeState != "running" || approvalState != "waiting_approval" || queuedState != "running" {
		t.Fatalf("work state was not preserved/delivered: active=%s approval=%s queued=%s", activeState, approvalState, queuedState)
	}
}

func TestResourceIdleSleepMessageAfterStopResumesSameGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	record := idleTestGeneration(workspace, "project1.task1", "gen-race", "ses-race", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	var once sync.Once
	fake.stopHook = func(sessionID string) {
		if sessionID != record.AgentHubSessionID {
			return
		}
		once.Do(func() { close(stopStarted) })
		<-releaseStop
	}
	pollDone := make(chan error, 1)
	go func() { pollDone <- manager.pollAgentHubSessions(context.Background()) }()
	<-stopStarted

	accepted := make(chan resourceMailboxMessage, 1)
	go func() {
		message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{Text: "after sleep", Mode: resourceMessageModeEnqueue})
		if err != nil {
			accepted <- resourceMailboxMessage{LastError: err.Error()}
			return
		}
		accepted <- message
	}()
	select {
	case message := <-accepted:
		t.Fatalf("message bypassed the in-flight Stop barrier: %#v", message)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseStop)
	message := <-accepted
	if err := <-pollDone; err != nil {
		t.Fatal(err)
	}
	if message.LastError != "" {
		t.Fatal(message.LastError)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
		if err != nil || !found || current.Generation != record.Generation || current.GenerationID != record.GenerationID {
			return false
		}
		stored, found, err := mailboxMessageByID(workspace.Path, message.ID)
		return err == nil && found && stored.Status == resourceMessageDelivered && stored.GenerationID == current.GenerationID
	})
	if message.GenerationID != record.GenerationID {
		t.Fatalf("message was not delivered to the retained generation: %#v", message)
	}
	if fakeStopCalls(fake) != 1 {
		t.Fatalf("Stop was duplicated during the message race: %d", fakeStopCalls(fake))
	}
	fake.mu.Lock()
	resumeSeen := false
	for _, action := range fake.actions {
		if action == "resume" {
			resumeSeen = true
		}
	}
	fake.mu.Unlock()
	if !resumeSeen {
		t.Fatal("message did not resume the retained Session")
	}
}

func TestResourceIdleSleepRecoversOverdueDeadlineAfterRestart(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	record := idleTestGeneration(workspace, "project1.task1", "gen-restart-idle", "ses-restart-idle", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	restarted.now = func() time.Time { return deadline.Add(time.Minute) }
	if err := restarted.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForFakeSessionState(t, fake, record.AgentHubSessionID, "stopped")
	stored := waitForIdleGenerationSuspended(t, restarted, workspace.Path, record.ID)
	if got := fakeStopCalls(fake); got != 1 {
		t.Fatalf("overdue restart recovery issued %d Stop calls, want 1", got)
	}
	if stored.Status != "idle-suspended" || !stored.IdleSleepStopRequested {
		t.Fatalf("restart recovery did not complete the durable sleep lifecycle: %#v", stored)
	}
}

func TestResourceIdleSleepStableSuspensionSkipsRetirement(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := idleTestGeneration(workspace, "project1.task1", "gen-stable-sleep", "ses-stable-sleep", time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	record.Status = "idle-suspended"
	record.IdleSleepStopRequested = true
	record.LifecycleReceipt = &GenerationLifecycleReceipt{
		Operation: GenerationOperationStopSession,
		State:     GenerationReceiptSucceeded,
	}
	seedIdleTestGeneration(t, fake, workspace, record, "stopped")

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	getSessionCalls := fake.getSessionCalls
	stopCalls := fake.stopCalls
	session := fake.sessions[record.AgentHubSessionID]
	fake.mu.Unlock()
	if getSessionCalls != 0 || stopCalls != 0 {
		t.Fatalf("stable idle suspension re-entered retirement: getSessionCalls=%d stopCalls=%d", getSessionCalls, stopCalls)
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || !resourceIdleSuspensionStable(current, session) {
		t.Fatalf("stable idle suspension changed: current=%#v found=%v err=%v", current, found, err)
	}
}

func TestResourceIdleSleepRetriesAmbiguousStopWithoutDuplicateAfterConvergence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.failNextStop = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	record := idleTestGeneration(workspace, "project1.task1", "gen-ambiguous-idle", "ses-ambiguous-idle", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool { return fakeStopCalls(fake) == 1 })
	stored, err := loadGenerationRecord(workspace.Path, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.IdleSleepStopRequested {
		t.Fatalf("ambiguous Stop did not retain the durable retry guard: %#v", stored)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForFakeSessionState(t, fake, record.AgentHubSessionID, "stopped")
	waitForIdleGenerationSuspended(t, manager, workspace.Path, record.ID)
	if got := fakeStopCalls(fake); got != 2 {
		t.Fatalf("ambiguous Stop did not retry exactly once: %d", got)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fakeStopCalls(fake); got != 2 {
		t.Fatalf("converged automatic sleep repeated Stop after Archive: %d", got)
	}
}

func TestResourceIdleSleepSchedulerMigrationResumesCurrentGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	migrateSchedulerV1ForTest(t, puaWorkspace, schedulerV1TestDefinition{
		ID:          "schedule-666666666666666666666666",
		Description: "Inspect the workspace",
		Condition:   "when the workspace needs review",
		Target:      "workspace",
		CreatedAt:   "2026-08-01T00:00:00Z",
		UpdatedAt:   "2026-08-01T00:00:00Z",
	})
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	record := idleTestGeneration(workspace, app.SchedulerResourceID, "gen-idle-scheduler", "ses-idle-scheduler", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var tick resourceMailboxMessage
	waitForRuntimeTest(t, func() bool {
		mailbox, loadErr := loadResourceMailbox(workspace.Path)
		if loadErr != nil {
			return false
		}
		for _, message := range mailbox.Messages {
			if message.ResourceID == app.SchedulerResourceID && message.Type == resourceMessageTypeScheduleMigration &&
				message.Status == resourceMessageDelivered && message.GenerationID == record.GenerationID {
				tick = message
				return true
			}
		}
		return false
	})
	if tick.Role != "system" || tick.SubscribeResult || tick.RequestedMode != resourceMessageModeEnqueue ||
		tick.ActualMode != resourceMessageModeEnqueue || !tick.ModeFrozen {
		t.Fatalf("Scheduler migration mode mapping = %#v", tick)
	}
	current, found, err := currentResourceGeneration(workspace.Path, app.SchedulerResourceID)
	if err != nil || !found || current.Generation != record.Generation || current.GenerationID != tick.GenerationID {
		t.Fatalf("Scheduler did not resume the current generation: current=%#v found=%v tick=%#v err=%v", current, found, tick, err)
	}
	if got := fakeStopCalls(fake); got != 1 {
		t.Fatalf("Scheduler wake duplicated the old generation Stop: %d", got)
	}
}

func TestStoppedCurrentGenerationAfterDaemonRestartResumesOnDemand(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := idleTestGeneration(workspace, "project1.task1", "gen-daemon-recovery", "ses-daemon-recovery", time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	record.Status = "stopped"
	record.IdleSinceAt = ""
	record.IdleDeadlineAt = ""
	record.AgentHubStoppedObserved = true
	seedIdleTestGeneration(t, fake, workspace, record, "stopped")
	accepted, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "after daemon recovery", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	if err := restarted.reconcileWorkspaceMailboxes(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
		if err != nil || !found || current.GenerationID != record.GenerationID {
			return false
		}
		message, found, loadErr := mailboxMessageByID(workspace.Path, accepted.ID)
		return loadErr == nil && found && message.Status == resourceMessageDelivered && message.GenerationID == record.GenerationID
	})
	fake.mu.Lock()
	resumeCount := len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeCount != 1 {
		t.Fatalf("daemon recovery did not resume exactly once: actions=%#v", fake.actions)
	}
}

func TestStoppedSessionResumeTemporaryFailureRetainsMailboxAndGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	record := idleTestGeneration(workspace, "project1.task1", "gen-resume-retry", "ses-resume-retry", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForFakeSessionState(t, fake, record.AgentHubSessionID, "stopped")
	waitForIdleGenerationSuspended(t, manager, workspace.Path, record.ID)
	fake.mu.Lock()
	fake.resumeUpdatesAt = true
	fake.failNextResume = true
	fake.resumeBeforeFailure = true
	fake.mu.Unlock()
	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{Text: "retry me", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	stored, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || stored.Status != resourceMessageQueued || stored.LastErrorCode != "temporarily_undeliverable" {
		t.Fatalf("temporary Resume failure did not retain queued mailbox: found=%v err=%v message=%#v", found, err, stored)
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.GenerationID != record.GenerationID {
		t.Fatalf("temporary Resume failure replaced current generation: current=%#v found=%v err=%v", current, found, err)
	}
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		updated, found, loadErr := mailboxMessageByID(workspace.Path, message.ID)
		return loadErr == nil && found && updated.Status == resourceMessageDelivered && updated.GenerationID == record.GenerationID
	})
	fake.mu.Lock()
	resumeCount := len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeCount != 1 {
		t.Fatalf("ambiguous Resume was replayed after ready observation: %d attempts", resumeCount)
	}
}

func TestStoppedSessionResumeFailurePersistsBackoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	record := idleTestGeneration(workspace, "project1.task1", "gen-resume-backoff", "ses-resume-backoff", now)
	record.Status = "stopped"
	record.IdleSinceAt = ""
	record.IdleDeadlineAt = ""
	record.IdleSleepStopRequested = true
	seedIdleTestGeneration(t, fake, workspace, record, "stopped")
	fake.mu.Lock()
	fake.failNextResume = true
	fake.mu.Unlock()
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "retry after backoff", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err == nil {
		t.Fatal("first temporary Resume failure unexpectedly succeeded")
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.ResumeFailureCount != 1 || current.ResumeRetryAt == "" || current.ResumeLastError == "" {
		t.Fatalf("resume backoff was not persisted: current=%#v found=%v err=%v", current, found, err)
	}
	if retryAt, parseErr := time.Parse(time.RFC3339Nano, current.ResumeRetryAt); parseErr != nil || !retryAt.Equal(now.Add(resumeRetryBase)) {
		t.Fatalf("first retry boundary = %q, parseErr=%v", current.ResumeRetryAt, parseErr)
	}
	fake.mu.Lock()
	resumeCount := len(fake.resumeEnvironments)
	stoppedSession := fake.sessions[record.AgentHubSessionID]
	fake.mu.Unlock()
	if resumeCount != 1 {
		t.Fatalf("first reconciliation issued %d Resume attempts", resumeCount)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	backoffPlan := PlanGeneration(AdaptLegacyGenerationFacts(LegacyGenerationLifecycleInput{
		Generation: current, Session: &stoppedSession, Mailbox: mailbox, Now: now, Revision: current.UpdatedAt,
	}))
	if backoffPlan.Operation != GenerationOperationWaitForSession || backoffPlan.Reason != "resume_backoff" {
		t.Fatalf("persisted retry boundary produced %#v", backoffPlan)
	}
	// Rebuild the manager to prove the retry boundary is recovered from the
	// generation manifest rather than relying on process-local state.
	restarted := newAgentManager(manager.server)
	restarted.now = func() time.Time { return now }
	manager.server.agents = restarted
	manager = restarted

	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	resumeCount = len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeCount != 1 {
		t.Fatalf("Resume retried before durable backoff elapsed: %d attempts", resumeCount)
	}

	now = now.Add(resumeRetryBase)
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		updated, messageFound, loadErr := mailboxMessageByID(workspace.Path, message.ID)
		return loadErr == nil && messageFound && updated.Status == resourceMessageDelivered
	})
	current, found, err = currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.ResumeFailureCount != 0 || current.ResumeRetryAt != "" || current.ResumeLastError != "" {
		t.Fatalf("successful Resume did not clear backoff: current=%#v found=%v err=%v", current, found, err)
	}
	fake.mu.Lock()
	resumeCount = len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeCount != 2 {
		t.Fatalf("Resume attempts after backoff = %d, want 2", resumeCount)
	}
}

func TestManualEndGenerationRetiresAndNextMessageCreatesSuccessor(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := idleTestGeneration(workspace, "project1.task1", "gen-manual-end", "ses-manual-end", time.Now())
	record.Status = "idle"
	record.IdleSinceAt = ""
	record.IdleDeadlineAt = ""
	seedIdleTestGeneration(t, fake, workspace, record, "ready")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/api/workspaces/"+workspace.ID+"/resources/"+record.ResourceID+"/generation/end?generationId="+record.GenerationID, nil)
	manager.server.handleWorkspace(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("end generation response = %d %s", recorder.Code, recorder.Body.String())
	}
	waitForRuntimeTest(t, func() bool {
		_, currentFound, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		return loadErr == nil && !currentFound
	})
	waitForFakeSessionState(t, fake, record.AgentHubSessionID, "archived")
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if !slices.Contains(actions, "stop") {
		t.Fatalf("manual end skipped safe Session retirement: actions=%#v", actions)
	}

	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{Text: "open the successor", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, currentFound, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !currentFound || current.Generation <= record.Generation || current.GenerationID == record.GenerationID {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.AgentHubAgentName != record.AgentHubAgentName {
		t.Fatalf("successor did not preserve the resource binding: current=%#v found=%v err=%v", current, found, err)
	}
}

func TestManualEndGenerationRejectsActiveTurn(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := idleTestGeneration(workspace, "project1.task1", "gen-manual-active", "ses-manual-active", time.Now())
	record.Status = "running"
	record.CurrentTurnID = "turn-active"
	seedIdleTestGeneration(t, fake, workspace, record, "running")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/api/workspaces/"+workspace.ID+"/resources/"+record.ResourceID+"/generation/end?generationId="+record.GenerationID, nil)
	manager.server.handleWorkspace(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "active_turn") {
		t.Fatalf("active Turn response = %d %s", recorder.Code, recorder.Body.String())
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.GenerationID != record.GenerationID || current.ReplacementPending {
		t.Fatalf("active generation was mutated: current=%#v found=%v err=%v", current, found, err)
	}
}

func TestStoppedSessionResumeTerminalFailureReplacesGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	record := idleTestGeneration(workspace, "project1.task1", "gen-resume-terminal", "ses-resume-terminal", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForFakeSessionState(t, fake, record.AgentHubSessionID, "stopped")
	waitForIdleGenerationSuspended(t, manager, workspace.Path, record.ID)
	fake.mu.Lock()
	fake.failNextResume = true
	fake.resumeErrorStatus = 422
	fake.resumeErrorCode = "provider_resume_unavailable"
	fake.resumeErrorMessage = "provider does not support session resume/load"
	fake.mu.Unlock()
	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{Text: "replace me", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !found || current.Generation <= record.Generation {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	if message.GenerationID == record.GenerationID {
		t.Fatalf("terminal Resume failure delivered to the unrecoverable generation: %#v", message)
	}
	waitForFakeSessionState(t, fake, record.AgentHubSessionID, "archived")
}

func TestStoppedSessionMissingRetiresAndReplacesGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := idleTestGeneration(workspace, "project1.task1", "gen-resume-missing", "ses-resume-missing", time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	record.Status = "stopped"
	record.IdleSinceAt = ""
	record.IdleDeadlineAt = ""
	record.AgentHubStoppedObserved = true
	seedIdleTestGeneration(t, fake, workspace, record, "stopped")
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "replace missing", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	delete(fake.sessions, record.AgentHubSessionID)
	fake.mu.Unlock()
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !found || current.Generation <= record.Generation {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	fake.mu.Lock()
	resumeAttempts := len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeAttempts != 0 {
		t.Fatalf("missing Session was resumed before replacement: %d attempts", resumeAttempts)
	}
}

func TestStoppedSessionBindingChangeWinsBeforeResume(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.extraAgents = []string{"replacement-agent"}
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	deadline := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	manager.now = func() time.Time { return deadline }
	record := idleTestGeneration(workspace, "project1.task1", "gen-resume-binding", "ses-resume-binding", deadline)
	seedIdleTestGeneration(t, fake, workspace, record, "ready")
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForIdleGenerationSuspended(t, manager, workspace.Path, record.ID)

	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	binding := app.AgentBinding{Kind: "agent", Name: "replacement-agent"}
	if _, err := puaWorkspace.SetResourceAgentBinding(record.ResourceID, binding); err != nil {
		t.Fatal(err)
	}
	if err := manager.resourceBindingChanged(context.Background(), workspace, record.ResourceID, binding); err != nil {
		t.Fatal(err)
	}
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "after binding change", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !found || current.Generation <= record.Generation {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	fake.mu.Lock()
	resumeAttempts := len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeAttempts != 0 {
		t.Fatalf("binding change resumed the old Session: %d attempts", resumeAttempts)
	}
}

func TestStoppedSessionArchivedRetiresAndReplacesGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := idleTestGeneration(workspace, "project1.task1", "gen-resume-archived", "ses-resume-archived", time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	record.Status = "idle-suspended"
	record.IdleSleepStopRequested = true
	seedIdleTestGeneration(t, fake, workspace, record, "archived")
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "replace archived", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr == nil && found && current.Generation > record.Generation {
			updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
			if messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, currentFound, currentErr := currentResourceGeneration(workspace.Path, record.ResourceID)
	updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
	if !currentFound || currentErr != nil || current.Generation <= record.Generation || !messageFound || messageErr != nil || updated.Status != resourceMessageDelivered || updated.GenerationID != current.GenerationID {
		fake.mu.Lock()
		actions := append([]string(nil), fake.actions...)
		fake.mu.Unlock()
		t.Fatalf("archived Session did not replace: current=%#v found=%v err=%v message=%#v found=%v err=%v actions=%#v", current, currentFound, currentErr, updated, messageFound, messageErr, actions)
	}
	fake.mu.Lock()
	resumeAttempts := len(fake.resumeEnvironments)
	fake.mu.Unlock()
	if resumeAttempts != 0 {
		t.Fatalf("archived Session was resumed before replacement: %d attempts", resumeAttempts)
	}
}
