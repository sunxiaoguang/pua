// Package schedulerapi defines the lossless JSON representation used by the
// Scheduler HTTP API. Portable scheduler.json files deliberately keep their
// native uint64 revision field; the HTTP boundary uses decimal strings so a
// JavaScript client cannot round a compare-and-swap token.
package schedulerapi

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/disksing/pua/internal/app"
)

// MaximumRevision is the canonical decimal spelling of math.MaxUint64.
const MaximumRevision = "18446744073709551615"

var errInvalidRevision = errors.New("schedule revision must be a canonical decimal string between 1 and " + MaximumRevision)

// Revision is a positive uint64 encoded as a canonical decimal JSON string.
type Revision string

// ParseRevision validates a canonical positive uint64 decimal string.
func ParseRevision(value string) (Revision, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
		return "", errInvalidRevision
	}
	return Revision(value), nil
}

// RevisionFromUint64 formats a portable schedule's validated revision for the
// HTTP wire contract.
func RevisionFromUint64(value uint64) Revision {
	return Revision(strconv.FormatUint(value, 10))
}

// Uint64 converts a validated wire revision to the portable domain value.
func (revision Revision) Uint64() (uint64, error) {
	validated, err := ParseRevision(string(revision))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(string(validated), 10, 64)
}

func (revision *Revision) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errInvalidRevision
	}
	validated, err := ParseRevision(value)
	if err != nil {
		return err
	}
	*revision = validated
	return nil
}

// Schedule is the HTTP projection of app.Schedule with a lossless revision.
type Schedule struct {
	ID          string               `json:"id"`
	Revision    Revision             `json:"revision"`
	Description string               `json:"description"`
	Condition   string               `json:"condition"`
	Guard       string               `json:"guard,omitempty"`
	Target      string               `json:"target"`
	State       string               `json:"state"`
	Trigger     *app.ScheduleTrigger `json:"trigger,omitempty"`
	CreatedAt   string               `json:"createdAt"`
	UpdatedAt   string               `json:"updatedAt"`
}

// ScheduleSnapshot adds runtime projection fields to a wire schedule.
type ScheduleSnapshot struct {
	Schedule
	EffectiveState   string `json:"effectiveState"`
	NextRunAt        string `json:"nextRunAt,omitempty"`
	LastOccurrenceAt string `json:"lastOccurrenceAt,omitempty"`
	LastOutcome      string `json:"lastOutcome,omitempty"`
	LastError        string `json:"lastError,omitempty"`
}

// Snapshot is the lossless Scheduler HTTP list/detail response.
type Snapshot struct {
	SchemaVersion int                `json:"schemaVersion"`
	AgentBinding  app.AgentBinding   `json:"agentBinding"`
	Schedules     []ScheduleSnapshot `json:"schedules"`
	NextWakeAt    string             `json:"nextWakeAt,omitempty"`
}

// FromSchedule projects a portable schedule onto the HTTP wire contract.
func FromSchedule(schedule app.Schedule) Schedule {
	return Schedule{
		ID: schedule.ID, Revision: RevisionFromUint64(schedule.Revision),
		Description: schedule.Description, Condition: schedule.Condition,
		Guard: schedule.Guard, Target: schedule.Target, State: schedule.State,
		Trigger: schedule.Trigger, CreatedAt: schedule.CreatedAt, UpdatedAt: schedule.UpdatedAt,
	}
}

// FromSnapshot projects a Scheduler runtime snapshot onto the HTTP contract.
func FromSnapshot(snapshot app.SchedulerSnapshot) Snapshot {
	result := Snapshot{
		SchemaVersion: snapshot.SchemaVersion, AgentBinding: snapshot.AgentBinding,
		Schedules: make([]ScheduleSnapshot, 0, len(snapshot.Schedules)), NextWakeAt: snapshot.NextWakeAt,
	}
	for _, schedule := range snapshot.Schedules {
		result.Schedules = append(result.Schedules, ScheduleSnapshot{
			Schedule: FromSchedule(schedule.Schedule), EffectiveState: schedule.EffectiveState,
			NextRunAt: schedule.NextRunAt, LastOccurrenceAt: schedule.LastOccurrenceAt,
			LastOutcome: schedule.LastOutcome, LastError: schedule.LastError,
		})
	}
	return result
}
