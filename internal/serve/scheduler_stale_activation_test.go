package serve

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

type staleMetadataActivationCase struct {
	name              string
	triggerType       string
	semanticEdit      string
	due               bool
	restartBeforeEdit bool
	busy              bool
	preparedOld       bool
	acceptedOld       bool
}

func schedulerStaleActivationTrigger(kind string, everyFiveMinutes bool, anchor time.Time) app.ScheduleTrigger {
	if kind == app.ScheduleTriggerCron {
		expression := "0 * * * * *"
		if everyFiveMinutes {
			expression = "0 */5 * * * *"
		}
		return app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: expression, TimeZone: "UTC"}
	}
	every := int64(60)
	if everyFiveMinutes {
		every = 300
	}
	return app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: every,
		AnchorAt: anchor.Format(time.RFC3339Nano),
	}
}

func markSchedulerTestMessageDelivered(t *testing.T, workspacePath, target, messageID string, at time.Time) {
	t.Helper()
	if _, err := mutateResourceMailboxStoreForResource(workspacePath, target, func(store *resourceMailboxStore) error {
		for index := range store.Mailbox.Messages {
			message := &store.Mailbox.Messages[index]
			if message.ID != messageID {
				continue
			}
			stamp := at.Format(time.RFC3339Nano)
			message.Status = resourceMessageDelivered
			message.DeliveredAt = stamp
			message.TerminalAt = stamp
			message.UpdatedAt = stamp
			return nil
		}
		t.Fatalf("accepted Scheduler message %q not found", messageID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func makeSchedulerTestTargetBusy(t *testing.T, workspacePath, target string, at time.Time) {
	t.Helper()
	if _, err := mutateResourceMailboxStoreForResource(workspacePath, target, func(store *resourceMailboxStore) error {
		store.Mailbox.NextSequence++
		stamp := at.Format(time.RFC3339Nano)
		store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
			ID: "stale-activation-busy", Sequence: store.Mailbox.NextSequence, ResourceID: target,
			Text: "already queued", Role: "user", RequestedMode: resourceMessageModeEnqueue,
			ActualMode: resourceMessageModeEnqueue, Status: resourceMessageQueued,
			AcceptedAt: stamp, UpdatedAt: stamp,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSchedulerMetadataUpdateMaterializesStaleNonzeroActivation(t *testing.T) {
	tests := []staleMetadataActivationCase{
		{name: "interval trigger due", triggerType: app.ScheduleTriggerInterval, semanticEdit: "trigger", due: true},
		{name: "cron trigger due after restart", triggerType: app.ScheduleTriggerCron, semanticEdit: "trigger", due: true, restartBeforeEdit: true},
		{name: "interval target due", triggerType: app.ScheduleTriggerInterval, semanticEdit: "target", due: true},
		{name: "cron target due after restart", triggerType: app.ScheduleTriggerCron, semanticEdit: "target", due: true, restartBeforeEdit: true},
		{name: "interval trigger future", triggerType: app.ScheduleTriggerInterval, semanticEdit: "trigger"},
		{name: "cron trigger future after restart", triggerType: app.ScheduleTriggerCron, semanticEdit: "trigger", restartBeforeEdit: true},
		{name: "interval target future", triggerType: app.ScheduleTriggerInterval, semanticEdit: "target"},
		{name: "cron target future after restart", triggerType: app.ScheduleTriggerCron, semanticEdit: "target", restartBeforeEdit: true},
		{name: "interval target busy", triggerType: app.ScheduleTriggerInterval, semanticEdit: "target", due: true, busy: true},
		{name: "cron trigger busy", triggerType: app.ScheduleTriggerCron, semanticEdit: "trigger", due: true, busy: true},
		{name: "interval trigger discards old prepared", triggerType: app.ScheduleTriggerInterval, semanticEdit: "trigger", due: true, preparedOld: true},
		{name: "cron target keeps accepted old occurrence", triggerType: app.ScheduleTriggerCron, semanticEdit: "target", due: true, acceptedOld: true, restartBeforeEdit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}

			base := time.Now().UTC().Truncate(time.Minute).Add(-20 * time.Minute)
			semanticBoundary := base.Add(15 * time.Minute)
			metadataBoundary := semanticBoundary.Add(30 * time.Second)
			if test.due {
				metadataBoundary = semanticBoundary.Add(2*time.Minute + 30*time.Second)
			}
			oldTarget, currentTarget := "workspace", "workspace"
			oldTrigger := schedulerStaleActivationTrigger(test.triggerType, test.semanticEdit == "trigger", base)
			currentTrigger := schedulerStaleActivationTrigger(test.triggerType, false, base)
			if test.semanticEdit == "target" {
				oldTarget, currentTarget = "project1.task1", "workspace"
				oldTrigger = currentTrigger
			}
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Original semantic revision", Condition: "original rule",
				Target: oldTarget, Trigger: &oldTrigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, base)
			native := newNativeScheduler(manager, workspace)
			oldRuntime, err := initialScheduleRuntime(created, base)
			if err != nil {
				t.Fatal(err)
			}

			var oldPrepared schedulerPreparedOccurrence
			if test.preparedOld || test.acceptedOld {
				oldFirst := generationTime(oldRuntime.NextRunAt)
				oldNext, err := app.NextScheduleOccurrence(*created.Trigger, oldFirst)
				if err != nil {
					t.Fatal(err)
				}
				oldPrepared, err = native.prepareOccurrence(created, oldFirst, oldFirst, oldNext, 1, false, oldFirst, schedulerOccurrenceReasonTime)
				if err != nil {
					t.Fatal(err)
				}
				oldRuntime.Prepared = &oldPrepared
			}
			if err := native.storeSchedulerRuntime(created.ID, oldRuntime); err != nil {
				t.Fatal(err)
			}
			if test.acceptedOld {
				if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(oldPrepared)); err != nil {
					t.Fatal(err)
				}
				markSchedulerTestMessageDelivered(t, workspace.Path, oldPrepared.Target, oldPrepared.MessageID, base.Add(time.Minute))
			}

			manager.now = func() time.Time { return semanticBoundary }
			semanticChange := NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
				Trigger: &currentTrigger,
			}
			if test.semanticEdit == "target" {
				semanticChange.Target = &currentTarget
			}
			current, err := native.Change(context.Background(), semanticChange)
			if err != nil {
				t.Fatal(err)
			}
			current = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, current.ID, semanticBoundary)
			if runtime, err := native.schedulerRuntime(created.ID); err != nil || runtime.Revision != created.Revision {
				t.Fatalf("semantic edit unexpectedly reconciled stale runtime = %#v, %v", runtime, err)
			}

			first, err := app.NextScheduleOccurrence(*current.Trigger, semanticBoundary)
			if err != nil {
				t.Fatal(err)
			}
			last, next, count, capped, err := app.CoalescedScheduleOccurrence(*current.Trigger, first, metadataBoundary)
			if err != nil || capped {
				t.Fatalf("current occurrence range = last=%s next=%s count=%d capped=%v err=%v", last, next, count, capped, err)
			}
			if test.busy {
				makeSchedulerTestTargetBusy(t, workspace.Path, current.Target, metadataBoundary.Add(-time.Second))
			}
			manager.now = func() time.Time { return metadataBoundary }
			if test.restartBeforeEdit {
				restarted := newAgentManager(manager.server)
				restarted.now = manager.now
				manager.server.agents = restarted
				manager = restarted
				native = newNativeScheduler(manager, workspace)
			}

			description := "Metadata revision after semantic activation"
			unchangedTrigger := *current.Trigger
			updated, err := native.Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: current.ID, ExpectedRevision: current.Revision,
				Description: &description, Trigger: &unchangedTrigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := native.schedulerRuntime(created.ID)
			if err != nil || runtime.Revision != updated.Revision || runtime.TriggerDigest != mustSchedulerTriggerDigest(t, current.Trigger) ||
				runtime.Target != current.Target || runtime.EffectiveState != app.ScheduleStateActive || runtime.Prepared != nil {
				t.Fatalf("materialized stale runtime = %#v, %v", runtime, err)
			}
			if test.due {
				wantOutcome := schedulerOutcomeAccepted
				if test.busy {
					wantOutcome = schedulerOutcomeBusy
				}
				if runtime.LastOccurrenceAt != last.Format(time.RFC3339Nano) || runtime.LastOutcome != wantOutcome ||
					runtime.NextRunAt != next.Format(time.RFC3339Nano) {
					t.Fatalf("due stale activation projection = %#v", runtime)
				}
			} else if runtime.LastOccurrenceAt != "" || runtime.LastOutcome != "" || runtime.NextRunAt != first.Format(time.RFC3339Nano) {
				t.Fatalf("future stale activation projection = %#v", runtime)
			}

			currentMessages := scheduleOccurrenceMessages(t, workspace.Path, current.Target)
			wantCurrent := 0
			if test.due && !test.busy {
				wantCurrent = 1
			}
			if len(currentMessages) != wantCurrent {
				t.Fatalf("current target occurrences = %#v, want %d", currentMessages, wantCurrent)
			}
			if wantCurrent == 1 && (currentMessages[0].Causation == nil ||
				currentMessages[0].Causation.ScheduleRevision != current.Revision ||
				currentMessages[0].Causation.ScheduledFor != first.Format(time.RFC3339Nano) ||
				currentMessages[0].Causation.CoalescedThrough != last.Format(time.RFC3339Nano) ||
				currentMessages[0].Causation.CoalescedCount != count) {
				t.Fatalf("current semantic occurrence = %#v", currentMessages)
			}
			if test.preparedOld && len(scheduleOccurrenceMessages(t, workspace.Path, oldPrepared.Target)) != wantCurrent {
				t.Fatalf("discarded prepared occurrence was appended: %s", oldPrepared.MessageID)
			}
			if test.acceptedOld {
				oldMessages := scheduleOccurrenceMessages(t, workspace.Path, oldPrepared.Target)
				if len(oldMessages) != 1 || oldMessages[0].ID != oldPrepared.MessageID {
					t.Fatalf("accepted old occurrence changed = %#v", oldMessages)
				}
			}
			wantInputs := wantCurrent
			if got := schedulerAgentHubInputCount(fake); got != wantInputs {
				t.Fatalf("metadata external inputs = %d, want %d", got, wantInputs)
			}

			restarted := newAgentManager(manager.server)
			restarted.now = manager.now
			manager.server.agents = restarted
			if _, err := newNativeScheduler(restarted, workspace).Reconcile(context.Background(), metadataBoundary); err != nil {
				t.Fatal(err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, current.Target); len(messages) != wantCurrent {
				t.Fatalf("restart duplicated current occurrence: %#v", messages)
			}
			if got := schedulerAgentHubInputCount(fake); got != wantInputs {
				t.Fatalf("restart duplicated external input: %d", got)
			}
		})
	}
}

func TestNativeSchedulerMetadataChainsPreserveProjectableRuntime(t *testing.T) {
	tests := []struct {
		name        string
		triggerType string
		prepared    bool
		accepted    bool
		restart     bool
	}{
		{name: "interval cursor", triggerType: app.ScheduleTriggerInterval},
		{name: "cron prepared cursor", triggerType: app.ScheduleTriggerCron, prepared: true, restart: true},
		{name: "interval accepted cursor", triggerType: app.ScheduleTriggerInterval, prepared: true, accepted: true, restart: true},
		{name: "cron accepted cursor", triggerType: app.ScheduleTriggerCron, prepared: true, accepted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			activation := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
			trigger := schedulerStaleActivationTrigger(test.triggerType, false, activation)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Project metadata revisions", Condition: "every minute",
				Target: "workspace", Trigger: &trigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
			native := newNativeScheduler(manager, workspace)
			runtime, err := initialScheduleRuntime(created, activation)
			if err != nil {
				t.Fatal(err)
			}
			var prepared schedulerPreparedOccurrence
			if test.prepared {
				first := generationTime(runtime.NextRunAt)
				next, err := app.NextScheduleOccurrence(*created.Trigger, first)
				if err != nil {
					t.Fatal(err)
				}
				prepared, err = native.prepareOccurrence(created, first, first, next, 1, false, first, schedulerOccurrenceReasonTime)
				if err != nil {
					t.Fatal(err)
				}
				runtime.Prepared = &prepared
			}
			if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
				t.Fatal(err)
			}
			if test.accepted {
				if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
					t.Fatal(err)
				}
				markSchedulerTestMessageDelivered(t, workspace.Path, prepared.Target, prepared.MessageID, activation.Add(time.Minute))
			}

			// Keep the prepared occurrence's successor in the future. A second
			// reconcile must therefore be a pure idempotency check rather than a
			// legitimate later-occurrence delivery.
			now := activation.Add(time.Minute + 30*time.Second)
			manager.now = func() time.Time { return now }
			unchangedTrigger := *created.Trigger
			description := "First metadata revision"
			firstUpdate, err := native.Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
				Description: &description, Trigger: &unchangedTrigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.restart {
				restarted := newAgentManager(manager.server)
				restarted.now = manager.now
				manager.server.agents = restarted
				manager = restarted
				native = newNativeScheduler(manager, workspace)
			}
			condition := "second metadata revision"
			secondUpdate, err := native.Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: firstUpdate.Revision,
				Condition: &condition, Trigger: &unchangedTrigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			beforeReconcile, err := native.schedulerRuntime(created.ID)
			if err != nil || beforeReconcile.Revision != created.Revision || beforeReconcile.TriggerDigest != runtime.TriggerDigest ||
				beforeReconcile.Target != runtime.Target || beforeReconcile.NextRunAt != runtime.NextRunAt ||
				beforeReconcile.LastOccurrenceAt != runtime.LastOccurrenceAt || beforeReconcile.LastOutcome != runtime.LastOutcome {
				t.Fatalf("projectable metadata chain changed runtime = %#v, %v", beforeReconcile, err)
			}
			if test.prepared && (beforeReconcile.Prepared == nil || beforeReconcile.Prepared.MessageID != prepared.MessageID) {
				t.Fatalf("projectable metadata chain changed prepared = %#v", beforeReconcile.Prepared)
			}
			wantBefore := 0
			if test.accepted {
				wantBefore = 1
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != wantBefore {
				t.Fatalf("projectable metadata chain appended occurrence: %#v", messages)
			}
			if got := schedulerAgentHubInputCount(fake); got != 0 {
				t.Fatalf("projectable metadata chain external inputs = %d", got)
			}

			if _, err := native.Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			after, err := native.schedulerRuntime(created.ID)
			if err != nil || after.Revision != secondUpdate.Revision || after.Prepared != nil || after.LastOutcome != schedulerOutcomeAccepted {
				t.Fatalf("projected metadata runtime = %#v, %v", after, err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target)
			if len(messages) != 1 {
				t.Fatalf("projected metadata occurrences = %#v", messages)
			}
			wantInputs := 1
			if test.accepted {
				wantInputs = 0
			}
			if got := schedulerAgentHubInputCount(fake); got != wantInputs {
				t.Fatalf("projected metadata external inputs = %d, want %d", got, wantInputs)
			}
			if _, err := native.Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if got := schedulerAgentHubInputCount(fake); got != wantInputs {
				t.Fatalf("projected metadata replay external inputs = %d", got)
			}
		})
	}
}

func TestSchedulerRuntimePreservesRepeatingActivationRequiresSemanticCheckpoint(t *testing.T) {
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: "2026-08-24T00:00:00Z",
	}
	schedule := app.Schedule{
		ID: "schedule-projectable", Revision: 3, ActivationRevision: 1, State: app.ScheduleStateActive,
		Target: "workspace", Trigger: &trigger,
	}
	digest := mustSchedulerTriggerDigest(t, schedule.Trigger)
	prepared := &schedulerPreparedOccurrence{
		ScheduleID: schedule.ID, ScheduleRevision: 1, OccurrenceID: "occurrence-one",
		MessageID: "message-one", Target: schedule.Target, ScheduledFor: "2026-08-24T00:01:00Z",
		Causation: &resourceMessageCausation{
			ScheduleID: schedule.ID, ScheduleRevision: 1, OccurrenceID: "occurrence-one",
			ScheduledFor: "2026-08-24T00:01:00Z",
		},
	}
	valid := schedulerScheduleRuntime{
		Revision: 2, ActivationRevision: 1, TriggerDigest: digest, Target: schedule.Target,
		EffectiveState: app.ScheduleStateActive, NextRunAt: "2026-08-24T00:01:00Z",
	}
	tests := []struct {
		name   string
		mutate func(*schedulerScheduleRuntime)
		want   bool
	}{
		{name: "older metadata revision", want: true},
		{name: "exact revision", mutate: func(runtime *schedulerScheduleRuntime) { runtime.Revision = schedule.Revision }, want: true},
		{name: "frozen prepared revision", mutate: func(runtime *schedulerScheduleRuntime) { runtime.Prepared = prepared }, want: true},
		{name: "attention with frozen prepared", mutate: func(runtime *schedulerScheduleRuntime) {
			runtime.Prepared = prepared
			runtime.EffectiveState = schedulerOutcomeAttention
			runtime.NextRunAt = ""
		}, want: true},
		{name: "missing revision", mutate: func(runtime *schedulerScheduleRuntime) { runtime.Revision = 0 }},
		{name: "newer revision", mutate: func(runtime *schedulerScheduleRuntime) { runtime.Revision = schedule.Revision + 1 }},
		{name: "different trigger", mutate: func(runtime *schedulerScheduleRuntime) { runtime.TriggerDigest = "different" }},
		{name: "different activation", mutate: func(runtime *schedulerScheduleRuntime) { runtime.ActivationRevision = 2 }},
		{name: "different target", mutate: func(runtime *schedulerScheduleRuntime) { runtime.Target = "project1.task1" }},
		{name: "missing lifecycle", mutate: func(runtime *schedulerScheduleRuntime) { runtime.EffectiveState = "" }},
		{name: "paused lifecycle", mutate: func(runtime *schedulerScheduleRuntime) { runtime.EffectiveState = app.ScheduleStatePaused }},
		{name: "active cursor missing", mutate: func(runtime *schedulerScheduleRuntime) { runtime.NextRunAt = "" }},
		{name: "active cursor malformed", mutate: func(runtime *schedulerScheduleRuntime) { runtime.NextRunAt = "tomorrow" }},
		{name: "attention without prepared", mutate: func(runtime *schedulerScheduleRuntime) {
			runtime.EffectiveState = schedulerOutcomeAttention
		}},
		{name: "prepared schedule mismatch", mutate: func(runtime *schedulerScheduleRuntime) {
			copy := *prepared
			copy.ScheduleID = "another-schedule"
			runtime.Prepared = &copy
		}},
		{name: "prepared target mismatch", mutate: func(runtime *schedulerScheduleRuntime) {
			copy := *prepared
			copy.Target = "project1.task1"
			runtime.Prepared = &copy
		}},
		{name: "prepared revision newer than checkpoint", mutate: func(runtime *schedulerScheduleRuntime) {
			copy := *prepared
			copy.ScheduleRevision = schedule.Revision
			runtime.Prepared = &copy
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := valid
			if test.mutate != nil {
				test.mutate(&runtime)
			}
			got, err := schedulerRuntimePreservesRepeatingActivation(runtime, schedule)
			if err != nil || got != test.want {
				t.Fatalf("projectability = %v, %v; runtime=%#v", got, err, runtime)
			}
		})
	}
}

func TestNativeSchedulerSemanticABADiscardsStaleActivationAfterRestart(t *testing.T) {
	tests := []struct {
		name     string
		edit     string
		prepared bool
	}{
		{name: "trigger cursor", edit: "trigger"},
		{name: "trigger prepared", edit: "trigger", prepared: true},
		{name: "target cursor", edit: "target"},
		{name: "target prepared", edit: "target", prepared: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}

			base := time.Now().UTC().Truncate(time.Minute).Add(-15 * time.Minute)
			firstBoundary := base.Add(5*time.Minute + 10*time.Second)
			secondBoundary := base.Add(6*time.Minute + 10*time.Second)
			now := base.Add(8*time.Minute + 30*time.Second)
			originalTrigger := schedulerStaleActivationTrigger(app.ScheduleTriggerInterval, false, base)
			alternateTrigger := schedulerStaleActivationTrigger(app.ScheduleTriggerInterval, true, base)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Reject stale ABA activation", Condition: "every minute",
				Target: "workspace", Trigger: &originalTrigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, base)
			native := newNativeScheduler(manager, workspace)
			staleRuntime, err := initialScheduleRuntime(created, base)
			if err != nil {
				t.Fatal(err)
			}
			oldFirst := generationTime(staleRuntime.NextRunAt)
			if test.prepared {
				oldNext, err := app.NextScheduleOccurrence(*created.Trigger, oldFirst)
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := native.prepareOccurrence(created, oldFirst, oldFirst, oldNext, 1, false, oldFirst, schedulerOccurrenceReasonTime)
				if err != nil {
					t.Fatal(err)
				}
				staleRuntime.Prepared = &prepared
			}
			if err := native.storeSchedulerRuntime(created.ID, staleRuntime); err != nil {
				t.Fatal(err)
			}

			manager.now = func() time.Time { return firstBoundary }
			firstChange := NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
				Trigger: &originalTrigger,
			}
			if test.edit == "trigger" {
				firstChange.Trigger = &alternateTrigger
			} else {
				alternateTarget := "project1.task1"
				firstChange.Target = &alternateTarget
			}
			alternate, err := native.Change(context.Background(), firstChange)
			if err != nil {
				t.Fatal(err)
			}
			alternate = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, alternate.ID, firstBoundary)

			manager.now = func() time.Time { return secondBoundary }
			secondChange := NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: alternate.ID, ExpectedRevision: alternate.Revision,
				Trigger: &originalTrigger,
			}
			if test.edit == "target" {
				originalTarget := created.Target
				secondChange.Target = &originalTarget
			}
			current, err := native.Change(context.Background(), secondChange)
			if err != nil {
				t.Fatal(err)
			}
			current = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, current.ID, secondBoundary)
			if current.ActivationRevision != current.Revision || current.ActivationRevision == created.ActivationRevision {
				t.Fatalf("ABA activation identity = %#v", current)
			}

			restarted := newAgentManager(manager.server)
			restarted.now = func() time.Time { return now }
			manager.server.agents = restarted
			native = newNativeScheduler(restarted, workspace)
			snapshot, err := native.Snapshot(now)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].NextRunAt != "" || snapshot.Schedules[0].LastOccurrenceAt != "" {
				t.Fatalf("stale ABA runtime projected before reconcile = %#v", snapshot.Schedules)
			}
			if _, err := native.Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}

			first, err := app.NextScheduleOccurrence(*current.Trigger, secondBoundary)
			if err != nil {
				t.Fatal(err)
			}
			last, next, count, capped, err := app.CoalescedScheduleOccurrence(*current.Trigger, first, now)
			if err != nil || capped {
				t.Fatalf("current activation occurrences = %s %s %d %v, %v", first, next, count, capped, err)
			}
			runtime, err := native.schedulerRuntime(created.ID)
			if err != nil || runtime.Revision != current.Revision || runtime.ActivationRevision != current.ActivationRevision ||
				runtime.Prepared != nil || runtime.LastOccurrenceAt != last.Format(time.RFC3339Nano) || runtime.NextRunAt != next.Format(time.RFC3339Nano) {
				t.Fatalf("reconciled ABA runtime = %#v, %v", runtime, err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, current.Target)
			if len(messages) != 1 || messages[0].Causation == nil ||
				messages[0].Causation.ScheduleRevision != current.Revision ||
				messages[0].Causation.ScheduledFor != first.Format(time.RFC3339Nano) ||
				messages[0].Causation.CoalescedThrough != last.Format(time.RFC3339Nano) ||
				messages[0].Causation.CoalescedCount != count {
				t.Fatalf("current ABA occurrence = %#v", messages)
			}
			if messages[0].Causation.ScheduledFor == oldFirst.Format(time.RFC3339Nano) {
				t.Fatalf("stale ABA occurrence was reused: %#v", messages[0])
			}
			if got := schedulerAgentHubInputCount(fake); got != 1 {
				t.Fatalf("ABA external inputs = %d", got)
			}
		})
	}
}
