package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

const (
	schedulerOccurrenceReasonTime      = "scheduled_time"
	schedulerOccurrenceReasonCoalesced = "coalesced_after_downtime"
	schedulerOutcomeAccepted           = "mailbox_accepted"
	schedulerOutcomeBusy               = "skipped_target_busy"
	schedulerOutcomePaused             = "skipped_while_paused"
	schedulerOutcomeAttention          = "attention_required"
	schedulerOutcomeRangeExhausted     = "completed_persistence_range_exhausted"
)

var (
	errNativeSchedulerUpdateTriggerRequired = errors.New("update requires a complete trigger")
	errNativeSchedulerPauseCompleted        = errors.New("completed schedule cannot be paused")
)

// NativeScheduler owns the three scheduler boundaries: portable definitions
// plus runtime projection (Snapshot), serialized mutations (Change), and
// deterministic due-work processing (Reconcile). It never creates a second
// execution protocol; occurrences are ordinary resource mailbox messages.
type NativeScheduler struct {
	manager   *agentManager
	workspace serveWorkspace
}

type NativeSchedulerChange struct {
	Operation        app.ScheduleChangeOperation
	ID               string
	ExpectedRevision uint64
	Description      *string
	Condition        *string
	Guard            *string
	Target           *string
	Trigger          *app.ScheduleTrigger
}

func newNativeScheduler(manager *agentManager, workspace serveWorkspace) *NativeScheduler {
	return &NativeScheduler{manager: manager, workspace: workspace}
}

// requireWorkspaceOwnership is evaluated from inside the Scheduler resource
// controller immediately before native scheduling may write portable or
// runtime state. Direct application-layer fixtures do not have a Server, but
// every production NativeScheduler does and must still own the Workspace at
// this durable boundary.
func (n *NativeScheduler) requireWorkspaceOwnership() error {
	if n.manager == nil || n.manager.server == nil {
		return nil
	}
	_, err := n.manager.server.requireWorkspaceInstanceOwnership(n.workspace)
	return err
}

func schedulerConfigDigest(config app.SchedulerConfig) (string, error) {
	data, err := json.Marshal(config.Schedules)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func schedulerTriggerDigest(trigger *app.ScheduleTrigger) (string, error) {
	if trigger == nil {
		return "", nil
	}
	data, err := json.Marshal(trigger)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (n *NativeScheduler) Snapshot(now time.Time) (app.SchedulerSnapshot, error) {
	workspace, err := app.OpenWorkspace(n.workspace.Path)
	if err != nil {
		return app.SchedulerSnapshot{}, err
	}
	config, err := workspace.Scheduler()
	if err != nil {
		return app.SchedulerSnapshot{}, err
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return app.SchedulerSnapshot{}, err
	}
	result := app.SchedulerSnapshot{SchemaVersion: config.SchemaVersion, AgentBinding: config.AgentBinding, Schedules: make([]app.ScheduleSnapshot, 0, len(config.Schedules))}
	var earliest time.Time
	for _, schedule := range config.Schedules {
		runtime := store.Scheduler.Schedules[schedule.ID]
		effective := schedule.State
		projection := app.ScheduleSnapshot{Schedule: schedule, EffectiveState: effective}
		projectRuntime := runtime.Revision == schedule.Revision &&
			schedulerRuntimeActivationRevision(runtime) == schedulerPortableActivationRevision(schedule) && runtime.EffectiveState != ""
		if !projectRuntime {
			projectRuntime, err = schedulerRuntimeCanProjectForward(runtime, schedule)
			if err != nil {
				return app.SchedulerSnapshot{}, err
			}
		}
		if projectRuntime {
			effective = runtime.EffectiveState
			projection.EffectiveState = effective
			projection.NextRunAt = runtime.NextRunAt
			projection.LastOccurrenceAt = runtime.LastOccurrenceAt
			projection.LastOutcome = runtime.LastOutcome
			projection.LastError = runtime.LastError
			deadline := schedulerRuntimeDeadline(runtime, now)
			if !deadline.IsZero() && (earliest.IsZero() || deadline.Before(earliest)) {
				earliest = deadline
			}
		}
		result.Schedules = append(result.Schedules, projection)
	}
	if !earliest.IsZero() {
		if earliest.Before(now) {
			earliest = now
		}
		result.NextWakeAt = earliest.Format(time.RFC3339Nano)
	}
	return result, nil
}

func schedulerRuntimeCanProjectForward(runtime schedulerScheduleRuntime, schedule app.Schedule) (bool, error) {
	if runtime.Revision == 0 || runtime.Revision >= schedule.Revision {
		return false, nil
	}
	return schedulerRuntimeMatchesPortableDefinition(runtime, schedule)
}

// schedulerRuntimeMatchesPortableDefinition is the shared semantic projection
// contract for Snapshot and metadata activation preflight. Revision ordering is
// intentionally left to those callers because Snapshot requires a strictly
// older checkpoint while activation also accepts the current revision.
func schedulerRuntimeMatchesPortableDefinition(runtime schedulerScheduleRuntime, schedule app.Schedule) (bool, error) {
	if runtime.EffectiveState == "" || runtime.TriggerDigest == "" {
		return false, nil
	}
	triggerDigest, err := schedulerTriggerDigest(schedule.Trigger)
	if err != nil {
		return false, err
	}
	if triggerDigest == "" || runtime.TriggerDigest != triggerDigest {
		return false, nil
	}
	// A one-time trigger has one nominal occurrence independent of later
	// metadata, including its target. Once that occurrence is terminal, a
	// target-only revision must not make Snapshot advertise fresh due work while
	// Reconcile catches the portable revision up.
	if _, knownTarget := schedulerRuntimeKnownTarget(runtime); knownTarget && runtimeCompletesSameOneTimeOccurrence(runtime, schedule) {
		switch schedule.State {
		case app.ScheduleStateActive, app.ScheduleStatePaused, app.ScheduleStateCompleted:
			return true, nil
		}
	}
	if schedulerRuntimeActivationRevision(runtime) != schedulerPortableActivationRevision(schedule) {
		return false, nil
	}
	if !schedulerRuntimeTargetsSchedule(runtime, schedule.Target) {
		return false, nil
	}
	switch schedule.State {
	case app.ScheduleStateActive:
		switch runtime.EffectiveState {
		case app.ScheduleStateActive:
			return true, nil
		case schedulerOutcomeAttention:
			return runtime.Prepared != nil, nil
		case app.ScheduleStateCompleted:
			return runtimeCompletesSameOneTimeOccurrence(runtime, schedule), nil
		}
	case app.ScheduleStatePaused:
		if runtime.EffectiveState == app.ScheduleStatePaused {
			return true, nil
		}
		return runtimeCompletesSameOneTimeOccurrence(runtime, schedule), nil
	case app.ScheduleStateCompleted:
		return runtime.EffectiveState == app.ScheduleStateCompleted, nil
	}
	return false, nil
}

func (n *NativeScheduler) Change(ctx context.Context, change NativeSchedulerChange) (app.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return app.Schedule{}, err
	}
	if err := change.Operation.Validate(); err != nil {
		return app.Schedule{}, err
	}
	if err := n.requireWorkspaceOwnership(); err != nil {
		return app.Schedule{}, err
	}
	workspace, err := app.OpenWorkspace(n.workspace.Path)
	if err != nil {
		return app.Schedule{}, err
	}
	switch change.Operation {
	case app.ScheduleChangeCreate:
		if change.Description == nil || change.Condition == nil || change.Target == nil || change.Trigger == nil {
			return app.Schedule{}, errors.New("create requires description, condition, target, and trigger")
		}
		guard := ""
		if change.Guard != nil {
			guard = *change.Guard
		}
		return workspace.AddSchedule(app.CreateScheduleInput{
			Description: *change.Description,
			Condition:   *change.Condition,
			Guard:       guard,
			Target:      *change.Target,
			Trigger:     change.Trigger,
		})
	case app.ScheduleChangeUpdate:
		if change.ID == "" || change.ExpectedRevision == 0 {
			return app.Schedule{}, errors.New("update requires id and expectedRevision")
		}
		if change.Trigger == nil {
			return app.Schedule{}, errNativeSchedulerUpdateTriggerRequired
		}
		input := app.UpdateScheduleInput{
			ID:               change.ID,
			ExpectedRevision: change.ExpectedRevision,
			Description:      change.Description,
			Condition:        change.Condition,
			Guard:            change.Guard,
			Target:           change.Target,
			Trigger:          change.Trigger,
		}
		current, preview, err := workspace.PreviewScheduleUpdate(input)
		if err != nil {
			return app.Schedule{}, err
		}
		materialize := scheduleUpdatePreservesRepeatingActivation(current, preview)
		if materialize {
			runtime, err := n.schedulerRuntime(current.ID)
			if err != nil {
				return app.Schedule{}, err
			}
			projectable, err := schedulerRuntimePreservesRepeatingActivation(runtime, current)
			if err != nil {
				return app.Schedule{}, err
			}
			materialize = !projectable
		}
		mutationBoundary := time.Now()
		if n.manager != nil {
			mutationBoundary = n.manager.now()
		}
		if materialize {
			if err := n.materializeScheduleActivation(ctx, current, mutationBoundary); err != nil {
				return app.Schedule{}, err
			}
		}
		updated, err := workspace.UpdateSchedule(input)
		if err != nil || !materialize {
			return updated, err
		}
		return updated, n.reconcileSchedule(ctx, updated, mutationBoundary)
	case app.ScheduleChangePause:
		if change.ID == "" {
			return app.Schedule{}, errors.New("pause requires id")
		}
		config, err := workspace.Scheduler()
		if err != nil {
			return app.Schedule{}, err
		}
		var pausing app.Schedule
		for _, schedule := range config.Schedules {
			if schedule.ID != change.ID || schedule.State == app.ScheduleStatePaused {
				continue
			}
			pausing = schedule
			if schedule.State == app.ScheduleStateCompleted {
				return app.Schedule{}, errNativeSchedulerPauseCompleted
			}
			if schedule.Trigger != nil && schedule.Trigger.Type == app.ScheduleTriggerAt {
				if err := n.reconcileSchedule(ctx, schedule, n.manager.now()); err != nil {
					return app.Schedule{}, err
				}
			}
			runtime, err := n.schedulerRuntime(change.ID)
			if err != nil {
				return app.Schedule{}, err
			}
			if runtimeClaimsSameOneTimeOccurrence(runtime, schedule) {
				return app.Schedule{}, errNativeSchedulerPauseCompleted
			}
			break
		}
		paused, err := workspace.PauseSchedule(change.ID)
		if err == nil {
			pauseBoundary := n.manager.now()
			if pausing.ID != "" {
				pauseBoundary, err = time.Parse(time.RFC3339Nano, paused.UpdatedAt)
				if err != nil {
					return app.Schedule{}, err
				}
			}
			return paused, n.reconcileSchedule(ctx, paused, pauseBoundary)
		}
		if !errors.Is(err, app.ErrScheduleOccurrenceDue) {
			return paused, err
		}
		at, parseErr := time.Parse(time.RFC3339Nano, pausing.Trigger.At)
		if parseErr != nil {
			return app.Schedule{}, parseErr
		}
		if reconcileErr := n.reconcileSchedule(ctx, pausing, at); reconcileErr != nil {
			return app.Schedule{}, reconcileErr
		}
		return app.Schedule{}, errNativeSchedulerPauseCompleted
	case app.ScheduleChangeResume:
		if change.ID == "" {
			return app.Schedule{}, errors.New("resume requires id")
		}
		config, err := workspace.Scheduler()
		if err != nil {
			return app.Schedule{}, err
		}
		resumingPaused := false
		for _, schedule := range config.Schedules {
			if schedule.ID == change.ID && schedule.State == app.ScheduleStatePaused {
				resumingPaused = true
				if err := n.reconcileSchedule(ctx, schedule, n.manager.now()); err != nil {
					return app.Schedule{}, err
				}
				break
			}
		}
		resumed, err := workspace.ResumeSchedule(change.ID)
		if err != nil {
			return app.Schedule{}, err
		}
		if resumingPaused {
			resumeBoundary, err := time.Parse(time.RFC3339Nano, resumed.UpdatedAt)
			if err != nil {
				return app.Schedule{}, err
			}
			return resumed, n.reconcileSchedule(ctx, resumed, resumeBoundary)
		}
		runtime, runtimeErr := n.schedulerRuntime(change.ID)
		if runtimeErr != nil {
			return app.Schedule{}, runtimeErr
		}
		if runtimeCompletesSameOneTimeOccurrence(runtime, resumed) && runtime.Revision != resumed.Revision {
			runtime.Revision = resumed.Revision
			runtime.ActivationRevision = schedulerPortableActivationRevision(resumed)
			runtime.TriggerDigest, runtimeErr = schedulerTriggerDigest(resumed.Trigger)
			if runtimeErr != nil {
				return app.Schedule{}, runtimeErr
			}
			runtime.Target = resumed.Target
			runtimeErr = n.storeSchedulerRuntime(change.ID, runtime)
		}
		if runtimeErr == nil && runtime.EffectiveState == schedulerOutcomeAttention {
			runtimeErr = n.reconcileSchedule(ctx, resumed, n.manager.now())
		}
		return resumed, runtimeErr
	case app.ScheduleChangeRemove:
		if change.ID == "" {
			return app.Schedule{}, errors.New("remove requires id")
		}
		return workspace.RemoveSchedule(change.ID)
	}
	return app.Schedule{}, fmt.Errorf("Scheduler change %q was not dispatched", change.Operation)
}

func scheduleUpdatePreservesRepeatingActivation(current, updated app.Schedule) bool {
	if current.State != app.ScheduleStateActive || current.Trigger == nil || updated.Trigger == nil ||
		current.Target != updated.Target || *current.Trigger != *updated.Trigger {
		return false
	}
	switch current.Trigger.Type {
	case app.ScheduleTriggerInterval, app.ScheduleTriggerCron:
		return true
	default:
		return false
	}
}

// schedulerRuntimePreservesRepeatingActivation proves that a checkpoint
// already represents the portable activation boundary, either at the current
// revision or through metadata-only revisions. A nonzero revision alone is
// insufficient: a semantic edit may have committed while the checkpoint still
// describes the preceding trigger or target.
func schedulerRuntimePreservesRepeatingActivation(runtime schedulerScheduleRuntime, schedule app.Schedule) (bool, error) {
	if runtime.Revision == 0 || runtime.Revision > schedule.Revision || runtime.EffectiveState == "" ||
		schedule.State != app.ScheduleStateActive || schedule.Trigger == nil {
		return false, nil
	}
	switch schedule.Trigger.Type {
	case app.ScheduleTriggerInterval, app.ScheduleTriggerCron:
	default:
		return false, nil
	}
	projectable, err := schedulerRuntimeMatchesPortableDefinition(runtime, schedule)
	if err != nil {
		return false, err
	}
	if !projectable {
		return false, nil
	}
	if runtime.Prepared != nil {
		prepared := runtime.Prepared
		if prepared.ScheduleID != schedule.ID || prepared.ScheduleRevision == 0 || prepared.ScheduleRevision > runtime.Revision ||
			prepared.Target != schedule.Target {
			return false, nil
		}
	}
	switch runtime.EffectiveState {
	case app.ScheduleStateActive:
		if runtime.Prepared != nil {
			return true, nil
		}
		_, err := time.Parse(time.RFC3339Nano, runtime.NextRunAt)
		return err == nil, nil
	case schedulerOutcomeAttention:
		return runtime.Prepared != nil, nil
	default:
		return false, nil
	}
}

// materializeScheduleActivation replaces a semantically stale checkpoint with
// the current definition's own UpdatedAt boundary before processing due work.
// This explicit reset is required for malformed lifecycle/prepared states that
// ordinary revision promotion must not preserve.
func (n *NativeScheduler) materializeScheduleActivation(ctx context.Context, schedule app.Schedule, now time.Time) error {
	runtime, err := initialScheduleRuntime(schedule, now)
	if err != nil {
		return err
	}
	if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		return err
	}
	return n.reconcileSchedule(ctx, schedule, now)
}

// Reconcile recovers any frozen occurrence, prepares due work, advances
// durable cursors, and returns the exact next timer deadline.
func (n *NativeScheduler) Reconcile(ctx context.Context, now time.Time) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if err := n.requireWorkspaceOwnership(); err != nil {
		return time.Time{}, err
	}
	workspace, err := app.OpenWorkspace(n.workspace.Path)
	if err != nil {
		return time.Time{}, err
	}
	config, err := workspace.Scheduler()
	if err != nil {
		return time.Time{}, err
	}
	if err := n.cancelLegacyTicks(ctx); err != nil {
		return time.Time{}, err
	}
	if err := n.reconcileMigration(ctx, config); err != nil {
		return time.Time{}, err
	}
	known := make(map[string]bool, len(config.Schedules))
	for _, schedule := range config.Schedules {
		known[schedule.ID] = true
		if err := n.reconcileSchedule(ctx, schedule, now); err != nil {
			return time.Time{}, fmt.Errorf("reconcile schedule %s: %w", schedule.ID, err)
		}
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return time.Time{}, err
	}
	stale := false
	for id := range store.Scheduler.Schedules {
		if !known[id] {
			stale = true
			break
		}
	}
	if stale {
		_, err = mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
			for id := range store.Scheduler.Schedules {
				if !known[id] {
					delete(store.Scheduler.Schedules, id)
				}
			}
			return nil
		})
		if err != nil {
			return time.Time{}, err
		}
	}
	snapshot, err := n.Snapshot(now)
	if err != nil || snapshot.NextWakeAt == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, snapshot.NextWakeAt)
}

func (n *NativeScheduler) reconcileSchedule(ctx context.Context, schedule app.Schedule, now time.Time) error {
	runtime, err := n.schedulerRuntime(schedule.ID)
	if err != nil {
		return err
	}
	if runtime.Target == "" && schedulerRuntimeTargetsSchedule(runtime, schedule.Target) {
		// Backfill legacy checkpoints only when their frozen delivery identity
		// establishes the portable target without ambiguity.
		runtime.Target = schedule.Target
	}
	triggerDigest, err := schedulerTriggerDigest(schedule.Trigger)
	if err != nil {
		return err
	}
	revisionChanged := runtime.Revision != schedule.Revision
	triggerChanged := runtime.TriggerDigest != triggerDigest
	activationChanged := schedulerRuntimeActivationRevision(runtime) != schedulerPortableActivationRevision(schedule)
	if revisionChanged || triggerChanged || activationChanged {
		sameKnownTrigger := runtime.TriggerDigest != "" && !triggerChanged
		_, hadKnownTarget := schedulerRuntimeKnownTarget(runtime)
		sameTarget := schedulerRuntimeTargetsSchedule(runtime, schedule.Target)
		sameDefinition := revisionChanged && !activationChanged && sameKnownTrigger && sameTarget
		acceptedRetarget := false
		if revisionChanged && sameKnownTrigger && !sameTarget && runtimeClaimsSameOneTimeOccurrence(runtime, schedule) && runtime.Prepared != nil {
			accepted, err := n.preparedOccurrenceAccepted(runtime.Prepared)
			if err != nil {
				return err
			}
			if accepted {
				commitAcceptedPreparedOccurrence(&runtime, schedule.State)
				acceptedRetarget = true
			}
		}
		terminalSameOneTime := revisionChanged && sameKnownTrigger && (acceptedRetarget || hadKnownTarget && runtimeCompletesSameOneTimeOccurrence(runtime, schedule))
		resumingPaused := sameDefinition && runtime.EffectiveState == app.ScheduleStatePaused && schedule.State == app.ScheduleStateActive
		enteringPaused := sameDefinition && runtime.EffectiveState != app.ScheduleStatePaused && schedule.State == app.ScheduleStatePaused
		if resumingPaused {
			resumeBoundary, err := time.Parse(time.RFC3339Nano, schedule.UpdatedAt)
			if err != nil {
				return err
			}
			completeExpiredOneTimeWhilePaused(&runtime, schedule.Trigger, resumeBoundary)
		}
		if terminalSameOneTime || sameDefinition && !enteringPaused && (!resumingPaused || runtimeCompletesSameOneTimeOccurrence(runtime, schedule)) {
			// Definition-only revisions promote the portable identity while the
			// prepared payload and every delivery/cursor field remain frozen. A
			// terminal one-time occurrence also survives a retarget because the
			// unchanged trigger instant is the identity of its only nominal work.
			runtime.Revision = schedule.Revision
			runtime.ActivationRevision = schedulerPortableActivationRevision(schedule)
			runtime.TriggerDigest = triggerDigest
			runtime.Target = schedule.Target
		} else {
			previousRuntime := runtime
			runtime, err = initialScheduleRuntime(schedule, now)
			if err != nil {
				if errors.Is(err, app.ErrScheduleOccurrenceOutOfRange) {
					return n.recordScheduleError(schedule.ID, runtime, now, err)
				}
				return err
			}
			if (enteringPaused || resumingPaused) && !runtimeCompletesSameOneTimeOccurrence(runtime, schedule) {
				preserveScheduleOccurrenceProjection(&runtime, previousRuntime)
			}
		}
		if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
			return err
		}
	}
	if schedule.State == app.ScheduleStateNeedsCompilation || schedule.State == app.ScheduleStateCompleted {
		return nil
	}
	if schedule.State == app.ScheduleStatePaused {
		return n.reconcilePausedSchedule(schedule, runtime, now)
	}
	if runtime.EffectiveState == app.ScheduleStateCompleted {
		return nil
	}
	if runtime.EffectiveState == schedulerOutcomeAttention {
		if runtime.Prepared == nil {
			return nil
		}
		return n.deliverPrepared(ctx, schedule, runtime, now)
	}
	if retry := generationTime(runtime.RetryAt); !retry.IsZero() && now.Before(retry) {
		return nil
	}
	if runtime.Prepared != nil {
		return n.deliverPrepared(ctx, schedule, runtime, now)
	}
	due := generationTime(runtime.NextRunAt)
	if due.IsZero() || due.After(now) {
		return nil
	}
	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(*schedule.Trigger, due, now)
	if err != nil {
		return n.recordScheduleError(schedule.ID, runtime, now, err)
	}
	reason := schedulerOccurrenceReasonTime
	if count > 1 || truncated {
		reason = schedulerOccurrenceReasonCoalesced
	}
	prepared, err := n.prepareOccurrence(schedule, due, last, next, count, truncated, now, reason)
	if err != nil {
		return n.recordScheduleError(schedule.ID, runtime, now, err)
	}
	runtime.Prepared = &prepared
	runtime.RetryAt = ""
	runtime.LastError = ""
	if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		return err
	}
	return n.deliverPrepared(ctx, schedule, runtime, now)
}

func schedulerRuntimeTargetsSchedule(runtime schedulerScheduleRuntime, target string) bool {
	knownTarget, known := schedulerRuntimeKnownTarget(runtime)
	return known && knownTarget == target
}

func schedulerRuntimeKnownTarget(runtime schedulerScheduleRuntime) (string, bool) {
	knownTarget := runtime.Target
	preparedTarget := ""
	if runtime.Prepared != nil {
		preparedTarget = runtime.Prepared.Target
	}
	for _, candidate := range []string{preparedTarget, runtime.AttentionTarget} {
		if candidate == "" {
			continue
		}
		if knownTarget != "" && knownTarget != candidate {
			return "", false
		}
		knownTarget = candidate
	}
	return knownTarget, knownTarget != ""
}

func runtimeCompletesSameOneTimeOccurrence(runtime schedulerScheduleRuntime, schedule app.Schedule) bool {
	if runtime.EffectiveState != app.ScheduleStateCompleted || schedule.Trigger == nil || schedule.Trigger.Type != app.ScheduleTriggerAt || runtime.LastOccurrenceAt == "" {
		return false
	}
	completedAt, completedErr := time.Parse(time.RFC3339Nano, runtime.LastOccurrenceAt)
	triggerAt, triggerErr := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
	return completedErr == nil && triggerErr == nil && completedAt.Equal(triggerAt)
}

func runtimeClaimsSameOneTimeOccurrence(runtime schedulerScheduleRuntime, schedule app.Schedule) bool {
	if runtimeCompletesSameOneTimeOccurrence(runtime, schedule) {
		return true
	}
	if runtime.Prepared == nil || schedule.Trigger == nil || schedule.Trigger.Type != app.ScheduleTriggerAt || runtime.Prepared.ScheduleID != schedule.ID {
		return false
	}
	preparedAt, preparedErr := time.Parse(time.RFC3339Nano, runtime.Prepared.ScheduledFor)
	triggerAt, triggerErr := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
	return preparedErr == nil && triggerErr == nil && preparedAt.Equal(triggerAt)
}

func (n *NativeScheduler) preparedOccurrenceAccepted(prepared *schedulerPreparedOccurrence) (bool, error) {
	if prepared == nil {
		return false, nil
	}
	_, accepted, err := mailboxMessageByID(n.workspace.Path, prepared.MessageID)
	if err == nil {
		return accepted, nil
	}
	var apiErr *resourceAPIError
	if errors.As(err, &apiErr) && apiErr.Code == "message_receipt_expired" {
		// Expired receipts remain durable proof that this deterministic message
		// ID crossed the target mailbox acceptance boundary.
		return true, nil
	}
	return false, err
}

func commitAcceptedPreparedOccurrence(runtime *schedulerScheduleRuntime, portableState string) {
	prepared := runtime.Prepared
	if prepared == nil {
		return
	}
	runtime.EffectiveState = portableState
	runtime.AttentionTarget = ""
	runtime.Prepared = nil
	runtime.NextRunAt = prepared.NextRunAt
	runtime.RetryAt = ""
	runtime.RetryCount = 0
	runtime.LastOccurrenceAt = prepared.CoalescedThrough
	runtime.LastOutcome = schedulerOutcomeAccepted
	runtime.LastError = ""
	if prepared.NextRunAt == "" {
		runtime.EffectiveState = app.ScheduleStateCompleted
	}
}

func initialScheduleRuntime(schedule app.Schedule, now time.Time) (schedulerScheduleRuntime, error) {
	triggerDigest, err := schedulerTriggerDigest(schedule.Trigger)
	if err != nil {
		return schedulerScheduleRuntime{}, err
	}
	runtime := schedulerScheduleRuntime{
		Revision: schedule.Revision, ActivationRevision: schedulerPortableActivationRevision(schedule),
		TriggerDigest: triggerDigest, Target: schedule.Target, EffectiveState: schedule.State,
	}
	if schedule.Trigger == nil {
		return runtime, nil
	}
	if schedule.State == app.ScheduleStatePaused {
		if !completeExpiredOneTimeWhilePaused(&runtime, schedule.Trigger, now) && schedule.Trigger.Type == app.ScheduleTriggerAt {
			runtime.NextRunAt = schedule.Trigger.At
		}
		return runtime, nil
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, schedule.UpdatedAt)
	if err != nil {
		return runtime, err
	}
	if schedule.Trigger.Type == app.ScheduleTriggerAt {
		at, _ := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
		runtime.NextRunAt = at.Format(time.RFC3339Nano)
		return runtime, nil
	}
	next, err := app.NextScheduleOccurrence(*schedule.Trigger, updatedAt)
	if err != nil {
		return runtime, err
	}
	if !next.IsZero() {
		runtime.NextRunAt = next.Format(time.RFC3339Nano)
	} else if schedule.Trigger.Type == app.ScheduleTriggerInterval {
		runtime.EffectiveState = app.ScheduleStateCompleted
		runtime.LastOutcome = schedulerOutcomeRangeExhausted
		runtime.LastError = app.ErrScheduleOccurrenceOutOfRange.Error()
	}
	return runtime, nil
}

// preserveScheduleOccurrenceProjection carries user-visible execution history
// across pause and resume revisions. Lifecycle changes rebuild delivery cursors,
// but are not themselves occurrence results.
func preserveScheduleOccurrenceProjection(runtime *schedulerScheduleRuntime, previous schedulerScheduleRuntime) {
	runtime.LastOccurrenceAt = previous.LastOccurrenceAt
	runtime.LastOutcome = previous.LastOutcome
	runtime.LastError = previous.LastError
}

func (n *NativeScheduler) reconcilePausedSchedule(schedule app.Schedule, runtime schedulerScheduleRuntime, now time.Time) error {
	changed := runtime.Prepared != nil || runtime.RetryAt != "" || runtime.RetryCount != 0 ||
		runtime.AttentionTarget != "" || runtime.EffectiveState != app.ScheduleStatePaused
	runtime.Prepared, runtime.RetryAt = nil, ""
	runtime.RetryCount = 0
	runtime.AttentionTarget = ""
	runtime.EffectiveState = app.ScheduleStatePaused
	if completeExpiredOneTimeWhilePaused(&runtime, schedule.Trigger, now) {
		changed = true
	} else {
		nextRunAt := ""
		if schedule.Trigger != nil && schedule.Trigger.Type == app.ScheduleTriggerAt {
			nextRunAt = schedule.Trigger.At
		}
		if runtime.NextRunAt != nextRunAt {
			runtime.NextRunAt = nextRunAt
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return n.storeSchedulerRuntime(schedule.ID, runtime)
}

// completeExpiredOneTimeWhilePaused applies the terminal runtime transition
// for a one-time occurrence that became due while paused. It clears delivery
// cursors without changing the schedule revision or trigger identity.
func completeExpiredOneTimeWhilePaused(runtime *schedulerScheduleRuntime, trigger *app.ScheduleTrigger, now time.Time) bool {
	if trigger == nil || trigger.Type != app.ScheduleTriggerAt {
		return false
	}
	at, _ := time.Parse(time.RFC3339Nano, trigger.At)
	if at.After(now) {
		return false
	}
	runtime.EffectiveState = app.ScheduleStateCompleted
	runtime.LastOccurrenceAt = at.Format(time.RFC3339Nano)
	runtime.LastOutcome = schedulerOutcomePaused
	runtime.LastError = ""
	runtime.Prepared, runtime.NextRunAt, runtime.RetryAt = nil, "", ""
	runtime.RetryCount = 0
	runtime.AttentionTarget = ""
	return true
}

func (n *NativeScheduler) prepareOccurrence(schedule app.Schedule, first, last, next time.Time, count int, cronEnumerationCapped bool, recoveryCutoff time.Time, reason string) (schedulerPreparedOccurrence, error) {
	instanceID, err := workspaceInstanceID(n.workspace.Path)
	if err != nil {
		return schedulerPreparedOccurrence{}, err
	}
	language, err := workspaceContentLanguage(n.workspace.Path)
	if err != nil {
		return schedulerPreparedOccurrence{}, err
	}
	revision := fmt.Sprintf("%d", schedule.Revision)
	occurrenceID := notificationMessageID("schedule-occurrence", instanceID, schedule.ID, revision, first.Format(time.RFC3339Nano))
	messageID := notificationMessageID(resourceMessageTypeScheduleOccurrence, instanceID, schedule.ID, revision, first.Format(time.RFC3339Nano))
	enumeratedThrough, enumeratedCount, recoveryCutoffText := "", 0, ""
	if cronEnumerationCapped {
		if recoveryCutoff.IsZero() {
			return schedulerPreparedOccurrence{}, errors.New("capped cron occurrence requires a recovery cutoff")
		}
		enumeratedThrough = last.Format(time.RFC3339Nano)
		enumeratedCount = count
		recoveryCutoffText = recoveryCutoff.Format(time.RFC3339Nano)
	}
	causation := &resourceMessageCausation{
		Type: resourceMessageTypeScheduleOccurrence, SourceWorkspaceInstanceID: instanceID,
		SourceResourceID: app.SchedulerResourceID, Reason: reason,
		ScheduleID: schedule.ID, ScheduleRevision: schedule.Revision,
		OccurrenceID: occurrenceID, ScheduledFor: first.Format(time.RFC3339Nano),
		CoalescedFrom: first.Format(time.RFC3339Nano), CoalescedThrough: last.Format(time.RFC3339Nano), CoalescedCount: count,
		CronEnumerationCapped: cronEnumerationCapped, EnumeratedThrough: enumeratedThrough,
		EnumeratedCount: enumeratedCount, RecoveryCutoff: recoveryCutoffText,
	}
	prepared := schedulerPreparedOccurrence{
		ScheduleID: schedule.ID, ScheduleRevision: schedule.Revision,
		OccurrenceID: occurrenceID, MessageID: messageID, Target: schedule.Target,
		ScheduledFor: first.Format(time.RFC3339Nano), CoalescedThrough: last.Format(time.RFC3339Nano),
		CoalescedCount: count, CronEnumerationCapped: cronEnumerationCapped,
		EnumeratedThrough: enumeratedThrough, EnumeratedCount: enumeratedCount,
		RecoveryCutoff: recoveryCutoffText, Reason: reason, Causation: causation,
	}
	if !next.IsZero() {
		prepared.NextRunAt = next.Format(time.RFC3339Nano)
	}
	prepared.Text = strings.TrimSuffix(localize.MustRender(language, "scheduler-occurrence.md", map[string]any{
		"OccurrenceID": occurrenceID, "ScheduleID": schedule.ID, "Revision": schedule.Revision,
		"ScheduledFor": prepared.ScheduledFor, "CoalescedThrough": prepared.CoalescedThrough,
		"CoalescedCount": count, "Action": schedule.Description, "Guard": schedule.Guard,
		"CronEnumerationCapped": prepared.CronEnumerationCapped,
		"EnumeratedThrough":     prepared.EnumeratedThrough, "EnumeratedCount": prepared.EnumeratedCount,
		"RecoveryCutoff": prepared.RecoveryCutoff,
		"HasGuard":       schedule.Guard != "", "Condition": schedule.Condition,
		"HasNext": prepared.NextRunAt != "", "NextRunAt": prepared.NextRunAt,
	}), "\n")
	return prepared, nil
}

func (n *NativeScheduler) deliverPrepared(ctx context.Context, schedule app.Schedule, runtime schedulerScheduleRuntime, now time.Time) error {
	prepared := runtime.Prepared
	if prepared == nil {
		return nil
	}

	deliver := func() error {
		accepted, alreadyAccepted, err := mailboxMessageByID(n.workspace.Path, prepared.MessageID)
		expiredAcceptance := false
		if err != nil {
			var apiErr *resourceAPIError
			if !errors.As(err, &apiErr) || apiErr.Code != "message_receipt_expired" {
				return err
			}
			// An expired receipt is still authoritative proof that the target
			// accepted this deterministic message ID. Prepared pins keep that
			// evidence until this source cursor commits the accepted outcome.
			alreadyAccepted = true
			expiredAcceptance = true
		}
		if alreadyAccepted {
			// Mailbox acceptance is the Scheduler's durable handoff boundary. A
			// crash can leave Prepared behind while the target is subsequently
			// archived or removed, so commit this evidence before consulting the
			// target's current portable state.
			commitAcceptedPreparedOccurrence(&runtime, schedule.State)
			if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
				return err
			}
			if !expiredAcceptance && (accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting) {
				if err := n.manager.reconcileResourceMailboxLocked(ctx, n.workspace, prepared.Target); err != nil {
					recordMailboxFailure(n.workspace.Path, accepted.ID, err)
					n.manager.requestReconcile(reconcileMailboxes)
				}
			}
			return nil
		}

		// This availability check and the acceptance/checkpoint transaction run
		// under all Scheduler delivery controllers for the target. Task delivery
		// includes its Project controller because Project archival moves the
		// complete Task subtree while holding that stable resource address. It is
		// intentionally reached only when no durable acceptance evidence exists.
		exists, archived, _, targetErr := resourceExistsAndArchived(n.workspace.Path, prepared.Target)
		if targetErr != nil && !errors.Is(targetErr, app.ErrResourceNotFound) {
			return targetErr
		}
		if targetErr != nil || !exists || archived {
			reason := "target resource is unavailable"
			if targetErr != nil {
				reason = targetErr.Error()
			} else if archived {
				reason = "target resource is archived"
			}
			runtime.NextRunAt = ""
			runtime.RetryAt = ""
			runtime.RetryCount = 0
			runtime.EffectiveState = schedulerOutcomeAttention
			runtime.LastOutcome = schedulerOutcomeAttention
			runtime.LastError = reason
			runtime.AttentionTarget = prepared.Target
			return n.storeSchedulerRuntime(schedule.ID, runtime)
		}
		if schedule.Trigger.Type != app.ScheduleTriggerAt {
			busy, err := n.targetBusy(prepared.Target)
			if err != nil {
				return err
			}
			if busy {
				runtime.Prepared = nil
				runtime.NextRunAt = prepared.NextRunAt
				runtime.RetryAt = ""
				runtime.RetryCount = 0
				runtime.LastOccurrenceAt = prepared.CoalescedThrough
				runtime.LastOutcome = schedulerOutcomeBusy
				runtime.LastError = ""
				runtime.AttentionTarget = ""
				if prepared.NextRunAt == "" {
					runtime.EffectiveState = app.ScheduleStateCompleted
				}
				return n.storeSchedulerRuntime(schedule.ID, runtime)
			}
		}
		if _, _, _, err := n.manager.resolveMailboxGenerationAgent(ctx, n.workspace, prepared.Target); err != nil {
			var apiErr *resourceAPIError
			if errors.As(err, &apiErr) && apiErr.Code == "binding_unavailable" {
				runtime.NextRunAt = ""
				runtime.RetryAt = ""
				runtime.RetryCount = 0
				runtime.EffectiveState = schedulerOutcomeAttention
				runtime.LastOutcome = schedulerOutcomeAttention
				runtime.LastError = err.Error()
				runtime.AttentionTarget = prepared.Target
				return n.storeSchedulerRuntime(schedule.ID, runtime)
			}
			runtime.EffectiveState = schedule.State
			runtime.AttentionTarget = ""
			return err
		}
		message := resourceMailboxMessage{
			ID: prepared.MessageID, ResourceID: prepared.Target, Text: prepared.Text,
			RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
			Type: resourceMessageTypeScheduleOccurrence, Causation: cloneMailboxCausation(prepared.Causation),
			SenderWorkspaceInstanceID: prepared.Causation.SourceWorkspaceInstanceID,
		}
		accepted, err = acceptGeneratedMailboxMessage(n.workspace.Path, message)
		if err != nil {
			return err
		}
		commitAcceptedPreparedOccurrence(&runtime, schedule.State)
		if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
			return err
		}
		if accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting {
			if err := n.manager.reconcileResourceMailboxLocked(ctx, n.workspace, prepared.Target); err != nil {
				recordMailboxFailure(n.workspace.Path, accepted.ID, err)
				n.manager.requestReconcile(reconcileMailboxes)
			}
		}
		return nil
	}

	var err error
	if prepared.Target == app.SchedulerResourceID {
		err = deliver()
	} else {
		// Production reconciliation already owns the Scheduler controller. It is
		// the outer orchestration lock: target/archive paths never acquire it.
		err = n.manager.withResourceControllers(ctx, n.workspace, schedulerDeliveryControllerIDs(prepared.Target), deliver)
	}
	if err == nil {
		return nil
	}
	// Ownership loss is a handoff boundary, not a retryable Scheduler failure.
	// Recording it would itself write the replacement Workspace after a target
	// controller detected the stale identity or lock inode.
	if resourceDeliveryErrorCode(err) == "workspace_not_owned" {
		return err
	}
	return n.recordScheduleError(schedule.ID, runtime, now, err)
}

func schedulerDeliveryControllerIDs(target string) []string {
	target = normalizedResourceID(target)
	if separator := strings.IndexByte(target, '.'); separator > 0 {
		return []string{target[:separator], target}
	}
	return []string{target}
}

func (n *NativeScheduler) targetBusy(resourceID string) (bool, error) {
	record, found, err := currentResourceGeneration(n.workspace.Path, resourceID)
	if err != nil {
		return false, err
	}
	if found && generationHasActiveTurn(record) {
		return true, nil
	}
	return resourceMailboxHasHotWork(n.workspace.Path, resourceID)
}

func (n *NativeScheduler) recordScheduleError(id string, runtime schedulerScheduleRuntime, now time.Time, cause error) error {
	if errors.Is(cause, app.ErrScheduleOccurrenceOutOfRange) || errors.Is(cause, app.ErrScheduleCronSuccessorUnavailable) {
		runtime.EffectiveState = schedulerOutcomeAttention
		runtime.NextRunAt = ""
		runtime.RetryAt = ""
		runtime.RetryCount = 0
		runtime.LastOutcome = schedulerOutcomeAttention
		runtime.LastError = cause.Error()
		if err := n.storeSchedulerRuntime(id, runtime); err != nil {
			return errors.Join(cause, err)
		}
		return nil
	}
	runtime.RetryCount++
	delay := 5 * time.Second
	for index := 1; index < runtime.RetryCount && delay < 30*time.Minute; index++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	runtime.RetryAt = now.Add(delay).Format(time.RFC3339Nano)
	runtime.LastError = cause.Error()
	if err := n.storeSchedulerRuntime(id, runtime); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func (n *NativeScheduler) schedulerRuntime(id string) (schedulerScheduleRuntime, error) {
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return schedulerScheduleRuntime{}, err
	}
	runtime := store.Scheduler.Schedules[id]
	if runtime.ActivationRevision == 0 {
		// Checkpoints written before activationRevision used their revision as
		// the only definition boundary. Materialize that meaning in memory so a
		// same-revision upgrade remains stable and an older checkpoint cannot be
		// promoted across a newer portable activation.
		runtime.ActivationRevision = runtime.Revision
	}
	return runtime, nil
}

func schedulerRuntimeActivationRevision(runtime schedulerScheduleRuntime) uint64 {
	if runtime.ActivationRevision != 0 {
		return runtime.ActivationRevision
	}
	return runtime.Revision
}

func schedulerPortableActivationRevision(schedule app.Schedule) uint64 {
	if schedule.ActivationRevision != 0 {
		return schedule.ActivationRevision
	}
	return schedule.Revision
}

func (n *NativeScheduler) storeSchedulerRuntime(id string, runtime schedulerScheduleRuntime) error {
	_, err := mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		if store.Scheduler.Schedules == nil {
			store.Scheduler.Schedules = make(map[string]schedulerScheduleRuntime)
		}
		store.Scheduler.Schedules[id] = runtime
		return nil
	})
	return err
}

func schedulerRuntimeDeadline(runtime schedulerScheduleRuntime, now time.Time) time.Time {
	if runtime.EffectiveState == app.ScheduleStateCompleted || runtime.EffectiveState == schedulerOutcomeAttention {
		return time.Time{}
	}
	if runtime.EffectiveState == app.ScheduleStatePaused {
		return generationTime(runtime.NextRunAt)
	}
	if runtime.RetryAt != "" {
		return generationTime(runtime.RetryAt)
	}
	if runtime.Prepared != nil {
		return now
	}
	return generationTime(runtime.NextRunAt)
}

func (n *NativeScheduler) cancelLegacyTicks(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return err
	}
	queued := false
	needsAgentHub := make([]resourceMailboxMessage, 0)
	for _, message := range store.Mailbox.Messages {
		if message.Type != resourceMessageTypeSchedulerTick {
			continue
		}
		switch message.Status {
		case resourceMessageQueued:
			queued = true
		case resourceMessageDelivering, resourceMessageInterrupting, resourceMessageDeliveryUnknown:
			needsAgentHub = append(needsAgentHub, message)
		case resourceMessageDelivered:
			// TurnTerminalAt is written only after observing the materialized
			// canonical Turn as closed. It is the durable restart boundary for
			// migration, whereas TerminalAt only means AgentHub accepted input.
			if strings.TrimSpace(message.TurnTerminalAt) == "" {
				needsAgentHub = append(needsAgentHub, message)
			}
		}
	}
	if !queued && len(needsAgentHub) == 0 {
		return nil
	}
	if queued {
		_, err = mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
			now := time.Now().Format(time.RFC3339Nano)
			for index := range store.Mailbox.Messages {
				message := &store.Mailbox.Messages[index]
				if message.Type != resourceMessageTypeSchedulerTick || message.Status != resourceMessageQueued {
					continue
				}
				message.Status = resourceMessageUndeliverable
				message.TerminalAt, message.UpdatedAt = now, now
				message.LastErrorCode = "scheduler_v1_retired"
				message.LastError = "Legacy scheduler tick was retired before AgentHub delivery"
			}
			return ctx.Err()
		})
		if err != nil {
			return err
		}
	}
	for _, message := range needsAgentHub {
		if err := n.resolveLegacyTick(ctx, message); err != nil {
			return fmt.Errorf("retire legacy Scheduler tick %s: %w", message.ID, err)
		}
	}
	return nil
}

// resolveLegacyTick closes the only unsafe migration window: AgentHub may
// have durably accepted a v1 tick while the local mailbox still says
// delivering or delivery_unknown. Migration cannot proceed until the stable
// input is proved absent, or its exact materialized Turn is terminal. Session
// source validation and trigger ownership prevent a stale receipt from being
// confused with an unrelated Scheduler conversation.
func (n *NativeScheduler) resolveLegacyTick(ctx context.Context, message resourceMailboxMessage) error {
	generationID := strings.TrimSpace(message.GenerationID)
	if generationID == "" {
		return errors.New("legacy tick has no generation identity")
	}
	record, found, err := generationRecordByID(n.workspace.Path, generationID)
	if err != nil {
		return err
	}
	if !found || normalizedResourceID(record.ResourceID) != app.SchedulerResourceID {
		return fmt.Errorf("legacy tick generation %s is unavailable or belongs to another resource", generationID)
	}
	sessionID := strings.TrimSpace(record.AgentHubSessionID)
	if sessionID == "" || (strings.TrimSpace(message.AgentHubSessionID) != "" && strings.TrimSpace(message.AgentHubSessionID) != sessionID) {
		return errors.New("legacy tick AgentHub Session identity is ambiguous")
	}
	cfg, client, err := n.manager.agentHubRuntimeConfig()
	if err != nil {
		return err
	}
	session, err := client.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("inspect AgentHub Session %s: %w", sessionID, err)
	}
	if !agentHubSessionExactlyMatchesGeneration(cfg, record, session) {
		return fmt.Errorf("AgentHub Session %s does not match generation %s", sessionID, generationID)
	}
	canonical, accepted, err := findCanonicalLegacyTickMessage(ctx, client, sessionID, message)
	if err != nil {
		return fmt.Errorf("inspect canonical input: %w", err)
	}
	if !accepted {
		if message.Status == resourceMessageDelivered {
			return errors.New("mailbox says delivered but AgentHub has no matching canonical input")
		}
		return n.markLegacyTickUnaccepted(message.ID)
	}
	if canonical.Steer {
		return errors.New("canonical legacy tick is an inserted message, not a Turn opener")
	}
	turn, found, err := findLegacyTickTurn(ctx, client, sessionID, message, canonical.TurnID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("canonical legacy tick has no materialized Turn yet")
	}
	turnID := firstNonEmpty(strings.TrimSpace(turn.TurnID), strings.TrimSpace(turn.ID))
	if turnID == "" || !legacyTickOwnsTurn(turn, message) {
		return errors.New("materialized Turn is not owned by the legacy tick")
	}
	if err := n.markLegacyTickAccepted(message.ID, generationID, sessionID, turnID); err != nil {
		return err
	}
	if turn.Closed || terminalTurnStatus(turn.Status) {
		return n.markLegacyTickTurnTerminal(message.ID, turn)
	}
	activeTurnID := activeAgentHubTurnID(session)
	if activeTurnID != turnID {
		if activeTurnID != "" {
			return fmt.Errorf("legacy tick Turn %s is open while unrelated Turn %s is active", turnID, activeTurnID)
		}
		return fmt.Errorf("legacy tick Turn %s is not terminal yet", turnID)
	}
	// AgentHub v1 exposes only a Session-scoped interrupt with an empty request
	// body. It has no expected Turn ID or other atomic precondition. Calling it
	// here would race the observed Turn finishing and a new Scheduler chat
	// starting, and could interrupt that unrelated Turn. Keep migration blocked
	// until the exact, ownership-checked legacy Turn is observed terminal.
	return fmt.Errorf("legacy tick Turn %s is active; AgentHub has no conditional Turn interrupt, waiting for terminal state", turnID)
}

// findCanonicalLegacyTickMessage is receipt-aware. Terminal mailbox receipts
// intentionally omit message text, so the ordinary exact-input matcher cannot
// reconstruct their provider envelope. The stable AgentHub message ID, the
// source-validated Session, and (for opaque v2 input) PUA's scheduler_tick
// payload still give an exact application identity without retaining prompt
// bodies forever in the receipt store.
func findCanonicalLegacyTickMessage(ctx context.Context, client *agentHubClient, sessionID string, expected resourceMailboxMessage) (agentHubInboundMessage, bool, error) {
	cursor := int64(0)
	for {
		frames, latest, err := client.SessionFrames(ctx, sessionID, cursor, agentHubEventMaxCount)
		if err != nil {
			return agentHubInboundMessage{}, false, err
		}
		for _, frame := range frames {
			for _, event := range frame.Events {
				if event.Type != "message.input" {
					continue
				}
				var canonical agentHubInboundMessage
				if json.Unmarshal(event.Data, &canonical) != nil || canonical.MessageID != expected.ID {
					continue
				}
				matches := canonicalAgentHubMessageMatches(canonical, expected)
				if strings.TrimSpace(expected.Text) == "" {
					matches = canonicalLegacyTickReceiptMatches(canonical)
				}
				if !matches {
					return agentHubInboundMessage{}, false, &resourceAPIError{Code: "message_conflict", Message: "stable message id conflicts with a different canonical AgentHub input"}
				}
				canonical.TurnID = event.TurnID
				return canonical, true, nil
			}
		}
		if len(frames) == 0 || frames[len(frames)-1].Cursor <= cursor || frames[len(frames)-1].Cursor >= latest {
			return agentHubInboundMessage{}, false, nil
		}
		cursor = frames[len(frames)-1].Cursor
	}
}

func canonicalLegacyTickReceiptMatches(canonical agentHubInboundMessage) bool {
	if canonical.SchemaVersion == agentHubOpaqueMessageSchema {
		payload, ok := decodePUAMessagePayload(canonical.Payload)
		return ok && payload.Role == "system" && payload.Type == resourceMessageTypeSchedulerTick &&
			payload.Causation != nil && payload.Causation.Type == resourceMessageTypeSchedulerTick &&
			normalizedResourceID(payload.Causation.SourceResourceID) == app.SchedulerResourceID
	}
	return normalizedProviderMessageRole(canonical.Role) == "system"
}

func findLegacyTickTurn(ctx context.Context, client *agentHubClient, sessionID string, message resourceMailboxMessage, canonicalTurnID string) (agentHubTurn, bool, error) {
	if turnID := firstNonEmpty(strings.TrimSpace(canonicalTurnID), strings.TrimSpace(message.TurnID)); turnID != "" {
		turn, _, err := client.SessionTurn(ctx, sessionID, turnID)
		if err != nil {
			return agentHubTurn{}, false, err
		}
		if legacyTickOwnsTurn(turn, message) {
			return turn, true, nil
		}
		return agentHubTurn{}, false, errors.New("canonical Turn does not belong to the legacy tick")
	}
	before, latest := int64(0), true
	for {
		page, err := client.SessionTurns(ctx, sessionID, before, latest, generationUsagePageSize)
		if err != nil {
			return agentHubTurn{}, false, err
		}
		for _, turn := range page.Turns {
			if legacyTickOwnsTurn(turn, message) {
				return turn, true, nil
			}
		}
		if !page.Page.HasMoreBefore {
			return agentHubTurn{}, false, nil
		}
		if page.Page.NextBefore <= 0 || page.Page.NextBefore == before {
			return agentHubTurn{}, false, errors.New("AgentHub Turn pagination did not advance")
		}
		before, latest = page.Page.NextBefore, false
	}
}

func legacyTickOwnsTurn(turn agentHubTurn, message resourceMailboxMessage) bool {
	if triggerID := strings.TrimSpace(turn.TriggerMessageID); triggerID != "" {
		return triggerID == message.ID
	}
	if payload, ok := decodePUAMessagePayload(turn.TriggerPayload); ok {
		return payload.Type == resourceMessageTypeSchedulerTick && payload.Text == message.Text && turn.TriggerRole == message.Role
	}
	for _, item := range turn.Items {
		if strings.TrimSpace(item.MessageID) == message.ID {
			return true
		}
	}
	return false
}

func (n *NativeScheduler) markLegacyTickUnaccepted(messageID string) error {
	_, err := updateMailboxMessage(n.workspace.Path, messageID, func(message *resourceMailboxMessage) {
		now := time.Now().Format(time.RFC3339Nano)
		message.Status = resourceMessageUndeliverable
		message.TerminalAt, message.UpdatedAt = now, now
		message.LastErrorCode = "scheduler_v1_retired"
		message.LastError = "Legacy scheduler tick was retired after AgentHub confirmed no canonical input"
	})
	return err
}

func (n *NativeScheduler) markLegacyTickAccepted(messageID, generationID, sessionID, turnID string) error {
	_, err := updateMailboxMessage(n.workspace.Path, messageID, func(message *resourceMailboxMessage) {
		now := time.Now().Format(time.RFC3339Nano)
		message.Status = resourceMessageDelivered
		message.GenerationID = generationID
		message.AgentHubSessionID = sessionID
		message.TurnID = turnID
		if message.DeliveredAt == "" {
			message.DeliveredAt = now
		}
		if message.TerminalAt == "" {
			message.TerminalAt = message.DeliveredAt
		}
		message.UpdatedAt = now
		message.LastError, message.LastErrorCode = "", ""
	})
	return err
}

func (n *NativeScheduler) markLegacyTickTurnTerminal(messageID string, turn agentHubTurn) error {
	_, err := updateMailboxMessage(n.workspace.Path, messageID, func(message *resourceMailboxMessage) {
		terminalAt := firstNonEmpty(strings.TrimSpace(turn.EndedAt), strings.TrimSpace(turn.CompletedAt))
		if terminalAt == "" {
			terminalAt = time.Now().Format(time.RFC3339Nano)
		}
		message.TurnTerminalAt = terminalAt
		if turnID := firstNonEmpty(strings.TrimSpace(turn.TurnID), strings.TrimSpace(turn.ID)); turnID != "" {
			message.TurnID = turnID
		}
		message.UpdatedAt = terminalAt
	})
	return err
}

func (n *NativeScheduler) reconcileMigration(ctx context.Context, config app.SchedulerConfig) error {
	pending := make([]app.Schedule, 0)
	for _, schedule := range config.Schedules {
		if schedule.State == app.ScheduleStateNeedsCompilation {
			pending = append(pending, schedule)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	digest, err := schedulerConfigDigest(config)
	if err != nil {
		return err
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil || store.Scheduler.MigrationDigest == digest {
		return err
	}
	instanceID, err := workspaceInstanceID(n.workspace.Path)
	if err != nil {
		return err
	}
	language, err := workspaceContentLanguage(n.workspace.Path)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(pending))
	for _, schedule := range pending {
		ids = append(ids, schedule.ID)
	}
	messageID := notificationMessageID(resourceMessageTypeScheduleMigration, instanceID, digest)
	text := strings.TrimSuffix(localize.MustRender(language, "scheduler-migration.md", map[string]any{
		"MessageID": messageID, "ScheduleIDs": strings.Join(ids, ", "), "Count": len(ids),
	}), "\n")
	message := resourceMailboxMessage{
		ID: messageID, ResourceID: app.SchedulerResourceID, Text: text,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type:      resourceMessageTypeScheduleMigration,
		Causation: &resourceMessageCausation{Type: resourceMessageTypeScheduleMigration, SourceWorkspaceInstanceID: instanceID, SourceResourceID: app.SchedulerResourceID, Reason: "compile_migrated_schedules", ScheduleDigest: digest},
	}
	accepted, err := acceptGeneratedMailboxMessage(n.workspace.Path, message)
	if err != nil {
		return err
	}
	_, err = mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		store.Scheduler.MigrationDigest = digest
		return nil
	})
	if err != nil {
		return err
	}
	if accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting {
		if err := n.manager.reconcileResourceMailboxLocked(ctx, n.workspace, app.SchedulerResourceID); err != nil {
			recordMailboxFailure(n.workspace.Path, accepted.ID, err)
			n.manager.requestReconcile(reconcileMailboxes)
		}
	}
	return nil
}

func (m *agentManager) reconcileSchedulerLocked(ctx context.Context, workspace serveWorkspace) error {
	return m.withResourceController(ctx, workspace, app.SchedulerResourceID, func() error {
		_, err := newNativeScheduler(m, workspace).Reconcile(ctx, m.now())
		return err
	})
}
