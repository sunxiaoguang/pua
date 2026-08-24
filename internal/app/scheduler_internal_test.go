package app

import (
	"errors"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

type boundedZeroCronSchedule struct {
	calls int
}

func (schedule *boundedZeroCronSchedule) Next(time.Time) time.Time {
	schedule.calls++
	return time.Time{}
}

type nonMonotonicCronSchedule struct{}

func (nonMonotonicCronSchedule) Next(after time.Time) time.Time {
	return after
}

func TestValidateScheduleTriggerForMutationRejectsAtEqualNow(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 123, time.UTC)
	trigger := ScheduleTrigger{Type: ScheduleTriggerAt, At: now.Format(time.RFC3339Nano)}
	if err := validateScheduleTriggerForMutation(trigger, now); !errors.Is(err, ErrScheduleTriggerAtNotFuture) {
		t.Fatalf("at-equals-now error = %v", err)
	}

	interval := ScheduleTrigger{Type: ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	if err := validateScheduleTriggerForMutation(interval, now); err != nil {
		t.Fatalf("past interval anchor error = %v", err)
	}
}

func TestValidateScheduleTriggerForUpdateStillValidatesIdenticalTrigger(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	invalid := ScheduleTrigger{Type: ScheduleTriggerAt, At: now.Add(-time.Hour).Format(time.RFC3339Nano), EverySeconds: 60}
	if err := validateScheduleTriggerForUpdate(invalid, &invalid, now); err == nil || err.Error() != "at trigger must contain only at" {
		t.Fatalf("identical invalid trigger error = %v", err)
	}
}

func TestCronSuccessorBoundsZeroScheduleToGregorianCycle(t *testing.T) {
	schedule := &boundedZeroCronSchedule{}
	after := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	next, err := nextParsedScheduleCronOccurrence(schedule, time.UTC, after)
	if !errors.Is(err, ErrScheduleCronSuccessorUnavailable) || !next.IsZero() {
		t.Fatalf("zero-schedule successor = %s, %v", next, err)
	}
	if schedule.calls != maximumCronSuccessorCalls {
		t.Fatalf("zero-schedule calls = %d, want %d", schedule.calls, maximumCronSuccessorCalls)
	}
}

func TestCronSuccessorRejectsNonMonotonicSchedule(t *testing.T) {
	after := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	next, err := nextParsedScheduleCronOccurrence(nonMonotonicCronSchedule{}, time.UTC, after)
	if !errors.Is(err, ErrScheduleCronSuccessorUnavailable) || !next.IsZero() {
		t.Fatalf("non-monotonic successor = %s, %v", next, err)
	}
}

var _ cron.Schedule = (*boundedZeroCronSchedule)(nil)
var _ cron.Schedule = nonMonotonicCronSchedule{}
