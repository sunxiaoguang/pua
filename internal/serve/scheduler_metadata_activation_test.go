package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func rewriteSchedulerTestUpdatedAt(t *testing.T, workspace *app.Workspace, id string, updatedAt time.Time) app.Schedule {
	t.Helper()
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	found := -1
	for index := range config.Schedules {
		if config.Schedules[index].ID == id {
			config.Schedules[index].UpdatedAt = updatedAt.Format(time.RFC3339Nano)
			found = index
			break
		}
	}
	if found < 0 {
		t.Fatalf("schedule %q not found in %#v", id, config.Schedules)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Schedules[found]
}

func TestNativeSchedulerMetadataUpdateMaterializesUncheckpointedActivation(t *testing.T) {
	tests := []struct {
		name              string
		trigger           func(time.Time) app.ScheduleTrigger
		restartBeforeEdit bool
	}{
		{
			name: "interval",
			trigger: func(activation time.Time) app.ScheduleTrigger {
				return app.ScheduleTrigger{
					Type: app.ScheduleTriggerInterval, EverySeconds: 60,
					AnchorAt: activation.Format(time.RFC3339Nano),
				}
			},
		},
		{
			name:              "cron after restart without a checkpoint",
			restartBeforeEdit: true,
			trigger: func(time.Time) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"}
			},
		},
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
			activation := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
			trigger := test.trigger(activation)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Retain the first activation", Condition: "repeat from creation",
				Target: "workspace", Trigger: &trigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
			first, err := app.NextScheduleOccurrence(*created.Trigger, activation)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Add(time.Second)
			last, next, count, capped, err := app.CoalescedScheduleOccurrence(*created.Trigger, first, now)
			if err != nil || capped || count < 1 || !next.After(now) {
				t.Fatalf("occurrence range = first=%s last=%s next=%s count=%d capped=%v err=%v", first, last, next, count, capped, err)
			}
			manager.now = func() time.Time { return now }
			native := newNativeScheduler(manager, workspace)
			if runtime, err := native.schedulerRuntime(created.ID); err != nil || runtime.Revision != 0 {
				t.Fatalf("initial runtime = %#v, %v", runtime, err)
			}
			if test.restartBeforeEdit {
				restarted := newAgentManager(manager.server)
				restarted.now = manager.now
				manager.server.agents = restarted
				manager = restarted
				native = newNativeScheduler(manager, workspace)
			}

			description := "Retain the first activation after wording changes"
			condition := "repeat from the original activation boundary"
			guard := "only when the metadata guard is true"
			unchangedTrigger := *created.Trigger
			updated, err := native.Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
				Description: &description, Condition: &condition, Guard: &guard, Trigger: &unchangedTrigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := native.schedulerRuntime(created.ID)
			if err != nil || runtime.Revision != updated.Revision || runtime.TriggerDigest != mustSchedulerTriggerDigest(t, updated.Trigger) ||
				runtime.Target != updated.Target || runtime.EffectiveState != app.ScheduleStateActive || runtime.Prepared != nil ||
				runtime.LastOccurrenceAt != last.Format(time.RFC3339Nano) || runtime.LastOutcome != schedulerOutcomeAccepted ||
				runtime.NextRunAt != next.Format(time.RFC3339Nano) || runtime.LastError != "" {
				t.Fatalf("promoted metadata runtime = %#v, %v", runtime, err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target)
			if len(messages) != 1 || messages[0].Causation == nil ||
				messages[0].Causation.ScheduleRevision != created.Revision || messages[0].Causation.ScheduleID != created.ID ||
				messages[0].Causation.ScheduledFor != first.Format(time.RFC3339Nano) ||
				messages[0].Causation.CoalescedThrough != last.Format(time.RFC3339Nano) || messages[0].Causation.CoalescedCount != count {
				t.Fatalf("old-revision occurrence = %#v", messages)
			}
			if got := schedulerAgentHubInputCount(fake); got != 1 {
				t.Fatalf("metadata update external inputs = %d, want 1", got)
			}

			restarted := newAgentManager(manager.server)
			restarted.now = manager.now
			manager.server.agents = restarted
			if _, err := newNativeScheduler(restarted, workspace).Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 1 {
				t.Fatalf("restart duplicated durable occurrence: %#v", messages)
			}
			if got := schedulerAgentHubInputCount(fake); got != 1 {
				t.Fatalf("restart duplicated external occurrence: inputs=%d", got)
			}
		})
	}
}

func TestNativeSchedulerMetadataUpdateRecoversAcceptedUncheckpointedOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	activation := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: activation.Format(time.RFC3339Nano)}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Recover accepted activation", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
	first, err := app.NextScheduleOccurrence(*created.Trigger, activation)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	last, next, count, capped, err := app.CoalescedScheduleOccurrence(*created.Trigger, first, now)
	if err != nil || capped {
		t.Fatalf("occurrence range = last=%s next=%s count=%d capped=%v err=%v", last, next, count, capped, err)
	}
	manager.now = func() time.Time { return now }
	native := newNativeScheduler(manager, workspace)
	reason := schedulerOccurrenceReasonTime
	if count > 1 {
		reason = schedulerOccurrenceReasonCoalesced
	}
	prepared, err := native.prepareOccurrence(created, first, last, next, count, false, now, reason)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
		t.Fatal(err)
	}
	if runtime, err := native.schedulerRuntime(created.ID); err != nil || runtime.Revision != 0 {
		t.Fatalf("accepted crash-window runtime = %#v, %v", runtime, err)
	}

	description := "Recover the accepted activation after metadata review"
	unchangedTrigger := *created.Trigger
	updated, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
		Description: &description, Trigger: &unchangedTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != updated.Revision || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted ||
		runtime.LastOccurrenceAt != last.Format(time.RFC3339Nano) || runtime.NextRunAt != next.Format(time.RFC3339Nano) {
		t.Fatalf("accepted metadata checkpoint = %#v, %v", runtime, err)
	}
	assertDeliveredPreparedOccurrence(t, scheduleOccurrenceMessages(t, workspace.Path, created.Target), prepared)
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("accepted metadata external inputs = %d, want 1", got)
	}
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 1 {
		t.Fatalf("accepted metadata replay duplicated occurrence: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("accepted metadata replay duplicated external input: %d", got)
	}
}

func TestNativeSchedulerMetadataUpdatePreservesFirstFutureOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	activation := time.Now().UTC().Truncate(time.Second)
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: activation.Format(time.RFC3339Nano)}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Keep the first future occurrence", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
	first, err := app.NextScheduleOccurrence(*created.Trigger, activation)
	if err != nil {
		t.Fatal(err)
	}
	now := activation.Add(30 * time.Second)
	manager.now = func() time.Time { return now }
	native := newNativeScheduler(manager, workspace)
	description := "Keep the first future occurrence after metadata review"
	unchangedTrigger := *created.Trigger
	updated, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
		Description: &description, Trigger: &unchangedTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != updated.Revision || runtime.EffectiveState != app.ScheduleStateActive ||
		runtime.NextRunAt != first.Format(time.RFC3339Nano) || runtime.LastOccurrenceAt != "" || runtime.LastOutcome != "" || runtime.Prepared != nil {
		t.Fatalf("future metadata runtime = %#v, %v", runtime, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
		t.Fatalf("future metadata delivered early: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 0 {
		t.Fatalf("future metadata external inputs = %d, want 0", got)
	}
}

func TestNativeSchedulerUncheckpointedSemanticEditsKeepMutationBoundary(t *testing.T) {
	tests := []struct {
		name   string
		change func(app.Schedule) NativeSchedulerChange
	}{
		{
			name: "trigger",
			change: func(created app.Schedule) NativeSchedulerChange {
				trigger := *created.Trigger
				trigger.EverySeconds = 300
				return NativeSchedulerChange{Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Trigger: &trigger}
			},
		},
		{
			name: "target",
			change: func(created app.Schedule) NativeSchedulerChange {
				target := "project1.task1"
				trigger := *created.Trigger
				return NativeSchedulerChange{Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Target: &target, Trigger: &trigger}
			},
		},
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
			activation := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
			trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: activation.Format(time.RFC3339Nano)}
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Reset semantic activation", Condition: "every minute", Target: "workspace", Trigger: &trigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
			manager.now = func() time.Time { return time.Now().UTC().Add(time.Second) }
			native := newNativeScheduler(manager, workspace)
			updated, err := native.Change(context.Background(), test.change(created))
			if err != nil {
				t.Fatal(err)
			}
			if runtime, err := native.schedulerRuntime(created.ID); err != nil || runtime.Revision != 0 {
				t.Fatalf("%s edit materialized old activation: %#v, %v", test.name, runtime, err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
				t.Fatalf("%s edit delivered old occurrences: %#v", test.name, messages)
			}
			mutationAt := generationTime(updated.UpdatedAt)
			manager.now = func() time.Time { return mutationAt }
			if _, err := native.Reconcile(context.Background(), mutationAt); err != nil {
				t.Fatal(err)
			}
			runtime, err := native.schedulerRuntime(created.ID)
			if err != nil || runtime.Revision != updated.Revision || runtime.LastOccurrenceAt != "" || runtime.LastOutcome != "" ||
				runtime.Prepared != nil || !generationTime(runtime.NextRunAt).After(mutationAt) {
				t.Fatalf("%s mutation-boundary runtime = %#v, %v", test.name, runtime, err)
			}
			if got := schedulerAgentHubInputCount(fake); got != 0 {
				t.Fatalf("%s edit external inputs = %d, want 0", test.name, got)
			}
		})
	}
}

func TestNativeSchedulerUncheckpointedMetadataPreflightHasNoRuntimeSideEffects(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	activation := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: activation.Format(time.RFC3339Nano)}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Reject invalid metadata", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
	manager.now = func() time.Time { return time.Now().UTC().Add(time.Second) }
	native := newNativeScheduler(manager, workspace)
	unchangedTrigger := *created.Trigger
	malformed := app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 1, AnchorAt: activation.Format(time.RFC3339Nano)}
	blank := " "
	if _, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
		Description: &blank, Trigger: &unchangedTrigger,
	}); err == nil || !strings.Contains(err.Error(), "description, condition, and target are required") {
		t.Fatalf("invalid metadata error = %v", err)
	}
	description := "Stale metadata"
	if _, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision + 1,
		Description: &description, Trigger: &malformed,
	}); err == nil {
		t.Fatal("stale metadata update succeeded")
	} else {
		var conflict *app.ScheduleRevisionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale metadata error = %v", err)
		}
	}
	if _, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Trigger: &malformed,
	}); err == nil || !strings.Contains(err.Error(), "everySeconds") {
		t.Fatalf("malformed metadata error = %v", err)
	}
	if got := schedulerTestScheduleByID(t, puaWorkspace, created.ID); !reflect.DeepEqual(got, created) {
		t.Fatalf("rejected metadata changed portable definition: got=%#v want=%#v", got, created)
	}
	if runtime, err := native.schedulerRuntime(created.ID); err != nil || runtime.Revision != 0 {
		t.Fatalf("rejected metadata created runtime = %#v, %v", runtime, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
		t.Fatalf("rejected metadata delivered occurrence: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 0 {
		t.Fatalf("rejected metadata external inputs = %d, want 0", got)
	}
}

func TestNativeSchedulerUncheckpointedMetadataSkipsBusyTargetOnce(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	activation := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: activation.Format(time.RFC3339Nano)}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Skip the busy activation", Condition: "every minute", Target: "project1.task1", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	created = rewriteSchedulerTestUpdatedAt(t, puaWorkspace, created.ID, activation)
	first, err := app.NextScheduleOccurrence(*created.Trigger, activation)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	last, next, _, _, err := app.CoalescedScheduleOccurrence(*created.Trigger, first, now)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := mutateResourceMailboxStoreForResource(workspace.Path, created.Target, func(store *resourceMailboxStore) error {
		store.Mailbox.NextSequence++
		store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
			ID: "metadata-busy-target", Sequence: store.Mailbox.NextSequence, ResourceID: created.Target,
			Text: "already queued", Role: "user", RequestedMode: resourceMessageModeEnqueue,
			ActualMode: resourceMessageModeEnqueue, Status: resourceMessageQueued, AcceptedAt: stamp, UpdatedAt: stamp,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return now }
	native := newNativeScheduler(manager, workspace)
	description := "Skip the busy activation after metadata review"
	unchangedTrigger := *created.Trigger
	updated, err := native.Change(context.Background(), NativeSchedulerChange{
		Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
		Description: &description, Trigger: &unchangedTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != updated.Revision || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeBusy ||
		runtime.LastOccurrenceAt != last.Format(time.RFC3339Nano) || runtime.NextRunAt != next.Format(time.RFC3339Nano) || !generationTime(runtime.NextRunAt).After(now) {
		t.Fatalf("busy metadata runtime = %#v, %v", runtime, err)
	}
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
		t.Fatalf("busy metadata appended occurrence: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 0 {
		t.Fatalf("busy metadata external inputs = %d, want 0", got)
	}
}
