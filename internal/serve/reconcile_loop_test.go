package serve

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func runtimeFakeListCalls(fake *runtimeFakeAgentHub) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.listCalls
}

func TestReconcileRequestsCoalesce(t *testing.T) {
	manager := newAgentManager(&server{})
	manager.requestReconcile(reconcileAgentHub)
	manager.requestReconcile(reconcileNotifications | reconcileScheduler)
	request := manager.takeReconcileRequests()
	if request&reconcileAgentHub == 0 || request&reconcileNotifications == 0 || request&reconcileScheduler == 0 {
		t.Fatalf("coalesced reconcile request = %08b", request)
	}
	select {
	case <-manager.reconcileWake:
	default:
		t.Fatal("coalesced reconcile request did not wake the loop")
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("coalesced reconcile request emitted more than one wake token")
	default:
	}
}

func TestColdAuditKeepsSchedulerWorkThatBecomesDue(t *testing.T) {
	startedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	deadline := startedAt.Add(time.Second)
	finishedAt := deadline.Add(time.Nanosecond)

	request, nextDeadline := finishColdAuditRequests(reconcileColdAudit, deadline, finishedAt)

	if request&reconcileScheduler == 0 {
		t.Fatalf("post-audit reconcile request = %08b, want Scheduler", request)
	}
	if !nextDeadline.IsZero() {
		t.Fatalf("post-audit Scheduler deadline = %v, want zero after promoting it to a request", nextDeadline)
	}
}

func TestColdAuditPreservesRequestedSchedulerWork(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Minute)
	request := reconcileColdAudit | reconcileAgentHub | reconcileMailboxes | reconcileNotifications | reconcileScheduler

	request, nextDeadline := finishColdAuditRequests(request, deadline, now)

	if request != reconcileColdAudit|reconcileScheduler {
		t.Fatalf("post-audit reconcile request = %08b, want cold audit and Scheduler", request)
	}
	if !nextDeadline.Equal(deadline) {
		t.Fatalf("post-audit Scheduler deadline = %v, want %v", nextDeadline, deadline)
	}
}

func TestGenerationNeedsFastReconcileSkipsStableAndSelectsWork(t *testing.T) {
	manager := newAgentManager(&server{})
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	record := generationRecord{
		ID: "gen-stable", GenerationID: "gen-stable", AgentHubSessionID: "ses-stable",
		Status: "idle", CurrentTurnID: "", CompletionSessionID: "ses-stable", CompletionCursor: 7,
		IdleDeadlineAt: now.Add(time.Minute).Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	session := agentHubSession{ID: "ses-stable", State: "ready", LastEventID: 7, UpdatedAt: record.UpdatedAt}
	rt := &agentRuntime{record: record, agentHubState: "ready"}
	if generationNeedsFastReconcile(manager, rt, record, session, true) {
		t.Fatal("stable idle generation entered the fast reconcile set")
	}

	due := record
	due.IdleDeadlineAt = now.Format(time.RFC3339Nano)
	if !generationNeedsFastReconcile(manager, rt, due, session, true) {
		t.Fatal("due idle deadline did not enter the fast reconcile set")
	}
	changed := session
	changed.State = "running"
	changed.CurrentTurnID = "turn-new"
	if !generationNeedsFastReconcile(manager, rt, record, changed, true) {
		t.Fatal("upstream active Turn did not enter the fast reconcile set")
	}
	pending := record
	pending.ReplacementPending = true
	if !generationNeedsFastReconcile(manager, rt, pending, session, true) {
		t.Fatal("pending lifecycle did not enter the fast reconcile set")
	}
}

func TestTurnCompletionWakesNotificationsAndScheduler(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-completion-wake", GenerationID: "gen-completion-wake", WorkspaceID: workspace.ID, ResourceID: "project1",
		AgentHubSessionID: "ses-completion-wake", CompletionSessionID: "ses-completion-wake", Status: "idle",
		CreatedAt: "2026-08-19T00:00:00Z", UpdatedAt: "2026-08-19T00:00:00Z",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	manager.registerRuntime(rt)
	_ = manager.takeReconcileRequests()
	select {
	case <-manager.reconcileWake:
	default:
	}
	rt.recordTurnCompletionHistory(agentHubSession{ID: record.AgentHubSessionID, State: "ready"}, []agentHubEvent{{
		ID: 1, Time: "2026-08-19T00:00:01Z", Type: "turn.completed", SessionID: record.AgentHubSessionID, TurnID: "turn-completion-wake",
	}}, 1)
	request := manager.takeReconcileRequests()
	if request&reconcileNotifications == 0 || request&reconcileScheduler == 0 || request&reconcileAgentHub == 0 {
		t.Fatalf("Turn completion reconcile request = %08b", request)
	}
}

func TestReconcileLoopBacksOffWhenStableAndAcceleratesWhenActive(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = time.Now
	manager.activePollInterval = 15 * time.Millisecond
	manager.stablePollInterval = 100 * time.Millisecond
	manager.coldAuditInterval = time.Hour
	manager.mailboxRetryInterval = time.Hour
	manager.notificationInterval = time.Hour
	manager.schedulerFallback = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	manager.runBackground(func() { manager.runReconcileLoop(ctx) })
	waitForRuntimeTest(t, func() bool { return runtimeFakeListCalls(fake) >= 1 })
	stableStart := runtimeFakeListCalls(fake)
	time.Sleep(240 * time.Millisecond)
	stableCalls := runtimeFakeListCalls(fake) - stableStart
	if stableCalls < 1 || stableCalls > 4 {
		cancel()
		manager.waitBackground()
		t.Fatalf("stable reconcile list calls = %d, want 1..4", stableCalls)
	}

	record := generationRecord{
		ID: "gen-active-cadence", GenerationID: "gen-active-cadence", WorkspaceID: workspace.ID, ResourceID: "project1",
		AgentHubSessionID: "ses-active-cadence", SourceExternalID: workspace.ID + "/active-cadence", Status: "running",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	session := agentHubSession{
		ID: "ses-active-cadence", State: "running", CurrentTurnID: "turn-active-cadence", UpdatedAt: record.UpdatedAt,
		Source: &agentHubSource{App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		cancel()
		manager.waitBackground()
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	_, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		cancel()
		manager.waitBackground()
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, client)
	rt.agentHubState = "running"
	activeStart := runtimeFakeListCalls(fake)
	manager.registerRuntime(rt)
	waitForRuntimeTest(t, func() bool { return runtimeFakeListCalls(fake)-activeStart >= 4 })

	cancel()
	manager.waitBackground()
}

func TestColdAuditDiscoversGenerationAbsentFromRuntimeIndex(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = time.Now
	manager.activePollInterval = time.Hour
	manager.stablePollInterval = time.Hour
	manager.coldAuditInterval = 30 * time.Millisecond
	manager.mailboxRetryInterval = time.Hour
	manager.notificationInterval = time.Hour
	manager.schedulerFallback = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	manager.runBackground(func() { manager.runReconcileLoop(ctx) })
	waitForRuntimeTest(t, func() bool { return runtimeFakeListCalls(fake) >= 1 })
	record := generationRecord{
		ID: "gen-cold-discovery", GenerationID: "gen-cold-discovery", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses-cold-discovery", SourceExternalID: workspace.ID + "/cold-discovery", Status: "stopped",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: "ses-cold-discovery", State: "stopped", UpdatedAt: record.UpdatedAt,
		Source: &agentHubSource{App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
	})
	if manager.runtimeByID(record.ID) != nil {
		cancel()
		manager.waitBackground()
		t.Fatal("out-of-process generation was registered before the cold audit")
	}
	waitForRuntimeTest(t, func() bool { return manager.runtimeByID(record.ID) != nil })
	cancel()
	manager.waitBackground()
}

func TestIdleDeadlineWakesStableReconcileLoop(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = time.Now
	manager.activePollInterval = time.Hour
	manager.stablePollInterval = time.Hour
	manager.coldAuditInterval = time.Hour
	manager.mailboxRetryInterval = time.Hour
	manager.notificationInterval = time.Hour
	manager.schedulerFallback = time.Hour
	now := time.Now()
	record := generationRecord{
		ID: "gen-idle-deadline", GenerationID: "gen-idle-deadline", Generation: 1, WorkspaceID: workspace.ID, ResourceID: "project1",
		AgentHubSessionID: "ses-idle-deadline", SourceExternalID: workspace.ID + "/idle-deadline", Status: "idle",
		CompletionSessionID: "ses-idle-deadline", IdleSinceAt: now.Format(time.RFC3339Nano),
		IdleDeadlineAt: now.Add(250 * time.Millisecond).Format(time.RFC3339Nano), CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: "ses-idle-deadline", State: "ready", UpdatedAt: record.UpdatedAt,
		Source: &agentHubSource{App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
	})

	ctx, cancel := context.WithCancel(context.Background())
	manager.runBackground(func() { manager.runReconcileLoop(ctx) })
	waitForRuntimeTest(t, func() bool { return runtimeFakeListCalls(fake) >= 2 })
	baseline := runtimeFakeListCalls(fake)
	waitForRuntimeTest(t, func() bool { return runtimeFakeListCalls(fake) > baseline })
	cancel()
	manager.waitBackground()
}
