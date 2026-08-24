package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
	"github.com/disksing/pua/internal/schedulerapi"
)

type schedulerChangeRequest struct {
	Operation        string                    `json:"operation"`
	ID               string                    `json:"id,omitempty"`
	ExpectedRevision schedulerExpectedRevision `json:"expectedRevision,omitempty"`
	Description      *string                   `json:"description,omitempty"`
	Condition        *string                   `json:"condition,omitempty"`
	Guard            *string                   `json:"guard,omitempty"`
	Target           *string                   `json:"target,omitempty"`
	Trigger          *app.ScheduleTrigger      `json:"trigger,omitempty"`
}

// schedulerExpectedRevision adds request-field context while delegating the
// lossless decimal-string contract to schedulerapi.Revision.
type schedulerExpectedRevision schedulerapi.Revision

func (revision *schedulerExpectedRevision) UnmarshalJSON(data []byte) error {
	var parsed schedulerapi.Revision
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("expectedRevision: %w", err)
	}
	*revision = schedulerExpectedRevision(parsed)
	return nil
}

func (revision schedulerExpectedRevision) Uint64() (uint64, error) {
	parsed, err := schedulerapi.Revision(revision).Uint64()
	if err != nil {
		return 0, fmt.Errorf("expectedRevision: %w", err)
	}
	return parsed, nil
}

type schedulerResourceDetail struct {
	app.ResourceDetailView
	Scheduler *schedulerapi.Snapshot `json:"scheduler,omitempty"`
}

// schedulerControllerJobOutcome keeps controller-owned results independent of
// an HTTP request's lifetime. withResourceController may return as soon as the
// caller is cancelled even though a job which already started must still
// finish its durable boundary.
type schedulerControllerJobOutcome[T any] struct {
	Value    T
	Material bool
	Err      error
}

func runSchedulerControllerJob[T any](
	ctx context.Context,
	s *server,
	workspace serveWorkspace,
	job func() schedulerControllerJobOutcome[T],
	afterMaterial func(T),
) (T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := make(chan schedulerControllerJobOutcome[T], 1)
	err := s.agents.withResourceController(ctx, workspace, app.SchedulerResourceID, func() error {
		outcome := job()
		if outcome.Err == nil && outcome.Material && s.ownsWorkspace(workspace.Path) && afterMaterial != nil {
			afterMaterial(outcome.Value)
		}
		result <- outcome
		return outcome.Err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return (<-result).Value, nil
}

func schedulerResourceDetailAPIResponse(detail app.ResourceDetailView) schedulerResourceDetail {
	response := schedulerResourceDetail{ResourceDetailView: detail}
	if detail.Scheduler != nil {
		snapshot := schedulerapi.FromSnapshot(*detail.Scheduler)
		response.Scheduler = &snapshot
	}
	return response
}

func (s *server) handleScheduler(w http.ResponseWriter, r *http.Request, workspaceID string, parts []string) {
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if s.agents == nil {
		writeError(w, errors.New("Scheduler owner Server is unavailable"), http.StatusServiceUnavailable)
		return
	}
	native := newNativeScheduler(s.agents, workspace)

	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			snapshot, readErr := runSchedulerControllerJob(r.Context(), s, workspace, func() schedulerControllerJobOutcome[app.SchedulerSnapshot] {
				value, snapshotErr := native.Snapshot(s.agents.now())
				return schedulerControllerJobOutcome[app.SchedulerSnapshot]{Value: value, Err: snapshotErr}
			}, nil)
			if readErr != nil {
				writeError(w, readErr, http.StatusBadRequest)
				return
			}
			writeJSON(w, schedulerapi.FromSnapshot(snapshot))
		case http.MethodPost:
			s.handleNaturalLanguageScheduleRequest(w, r, workspace, app.ScheduleChangeCreate, "")
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if len(parts) == 1 && parts[0] == "changes" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body schedulerChangeRequest
		if err := decodeSchedulerBody(r, &body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		change, err := schedulerNativeChange(body)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		changed, changeErr := s.changeNativeSchedule(r.Context(), workspace, native, change)
		if changeErr != nil {
			writeSchedulerChangeError(w, changeErr)
			return
		}
		writeJSON(w, schedulerapi.FromSchedule(changed))
		return
	}

	id := strings.TrimSpace(parts[0])
	if id == "" || len(parts) > 2 {
		writeError(w, errors.New("schedule id is required"), http.StatusBadRequest)
		return
	}
	if len(parts) == 2 {
		operation, err := app.ParseScheduleChangeOperation(parts[1])
		if r.Method != http.MethodPost || err != nil || (operation != app.ScheduleChangePause && operation != app.ScheduleChangeResume) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.applyDirectScheduleChange(w, r, workspace, NativeSchedulerChange{Operation: operation, ID: id})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleNaturalLanguageScheduleRequest(w, r, workspace, app.ScheduleChangeUpdate, id)
	case http.MethodDelete:
		s.applyDirectScheduleChange(w, r, workspace, NativeSchedulerChange{Operation: app.ScheduleChangeRemove, ID: id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func decodeSchedulerBody(r *http.Request, output any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func schedulerNativeChange(body schedulerChangeRequest) (NativeSchedulerChange, error) {
	operation, err := app.ParseScheduleChangeOperation(body.Operation)
	if err != nil {
		return NativeSchedulerChange{}, err
	}
	expectedRevision := uint64(0)
	if body.ExpectedRevision != "" {
		expectedRevision, err = body.ExpectedRevision.Uint64()
		if err != nil {
			return NativeSchedulerChange{}, err
		}
	}
	if operation == app.ScheduleChangeUpdate && expectedRevision == 0 {
		return NativeSchedulerChange{}, errors.New("expectedRevision is required for schedule updates")
	}
	if operation != app.ScheduleChangeUpdate && expectedRevision != 0 {
		return NativeSchedulerChange{}, errors.New("expectedRevision is only valid for schedule updates")
	}
	return NativeSchedulerChange{
		Operation:        operation,
		ID:               strings.TrimSpace(body.ID),
		ExpectedRevision: expectedRevision,
		Description:      body.Description,
		Condition:        body.Condition,
		Guard:            body.Guard,
		Target:           body.Target,
		Trigger:          body.Trigger,
	}, nil
}

func (s *server) applyDirectScheduleChange(w http.ResponseWriter, r *http.Request, workspace serveWorkspace, change NativeSchedulerChange) {
	native := newNativeScheduler(s.agents, workspace)
	changed, err := s.changeNativeSchedule(r.Context(), workspace, native, change)
	if err != nil {
		writeSchedulerChangeError(w, err)
		return
	}
	writeJSON(w, schedulerapi.FromSchedule(changed))
}

func (s *server) changeNativeSchedule(ctx context.Context, workspace serveWorkspace, native *NativeScheduler, change NativeSchedulerChange) (app.Schedule, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	jobCtx := context.WithoutCancel(ctx)
	return runSchedulerControllerJob(ctx, s, workspace, func() schedulerControllerJobOutcome[app.Schedule] {
		if err := s.requireWorkspaceOwnership(workspace.Path); err != nil {
			return schedulerControllerJobOutcome[app.Schedule]{Err: err}
		}
		before, err := nativeScheduleMaterialState(workspace, native, change)
		if err != nil {
			return schedulerControllerJobOutcome[app.Schedule]{Err: err}
		}
		changed, err := native.Change(jobCtx, change)
		if err != nil {
			return schedulerControllerJobOutcome[app.Schedule]{Value: changed, Err: err}
		}
		after, err := nativeScheduleMaterialState(workspace, native, change)
		if err != nil {
			return schedulerControllerJobOutcome[app.Schedule]{Value: changed, Err: err}
		}
		return schedulerControllerJobOutcome[app.Schedule]{
			Value: changed, Material: nativeScheduleChangeIsMaterial(change, before, after),
		}
	}, func(app.Schedule) {
		s.agents.requestReconcile(reconcileScheduler)
	})
}

type nativeScheduleMaterialityState struct {
	PortableState string
	Runtime       schedulerScheduleRuntime
}

// nativeScheduleMaterialState is called before and after Change while the
// Scheduler controller is held. No other Scheduler job can change portable
// definitions or checkpoints between these observations.
func nativeScheduleMaterialState(workspace serveWorkspace, native *NativeScheduler, change NativeSchedulerChange) (nativeScheduleMaterialityState, error) {
	switch change.Operation {
	case app.ScheduleChangePause, app.ScheduleChangeResume:
	default:
		return nativeScheduleMaterialityState{}, nil
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return nativeScheduleMaterialityState{}, err
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		return nativeScheduleMaterialityState{}, err
	}
	for _, schedule := range config.Schedules {
		if schedule.ID == change.ID {
			state := nativeScheduleMaterialityState{PortableState: schedule.State}
			state.Runtime, err = native.schedulerRuntime(change.ID)
			return state, err
		}
	}
	return nativeScheduleMaterialityState{}, nil
}

func nativeScheduleChangeIsMaterial(change NativeSchedulerChange, before, after nativeScheduleMaterialityState) bool {
	switch change.Operation {
	case app.ScheduleChangePause, app.ScheduleChangeResume:
		return before.PortableState != after.PortableState || !reflect.DeepEqual(before.Runtime, after.Runtime)
	default:
		// Successful create, update, and remove operations always rewrite the
		// portable definition. Validation and write failures are filtered by
		// the job outcome before its follow-up runs.
		return true
	}
}

func writeSchedulerChangeError(w http.ResponseWriter, err error) {
	if errors.Is(err, app.ErrScheduleNotFound) {
		writeError(w, &resourceAPIError{Code: "schedule_not_found", Message: err.Error()}, http.StatusNotFound)
		return
	}
	var conflict *app.ScheduleRevisionConflictError
	if errors.As(err, &conflict) {
		writeError(w, &resourceAPIError{Code: "schedule_revision_conflict", Message: conflict.Error()}, http.StatusConflict)
		return
	}
	if errors.Is(err, app.ErrScheduleRevisionExhausted) {
		writeError(w, &resourceAPIError{Code: "schedule_revision_exhausted", Message: err.Error()}, http.StatusConflict)
		return
	}
	if errors.Is(err, app.ErrScheduleOccurrenceOutOfRange) {
		writeError(w, &resourceAPIError{Code: "schedule_occurrence_out_of_range", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if errors.Is(err, app.ErrScheduleTargetScheduler) {
		writeSchedulerTargetError(w)
		return
	}
	if errors.Is(err, errNativeSchedulerUpdateTriggerRequired) {
		writeError(w, &resourceAPIError{Code: "schedule_trigger_required", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if errors.Is(err, errNativeSchedulerPauseCompleted) {
		writeError(w, &resourceAPIError{Code: "schedule_state_conflict", Message: err.Error()}, http.StatusConflict)
		return
	}
	writeError(w, err, http.StatusBadRequest)
}

func writeSchedulerTargetError(w http.ResponseWriter) {
	writeError(w, &resourceAPIError{
		Code: "schedule_target_invalid", Message: app.ErrScheduleTargetScheduler.Error(),
	}, http.StatusBadRequest)
}

func (s *server) handleNaturalLanguageScheduleRequest(w http.ResponseWriter, r *http.Request, workspace serveWorkspace, operation app.ScheduleChangeOperation, id string) {
	var body struct {
		ExpectedRevision *schedulerExpectedRevision `json:"expectedRevision,omitempty"`
		Description      string                     `json:"description"`
		Condition        string                     `json:"condition"`
		Target           string                     `json:"target"`
	}
	if err := decodeSchedulerBody(r, &body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	description, condition, target := strings.TrimSpace(body.Description), strings.TrimSpace(body.Condition), strings.TrimSpace(body.Target)
	if description == "" || condition == "" || target == "" {
		writeError(w, errors.New("description, condition, and target are required"), http.StatusBadRequest)
		return
	}
	if operation == app.ScheduleChangeCreate && body.ExpectedRevision != nil {
		writeError(w, errors.New("expectedRevision is only valid for schedule updates"), http.StatusBadRequest)
		return
	}
	if operation == app.ScheduleChangeUpdate && body.ExpectedRevision == nil {
		writeError(w, errors.New("expectedRevision is required for schedule updates"), http.StatusBadRequest)
		return
	}
	expectedRevision := uint64(0)
	if body.ExpectedRevision != nil {
		parsedRevision, parseErr := body.ExpectedRevision.Uint64()
		if parseErr != nil {
			writeError(w, parseErr, http.StatusBadRequest)
			return
		}
		expectedRevision = parsedRevision
	}
	if err := app.ValidateScheduleTarget(target); err != nil {
		writeSchedulerTargetError(w)
		return
	}
	userName, err := s.workspaceUserName(r, workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	language, err := workspaceContentLanguage(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	text := strings.TrimSuffix(localize.MustRender(language, "scheduler-compilation.md", map[string]any{
		"Operation":           string(operation),
		"HasID":               id != "",
		"ID":                  id,
		"HasExpectedRevision": operation == app.ScheduleChangeUpdate,
		"ExpectedRevision":    strconv.FormatUint(expectedRevision, 10),
		"Description":         description,
		"Condition":           condition,
		"Target":              target,
	}), "\n")
	role, sender := agentHubMessageProvenance(userName)
	message, acceptErr := runSchedulerControllerJob(r.Context(), s, workspace, func() schedulerControllerJobOutcome[resourceMailboxMessage] {
		accepted, acceptErr := s.agents.acceptResourceMessageDurable(context.WithoutCancel(r.Context()), workspace, app.SchedulerResourceID, resourceMessageRequest{
			Text: text, Mode: resourceMessageModeEnqueue, Role: role, Sender: sender,
		})
		return schedulerControllerJobOutcome[resourceMailboxMessage]{Value: accepted, Material: acceptErr == nil, Err: acceptErr}
	}, func(accepted resourceMailboxMessage) {
		s.enqueueSchedulerMailboxReconcile(workspace, accepted)
	})
	if acceptErr != nil {
		writeError(w, acceptErr, resourceErrorStatus(acceptErr))
		return
	}
	s.markResourceReadOnUserMessage(workspace.Path, app.SchedulerResourceID, userName)
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspace.ID, message.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *server) enqueueSchedulerMailboxReconcile(workspace serveWorkspace, message resourceMailboxMessage) {
	if wakeErr := s.agents.enqueueResourceController(workspace, app.SchedulerResourceID, func() error {
		if err := s.agents.reconcileResourceMailboxLocked(context.Background(), workspace, app.SchedulerResourceID); err != nil {
			recordMailboxFailure(workspace.Path, message.ID, err)
		}
		s.agents.requestReconcile(reconcileNotifications)
		return nil
	}); wakeErr != nil {
		recordMailboxFailure(workspace.Path, message.ID, wakeErr)
		s.agents.requestReconcile(reconcileNotifications)
	}
}
