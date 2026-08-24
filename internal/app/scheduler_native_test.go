package app_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

type schedulerV1TestDefinition struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Condition   string `json:"condition"`
	Target      string `json:"target"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func migrateSchedulerV1ForTest(t *testing.T, workspace *app.Workspace, definitions ...schedulerV1TestDefinition) ([]app.Schedule, []byte) {
	t.Helper()
	legacy := struct {
		SchemaVersion       int                         `json:"schemaVersion"`
		AgentBinding        map[string]string           `json:"agentBinding"`
		WakeIntervalMinutes int                         `json:"wakeIntervalMinutes"`
		Schedules           []schedulerV1TestDefinition `json:"schedules"`
	}{
		SchemaVersion:       1,
		AgentBinding:        map[string]string{"kind": "profile", "name": "default"},
		WakeIntervalMinutes: 45,
		Schedules:           definitions,
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate(""); err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	return config.Schedules, data
}

func TestSchedulerV1MigrationPreservesDefinitionsForCompilation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	schedules, legacyData := migrateSchedulerV1ForTest(t, workspace, schedulerV1TestDefinition{
		ID:          "schedule-0123456789abcdef01234567",
		Description: "Keep action",
		Condition:   "tomorrow morning when green",
		Target:      app.SchedulerResourceID,
		CreatedAt:   "2026-08-01T00:00:00Z",
		UpdatedAt:   "2026-08-02T00:00:00Z",
	})
	if len(schedules) != 1 {
		t.Fatalf("migrated schedules = %#v", schedules)
	}
	schedule := schedules[0]
	if schedule.Revision != 1 || schedule.ActivationRevision != 1 || schedule.State != app.ScheduleStateNeedsCompilation || schedule.Trigger != nil ||
		schedule.Description != "Keep action" || schedule.Condition != "tomorrow morning when green" || schedule.Target != app.SchedulerResourceID ||
		schedule.CreatedAt != "2026-08-01T00:00:00Z" || schedule.UpdatedAt != "2026-08-02T00:00:00Z" {
		t.Fatalf("migrated schedule = %#v", schedule)
	}
	raw, _ := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
	if string(raw) == "" || string(raw) == string(legacyData) {
		t.Fatal("migration did not atomically replace the v1 definition")
	}
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-08-30T12:00:00Z"}
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: schedule.ID, ExpectedRevision: schedule.Revision, Trigger: &trigger}); !errors.Is(err, app.ErrScheduleTargetScheduler) {
		t.Fatalf("historical Scheduler self-target compilation error = %v", err)
	}
	config, err := workspace.Scheduler()
	if err != nil || len(config.Schedules) != 1 || !reflect.DeepEqual(config.Schedules[0], schedule) {
		t.Fatalf("rejected self-target compilation changed migrated definition: %#v, %v", config.Schedules, err)
	}
	target := "workspace"
	compiled, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: schedule.ID, ExpectedRevision: schedule.Revision, Target: &target, Trigger: &trigger})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Revision != 2 || compiled.ActivationRevision != compiled.Revision || compiled.State != app.ScheduleStateActive || compiled.Trigger == nil || *compiled.Trigger != trigger ||
		compiled.Description != schedule.Description || compiled.Condition != schedule.Condition || compiled.Target != target {
		t.Fatalf("compiled migrated schedule = %#v", compiled)
	}
}

func TestAddScheduleRequiresTriggerWithoutMutation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspace.AddSchedule(app.CreateScheduleInput{Description: "Compile me", Condition: "tomorrow", Target: "workspace"})
	if !errors.Is(err, app.ErrScheduleTriggerRequired) || err.Error() != "add schedule: schedule trigger is required" {
		t.Fatalf("triggerless create error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("triggerless create mutated scheduler.json:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestScheduleAtMutationRequiresFutureOnlyWhenChanged(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Recovery target", "recovery-target")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Overdue recovery", Slug: "overdue-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	beforeCreate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pastTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "2000-01-01T00:00:00Z"}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Too late", Condition: "in the past", Target: "workspace", Trigger: &pastTrigger,
	}); !errors.Is(err, app.ErrScheduleTriggerAtNotFuture) {
		t.Fatalf("past one-time create error = %v", err)
	}
	afterCreate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterCreate) != string(beforeCreate) {
		t.Fatalf("failed create mutated scheduler.json:\nbefore=%s\nafter=%s", beforeCreate, afterCreate)
	}

	futureTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-12-31T23:59:58Z"}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "In time", Condition: "in the future", Target: "workspace", Trigger: &futureTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	historicalTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "2000-01-01T00:00:00Z"}
	config.Schedules[0].Trigger = &historicalTrigger
	fixture, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixture = append(fixture, '\n')
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	changedPastTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "2000-01-01T00:00:01Z"}
	_, err = workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision + 1, Trigger: &changedPastTrigger,
	})
	var conflict *app.ScheduleRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Actual != created.Revision {
		t.Fatalf("past trigger revision conflict = %#v, %v", conflict, err)
	}
	afterConflict, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterConflict) != string(fixture) {
		t.Fatalf("revision conflict mutated scheduler.json:\nbefore=%s\nafter=%s", fixture, afterConflict)
	}

	description := "Recover overdue delivery"
	target := task.ID
	retargeted, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision, Description: &description, Target: &target, Trigger: &historicalTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retargeted.Revision != created.Revision+1 || retargeted.Description != description || retargeted.Target != task.ID ||
		retargeted.Trigger == nil || *retargeted.Trigger != historicalTrigger {
		t.Fatalf("overdue retarget update = %#v", retargeted)
	}

	beforeUpdate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rejectedDescription := "Must stay unchanged"
	rejectedTarget := "workspace"
	_, err = workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: retargeted.ID, ExpectedRevision: retargeted.Revision, Description: &rejectedDescription, Target: &rejectedTarget, Trigger: &changedPastTrigger,
	})
	if !errors.Is(err, app.ErrScheduleTriggerAtNotFuture) || err.Error() != "update schedule: schedule trigger.at must be strictly in the future" {
		t.Fatalf("changed past one-time update error = %v", err)
	}
	afterUpdate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterUpdate) != string(beforeUpdate) {
		t.Fatalf("failed update mutated scheduler.json:\nbefore=%s\nafter=%s", beforeUpdate, afterUpdate)
	}
	config, err = workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != 1 || config.Schedules[0].Revision != retargeted.Revision || config.Schedules[0].Description != retargeted.Description ||
		config.Schedules[0].Target != retargeted.Target || config.Schedules[0].Trigger == nil || *config.Schedules[0].Trigger != historicalTrigger {
		t.Fatalf("failed update changed schedule = %#v", config.Schedules)
	}

	futureUpdate := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-12-31T23:59:59Z"}
	updated, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: retargeted.ID, ExpectedRevision: retargeted.Revision, Trigger: &futureUpdate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != retargeted.Revision+1 || updated.Trigger == nil || *updated.Trigger != futureUpdate {
		t.Fatalf("valid future update = %#v", updated)
	}
}

func TestSchedulerLoadsCompletedPastAtTrigger(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	futureTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-12-31T23:59:59Z"}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Already done", Condition: "once", Target: "workspace", Trigger: &futureTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	config.Schedules[0].State = app.ScheduleStateCompleted
	config.Schedules[0].Trigger.At = "2000-01-01T00:00:00Z"
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Schedules) != 1 || loaded.Schedules[0].ID != created.ID || loaded.Schedules[0].State != app.ScheduleStateCompleted || loaded.Schedules[0].Trigger.At != "2000-01-01T00:00:00Z" {
		t.Fatalf("loaded completed schedule = %#v", loaded.Schedules)
	}
}

func TestScheduleTriggerValidationAndRevisionCAS(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 59, AnchorAt: "2026-08-01T00:00:00Z"}); err == nil {
		t.Fatal("sub-minute interval unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "TZ=UTC 0 * * * * *", TimeZone: "UTC"}); err == nil {
		t.Fatal("embedded cron timezone unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "Not/AZone"}); err == nil {
		t.Fatal("invalid IANA timezone unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "Local"}); err == nil {
		t.Fatal("implicit local timezone unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "* * * * * *", TimeZone: "UTC"}); err == nil {
		t.Fatal("sub-minute cron unexpectedly accepted")
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run", Condition: "at noon UTC", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-08-30T12:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.ActivationRevision != 1 || created.State != app.ScheduleStateActive || created.Trigger == nil || created.Trigger.At != "9999-08-30T12:00:00Z" {
		t.Fatalf("valid create = %#v", created)
	}
	description := "Changed"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Description: &description}); err == nil {
		t.Fatal("update without expectedRevision unexpectedly succeeded")
	}
	_, err = workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: 7, Description: &description})
	var conflict *app.ScheduleRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Actual != 1 {
		t.Fatalf("revision conflict = %#v, %v", conflict, err)
	}
	config, _ := workspace.Scheduler()
	if config.Schedules[0].Description != created.Description || config.Schedules[0].Revision != 1 {
		t.Fatalf("failed CAS changed definition: %#v", config.Schedules[0])
	}
	paused, err := workspace.PauseSchedule(created.ID)
	if err != nil || paused.State != app.ScheduleStatePaused || paused.Revision != 2 {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	pausedAgain, err := workspace.PauseSchedule(created.ID)
	if err != nil || pausedAgain.Revision != paused.Revision {
		t.Fatalf("idempotent pause = %#v, %v", pausedAgain, err)
	}
	resumed, err := workspace.ResumeSchedule(created.ID)
	if err != nil || resumed.State != app.ScheduleStateActive || resumed.Revision != 3 {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
}

func TestScheduleActivationRevisionTracksOnlySemanticChanges(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Activation target", "activation-target")
	if err != nil {
		t.Fatal(err)
	}
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Track activation", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	description := "Metadata only"
	metadata, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision, Description: &description,
	})
	if err != nil || metadata.Revision != 2 || metadata.ActivationRevision != created.ActivationRevision {
		t.Fatalf("metadata update = %#v, %v", metadata, err)
	}
	paused, err := workspace.PauseSchedule(created.ID)
	if err != nil || paused.Revision != 3 || paused.ActivationRevision != created.ActivationRevision {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	resumed, err := workspace.ResumeSchedule(created.ID)
	if err != nil || resumed.Revision != 4 || resumed.ActivationRevision != created.ActivationRevision {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	target := project.ID
	retargeted, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: resumed.Revision, Target: &target,
	})
	if err != nil || retargeted.Revision != 5 || retargeted.ActivationRevision != retargeted.Revision {
		t.Fatalf("target update = %#v, %v", retargeted, err)
	}
	unchanged := *retargeted.Trigger
	identicalTrigger, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: retargeted.Revision, Trigger: &unchanged,
	})
	if err != nil || identicalTrigger.Revision != 6 || identicalTrigger.ActivationRevision != retargeted.ActivationRevision {
		t.Fatalf("identical trigger update = %#v, %v", identicalTrigger, err)
	}
	changed := unchanged
	changed.EverySeconds = 120
	changedTrigger, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: identicalTrigger.Revision, Trigger: &changed,
	})
	if err != nil || changedTrigger.Revision != 7 || changedTrigger.ActivationRevision != changedTrigger.Revision {
		t.Fatalf("trigger update = %#v, %v", changedTrigger, err)
	}
}

func TestSchedulerV2WithoutActivationRevisionLoadsConservatively(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Legacy v2", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	schedules := document["schedules"].([]any)
	delete(schedules[0].(map[string]any), "activationRevision")
	legacy, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(legacy, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := workspace.Scheduler()
	if err != nil || len(loaded.Schedules) != 1 || loaded.Schedules[0].ActivationRevision != created.Revision {
		t.Fatalf("legacy v2 schedule = %#v, %v", loaded.Schedules, err)
	}
}

func TestMissingScheduleMutationsAreTypedAndAtomic(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Keep this definition", Condition: "every minute",
		Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "scheduler", "scheduler.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	missingID := "schedule-ffffffffffffffffffffffff"
	description := "Missing"
	operations := map[string]func() (app.Schedule, error){
		"update": func() (app.Schedule, error) {
			return workspace.UpdateSchedule(app.UpdateScheduleInput{
				ID: missingID, ExpectedRevision: ^uint64(0), Description: &description,
			})
		},
		"pause":  func() (app.Schedule, error) { return workspace.PauseSchedule(missingID) },
		"resume": func() (app.Schedule, error) { return workspace.ResumeSchedule(missingID) },
		"remove": func() (app.Schedule, error) { return workspace.RemoveSchedule(missingID) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			_, mutationErr := operation()
			var notFound *app.ScheduleNotFoundError
			if !errors.Is(mutationErr, app.ErrScheduleNotFound) || !errors.As(mutationErr, &notFound) || notFound.ScheduleID != missingID {
				t.Fatalf("missing schedule error = %#v, %v", notFound, mutationErr)
			}
			var conflict *app.ScheduleRevisionConflictError
			if errors.As(mutationErr, &conflict) || errors.Is(mutationErr, app.ErrScheduleRevisionExhausted) {
				t.Fatalf("missing schedule was reported as a revision failure: %v", mutationErr)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("missing %s changed scheduler.json:\nbefore=%s\nafter=%s", name, before, after)
			}
			config, readErr := workspace.Scheduler()
			if readErr != nil || len(config.Schedules) != 1 || !reflect.DeepEqual(config.Schedules[0], created) {
				t.Fatalf("missing %s changed Scheduler state: %#v, %v", name, config.Schedules, readErr)
			}
		})
	}

	removed, err := workspace.RemoveSchedule(created.ID)
	if err != nil || removed.ID != created.ID {
		t.Fatalf("first remove = %#v, %v", removed, err)
	}
	afterFirstRemove, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspace.RemoveSchedule(created.ID)
	var notFound *app.ScheduleNotFoundError
	if !errors.Is(err, app.ErrScheduleNotFound) || !errors.As(err, &notFound) || notFound.ScheduleID != created.ID {
		t.Fatalf("second remove error = %#v, %v", notFound, err)
	}
	afterSecondRemove, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterSecondRemove, afterFirstRemove) {
		t.Fatalf("second remove was not atomic:\nfirst=%s\nsecond=%s", afterFirstRemove, afterSecondRemove)
	}
}

func TestScheduleIntervalMutationRequiresPersistableSuccessor(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
	unsafe := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: latest.Format(time.RFC3339Nano),
	}
	if err := app.ValidateScheduleTrigger(unsafe); err != nil {
		t.Fatalf("legacy interval is not structurally readable: %v", err)
	}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Never persist an invalid cursor", Condition: "every minute", Target: "workspace", Trigger: &unsafe,
	}); !errors.Is(err, app.ErrScheduleOccurrenceOutOfRange) {
		t.Fatalf("unsafe create error = %v", err)
	}

	safe := unsafe
	safe.AnchorAt = latest.Add(-time.Minute).Format(time.RFC3339Nano)
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Reach the persistence boundary", Condition: "every minute", Target: "workspace", Trigger: &safe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision, Trigger: &unsafe,
	}); !errors.Is(err, app.ErrScheduleOccurrenceOutOfRange) {
		t.Fatalf("unsafe update error = %v", err)
	}
	stored, err := workspace.Scheduler()
	if err != nil || len(stored.Schedules) != 1 || stored.Schedules[0].Revision != created.Revision || *stored.Schedules[0].Trigger != safe {
		t.Fatalf("rejected mutation changed definitions: %#v, %v", stored.Schedules, err)
	}
}

func schedulerRevisionFixture(t *testing.T, state string, revision uint64) (*app.Workspace, app.Schedule, string, []byte) {
	t.Helper()
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Revision boundary", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{
			Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: "2026-08-24T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	config.Schedules[0].Revision = revision
	config.Schedules[0].State = state
	fixture, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixture = append(fixture, '\n')
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	created = config.Schedules[0]
	return workspace, created, path, fixture
}

func assertSchedulerRevisionFixture(t *testing.T, workspace *app.Workspace, path string, before []byte, want app.Schedule) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("rejected revision mutation changed scheduler.json:\nbefore=%s\nafter=%s", before, after)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatalf("Scheduler failed after rejected revision mutation: %v", err)
	}
	if len(config.Schedules) != 1 || !reflect.DeepEqual(config.Schedules[0], want) {
		t.Fatalf("schedule changed after rejected revision mutation: got %#v, want %#v", config.Schedules, want)
	}
}

func TestScheduleRevisionExhaustionIsAtomic(t *testing.T) {
	maximumRevision := ^uint64(0)
	tests := []struct {
		name  string
		state string
		apply func(*app.Workspace, app.Schedule) (app.Schedule, error)
	}{
		{
			name: "active to paused", state: app.ScheduleStateActive,
			apply: func(workspace *app.Workspace, schedule app.Schedule) (app.Schedule, error) {
				return workspace.PauseSchedule(schedule.ID)
			},
		},
		{
			name: "paused to active", state: app.ScheduleStatePaused,
			apply: func(workspace *app.Workspace, schedule app.Schedule) (app.Schedule, error) {
				return workspace.ResumeSchedule(schedule.ID)
			},
		},
		{
			name: "semantic update", state: app.ScheduleStateActive,
			apply: func(workspace *app.Workspace, schedule app.Schedule) (app.Schedule, error) {
				description := "Changed at the boundary"
				return workspace.UpdateSchedule(app.UpdateScheduleInput{
					ID: schedule.ID, ExpectedRevision: schedule.Revision, Description: &description,
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace, original, path, before := schedulerRevisionFixture(t, test.state, maximumRevision)
			_, err := test.apply(workspace, original)
			if !errors.Is(err, app.ErrScheduleRevisionExhausted) {
				t.Fatalf("revision exhaustion error = %v", err)
			}
			assertSchedulerRevisionFixture(t, workspace, path, before, original)
		})
	}
}

func TestScheduleRevisionExhaustionKeepsSameStateIdempotent(t *testing.T) {
	maximumRevision := ^uint64(0)
	tests := []struct {
		name  string
		state string
		apply func(*app.Workspace, string) (app.Schedule, error)
	}{
		{name: "pause paused", state: app.ScheduleStatePaused, apply: (*app.Workspace).PauseSchedule},
		{name: "resume active", state: app.ScheduleStateActive, apply: (*app.Workspace).ResumeSchedule},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace, original, path, before := schedulerRevisionFixture(t, test.state, maximumRevision)
			unchanged, err := test.apply(workspace, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unchanged, original) {
				t.Fatalf("idempotent state change = %#v, want %#v", unchanged, original)
			}
			assertSchedulerRevisionFixture(t, workspace, path, before, original)
		})
	}
}

func TestScheduleRevisionMaximumRemainsValid(t *testing.T) {
	maximumRevision := ^uint64(0)
	tests := []struct {
		name  string
		state string
		apply func(*app.Workspace, app.Schedule) (app.Schedule, error)
	}{
		{
			name: "pause", state: app.ScheduleStateActive,
			apply: func(workspace *app.Workspace, schedule app.Schedule) (app.Schedule, error) {
				return workspace.PauseSchedule(schedule.ID)
			},
		},
		{
			name: "resume", state: app.ScheduleStatePaused,
			apply: func(workspace *app.Workspace, schedule app.Schedule) (app.Schedule, error) {
				return workspace.ResumeSchedule(schedule.ID)
			},
		},
		{
			name: "update", state: app.ScheduleStateActive,
			apply: func(workspace *app.Workspace, schedule app.Schedule) (app.Schedule, error) {
				description := "Maximum revision"
				return workspace.UpdateSchedule(app.UpdateScheduleInput{
					ID: schedule.ID, ExpectedRevision: schedule.Revision, Description: &description,
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace, original, _, _ := schedulerRevisionFixture(t, test.state, maximumRevision-1)
			updated, err := test.apply(workspace, original)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Revision != maximumRevision {
				t.Fatalf("revision = %d, want %d", updated.Revision, maximumRevision)
			}
			config, err := workspace.Scheduler()
			if err != nil {
				t.Fatalf("Scheduler failed at maximum revision: %v", err)
			}
			if len(config.Schedules) != 1 || config.Schedules[0].Revision != maximumRevision {
				t.Fatalf("persisted maximum revision = %#v", config.Schedules)
			}
		})
	}
}

func TestScheduleRevisionConflictPrecedesExhaustion(t *testing.T) {
	maximumRevision := ^uint64(0)
	workspace, original, path, before := schedulerRevisionFixture(t, app.ScheduleStateActive, maximumRevision)
	description := "Stale update"
	_, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: original.ID, ExpectedRevision: maximumRevision - 1, Description: &description,
	})
	var conflict *app.ScheduleRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Expected != maximumRevision-1 || conflict.Actual != maximumRevision {
		t.Fatalf("stale maximum-revision update error = %#v, %v", conflict, err)
	}
	if errors.Is(err, app.ErrScheduleRevisionExhausted) {
		t.Fatalf("stale update returned revision exhaustion: %v", err)
	}
	assertSchedulerRevisionFixture(t, workspace, path, before, original)
}

func TestPauseScheduleCommitsOnlyBeforeOneTimeDeadline(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	futureAt := time.Now().UTC().Add(time.Hour)
	future, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Pause before due", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: futureAt.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := workspace.PauseSchedule(future.ID)
	if err != nil {
		t.Fatal(err)
	}
	pauseBoundary, err := time.Parse(time.RFC3339Nano, paused.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != app.ScheduleStatePaused || paused.Revision != future.Revision+1 || !pauseBoundary.Before(futureAt) {
		t.Fatalf("future pause transition = %#v, boundary %s", paused, pauseBoundary)
	}

	due, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Do not pause after due", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	for index := range config.Schedules {
		if config.Schedules[index].ID == due.ID {
			config.Schedules[index].Trigger.At = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
		}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := workspace.PauseSchedule(due.ID); !errors.Is(err, app.ErrScheduleOccurrenceDue) {
		t.Fatalf("due one-time pause error = %v", err)
	}
	loaded, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	for _, schedule := range loaded.Schedules {
		if schedule.ID == due.ID && (schedule.State != app.ScheduleStateActive || schedule.Revision != due.Revision || schedule.UpdatedAt != due.UpdatedAt) {
			t.Fatalf("rejected pause mutated due definition: before=%#v after=%#v", due, schedule)
		}
	}
}

func TestScheduleChangeOperationRejectsUnknownValues(t *testing.T) {
	operation, err := app.ParseScheduleChangeOperation(" pause ")
	if err != nil || operation != app.ScheduleChangePause {
		t.Fatalf("parsed operation = %q, %v", operation, err)
	}
	if _, err := app.ParseScheduleChangeOperation("restart"); err == nil || err.Error() != `unsupported Scheduler change "restart"` {
		t.Fatalf("invalid operation error = %v", err)
	}
}

func TestCronDowntimeCoalescingIsBounded(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"}
	first := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	now := first.Add(100_001 * time.Minute)
	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(trigger, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || count != 100_000 || !last.Equal(first.Add(99_999*time.Minute)) || !next.After(now) {
		t.Fatalf("bounded coalescing = last %s next %s count %d truncated %v", last, next, count, truncated)
	}
}

func TestCronSuccessorExtendsAcrossSparseLeapGaps(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 0 0 29 2 *", TimeZone: "UTC"}
	after := time.Date(2096, time.February, 29, 0, 0, 0, 0, time.UTC)
	next, err := app.NextScheduleOccurrence(trigger, after)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2104, time.February, 29, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("sparse leap successor = %s, want %s", next, want)
	}

	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(
		trigger,
		time.Date(2096, time.February, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2104, time.March, 1, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if wantLast, wantNext := time.Date(2104, time.February, 29, 0, 0, 0, 0, time.UTC), time.Date(2108, time.February, 29, 0, 0, 0, 0, time.UTC); !last.Equal(wantLast) || !next.Equal(wantNext) || count != 2 || truncated {
		t.Fatalf("sparse coalescing = last %s next %s count %d truncated %v", last, next, count, truncated)
	}
}

func TestCronSuccessorPreservesSixFieldSecond(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "30 * * * * *", TimeZone: "UTC"}
	after := time.Date(2026, time.August, 24, 12, 0, 29, 999999999, time.UTC)
	next, err := app.NextScheduleOccurrence(trigger, after)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.August, 24, 12, 0, 30, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("second-field successor = %s, want %s", next, want)
	}
}

func TestCronSuccessorTerminatesAtPersistenceBoundary(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "59 59 23 31 12 *", TimeZone: "UTC"}
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	if next, err := app.NextScheduleOccurrence(trigger, latest); !errors.Is(err, app.ErrScheduleOccurrenceOutOfRange) || !next.IsZero() {
		t.Fatalf("terminal cron successor = %s, %v", next, err)
	}

	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(trigger, latest, latest)
	if err != nil || !last.Equal(latest) || !next.IsZero() || count != 1 || truncated {
		t.Fatalf("terminal cron coalescing = last %s next %s count %d truncated %v err %v", last, next, count, truncated, err)
	}
}

func TestCronTruncationTerminatesAtPersistenceBoundary(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"}
	now := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	first := now.Truncate(time.Minute).Add(-100_001 * time.Minute)
	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(trigger, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if wantLast := first.Add(99_999 * time.Minute); !last.Equal(wantLast) || !next.IsZero() || count != 100_000 || !truncated {
		t.Fatalf("terminal truncation = last %s next %s count %d truncated %v", last, next, count, truncated)
	}
}

func TestIntervalNextOccurrenceUsesOverflowSafeOrdinals(t *testing.T) {
	ancient := time.Date(1700, time.January, 1, 0, 0, 0, 123456789, time.UTC)
	aligned := time.Date(2026, time.August, 24, 12, 34, 0, 123456789, time.UTC)
	minuteTrigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: ancient.Format(time.RFC3339Nano),
	}
	maximumEverySeconds := int64(^uint64(0)>>1) / int64(time.Second)
	maximumTrigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: maximumEverySeconds,
		AnchorAt: ancient.Format(time.RFC3339Nano),
	}
	earliest := time.Date(0, time.January, 1, 0, 0, 0, 987654321, time.UTC)
	earliestTrigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: earliest.Format(time.RFC3339Nano),
	}
	offsetTrigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: "1700-01-01T00:00:00.333333333-07:30",
	}
	offsetBoundary := time.Date(2026, time.August, 24, 12, 34, 0, 333333333, time.UTC)

	tests := []struct {
		name    string
		trigger app.ScheduleTrigger
		after   time.Time
		want    time.Time
	}{
		{name: "ancient equality", trigger: minuteTrigger, after: aligned, want: aligned.Add(time.Minute)},
		{name: "ancient nanosecond before", trigger: minuteTrigger, after: aligned.Add(-time.Nanosecond), want: aligned},
		{name: "ancient nanosecond after", trigger: minuteTrigger, after: aligned.Add(time.Nanosecond), want: aligned.Add(time.Minute)},
		{name: "before anchor", trigger: minuteTrigger, after: ancient.Add(-time.Nanosecond), want: ancient},
		{name: "earliest anchor equality", trigger: earliestTrigger, after: earliest, want: earliest.Add(time.Minute)},
		{name: "earliest anchor to modern boundary", trigger: earliestTrigger, after: aligned.Add(864197532 * time.Nanosecond), want: aligned.Add(time.Minute).Add(864197532 * time.Nanosecond)},
		{name: "offset anchor absolute instant", trigger: offsetTrigger, after: offsetBoundary, want: offsetBoundary.Add(time.Minute)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next, err := app.NextScheduleOccurrence(test.trigger, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if !next.Equal(test.want) {
				t.Fatalf("next = %s, want %s", next, test.want)
			}
			if !next.After(test.after) {
				t.Fatalf("next %s is not after boundary %s", next, test.after)
			}
			anchor, _ := time.Parse(time.RFC3339Nano, test.trigger.AnchorAt)
			if next.Nanosecond() != anchor.Nanosecond() {
				t.Fatalf("next nanosecond = %d, anchor nanosecond = %d", next.Nanosecond(), anchor.Nanosecond())
			}
		})
	}

	maximumNext, err := app.NextScheduleOccurrence(maximumTrigger, aligned)
	if err != nil {
		t.Fatal(err)
	}
	if !maximumNext.After(aligned) || maximumNext.Nanosecond() != ancient.Nanosecond() {
		t.Fatalf("maximum interval next = %s after %s", maximumNext, aligned)
	}
	secondMaximumNext, err := app.NextScheduleOccurrence(maximumTrigger, maximumNext)
	if err != nil {
		t.Fatal(err)
	}
	if !secondMaximumNext.After(maximumNext) {
		t.Fatalf("second maximum interval occurrence %s is not after %s", secondMaximumNext, maximumNext)
	}
}

func TestIntervalOccurrenceRejectsRFC3339NanoOverflow(t *testing.T) {
	anchor := time.Date(9999, time.December, 31, 23, 58, 59, 999999999, time.UTC)
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: anchor.Format(time.RFC3339Nano),
	}
	latestTrigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: latest.Format(time.RFC3339Nano),
	}

	next, err := app.NextScheduleOccurrence(trigger, anchor)
	if err != nil || !next.Equal(latest) {
		t.Fatalf("latest representable next = %s, %v", next, err)
	}
	next, err = app.NextScheduleOccurrence(trigger, latest.Add(-time.Nanosecond))
	if err != nil || !next.Equal(latest) {
		t.Fatalf("next immediately before latest = %s, %v", next, err)
	}
	if next, err = app.NextScheduleOccurrence(latestTrigger, latest.Add(-time.Nanosecond)); err != nil || !next.Equal(latest) {
		t.Fatalf("latest anchor next = %s, %v", next, err)
	}
	if next, err = app.NextScheduleOccurrence(latestTrigger, latest); err != nil || !next.IsZero() {
		t.Fatalf("terminal next = %s, %v", next, err)
	}

	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(trigger, anchor, anchor)
	if err != nil || !last.Equal(anchor) || !next.Equal(latest) || count != 1 || truncated {
		t.Fatalf("latest coalescing = last %s next %s count %d truncated %v err %v", last, next, count, truncated, err)
	}
	last, next, count, truncated, err = app.CoalescedScheduleOccurrence(latestTrigger, latest, latest)
	if err != nil || !last.Equal(latest) || !next.IsZero() || count != 1 || truncated {
		t.Fatalf("terminal coalescing = last %s next %s count %d truncated %v err %v", last, next, count, truncated, err)
	}
}

func TestIntervalCoalescingUsesTruthfulOverflowSafeBounds(t *testing.T) {
	first := time.Date(1700, time.January, 1, 0, 0, 0, 246813579, time.UTC)
	now := time.Date(2026, time.August, 24, 12, 34, 0, 246813579, time.UTC)
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60,
		AnchorAt: first.Format(time.RFC3339Nano),
	}
	wantCount := int((now.Unix()-first.Unix())/trigger.EverySeconds) + 1

	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(trigger, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if !last.Equal(now) || !next.Equal(now.Add(time.Minute)) || count != wantCount || truncated {
		t.Fatalf("coalescing = last %s next %s count %d truncated %v", last, next, count, truncated)
	}
	if last.After(now) || !next.After(now) || !last.Before(next) || count <= 1 {
		t.Fatalf("non-monotonic coalescing = last %s now %s next %s count %d", last, now, next, count)
	}

	last, next, count, truncated, err = app.CoalescedScheduleOccurrence(trigger, first, first.Add(-time.Nanosecond))
	if err != nil || !last.IsZero() || !next.Equal(first) || count != 0 || truncated {
		t.Fatalf("pre-anchor coalescing = last %s next %s count %d truncated %v err %v", last, next, count, truncated, err)
	}

	last, next, count, truncated, err = app.CoalescedScheduleOccurrence(trigger, first, now.Add(-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if !last.Equal(now.Add(-time.Minute)) || !next.Equal(now) || count != wantCount-1 || truncated {
		t.Fatalf("pre-boundary coalescing = last %s next %s count %d truncated %v", last, next, count, truncated)
	}

	maximumEverySeconds := int64(^uint64(0)>>1) / int64(time.Second)
	maximumTrigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: maximumEverySeconds,
		AnchorAt: first.Format(time.RFC3339Nano),
	}
	wantLast := time.Unix(first.Unix()+maximumEverySeconds, int64(first.Nanosecond())).UTC()
	wantNext := time.Unix(first.Unix()+2*maximumEverySeconds, int64(first.Nanosecond())).UTC()
	last, next, count, truncated, err = app.CoalescedScheduleOccurrence(maximumTrigger, first, now)
	if err != nil || !last.Equal(wantLast) || !next.Equal(wantNext) || count != 2 || truncated {
		t.Fatalf("maximum interval coalescing = last %s next %s count %d truncated %v err %v", last, next, count, truncated, err)
	}
}

func TestNeedsCompilationScheduleCannotEnterInvalidPausedState(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	schedules, _ := migrateSchedulerV1ForTest(t, workspace, schedulerV1TestDefinition{
		ID:          "schedule-111111111111111111111111",
		Description: "Compile me",
		Condition:   "tomorrow",
		Target:      "workspace",
		CreatedAt:   "2026-08-01T00:00:00Z",
		UpdatedAt:   "2026-08-01T00:00:00Z",
	})
	created := schedules[0]
	if _, err := workspace.PauseSchedule(created.ID); err == nil {
		t.Fatal("uncompiled schedule unexpectedly paused")
	}
	config, err := workspace.Scheduler()
	if err != nil || config.Schedules[0].State != app.ScheduleStateNeedsCompilation {
		t.Fatalf("failed pause corrupted Scheduler configuration: %#v, %v", config, err)
	}
}

func TestCronNextOccurrenceHonorsDSTInstants(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 30 2 * * *", TimeZone: "America/New_York"}
	after := time.Date(2026, 3, 7, 8, 0, 0, 0, time.UTC)
	next, err := app.NextScheduleOccurrence(trigger, after)
	if err != nil {
		t.Fatal(err)
	}
	// 02:30 does not exist on the spring-forward day, so the next nominal
	// occurrence is the following day at 02:30 EDT (06:30 UTC).
	if want := time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("spring DST next = %s, want %s", next, want)
	}

	fall := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 30 1 * * *", TimeZone: "America/New_York"}
	first, err := app.NextScheduleOccurrence(fall, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.NextScheduleOccurrence(fall, first)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) || !second.Equal(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("fall DST occurrences = %s and %s", first, second)
	}
}
