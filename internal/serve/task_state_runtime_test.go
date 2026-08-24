package serve

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func removeTaskOpenerFixture(t *testing.T, workspacePath, resourceID, messageID, state string) {
	t.Helper()
	if _, err := mutateResourceMailboxStoreForResource(workspacePath, resourceID, func(store *resourceMailboxStore) error {
		messages := store.Mailbox.Messages[:0]
		for _, message := range store.Mailbox.Messages {
			if message.ID != messageID {
				messages = append(messages, message)
			}
		}
		store.Mailbox.Messages = messages
		receipts := store.Receipts.Receipts[:0]
		for _, receipt := range store.Receipts.Receipts {
			if receipt.ID != messageID {
				receipts = append(receipts, receipt)
			}
		}
		store.Receipts.Receipts = receipts
		expired := store.Receipts.Expired[:0]
		for _, entry := range store.Receipts.Expired {
			if entry.ID != messageID {
				expired = append(expired, entry)
			}
		}
		store.Receipts.Expired = expired
		if state == resourceMailboxExpiredState {
			store.Receipts.Expired = append(store.Receipts.Expired, resourceMailboxExpiredEntry{
				ID: messageID, ExpiredAt: time.Now().Format(time.RFC3339Nano),
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskMessageAcceptanceWaitsForDeliveryBoundary(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateBlocked, "waiting for input"); err != nil {
		t.Fatal(err)
	}
	message, err := manager.acceptResourceMessageDurable(context.Background(), workspace, "project1.task1", resourceMessageRequest{
		Text: "resume", Role: "user", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateBlocked {
		t.Fatalf("mailbox acceptance changed Task state before delivery: detail=%#v err=%v", detail, err)
	}

	record := generationRecord{
		ID: "task-state-delivery-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-state-delivery", Status: "idle", Title: "Task state delivery",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:2", TaskStateCompletionMarker: "session:1", TaskStateContinuationCount: 2,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	if err := manager.prepareTaskWorkChain(workspace, message, rt); err != nil {
		t.Fatal(err)
	}
	detail, err = puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateInProgress {
		t.Fatalf("delivery boundary did not start Task work: detail=%#v err=%v", detail, err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateChainID != message.ID || updated.TaskStateChainKind != taskStateChainKindOrdinary ||
		updated.TaskStateContinuationCount != 0 || updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("fresh work chain did not consume the stale completion: %#v", updated)
	}

	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateCompleted, ""); err != nil {
		t.Fatal(err)
	}
	message.Status = resourceMessageDelivering
	if err := manager.prepareTaskWorkChain(workspace, message, rt); err != nil {
		t.Fatal(err)
	}
	detail, err = puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateCompleted {
		t.Fatalf("delivery retry restarted an already handled Turn: detail=%#v err=%v", detail, err)
	}
}

func TestScheduledTaskOccurrenceIsOneShot(t *testing.T) {
	tests := []struct {
		name         string
		guard        string
		initial      app.TaskState
		initialNote  string
		receiptState string
	}{
		{name: "expired opener receipt", initial: app.TaskStateBlocked, initialNote: "waiting for the schedule", receiptState: resourceMailboxExpiredState},
		{name: "pruned opener receipt", guard: "the release branch is green", initial: app.TaskStateInProgress},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := puaWorkspace.SetTaskState("project1.task1", test.initial, test.initialNote); err != nil {
				t.Fatal(err)
			}

			record := generationRecord{
				ID: "scheduled-task-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
				Generation: 1, GenerationID: "gen-scheduled-task", Status: "stopped", ReplacementPending: true,
				Title: "Scheduled task", CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
			}
			if err := saveGenerationRecord(workspace.Path, record); err != nil {
				t.Fatal(err)
			}
			rt := newAgentHubRuntime(manager, workspace, record, nil)
			at := manager.now()
			next := time.Time{}
			if test.guard != "" {
				next = at.Add(time.Hour)
			}
			prepared, err := newNativeScheduler(manager, workspace).prepareOccurrence(app.Schedule{
				ID: "schedule-task", Revision: 1, Description: "Check release", Condition: "at the scheduled time",
				Guard: test.guard, Target: record.ResourceID,
			}, at, at, next, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prepared.Text, "in_progress") {
				t.Fatalf("occurrence prompt still delegates Task continuation control:\n%s", prepared.Text)
			}
			if test.guard != "" && !strings.Contains(prepared.Text, "If it is false, end this Turn") {
				t.Fatalf("guard false protocol is missing:\n%s", prepared.Text)
			}
			opener, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
				ID: prepared.MessageID, ResourceID: prepared.Target, Text: prepared.Text,
				RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
				Type: resourceMessageTypeScheduleOccurrence, Causation: cloneMailboxCausation(prepared.Causation),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.prepareTaskWorkChain(workspace, opener, rt); err != nil {
				t.Fatal(err)
			}
			if updated := rt.snapshotGeneration(); updated.TaskStateChainKind != taskStateChainKindScheduleOccurrence {
				t.Fatalf("scheduled opener kind was not durable: %#v", updated)
			}
			detail, err := puaWorkspace.Resource(record.ResourceID)
			if err != nil || detail.State != test.initial || detail.StateNote != test.initialNote {
				t.Fatalf("scheduled opener changed Task state: detail=%#v err=%v", detail, err)
			}
			if _, err := updateMailboxMessage(workspace.Path, opener.ID, func(current *resourceMailboxMessage) {
				current.Status = resourceMessageDelivered
				current.GenerationID = record.GenerationID
				current.TurnID = "turn-scheduled"
				current.DeliveredAt = time.Now().Format(time.RFC3339Nano)
			}); err != nil {
				t.Fatal(err)
			}
			stored, found, err := mailboxMessageByID(workspace.Path, opener.ID)
			if err != nil || !found || !stored.receipt || !isScheduleOccurrenceMessage(stored) {
				t.Fatalf("durable scheduled opener = %#v, found=%v err=%v", stored, found, err)
			}
			removeTaskOpenerFixture(t, workspace.Path, record.ResourceID, opener.ID, test.receiptState)
			_, found, err = mailboxMessageByID(workspace.Path, opener.ID)
			if test.receiptState == resourceMailboxExpiredState {
				var apiErr *resourceAPIError
				if found || !errors.As(err, &apiErr) || apiErr.Code != "message_receipt_expired" {
					t.Fatalf("expired opener lookup = found=%v err=%v", found, err)
				}
			} else if found || err != nil {
				t.Fatalf("pruned opener lookup = found=%v err=%v", found, err)
			}
			persisted, err := loadGenerationRecord(workspace.Path, record.ID)
			if err != nil || persisted.TaskStateChainKind != taskStateChainKindScheduleOccurrence {
				t.Fatalf("reloaded scheduled chain kind = %q, err=%v", persisted.TaskStateChainKind, err)
			}
			rt = newAgentHubRuntime(manager, workspace, persisted, nil)
			marker := "session-scheduled:2"
			if _, err := rt.mutateGeneration(func(current *generationRecord) {
				current.CompletionMarker = marker
				current.CompletionTurnID = "turn-scheduled"
			}); err != nil {
				t.Fatal(err)
			}
			if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
				t.Fatal(err)
			}
			updated := rt.snapshotGeneration()
			if updated.TaskStateCompletionMarker != marker || updated.TaskStateContinuationCount != 0 {
				t.Fatalf("scheduled completion was not handled once: %#v", updated)
			}
			mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
			if err != nil || len(mailbox.Messages) != 0 {
				t.Fatalf("scheduled completion enqueued continuation: mailbox=%#v err=%v", mailbox, err)
			}
		})
	}
}

func TestScheduledTaskOccurrenceSteerPreservesOneShotChain(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	const waitingNote = "waiting for the scheduled check"
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateWaiting, waitingNote); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{
		ID: "scheduled-steer-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-scheduled-steer", AgentHubSessionID: "ses-scheduled-steer",
		AgentHubAgentName: "fake-agent", SourceExternalID: workspace.ID + "/scheduled-steer",
		Status: "idle", CompletionMarker: "session-before-schedule:2",
		TaskStateCompletionMarker: "session-before-schedule:1", TaskStateContinuationCount: 2,
		CreatedAt: now, UpdatedAt: now,
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: "ready", UpdatedAt: now,
	})
	opener, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "schedule-occurrence-steer", ResourceID: record.ResourceID, Text: "Run the scheduled check",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
		Type: resourceMessageTypeScheduleOccurrence,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeScheduleOccurrence, SourceResourceID: app.SchedulerResourceID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	rt := manager.runtimeByID(record.ID)
	if rt == nil {
		t.Fatal("scheduled occurrence did not attach its runtime")
	}
	afterOpener := rt.snapshotGeneration()
	if afterOpener.TaskStateChainID != opener.ID || afterOpener.TaskStateChainKind != taskStateChainKindScheduleOccurrence ||
		afterOpener.TaskStateContinuationCount != 0 ||
		afterOpener.TaskStateCompletionMarker != afterOpener.CompletionMarker {
		t.Fatalf("scheduled opener did not establish its one-shot chain: %#v", afterOpener)
	}
	detail, err := puaWorkspace.Resource(record.ResourceID)
	if err != nil || detail.State != app.TaskStateWaiting || detail.StateNote != waitingNote {
		t.Fatalf("scheduled opener changed Task workflow state: detail=%#v err=%v", detail, err)
	}

	const turnID = "turn-scheduled-steer"
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = turnID
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	steer, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
		Text: "Browser follow-up for the current Turn", Role: "user", Mode: resourceMessageModeSteer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if steer.Status != resourceMessageDelivered || steer.ActualMode != resourceMessageModeSteer || steer.TurnID != turnID {
		t.Fatalf("browser message was not delivered into the scheduled Turn: %#v", steer)
	}
	afterSteer := rt.snapshotGeneration()
	detailAfterSteer, detailErr := puaWorkspace.Resource(record.ResourceID)

	const completionMarker = "session-scheduled-steer:8"
	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		current.Status = "stopped"
		current.CurrentTurnID = ""
		current.LastTurnID = turnID
		current.CompletionMarker = completionMarker
		current.CompletionTurnID = turnID
		current.ReplacementPending = true
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	afterCompletion := rt.snapshotGeneration()
	mailbox, mailboxErr := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if afterSteer.TaskStateChainID != afterOpener.TaskStateChainID ||
		afterSteer.TaskStateChainKind != afterOpener.TaskStateChainKind ||
		afterSteer.TaskStateContinuationCount != afterOpener.TaskStateContinuationCount ||
		afterSteer.TaskStateCompletionMarker != afterOpener.TaskStateCompletionMarker ||
		detailErr != nil || detailAfterSteer.State != app.TaskStateWaiting || detailAfterSteer.StateNote != waitingNote ||
		afterCompletion.TaskStateCompletionMarker != completionMarker ||
		afterCompletion.TaskStateChainKind != taskStateChainKindScheduleOccurrence ||
		afterCompletion.TaskStateContinuationCount != afterOpener.TaskStateContinuationCount ||
		mailboxErr != nil || len(mailbox.Messages) != 0 {
		t.Fatalf("active steer changed scheduled one-shot handling: opener=%#v steer=%#v detail=%#v detailErr=%v completion=%#v mailbox=%#v mailboxErr=%v",
			afterOpener, afterSteer, detailAfterSteer, detailErr, afterCompletion, mailbox, mailboxErr)
	}

	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		current.Status = "idle"
		current.ReplacementPending = false
	}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session = fake.sessions[record.AgentHubSessionID]
	session.State = "ready"
	session.CurrentTurnID = ""
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	ordinary, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
		Text: "Start the next ordinary Turn", Role: "user", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	afterOrdinary := rt.snapshotGeneration()
	detail, err = puaWorkspace.Resource(record.ResourceID)
	if ordinary.Status != resourceMessageDelivered || ordinary.ActualMode != resourceMessageModeEnqueue ||
		afterOrdinary.TaskStateChainID != ordinary.ID || afterOrdinary.TaskStateChainKind != taskStateChainKindOrdinary ||
		afterOrdinary.TaskStateContinuationCount != 0 ||
		detail.State != app.TaskStateInProgress || err != nil {
		t.Fatalf("genuine next-Turn opener did not start a fresh chain: message=%#v generation=%#v detail=%#v err=%v",
			ordinary, afterOrdinary, detail, err)
	}
}

func TestOrdinaryTaskCompletionStillEnqueuesContinuation(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateBlocked, "waiting for input"); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "ordinary-task-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-ordinary-task", Status: "stopped", ReplacementPending: true,
		Title: "Ordinary task", CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	opener, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{
		Text: "PUA scheduled occurrence lookalike with no structured type", Role: "user", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareTaskWorkChain(workspace, opener, rt); err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource(record.ResourceID)
	if err != nil || detail.State != app.TaskStateInProgress {
		t.Fatalf("ordinary opener did not start Task work: detail=%#v err=%v", detail, err)
	}
	if _, err := updateMailboxMessage(workspace.Path, opener.ID, func(current *resourceMailboxMessage) {
		current.Status = resourceMessageDelivered
		current.GenerationID = record.GenerationID
		current.TurnID = "turn-ordinary"
		current.DeliveredAt = time.Now().Format(time.RFC3339Nano)
	}); err != nil {
		t.Fatal(err)
	}
	afterOpener := rt.snapshotGeneration()
	if afterOpener.TaskStateChainKind != taskStateChainKindOrdinary {
		t.Fatalf("ordinary opener kind was not durable: %#v", afterOpener)
	}
	removeTaskOpenerFixture(t, workspace.Path, record.ResourceID, opener.ID, "")
	if _, found, err := mailboxMessageByID(workspace.Path, opener.ID); err != nil || found {
		t.Fatalf("removed ordinary opener lookup = found=%v err=%v", found, err)
	}
	persisted, err := loadGenerationRecord(workspace.Path, record.ID)
	if err != nil || persisted.TaskStateChainKind != taskStateChainKindOrdinary {
		t.Fatalf("reloaded ordinary chain kind = %q, err=%v", persisted.TaskStateChainKind, err)
	}
	rt = newAgentHubRuntime(manager, workspace, persisted, nil)
	marker := "session-ordinary:2"
	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		current.CompletionMarker = marker
		current.CompletionTurnID = "turn-ordinary"
	}); err != nil {
		t.Fatal(err)
	}
	manager.registerRuntime(rt)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateCompletionMarker != marker || updated.TaskStateChainKind != taskStateChainKindOrdinary ||
		updated.TaskStateContinuationCount != 1 {
		t.Fatalf("ordinary completion did not follow continuation policy: %#v", updated)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].Type != resourceMessageTypeTaskContinuation {
		t.Fatalf("ordinary completion mailbox = %#v, err=%v", mailbox, err)
	}
}

func TestTaskContinuationInheritsPersistedChainKind(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{
		ID: "task-continuation-kind-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-continuation-kind", Status: "idle", Title: "Task continuation kind",
		CreatedAt: now, UpdatedAt: now, CompletionMarker: "session:2", TaskStateCompletionMarker: "session:1",
		TaskStateChainID: "ordinary-opener", TaskStateChainKind: taskStateChainKindOrdinary, TaskStateContinuationCount: 1,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	continuation := resourceMailboxMessage{
		ID: "generated-task-continuation", ResourceID: record.ResourceID, Type: resourceMessageTypeTaskContinuation,
		Status: resourceMessageQueued, RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
	}
	if err := manager.prepareTaskWorkChain(workspace, continuation, rt); err != nil {
		t.Fatal(err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateChainID != record.TaskStateChainID || updated.TaskStateChainKind != record.TaskStateChainKind ||
		updated.TaskStateContinuationCount != record.TaskStateContinuationCount ||
		updated.TaskStateCompletionMarker != record.TaskStateCompletionMarker {
		t.Fatalf("generated continuation replaced its work chain: before=%#v after=%#v", record, updated)
	}
}

func TestLegacyTaskChainKindIsInferredFromReceipt(t *testing.T) {
	tests := []struct {
		name      string
		scheduled bool
		wantKind  taskStateChainKind
		wantCount int
	}{
		{name: "scheduled occurrence", scheduled: true, wantKind: taskStateChainKindScheduleOccurrence},
		{name: "ordinary opener", wantKind: taskStateChainKindOrdinary, wantCount: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateInProgress, ""); err != nil {
				t.Fatal(err)
			}
			message := resourceMailboxMessage{
				ID: "legacy-chain-opener", ResourceID: "project1.task1", Text: "Open legacy chain", Role: "user",
				Status: resourceMessageQueued, RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
			}
			if test.scheduled {
				message.Type = resourceMessageTypeScheduleOccurrence
				message.Causation = &resourceMessageCausation{
					Type: resourceMessageTypeScheduleOccurrence, SourceResourceID: app.SchedulerResourceID,
				}
			}
			var opener resourceMailboxMessage
			if test.scheduled {
				opener, err = acceptGeneratedMailboxMessage(workspace.Path, message)
			} else {
				opener, err = acceptMailboxMessage(workspace.Path, message.ResourceID, resourceMessageRequest{
					Text: message.Text, Role: message.Role, Mode: message.ActualMode,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := updateMailboxMessage(workspace.Path, opener.ID, func(current *resourceMailboxMessage) {
				current.Status = resourceMessageDelivered
				current.GenerationID = "gen-legacy-chain-kind"
				current.TurnID = "turn-legacy"
				current.DeliveredAt = time.Now().Format(time.RFC3339Nano)
			}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().Format(time.RFC3339Nano)
			record := generationRecord{
				ID: "legacy-chain-kind-gen", WorkspaceID: workspace.ID, ResourceID: opener.ResourceID,
				Generation: 1, GenerationID: "gen-legacy-chain-kind", Status: "stopped", ReplacementPending: true,
				Title: "Legacy chain kind", CreatedAt: now, UpdatedAt: now,
				CompletionMarker: "session-legacy:2", CompletionTurnID: "turn-legacy", TaskStateChainID: opener.ID,
			}
			if err := saveGenerationRecord(workspace.Path, record); err != nil {
				t.Fatal(err)
			}
			rt := newAgentHubRuntime(manager, workspace, record, nil)
			manager.registerRuntime(rt)
			if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
				t.Fatal(err)
			}
			updated := rt.snapshotGeneration()
			if updated.TaskStateChainKind != test.wantKind || updated.TaskStateContinuationCount != test.wantCount ||
				updated.TaskStateCompletionMarker != record.CompletionMarker {
				t.Fatalf("legacy chain inference = %#v", updated)
			}
			persisted, err := loadGenerationRecord(workspace.Path, record.ID)
			if err != nil || persisted.TaskStateChainKind != test.wantKind {
				t.Fatalf("persisted legacy chain kind = %q, err=%v", persisted.TaskStateChainKind, err)
			}
		})
	}
}

func TestTaskTurnCompletionIsSupersededByQueuedWork(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateWaiting, "waiting for an external event"); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "task-state-superseded-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-state-superseded", Status: "idle", Title: "Task state superseded",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:2", CompletionTurnID: "turn-2", TaskStateChainID: "message-old",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	if _, err := acceptMailboxMessage(workspace.Path, "project1.task1", resourceMessageRequest{Text: "new work", Role: "user", Mode: resourceMessageModeEnqueue}); err != nil {
		t.Fatal(err)
	}
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("queued work did not consume stale completion: %#v", updated)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].Type == resourceMessageTypeTaskContinuation {
		t.Fatalf("stale completion generated duplicate work: mailbox=%#v err=%v", mailbox, err)
	}
}

func TestTaskTurnCompletionIsSupersededByActiveTurn(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateInProgress, ""); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "task-state-active-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-state-active", Status: "running", CurrentTurnID: "turn-new",
		LastTurnID: "turn-new", Title: "Task state active",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:2", CompletionTurnID: "turn-old", TaskStateChainID: "message-new",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("active Turn did not consume stale completion: %#v", updated)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 0 {
		t.Fatalf("active Turn received duplicate continuation: mailbox=%#v err=%v", mailbox, err)
	}
}

func TestWaitingTaskWithoutTargetScheduleGetsReminder(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Wake Project", Condition: "when the project should resume", Target: "project1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 300, AnchorAt: time.Now().Add(time.Minute).Format(time.RFC3339Nano)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateWaiting, "waiting for an external event"); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "task-waiting-schedule-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-waiting-schedule", AgentHubSessionID: "session-stopped",
		Status: "stopped", ReplacementPending: true, Title: "Task waiting schedule",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:2", CompletionTurnID: "turn-2", TaskStateChainID: "message-chain",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	manager.registerRuntime(rt)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("waiting Task reminder mailbox = %#v, err=%v", mailbox, err)
	}
	reminder := mailbox.Messages[0]
	if reminder.Type != resourceMessageTypeTaskContinuation || reminder.Role != "system" ||
		reminder.Causation == nil || reminder.Causation.Reason != "task_waiting_without_schedule" ||
		!strings.Contains(reminder.Text, "Scheduler") || !strings.Contains(reminder.Text, record.ResourceID) {
		t.Fatalf("waiting Task reminder = %#v", reminder)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateContinuationCount != 1 || updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("waiting Task reminder was not durably checkpointed: %#v", updated)
	}
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	mailbox, err = loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("duplicate terminal observation created another reminder: mailbox=%#v err=%v", mailbox, err)
	}
}

func TestWaitingTaskWithTargetScheduleNeedsNoReminder(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Resume Task", Condition: "when the external event occurs", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 300, AnchorAt: time.Now().Add(time.Minute).Format(time.RFC3339Nano)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateWaiting, "waiting for an external event"); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "task-waiting-scheduled-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-waiting-scheduled", Status: "idle", Title: "Task waiting scheduled",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:2", CompletionTurnID: "turn-2", TaskStateChainID: "message-chain",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateCompletionMarker != record.CompletionMarker || updated.TaskStateContinuationCount != 0 {
		t.Fatalf("scheduled waiting Task did not close cleanly: %#v", updated)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 0 {
		t.Fatalf("scheduled waiting Task received a reminder: mailbox=%#v err=%v", mailbox, err)
	}
}

func TestWaitingTaskCompletedScheduleRevisionLagGetsReminder(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2099, time.July, 8, 9, 10, 11, 0, time.UTC)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Resume Task once", Condition: "at the configured time", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState(created.Target, app.TaskStateWaiting, "waiting for the one-time check"); err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	runtime := schedulerScheduleRuntime{
		Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
		EffectiveState: app.ScheduleStateCompleted, LastOccurrenceAt: at.Format(time.RFC3339Nano), LastOutcome: schedulerOutcomeAccepted,
	}
	if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
		t.Fatal(err)
	}
	description := "Resume Task once with clearer wording"
	trigger := *created.Trigger
	if _, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
		Description: &description, Trigger: &trigger,
	}); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(time.Minute) }
	snapshot, err := native.Snapshot(manager.now())
	if err != nil {
		t.Fatal(err)
	}
	if taskHasTargetSchedule(snapshot, created.Target) {
		t.Fatalf("completed revision-lag schedule still satisfies waiting Task requirement: %#v", snapshot)
	}

	now := manager.now().Format(time.RFC3339Nano)
	record := generationRecord{
		ID: "task-waiting-completed-schedule-gen", WorkspaceID: workspace.ID, ResourceID: created.Target,
		Generation: 1, GenerationID: "gen-task-waiting-completed-schedule", Status: "stopped", ReplacementPending: true,
		Title: "Task waiting after completed schedule", CreatedAt: now, UpdatedAt: now,
		CompletionMarker: "session:2", CompletionTurnID: "turn-2", TaskStateChainID: "message-chain",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	manager.registerRuntime(rt)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, record.ResourceID)
	if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].Type != resourceMessageTypeTaskContinuation ||
		mailbox.Messages[0].Causation == nil || mailbox.Messages[0].Causation.Reason != "task_waiting_without_schedule" {
		t.Fatalf("waiting Task revision-lag reminder = %#v, err=%v", mailbox, err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateContinuationCount != 1 || updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("waiting Task revision-lag reminder checkpoint = %#v", updated)
	}
}

func TestTaskTargetScheduleUsesRuntimeEffectiveState(t *testing.T) {
	schedule := app.ScheduleSnapshot{
		Schedule:       app.Schedule{Target: "project1.task1", State: app.ScheduleStateActive, Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "2026-08-23T09:00:00Z"}},
		EffectiveState: app.ScheduleStateCompleted,
	}
	if taskHasTargetSchedule(app.SchedulerSnapshot{Schedules: []app.ScheduleSnapshot{schedule}}, "project1.task1") {
		t.Fatal("completed one-time schedule still satisfies waiting Task requirement")
	}
	schedule.EffectiveState = app.ScheduleStateActive
	if !taskHasTargetSchedule(app.SchedulerSnapshot{Schedules: []app.ScheduleSnapshot{schedule}}, "project1.task1") {
		t.Fatal("active target schedule was not recognized")
	}
}

func TestWaitingTaskWithoutScheduleStopsAfterThreeReminders(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateWaiting, "waiting without a wake condition"); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "task-waiting-exhausted-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-waiting-exhausted", Status: "idle", Title: "Task waiting exhausted",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:4", CompletionTurnID: "turn-4", TaskStateContinuationCount: 3,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateError || !strings.Contains(detail.StateNote, "Scheduler") {
		t.Fatalf("waiting Task after retry exhaustion = %#v, err=%v", detail, err)
	}
}

func TestTaskTurnCompletionStopsAfterThreeAutomaticContinuations(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateInProgress, ""); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "task-state-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-state", Status: "idle", Title: "Task state",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:4", CompletionTurnID: "turn-4", TaskStateContinuationCount: 3,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateError {
		t.Fatalf("task state after retry exhaustion = %#v, %v", detail, err)
	}
	updated := rt.snapshotGeneration()
	if updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("completion marker was not handled: %#v", updated)
	}
	// Duplicate terminal observations are no-ops.
	if err := manager.handleTaskTurnCompletionLocked(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
}

func TestTaskContinuationMessageIDIsStablePerAttempt(t *testing.T) {
	first := taskStateContinuationMessageID("project1.task1", "gen-1", "msg-chain", "session:2", 1)
	if first != taskStateContinuationMessageID("project1.task1", "gen-1", "msg-chain", "session:2", 1) {
		t.Fatal("stable continuation input produced different message ids")
	}
	if first == taskStateContinuationMessageID("project1.task1", "gen-1", "msg-chain", "session:2", 2) {
		t.Fatal("different continuation attempts shared a message id")
	}
	waiting := taskWaitingScheduleMessageID("project1.task1", "gen-1", "msg-chain", "session:2", 1)
	if waiting != taskWaitingScheduleMessageID("project1.task1", "gen-1", "msg-chain", "session:2", 1) || waiting == first {
		t.Fatal("waiting schedule reminder ids are not stable and distinct")
	}
}

func TestPauseTaskAfterManualTurnStopIgnoresNonTasks(t *testing.T) {
	_, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	state, err := pauseTaskAfterManualTurnStop(workspace, "project1")
	if err != nil || state != "" {
		t.Fatalf("Project manual stop tried to set Task state: state=%q err=%v", state, err)
	}
	state, err = pauseTaskAfterManualTurnStop(workspace, "workspace")
	if err != nil || state != "" {
		t.Fatalf("Workspace manual stop tried to set Task state: state=%q err=%v", state, err)
	}
}

func TestTaskTurnCompletionOutsideInProgressIsHandledSynchronously(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	record := generationRecord{
		ID: "task-state-idle-gen", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-task-state-idle", Status: "idle", Title: "Task state idle",
		CreatedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		CompletionMarker: "session:2", CompletionTurnID: "turn-2",
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)

	manager.scheduleTaskTurnCompletion(rt, record)

	updated := rt.snapshotGeneration()
	if updated.TaskStateCompletionMarker != record.CompletionMarker {
		t.Fatalf("completion marker was not handled synchronously: %#v", updated)
	}
}

func TestTaskStartFailureExhaustionIsDurable(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateInProgress, ""); err != nil {
		t.Fatal(err)
	}
	message, err := acceptMailboxMessage(workspace.Path, "project1.task1", resourceMessageRequest{Text: "start", Role: "user", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= maxTaskStateRecoveryAttempts; attempt++ {
		exhausted, err := manager.recordTaskStartFailure(workspace, message, &resourceAPIError{Code: "binding_unavailable", Message: "provider unavailable"})
		if err != nil {
			t.Fatal(err)
		}
		if exhausted != (attempt == maxTaskStateRecoveryAttempts) {
			t.Fatalf("attempt %d exhausted = %v", attempt, exhausted)
		}
	}
	stored, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || stored.Status != resourceMessageUndeliverable || stored.LastErrorCode != "task_state_retry_exhausted" {
		t.Fatalf("stored start failure = %#v, found=%v, err=%v", stored, found, err)
	}
	detail, err := puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateError {
		t.Fatalf("task state after start failures = %#v, %v", detail, err)
	}
}
