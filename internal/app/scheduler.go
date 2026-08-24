package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/disksing/pua/internal/localize"
	"github.com/robfig/cron/v3"
)

var scheduleIDPattern = regexp.MustCompile(`^schedule-[0-9a-f]{24}$`)

// ErrScheduleTriggerRequired identifies a normal create that omits the native
// trigger required by the v2 Scheduler contract.
var ErrScheduleTriggerRequired = errors.New("schedule trigger is required")

// ErrScheduleTriggerAtNotFuture identifies a one-time trigger that is not
// strictly later than the mutation that would make it active.
var ErrScheduleTriggerAtNotFuture = errors.New("schedule trigger.at must be strictly in the future")

// ErrScheduleOccurrenceDue identifies a one-time schedule whose occurrence
// reached its deadline before the pause transition could be persisted.
var ErrScheduleOccurrenceDue = errors.New("one-time schedule occurrence is due")

// ErrScheduleOccurrenceOutOfRange identifies an occurrence that cannot be
// round-tripped through the Scheduler's RFC3339Nano runtime persistence.
var ErrScheduleOccurrenceOutOfRange = errors.New("schedule occurrence is outside the RFC3339Nano persistence range")

// ErrScheduleCronSuccessorUnavailable identifies a parsed cron schedule that
// violates the recurring successor contract by returning a non-monotonic
// instant or no instant within a complete Gregorian calendar cycle.
var ErrScheduleCronSuccessorUnavailable = errors.New("cron schedule has no monotonic successor within a Gregorian calendar cycle")

// ErrScheduleRevisionExhausted identifies a schedule whose revision cannot be
// incremented without wrapping the persisted uint64 value.
var ErrScheduleRevisionExhausted = errors.New("schedule revision is exhausted")

// ErrScheduleNotFound identifies a mutation whose schedule ID does not exist.
var ErrScheduleNotFound = errors.New("schedule not found")

// ErrScheduleTargetScheduler identifies an execution target that would wake
// the Scheduler management/compiler resource for ordinary scheduled work.
var ErrScheduleTargetScheduler = errors.New("the Scheduler resource cannot be a schedule target")

const (
	SchedulerResourceID         = "scheduler"
	schedulerDir                = "scheduler"
	schedulerJSONFile           = "scheduler.json"
	schedulerV1JSONFile         = "scheduler.v1.json"
	schedulerMarkdownFile       = "scheduler.md"
	schedulerSchemaVersion      = 2
	minimumSchedulerV1WakeMins  = 1
	maximumSchedulerV1WakeMins  = 7 * 24 * 60
	minimumScheduleEverySeconds = 60
	maximumScheduleEverySeconds = int64(^uint64(0)>>1) / int64(time.Second)
	maximumScheduleTextLength   = 64 * 1024
	maximumCronOccurrences      = 100000
	robfigCronSearchWindowYears = 5
	cronGregorianCycleYears     = 400
	maximumCronSuccessorCalls   = cronGregorianCycleYears / robfigCronSearchWindowYears
)

const (
	ScheduleStateActive           = "active"
	ScheduleStatePaused           = "paused"
	ScheduleStateCompleted        = "completed"
	ScheduleStateNeedsCompilation = "needs_compilation"

	ScheduleTriggerAt       = "at"
	ScheduleTriggerInterval = "interval"
	ScheduleTriggerCron     = "cron"
)

// ScheduleChangeOperation identifies one mutation supported by the native
// Scheduler. Transport adapters parse their external operation names into this
// type before handing a change to the Scheduler owner.
type ScheduleChangeOperation string

const (
	ScheduleChangeCreate ScheduleChangeOperation = "create"
	ScheduleChangeUpdate ScheduleChangeOperation = "update"
	ScheduleChangePause  ScheduleChangeOperation = "pause"
	ScheduleChangeResume ScheduleChangeOperation = "resume"
	ScheduleChangeRemove ScheduleChangeOperation = "remove"
)

var scheduleChangeOperations = map[ScheduleChangeOperation]struct{}{
	ScheduleChangeCreate: {},
	ScheduleChangeUpdate: {},
	ScheduleChangePause:  {},
	ScheduleChangeResume: {},
	ScheduleChangeRemove: {},
}

// ParseScheduleChangeOperation validates an external Scheduler mutation name.
func ParseScheduleChangeOperation(value string) (ScheduleChangeOperation, error) {
	operation := ScheduleChangeOperation(strings.TrimSpace(value))
	if err := operation.Validate(); err != nil {
		return "", err
	}
	return operation, nil
}

// Validate rejects operations outside the native Scheduler mutation domain.
func (operation ScheduleChangeOperation) Validate() error {
	if _, ok := scheduleChangeOperations[operation]; !ok {
		return fmt.Errorf("unsupported Scheduler change %q", operation)
	}
	return nil
}

// SchedulerConfig is the portable, Workspace-owned Scheduler definition. It
// deliberately excludes execution cursors and delivery results, which belong
// to the Server runtime checkpoint.
type SchedulerConfig struct {
	SchemaVersion int          `json:"schemaVersion"`
	AgentBinding  AgentBinding `json:"agentBinding"`
	Schedules     []Schedule   `json:"schedules"`
}

// Schedule is the portable schedule definition. Trigger is absent only for a
// migrated v1 definition waiting for Scheduler Agent compilation.
type Schedule struct {
	ID                 string           `json:"id"`
	Revision           uint64           `json:"revision"`
	ActivationRevision uint64           `json:"activationRevision"`
	Description        string           `json:"description"`
	Condition          string           `json:"condition"`
	Guard              string           `json:"guard,omitempty"`
	Target             string           `json:"target"`
	State              string           `json:"state"`
	Trigger            *ScheduleTrigger `json:"trigger,omitempty"`
	CreatedAt          string           `json:"createdAt"`
	UpdatedAt          string           `json:"updatedAt"`
}

// ScheduleTrigger is a tagged union. Only fields belonging to Type may be
// populated.
type ScheduleTrigger struct {
	Type         string `json:"type"`
	At           string `json:"at,omitempty"`
	EverySeconds int64  `json:"everySeconds,omitempty"`
	AnchorAt     string `json:"anchorAt,omitempty"`
	Cron         string `json:"cron,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`
}

type CreateScheduleInput struct {
	Description string
	Condition   string
	Guard       string
	Target      string
	Trigger     *ScheduleTrigger
}

type UpdateScheduleInput struct {
	ID               string
	ExpectedRevision uint64
	Description      *string
	Condition        *string
	Guard            *string
	Target           *string
	Trigger          *ScheduleTrigger
}

type SchedulerSnapshot struct {
	SchemaVersion int                `json:"schemaVersion"`
	AgentBinding  AgentBinding       `json:"agentBinding"`
	Schedules     []ScheduleSnapshot `json:"schedules"`
	NextWakeAt    string             `json:"nextWakeAt,omitempty"`
}

type ScheduleSnapshot struct {
	Schedule
	EffectiveState   string `json:"effectiveState"`
	NextRunAt        string `json:"nextRunAt,omitempty"`
	LastOccurrenceAt string `json:"lastOccurrenceAt,omitempty"`
	LastOutcome      string `json:"lastOutcome,omitempty"`
	LastError        string `json:"lastError,omitempty"`
}

// ScheduleRevisionConflictError is returned by compare-and-swap updates.
type ScheduleRevisionConflictError struct {
	ScheduleID string
	Expected   uint64
	Actual     uint64
}

// ScheduleNotFoundError retains the stable ID of a missing schedule while
// remaining matchable through ErrScheduleNotFound.
type ScheduleNotFoundError struct {
	ScheduleID string
}

func (e *ScheduleNotFoundError) Error() string {
	return fmt.Sprintf("%s: %s", ErrScheduleNotFound, e.ScheduleID)
}

func (e *ScheduleNotFoundError) Unwrap() error {
	return ErrScheduleNotFound
}

func (e *ScheduleRevisionConflictError) Error() string {
	return fmt.Sprintf("schedule_revision_conflict: schedule %s revision is %d, expected %d", e.ScheduleID, e.Actual, e.Expected)
}

func incrementScheduleRevision(schedule *Schedule) error {
	if schedule.Revision == ^uint64(0) {
		return ErrScheduleRevisionExhausted
	}
	schedule.Revision++
	return nil
}

func defaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		SchemaVersion: schedulerSchemaVersion,
		AgentBinding:  AgentBinding{Kind: "profile", Name: "default"},
		Schedules:     []Schedule{},
	}
}

func schedulerPath(root string) string {
	return filepath.Join(root, schedulerDir)
}

func schedulerJSONPath(root string) string {
	return filepath.Join(schedulerPath(root), schedulerJSONFile)
}

// IsSchedulerPath reports whether start is the Scheduler directory or one of
// its descendants. It is used only for CLI provenance/resource selection.
func (w *Workspace) IsSchedulerPath(start string) (bool, error) {
	if err := w.require(); err != nil {
		return false, err
	}
	start = strings.TrimSpace(start)
	if start == "" {
		return false, errors.New("selection start path is required")
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return false, err
	}
	base, err := filepath.EvalSymlinks(schedulerPath(w.root))
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

// EnsureScheduler non-destructively creates or validates the Scheduler
// resource files and refreshes only the PUA-managed AGENTS.md block.
func (w *Workspace) EnsureScheduler() (SchedulerConfig, error) {
	if err := w.require(); err != nil {
		return SchedulerConfig{}, err
	}
	var result SchedulerConfig
	err := withWorkspaceMutationLock(w.root, func() error {
		cfg, err := readWorkspaceConfig(w.root)
		if err != nil {
			return err
		}
		result, err = ensureSchedulerLocked(w.root, cfg.Language)
		return err
	})
	if err != nil {
		return SchedulerConfig{}, &APIError{Operation: "ensure Scheduler", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	return result, nil
}

func ensureSchedulerLocked(root, language string) (SchedulerConfig, error) {
	dir := schedulerPath(root)
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return SchedulerConfig{}, err
		}
	case err != nil:
		return SchedulerConfig{}, err
	case info.Mode()&os.ModeSymlink != 0:
		return SchedulerConfig{}, fmt.Errorf("Scheduler path must not be a symbolic link: %s", dir)
	case !info.IsDir():
		return SchedulerConfig{}, fmt.Errorf("Scheduler path is not a directory: %s", dir)
	}

	jsonPath := schedulerJSONPath(root)
	if err := requireRegularOrMissing(jsonPath); err != nil {
		return SchedulerConfig{}, err
	}
	var config SchedulerConfig
	if _, err := os.Stat(jsonPath); os.IsNotExist(err) {
		config = defaultSchedulerConfig()
		if err := writeSchedulerJSON(jsonPath, config); err != nil {
			return SchedulerConfig{}, err
		}
	} else if err != nil {
		return SchedulerConfig{}, err
	} else {
		config, err = readSchedulerJSON(jsonPath)
		if err != nil {
			return SchedulerConfig{}, err
		}
	}

	markdownPath := filepath.Join(dir, schedulerMarkdownFile)
	if err := createTextFileIfMissing(markdownPath, defaultSchedulerMarkdown(language)); err != nil {
		return SchedulerConfig{}, err
	}
	agentsPath := filepath.Join(dir, "AGENTS.md")
	if err := requireRegularOrMissing(agentsPath); err != nil {
		return SchedulerConfig{}, err
	}
	if err := updateAgentsMDWithBlock(agentsPath, schedulerAgentsBlock(language)); err != nil {
		return SchedulerConfig{}, err
	}
	if err := syncDirectory(dir); err != nil {
		return SchedulerConfig{}, err
	}
	return config, nil
}

func requireRegularOrMissing(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("Scheduler file must be a regular file: %s", path)
	}
	return nil
}

func createTextFileIfMissing(path, content string) error {
	if err := requireRegularOrMissing(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func readSchedulerJSON(path string) (SchedulerConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return SchedulerConfig{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var config SchedulerConfig
	if err := decoder.Decode(&config); err != nil {
		return SchedulerConfig{}, fmt.Errorf("read Scheduler configuration: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return SchedulerConfig{}, fmt.Errorf("read Scheduler configuration: %w", err)
	}
	// activationRevision was added within schema v2. Older portable files do
	// not carry it, so use their current revision as a conservative semantic
	// boundary. This preserves a same-revision checkpoint while preventing an
	// older checkpoint from crossing any already-committed definition changes.
	for index := range config.Schedules {
		if config.Schedules[index].ActivationRevision == 0 {
			config.Schedules[index].ActivationRevision = config.Schedules[index].Revision
		}
	}
	if err := validateSchedulerConfig(config); err != nil {
		return SchedulerConfig{}, err
	}
	return config, nil
}

type schedulerV1Config struct {
	SchemaVersion       int          `json:"schemaVersion"`
	AgentBinding        AgentBinding `json:"agentBinding"`
	WakeIntervalMinutes int          `json:"wakeIntervalMinutes"`
	Schedules           []scheduleV1 `json:"schedules"`
}

type scheduleV1 struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Condition   string `json:"condition"`
	Target      string `json:"target"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// migrateSchedulerJSONLocked upgrades the only historical Scheduler schema.
// Definitions remain byte-for-byte equivalent at the semantic fields and are
// explicitly inert until a Scheduler Agent compiles a native trigger.
func migrateSchedulerJSONLocked(root string) error {
	return migrateSchedulerJSONLockedWithWriter(root, writeSchedulerJSON)
}

func migrateSchedulerJSONLockedWithWriter(root string, writeV2 func(string, SchedulerConfig) error) error {
	path := schedulerJSONPath(root)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("read Scheduler configuration: %w", err)
	}
	if header.SchemaVersion == schedulerSchemaVersion {
		return nil
	}
	if header.SchemaVersion != 1 {
		return fmt.Errorf("unsupported Scheduler schemaVersion %d; expected 1 or %d", header.SchemaVersion, schedulerSchemaVersion)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var legacy schedulerV1Config
	if err := decoder.Decode(&legacy); err != nil {
		return fmt.Errorf("read Scheduler configuration: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fmt.Errorf("read Scheduler configuration: %w", err)
	}
	if legacy.WakeIntervalMinutes < minimumSchedulerV1WakeMins || legacy.WakeIntervalMinutes > maximumSchedulerV1WakeMins {
		return fmt.Errorf("Scheduler wake interval must be between %d and %d minutes", minimumSchedulerV1WakeMins, maximumSchedulerV1WakeMins)
	}
	config := SchedulerConfig{SchemaVersion: schedulerSchemaVersion, AgentBinding: legacy.AgentBinding, Schedules: make([]Schedule, 0, len(legacy.Schedules))}
	for _, old := range legacy.Schedules {
		config.Schedules = append(config.Schedules, Schedule{
			ID: old.ID, Revision: 1, ActivationRevision: 1, Description: old.Description,
			Condition: old.Condition, Target: old.Target,
			State:     ScheduleStateNeedsCompilation,
			CreatedAt: old.CreatedAt, UpdatedAt: old.UpdatedAt,
		})
	}
	if err := validateSchedulerConfig(config); err != nil {
		return err
	}
	if err := preserveSchedulerV1RollbackEvidence(path, data); err != nil {
		return err
	}
	return writeV2(path, config)
}

func preserveSchedulerV1RollbackEvidence(path string, data []byte) error {
	evidencePath := filepath.Join(filepath.Dir(path), schedulerV1JSONFile)
	info, err := os.Lstat(evidencePath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("Scheduler v1 rollback evidence must be a regular file: %s", evidencePath)
		}
		existing, err := os.ReadFile(evidencePath)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("Scheduler v1 rollback evidence conflicts with the current v1 definition: %s", evidencePath)
		}
		return nil
	case !os.IsNotExist(err):
		return err
	}
	return writeSchedulerBytesAtomically(evidencePath, ".scheduler-v1-*.tmp", data)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("unexpected data after JSON document")
}

func validateSchedulerConfig(config SchedulerConfig) error {
	if config.SchemaVersion != schedulerSchemaVersion {
		return fmt.Errorf("unsupported Scheduler schemaVersion %d; expected %d", config.SchemaVersion, schedulerSchemaVersion)
	}
	if _, err := NormalizeAgentBinding(config.AgentBinding); err != nil {
		return fmt.Errorf("invalid Scheduler agent binding: %w", err)
	}
	seen := make(map[string]bool, len(config.Schedules))
	for _, schedule := range config.Schedules {
		if err := validateSchedule(schedule); err != nil {
			return fmt.Errorf("invalid schedule %q: %w", schedule.ID, err)
		}
		if seen[schedule.ID] {
			return fmt.Errorf("duplicate schedule id %q", schedule.ID)
		}
		seen[schedule.ID] = true
	}
	return nil
}

func validateSchedule(schedule Schedule) error {
	if !scheduleIDPattern.MatchString(schedule.ID) {
		return errors.New("id must be a stable schedule-* identifier")
	}
	if schedule.Revision < 1 {
		return errors.New("revision must be at least 1")
	}
	if schedule.ActivationRevision < 1 || schedule.ActivationRevision > schedule.Revision {
		return errors.New("activationRevision must be between 1 and revision")
	}
	for name, value := range map[string]string{
		"description": schedule.Description,
		"condition":   schedule.Condition,
		"target":      schedule.Target,
		"state":       schedule.State,
		"createdAt":   schedule.CreatedAt,
		"updatedAt":   schedule.UpdatedAt,
	} {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
		if len(value) > maximumScheduleTextLength || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, schedule.CreatedAt); err != nil {
		return errors.New("createdAt must be RFC3339")
	}
	if _, err := time.Parse(time.RFC3339Nano, schedule.UpdatedAt); err != nil {
		return errors.New("updatedAt must be RFC3339")
	}
	if strings.ContainsRune(schedule.Guard, '\x00') || len(schedule.Guard) > maximumScheduleTextLength {
		return errors.New("guard is invalid")
	}
	switch schedule.State {
	case ScheduleStateActive, ScheduleStatePaused, ScheduleStateCompleted:
		if err := ValidateScheduleTarget(schedule.Target); err != nil {
			return err
		}
		if schedule.Trigger == nil {
			return errors.New("trigger is required")
		}
		if err := ValidateScheduleTrigger(*schedule.Trigger); err != nil {
			return err
		}
	case ScheduleStateNeedsCompilation:
		if schedule.Trigger != nil {
			return errors.New("needs_compilation schedule must not have a trigger")
		}
	default:
		return fmt.Errorf("unsupported state %q", schedule.State)
	}
	return nil
}

// ValidateScheduleTarget rejects the special Scheduler management/compiler
// resource as an occurrence execution target. Resource existence is validated
// separately against the owning Workspace.
func ValidateScheduleTarget(target string) error {
	if strings.TrimSpace(target) == SchedulerResourceID {
		return ErrScheduleTargetScheduler
	}
	return nil
}

// ValidateScheduleTrigger validates a complete trigger without consulting the
// host's local timezone.
func ValidateScheduleTrigger(trigger ScheduleTrigger) error {
	switch trigger.Type {
	case ScheduleTriggerAt:
		if trigger.At == "" || trigger.EverySeconds != 0 || trigger.AnchorAt != "" || trigger.Cron != "" || trigger.TimeZone != "" {
			return errors.New("at trigger must contain only at")
		}
		if _, err := time.Parse(time.RFC3339Nano, trigger.At); err != nil {
			return fmt.Errorf("at must be RFC3339 with an explicit offset: %w", err)
		}
	case ScheduleTriggerInterval:
		if trigger.EverySeconds < minimumScheduleEverySeconds || trigger.EverySeconds > maximumScheduleEverySeconds || trigger.AnchorAt == "" || trigger.At != "" || trigger.Cron != "" || trigger.TimeZone != "" {
			return fmt.Errorf("interval trigger requires everySeconds between %d and %d and anchorAt only", minimumScheduleEverySeconds, maximumScheduleEverySeconds)
		}
		if _, err := time.Parse(time.RFC3339Nano, trigger.AnchorAt); err != nil {
			return fmt.Errorf("anchorAt must be RFC3339 with an explicit offset: %w", err)
		}
	case ScheduleTriggerCron:
		if trigger.Cron == "" || trigger.TimeZone == "" || trigger.At != "" || trigger.EverySeconds != 0 || trigger.AnchorAt != "" {
			return errors.New("cron trigger requires only cron and timeZone")
		}
		if strings.Contains(trigger.Cron, "TZ=") || strings.Contains(trigger.Cron, "CRON_TZ=") || strings.ContainsAny(trigger.Cron, "@") {
			return errors.New("cron descriptors and embedded timezones are not supported")
		}
		if len(strings.Fields(trigger.Cron)) != 6 {
			return errors.New("cron must contain exactly six fields including seconds")
		}
		if trigger.TimeZone == "Local" {
			return errors.New("timeZone must be an explicit IANA timezone, not Local")
		}
		location, err := time.LoadLocation(trigger.TimeZone)
		if err != nil {
			return fmt.Errorf("invalid IANA timeZone %q: %w", trigger.TimeZone, err)
		}
		parsed, err := parseScheduleCron(trigger)
		if err != nil {
			return fmt.Errorf("invalid six-field cron expression: %w", err)
		}
		probe := time.Date(2000, time.January, 1, 0, 0, 0, 0, location).Add(-time.Nanosecond)
		first, err := nextParsedScheduleCronOccurrence(parsed, location, probe)
		if err != nil {
			return fmt.Errorf("cron must produce recurring occurrences: %w", err)
		}
		second, err := nextParsedScheduleCronOccurrence(parsed, location, first)
		if err != nil {
			return fmt.Errorf("cron must produce recurring occurrences: %w", err)
		}
		if second.Sub(first) < time.Duration(minimumScheduleEverySeconds)*time.Second {
			return fmt.Errorf("cron occurrences must be at least %d seconds apart", minimumScheduleEverySeconds)
		}
	default:
		return fmt.Errorf("unsupported trigger type %q", trigger.Type)
	}
	return nil
}

func validateScheduleTriggerForMutation(trigger ScheduleTrigger, now time.Time) error {
	if err := ValidateScheduleTrigger(trigger); err != nil {
		return err
	}
	if err := validateScheduleIntervalPersistence(trigger); err != nil {
		return err
	}
	return validateScheduleTriggerAtFuture(trigger, now)
}

func validateScheduleTriggerForUpdate(trigger ScheduleTrigger, persisted *ScheduleTrigger, now time.Time) error {
	if err := ValidateScheduleTrigger(trigger); err != nil {
		return err
	}
	if err := validateScheduleIntervalPersistence(trigger); err != nil {
		return err
	}
	if persisted != nil && trigger == *persisted {
		return nil
	}
	return validateScheduleTriggerAtFuture(trigger, now)
}

// validateScheduleIntervalPersistence is intentionally a mutation-only
// contract. Portable definitions written by older releases must remain
// readable so the runtime can terminate them deterministically, while new
// definitions must have at least one persistable successor to be recurring.
func validateScheduleIntervalPersistence(trigger ScheduleTrigger) error {
	if trigger.Type != ScheduleTriggerInterval {
		return nil
	}
	anchor, _ := time.Parse(time.RFC3339Nano, trigger.AnchorAt)
	if _, err := intervalOccurrenceAt(anchor, 1, trigger.EverySeconds); err != nil {
		return fmt.Errorf("interval trigger must have a successor inside the RFC3339Nano persistence range: %w", err)
	}
	return nil
}

func validateScheduleTriggerAtFuture(trigger ScheduleTrigger, now time.Time) error {
	if trigger.Type != ScheduleTriggerAt {
		return nil
	}
	at, _ := time.Parse(time.RFC3339Nano, trigger.At)
	if !at.After(now) {
		return ErrScheduleTriggerAtNotFuture
	}
	return nil
}

func scheduleCronParser() cron.Parser {
	return cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
}

func parseScheduleCron(trigger ScheduleTrigger) (cron.Schedule, error) {
	return scheduleCronParser().Parse("CRON_TZ=" + trigger.TimeZone + " " + trigger.Cron)
}

// nextParsedScheduleCronOccurrence extends robfig/cron's five-year search
// window across one complete Gregorian calendar cycle. Successive windows
// overlap within their shared boundary year, so advancing the probe cannot
// skip a valid instant when AddDate crosses a leap day or IANA offset change.
func nextParsedScheduleCronOccurrence(schedule cron.Schedule, location *time.Location, after time.Time) (time.Time, error) {
	if schedule == nil || location == nil {
		return time.Time{}, ErrScheduleCronSuccessorUnavailable
	}
	originalLocation := after.Location()
	cursor := after.In(location)
	for call := 0; call < maximumCronSuccessorCalls; call++ {
		candidate := schedule.Next(cursor)
		if !candidate.IsZero() {
			if !candidate.After(cursor) || !candidate.After(after) {
				return time.Time{}, ErrScheduleCronSuccessorUnavailable
			}
			candidate = candidate.In(originalLocation)
			if !scheduleOccurrenceRepresentable(candidate) {
				return time.Time{}, ErrScheduleOccurrenceOutOfRange
			}
			return candidate, nil
		}

		// A zero result proves robfig exhausted cursor's local start year and
		// the following five years. Once that proof reaches year 9999, no
		// successor can be persisted and extending the search would only
		// manufacture an invalid RFC3339Nano timestamp.
		searchStart := cursor.Add(time.Second - time.Duration(cursor.Nanosecond())*time.Nanosecond)
		if searchStart.Year()+robfigCronSearchWindowYears >= 9999 {
			return time.Time{}, ErrScheduleOccurrenceOutOfRange
		}
		nextCursor := cursor.AddDate(robfigCronSearchWindowYears, 0, 0)
		if !nextCursor.After(cursor) {
			return time.Time{}, ErrScheduleCronSuccessorUnavailable
		}
		cursor = nextCursor
	}
	return time.Time{}, ErrScheduleCronSuccessorUnavailable
}

// NextScheduleOccurrence returns the first nominal occurrence strictly after
// after. The at trigger is returned only when it is still in the future.
func NextScheduleOccurrence(trigger ScheduleTrigger, after time.Time) (time.Time, error) {
	if err := ValidateScheduleTrigger(trigger); err != nil {
		return time.Time{}, err
	}
	switch trigger.Type {
	case ScheduleTriggerAt:
		at, _ := time.Parse(time.RFC3339Nano, trigger.At)
		if at.After(after) {
			return at, nil
		}
		return time.Time{}, nil
	case ScheduleTriggerInterval:
		anchor, _ := time.Parse(time.RFC3339Nano, trigger.AnchorAt)
		_, next, _, err := intervalOccurrenceBounds(anchor, after, trigger.EverySeconds)
		return next, err
	case ScheduleTriggerCron:
		schedule, err := parseScheduleCron(trigger)
		if err != nil {
			return time.Time{}, err
		}
		location, _ := time.LoadLocation(trigger.TimeZone)
		return nextParsedScheduleCronOccurrence(schedule, location, after)
	default:
		return time.Time{}, errors.New("unsupported trigger")
	}
}

// CoalescedScheduleOccurrence finds the first overdue nominal occurrence, the
// last overdue nominal occurrence, and the first future occurrence. Cron
// enumeration is deliberately bounded during long downtime.
func CoalescedScheduleOccurrence(trigger ScheduleTrigger, first, now time.Time) (last, next time.Time, count int, truncated bool, err error) {
	if err := ValidateScheduleTrigger(trigger); err != nil {
		return time.Time{}, time.Time{}, 0, false, err
	}
	if trigger.Type == ScheduleTriggerInterval {
		last, next, intervalCount, err := intervalOccurrenceBounds(first, now, trigger.EverySeconds)
		if err != nil {
			return time.Time{}, time.Time{}, 0, false, err
		}
		if intervalCount > int64(^uint(0)>>1) {
			return time.Time{}, time.Time{}, 0, false, errors.New("schedule occurrence count exceeds platform int range")
		}
		return last, next, int(intervalCount), false, nil
	}
	if first.After(now) {
		return time.Time{}, first, 0, false, nil
	}
	last, count = first, 1
	if trigger.Type == ScheduleTriggerAt {
		return last, time.Time{}, count, false, nil
	}
	parsed, err := parseScheduleCron(trigger)
	if err != nil {
		return time.Time{}, time.Time{}, 0, false, err
	}
	location, _ := time.LoadLocation(trigger.TimeZone)
	cursor := first
	for count < maximumCronOccurrences {
		candidate, nextErr := nextParsedScheduleCronOccurrence(parsed, location, cursor)
		if nextErr != nil {
			if errors.Is(nextErr, ErrScheduleOccurrenceOutOfRange) {
				return last, time.Time{}, count, false, nil
			}
			return time.Time{}, time.Time{}, 0, false, nextErr
		}
		if candidate.After(now) {
			return last, candidate, count, false, nil
		}
		last, cursor, count = candidate, candidate, count+1
	}
	next, err = nextParsedScheduleCronOccurrence(parsed, location, now)
	if errors.Is(err, ErrScheduleOccurrenceOutOfRange) {
		return last, time.Time{}, count, true, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, 0, false, err
	}
	return last, next, count, true, nil
}

// intervalOccurrenceBounds returns the last occurrence at or before boundary,
// the first occurrence strictly after it, and the number of occurrences from
// first through last. It operates in seconds because interval triggers retain
// the anchor's nanosecond within every occurrence, while the complete
// RFC3339Nano year range is much wider than time.Duration.
func intervalOccurrenceBounds(first, boundary time.Time, everySeconds int64) (last, next time.Time, count int64, err error) {
	if !scheduleOccurrenceRepresentable(first) {
		return time.Time{}, time.Time{}, 0, ErrScheduleOccurrenceOutOfRange
	}
	if first.After(boundary) {
		return time.Time{}, first, 0, nil
	}

	latestBoundary := time.Date(10000, time.January, 1, 0, 0, 0, 0, first.Location())
	if !boundary.Before(latestBoundary) {
		return time.Time{}, time.Time{}, 0, ErrScheduleOccurrenceOutOfRange
	}

	elapsedSeconds := boundary.Unix() - first.Unix()
	if boundary.Nanosecond() < first.Nanosecond() {
		elapsedSeconds--
	}
	lastOrdinal := elapsedSeconds / everySeconds
	last, err = intervalOccurrenceAt(first, lastOrdinal, everySeconds)
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}
	next, err = intervalOccurrenceAt(first, lastOrdinal+1, everySeconds)
	if err != nil {
		if errors.Is(err, ErrScheduleOccurrenceOutOfRange) {
			// The representable time domain is finite. Preserve the final valid
			// occurrence and use an empty successor as its terminal cursor.
			return last, time.Time{}, lastOrdinal + 1, nil
		}
		return time.Time{}, time.Time{}, 0, err
	}
	return last, next, lastOrdinal + 1, nil
}

func intervalOccurrenceAt(first time.Time, ordinal, everySeconds int64) (time.Time, error) {
	if ordinal < 0 || ordinal > int64(^uint64(0)>>1)/everySeconds {
		return time.Time{}, ErrScheduleOccurrenceOutOfRange
	}
	elapsedSeconds := ordinal * everySeconds
	firstSeconds := first.Unix()
	if firstSeconds > int64(^uint64(0)>>1)-elapsedSeconds {
		return time.Time{}, ErrScheduleOccurrenceOutOfRange
	}
	occurrence := time.Unix(firstSeconds+elapsedSeconds, int64(first.Nanosecond())).In(first.Location())
	if !scheduleOccurrenceRepresentable(occurrence) {
		return time.Time{}, ErrScheduleOccurrenceOutOfRange
	}
	return occurrence, nil
}

func scheduleOccurrenceRepresentable(occurrence time.Time) bool {
	encoded := occurrence.Format(time.RFC3339Nano)
	decoded, err := time.Parse(time.RFC3339Nano, encoded)
	return err == nil && decoded.Equal(occurrence)
}

func writeSchedulerJSON(path string, config SchedulerConfig) error {
	if config.Schedules == nil {
		config.Schedules = []Schedule{}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeSchedulerBytesAtomically(path, ".scheduler-*.tmp", data)
}

func writeSchedulerBytesAtomically(path, tempPattern string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	remove = false
	return syncDirectory(dir)
}

// Scheduler returns the validated Scheduler configuration.
func (w *Workspace) Scheduler() (SchedulerConfig, error) {
	if err := w.require(); err != nil {
		return SchedulerConfig{}, err
	}
	config, err := readSchedulerJSON(schedulerJSONPath(w.root))
	if err != nil {
		return SchedulerConfig{}, &APIError{Operation: "read Scheduler", Kind: "scheduler", Workspace: w.root, Path: schedulerDir + "/" + schedulerJSONFile, Err: err}
	}
	return config, nil
}

func (w *Workspace) schedulerResourceDetail() (ResourceDetailView, error) {
	config, err := w.Scheduler()
	if err != nil {
		return ResourceDetailView{}, err
	}
	dir := schedulerPath(w.root)
	info, err := os.Stat(schedulerJSONPath(w.root))
	if err != nil {
		return ResourceDetailView{}, &APIError{Operation: "read Scheduler detail", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	timestamp := info.ModTime().Format(time.RFC3339)
	files := make([]ResourceFile, 0, 2)
	for _, name := range []string{schedulerMarkdownFile, "AGENTS.md"} {
		path := filepath.Join(dir, name)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		files = append(files, ResourceFile{Name: name, Path: relPath(w.root, path), Content: string(data), ContentHash: markdownContentHash(data)})
	}
	snapshot := SchedulerSnapshot{SchemaVersion: config.SchemaVersion, AgentBinding: config.AgentBinding, Schedules: make([]ScheduleSnapshot, 0, len(config.Schedules))}
	for _, schedule := range config.Schedules {
		snapshot.Schedules = append(snapshot.Schedules, ScheduleSnapshot{Schedule: schedule, EffectiveState: schedule.State})
	}
	return ResourceDetailView{
		ID: SchedulerResourceID, Type: SchedulerResourceID, Title: "Scheduler",
		CreatedAt: timestamp, UpdatedAt: timestamp, Path: schedulerDir,
		AgentBinding: config.AgentBinding, Files: files,
		Artifacts: []FileTreeEntry{}, Worktrees: []FileTreeEntry{}, Scheduler: &snapshot,
	}, nil
}

func (w *Workspace) AddSchedule(input CreateScheduleInput) (Schedule, error) {
	if err := w.require(); err != nil {
		return Schedule{}, err
	}
	if input.Trigger == nil {
		return Schedule{}, &APIError{Operation: "add schedule", Kind: "scheduler", Workspace: w.root, Err: ErrScheduleTriggerRequired}
	}
	trigger := *input.Trigger
	if err := ValidateScheduleTrigger(trigger); err != nil {
		return Schedule{}, &APIError{Operation: "add schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	var created Schedule
	err := withWorkspaceMutationLock(w.root, func() error {
		config, err := readSchedulerJSON(schedulerJSONPath(w.root))
		if err != nil {
			return err
		}
		description, condition, target, err := w.normalizeScheduleFields(input.Description, input.Condition, input.Target, true)
		if err != nil {
			return err
		}
		guard := strings.TrimSpace(input.Guard)
		if len(guard) > maximumScheduleTextLength || strings.ContainsRune(guard, '\x00') {
			return errors.New("schedule guard is invalid")
		}
		mutationTime := time.Now()
		if err := validateScheduleTriggerForMutation(trigger, mutationTime); err != nil {
			return err
		}
		id, err := newScheduleID()
		if err != nil {
			return err
		}
		now := mutationTime.Format(time.RFC3339Nano)
		created = Schedule{ID: id, Revision: 1, ActivationRevision: 1, Description: description, Condition: condition, Guard: guard, Target: target, State: ScheduleStateActive, Trigger: &trigger, CreatedAt: now, UpdatedAt: now}
		config.Schedules = append(config.Schedules, created)
		return writeSchedulerJSON(schedulerJSONPath(w.root), config)
	})
	if err != nil {
		return Schedule{}, &APIError{Operation: "add schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	return created, nil
}

func (w *Workspace) UpdateSchedule(input UpdateScheduleInput) (Schedule, error) {
	if err := w.require(); err != nil {
		return Schedule{}, err
	}
	if err := validateUpdateScheduleInput(&input); err != nil {
		return Schedule{}, &APIError{Operation: "update schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	var updated Schedule
	err := withWorkspaceMutationLock(w.root, func() error {
		config, err := readSchedulerJSON(schedulerJSONPath(w.root))
		if err != nil {
			return err
		}
		index := scheduleIndex(config.Schedules, input.ID)
		if index < 0 {
			return &ScheduleNotFoundError{ScheduleID: input.ID}
		}
		updated, err = w.prepareScheduleUpdate(config.Schedules[index], input)
		if err != nil {
			return err
		}
		config.Schedules[index] = updated
		return writeSchedulerJSON(schedulerJSONPath(w.root), config)
	})
	if err != nil {
		return Schedule{}, &APIError{Operation: "update schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	return updated, nil
}

// PreviewScheduleUpdate runs the same compare-and-swap and portable-definition
// validation as UpdateSchedule without writing scheduler.json. NativeScheduler
// uses the current definition to decide whether old runtime work must be
// materialized before a metadata-only revision changes UpdatedAt.
func (w *Workspace) PreviewScheduleUpdate(input UpdateScheduleInput) (Schedule, Schedule, error) {
	if err := w.require(); err != nil {
		return Schedule{}, Schedule{}, err
	}
	if err := validateUpdateScheduleInput(&input); err != nil {
		return Schedule{}, Schedule{}, &APIError{Operation: "update schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	var current, updated Schedule
	err := withWorkspaceMutationLock(w.root, func() error {
		config, err := readSchedulerJSON(schedulerJSONPath(w.root))
		if err != nil {
			return err
		}
		index := scheduleIndex(config.Schedules, input.ID)
		if index < 0 {
			return &ScheduleNotFoundError{ScheduleID: input.ID}
		}
		current = config.Schedules[index]
		updated, err = w.prepareScheduleUpdate(current, input)
		return err
	})
	if err != nil {
		return Schedule{}, Schedule{}, &APIError{Operation: "update schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	return current, updated, nil
}

func validateUpdateScheduleInput(input *UpdateScheduleInput) error {
	input.ID = strings.TrimSpace(input.ID)
	if input.ID == "" || (input.Description == nil && input.Condition == nil && input.Guard == nil && input.Target == nil && input.Trigger == nil) {
		return errors.New("schedule id and at least one updated field are required")
	}
	if input.ExpectedRevision == 0 {
		return errors.New("expectedRevision is required")
	}
	return nil
}

func (w *Workspace) prepareScheduleUpdate(current Schedule, input UpdateScheduleInput) (Schedule, error) {
	if input.ExpectedRevision != current.Revision {
		return Schedule{}, &ScheduleRevisionConflictError{ScheduleID: input.ID, Expected: input.ExpectedRevision, Actual: current.Revision}
	}
	description, condition, target := current.Description, current.Condition, current.Target
	if input.Description != nil {
		description = *input.Description
	}
	if input.Condition != nil {
		condition = *input.Condition
	}
	if input.Target != nil {
		target = *input.Target
	}
	description, condition, target, err := w.normalizeScheduleFields(description, condition, target, input.Target != nil)
	if err != nil {
		return Schedule{}, err
	}
	guard := current.Guard
	if input.Guard != nil {
		guard = strings.TrimSpace(*input.Guard)
		if len(guard) > maximumScheduleTextLength || strings.ContainsRune(guard, '\x00') {
			return Schedule{}, errors.New("schedule guard is invalid")
		}
	}
	mutationTime := time.Now()
	trigger := current.Trigger
	if input.Trigger != nil {
		copy := *input.Trigger
		if err := validateScheduleTriggerForUpdate(copy, current.Trigger, mutationTime); err != nil {
			return Schedule{}, err
		}
		trigger = &copy
	}
	updated := current
	updated.Description, updated.Condition, updated.Guard, updated.Target = description, condition, guard, target
	updated.Trigger = trigger
	if updated.State == ScheduleStateNeedsCompilation && trigger != nil {
		updated.State = ScheduleStateActive
	}
	if err := incrementScheduleRevision(&updated); err != nil {
		return Schedule{}, err
	}
	if current.Target != updated.Target || !scheduleTriggersEqual(current.Trigger, updated.Trigger) {
		updated.ActivationRevision = updated.Revision
	}
	updated.UpdatedAt = mutationTime.Format(time.RFC3339Nano)
	if err := validateSchedule(updated); err != nil {
		return Schedule{}, err
	}
	return updated, nil
}

func scheduleTriggersEqual(left, right *ScheduleTrigger) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (w *Workspace) RemoveSchedule(id string) (Schedule, error) {
	if err := w.require(); err != nil {
		return Schedule{}, err
	}
	id = strings.TrimSpace(id)
	var removed Schedule
	err := withWorkspaceMutationLock(w.root, func() error {
		config, err := readSchedulerJSON(schedulerJSONPath(w.root))
		if err != nil {
			return err
		}
		index := scheduleIndex(config.Schedules, id)
		if index < 0 {
			return &ScheduleNotFoundError{ScheduleID: id}
		}
		removed = config.Schedules[index]
		config.Schedules = append(config.Schedules[:index], config.Schedules[index+1:]...)
		return writeSchedulerJSON(schedulerJSONPath(w.root), config)
	})
	if err != nil {
		return Schedule{}, &APIError{Operation: "remove schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	return removed, nil
}

func (w *Workspace) PauseSchedule(id string) (Schedule, error) {
	return w.changeScheduleState(id, ScheduleStatePaused)
}

func (w *Workspace) ResumeSchedule(id string) (Schedule, error) {
	return w.changeScheduleState(id, ScheduleStateActive)
}

func (w *Workspace) changeScheduleState(id, state string) (Schedule, error) {
	if err := w.require(); err != nil {
		return Schedule{}, err
	}
	id = strings.TrimSpace(id)
	var updated Schedule
	err := withWorkspaceMutationLock(w.root, func() error {
		config, err := readSchedulerJSON(schedulerJSONPath(w.root))
		if err != nil {
			return err
		}
		index := scheduleIndex(config.Schedules, id)
		if index < 0 {
			return &ScheduleNotFoundError{ScheduleID: id}
		}
		updated = config.Schedules[index]
		if updated.State == state {
			return nil
		}
		if updated.State == ScheduleStateCompleted {
			return errors.New("completed schedule cannot change state")
		}
		if updated.Trigger == nil {
			return errors.New("schedule requires compilation before it can change state")
		}
		mutationTime := time.Now()
		if state == ScheduleStatePaused && updated.Trigger.Type == ScheduleTriggerAt {
			at, err := time.Parse(time.RFC3339Nano, updated.Trigger.At)
			if err != nil {
				return err
			}
			if !at.After(mutationTime) {
				return ErrScheduleOccurrenceDue
			}
		}
		updated.State = state
		if err := incrementScheduleRevision(&updated); err != nil {
			return err
		}
		updated.UpdatedAt = mutationTime.Format(time.RFC3339Nano)
		config.Schedules[index] = updated
		return writeSchedulerJSON(schedulerJSONPath(w.root), config)
	})
	if err != nil {
		return Schedule{}, &APIError{Operation: state + " schedule", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	return updated, nil
}

func (w *Workspace) normalizeScheduleFields(description, condition, target string, validateTarget bool) (string, string, string, error) {
	description = strings.TrimSpace(description)
	condition = strings.TrimSpace(condition)
	target = strings.TrimSpace(target)
	if description == "" || condition == "" || target == "" {
		return "", "", "", errors.New("description, condition, and target are required")
	}
	if len(description) > maximumScheduleTextLength || len(condition) > maximumScheduleTextLength || len(target) > 200 ||
		strings.ContainsRune(description, '\x00') || strings.ContainsRune(condition, '\x00') || strings.ContainsRune(target, '\x00') {
		return "", "", "", errors.New("schedule field is invalid")
	}
	if validateTarget {
		if err := ValidateScheduleTarget(target); err != nil {
			return "", "", "", err
		}
		if target != "workspace" {
			if _, _, err := loadOpenResource(w.root, target); err != nil {
				return "", "", "", fmt.Errorf("target must be an open resource in the current Workspace: %w", err)
			}
		}
	}
	return description, condition, target, nil
}

func scheduleIndex(schedules []Schedule, id string) int {
	for index := range schedules {
		if schedules[index].ID == id {
			return index
		}
	}
	return -1
}

func newScheduleID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "schedule-" + hex.EncodeToString(value[:]), nil
}

func defaultSchedulerMarkdown(language string) string {
	return localize.MustRender(language, "scheduler.md", nil)
}

func schedulerAgentsBlock(language string) string {
	return puaPromptStart + "\n" + schedulerAgentsPrompt(language) + "\n" + puaPromptEnd
}

func schedulerAgentsPrompt(language string) string {
	return localize.MustRender(language, "scheduler-agents.md", nil)
}
