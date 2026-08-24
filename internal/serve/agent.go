package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

// generationRecord is the internal persisted record of one resource
// generation. ID is an implementation key only; resource APIs address
// records by GenerationID.
type generationRecord struct {
	ID                 string `json:"id"`
	WorkspaceID        string `json:"workspaceId"`
	ResourceID         string `json:"resourceId,omitempty"`
	Generation         int    `json:"generation,omitempty"`
	GenerationID       string `json:"generationId,omitempty"`
	SourceInstanceID   string `json:"sourceInstanceId,omitempty"`
	BindingKind        string `json:"bindingKind,omitempty"`
	BindingName        string `json:"bindingName,omitempty"`
	ProfileRevision    string `json:"profileRevision,omitempty"`
	ResolvedProfile    string `json:"resolvedProfile,omitempty"`
	AgentConfigError   string `json:"agentConfigError,omitempty"`
	ReplacementPending bool   `json:"replacementPending,omitempty"`
	// ManualStopRequested distinguishes an explicit user request from
	// binding-driven replacement. A later message creates the successor lazily.
	ManualStopRequested  bool   `json:"manualStopRequested,omitempty"`
	AgentProfile         string `json:"agentProfile,omitempty"`
	AgentSelectionReason string `json:"agentSelectionReason,omitempty"`
	AgentHubSessionID    string `json:"agentHubSessionId,omitempty"`
	AgentHubAgentName    string `json:"agentHubAgentName,omitempty"`
	// AgentHubProviderID, AgentHubProviderName, and AgentHubModel are immutable
	// launch-time catalog snapshots. History must not re-resolve them from the
	// current AgentHub catalog after a binding or provider configuration change.
	AgentHubProviderID      string `json:"agentHubProviderId,omitempty"`
	AgentHubProviderName    string `json:"agentHubProviderName,omitempty"`
	AgentHubModel           string `json:"agentHubModel,omitempty"`
	SourceExternalID        string `json:"sourceExternalId,omitempty"`
	AgentHubStoppedObserved bool   `json:"agentHubStoppedObserved,omitempty"`
	// IdleSinceAt and IdleDeadlineAt are the durable ready-boundary clock for
	// automatic resource Session sleep. They are never derived from the
	// generation projection's UpdatedAt, because ordinary polling must not
	// postpone the deadline.
	IdleSinceAt    string `json:"idleSinceAt,omitempty"`
	IdleDeadlineAt string `json:"idleDeadlineAt,omitempty"`
	// IdleSleepStopRequested records that this current generation was put to
	// sleep by the idle policy. It remains set after the Session reaches stopped
	// so the public projection can distinguish an idle-suspended current
	// generation from a retired one; a later mailbox item resumes this same
	// generation and clears the marker at the ready boundary.
	IdleSleepStopRequested bool `json:"idleSleepStopRequested,omitempty"`
	// LifecycleReceipt is the durable boundary for the last lifecycle effect.
	// It is deliberately stored on the generation so a PUA restart can retry
	// or observe an idempotent Resume without a process-local state machine.
	LifecycleReceipt *GenerationLifecycleReceipt `json:"lifecycleReceipt,omitempty"`
	// SessionResumeUnavailable is set only after AgentHub explicitly reports
	// that this exact Session cannot be resumed (or its identity no longer
	// matches). It then permits the existing replacement/retirement path.
	SessionResumeUnavailable bool `json:"sessionResumeUnavailable,omitempty"`
	// Resume retry state is durable so a Server restart cannot turn a temporary
	// AgentHub failure into a tight Resume loop.
	ResumeFailureCount int    `json:"resumeFailureCount,omitempty"`
	ResumeRetryAt      string `json:"resumeRetryAt,omitempty"`
	ResumeLastError    string `json:"resumeLastError,omitempty"`
	// StallWatchdog is the durable Stop -> Resume recovery checkpoint for one
	// stalled Turn. Keeping the checkpoint on the generation makes a restart
	// fail closed instead of issuing a second non-idempotent Stop.
	StallWatchdog *stallWatchdogState `json:"stallWatchdog,omitempty"`
	// ArchivedTaskStopRequested is the legacy-named durable progress marker for
	// any archived Project/Task generation stop. It records that reconciliation
	// has entered the Stop -> stopped -> Archive sequence; unknown outcomes are
	// retried until observed rather than treated as terminal.
	ArchivedTaskStopRequested bool   `json:"archivedTaskStopRequested,omitempty"`
	PendingInitialMessage     string `json:"pendingInitialMessage,omitempty"`
	Title                     string `json:"title"`
	Cwd                       string `json:"cwd"`
	Status                    string `json:"status"`
	CreatedAt                 string `json:"createdAt"`
	UpdatedAt                 string `json:"updatedAt"`
	LastOutputAt              string `json:"lastOutputAt,omitempty"`
	// TurnNumber is the durable ordinal of the latest AgentHub turn observed
	// for this generation. LastTurnID survives an idle edge so a PUA restart
	// or repeated session projection cannot count the same turn twice.
	TurnNumber    int    `json:"turnNumber,omitempty"`
	CurrentTurnID string `json:"currentTurnId,omitempty"`
	LastTurnID    string `json:"lastTurnId,omitempty"`
	TurnStartedAt string `json:"turnStartedAt,omitempty"`
	// GenerationCompletedTurns and GenerationTurnDurationMS are derived from
	// AgentHub's materialized closed Turns for this exact Session. The event
	// cursor prevents active polling from repeatedly fetching unchanged Turn
	// pages while still allowing older current generations to initialize once.
	GenerationCompletedTurns int   `json:"generationCompletedTurns,omitempty"`
	GenerationTurnDurationMS int64 `json:"generationTurnDurationMs,omitempty"`
	GenerationUsageEventID   int64 `json:"generationUsageEventId,omitempty"`
	GenerationUsageReady     bool  `json:"generationUsageReady,omitempty"`
	// CompletionCursor is the last durable AgentHub event cursor inspected for
	// a completed turn. CompletionMarker is only advanced from canonical
	// turn.* terminal events, so status projections cannot manufacture a
	// completion. Both fields live in the local generation record and are
	// rebuilt/reconciled from AgentHub's durable event log.
	CompletionCursor        int64  `json:"completionCursor,omitempty"`
	CompletionSessionID     string `json:"completionSessionId,omitempty"`
	CompletionEventID       int64  `json:"completionEventId,omitempty"`
	CompletionMarker        string `json:"completionMarker,omitempty"`
	CompletionState         string `json:"completionState,omitempty"`
	CompletionHasFinalReply bool   `json:"completionHasFinalReply,omitempty"`
	CompletionTurnID        string `json:"completionTurnId,omitempty"`
	CompletionAt            string `json:"completionAt,omitempty"`
	CompletionPending       bool   `json:"completionPending,omitempty"`
	// Task workflow continuation fields make terminal handling idempotent across
	// duplicate AgentHub observations and PUA Server restarts.
	TaskStateChainID           string             `json:"taskStateChainId,omitempty"`
	TaskStateChainKind         taskStateChainKind `json:"taskStateChainKind,omitempty"`
	TaskStateContinuationCount int                `json:"taskStateContinuationCount,omitempty"`
	TaskStateCompletionMarker  string             `json:"taskStateCompletionMarker,omitempty"`
	// Retired is a storage projection flag, not a public runtime field.
	// Retired records are immutable history and must never enter the
	// lifecycle reconciler.
	Retired      bool   `json:"-"`
	RetireReason string `json:"retireReason,omitempty"`
}

type stallWatchdogState struct {
	GenerationID      string `json:"generationId"`
	SessionID         string `json:"sessionId"`
	TurnID            string `json:"turnId"`
	RecoveryTurnID    string `json:"recoveryTurnId,omitempty"`
	RecoveryMessageID string `json:"recoveryMessageId"`
	DetectedAt        string `json:"detectedAt"`
	Attempt           int    `json:"attempt"`
	StopRequested     bool   `json:"stopRequested,omitempty"`
	RecoveryExhausted bool   `json:"recoveryExhausted,omitempty"`
}

const (
	agentHubEventMaxCount         = 500
	agentUploadMaxBytes           = 512 * 1024 * 1024
	defaultResourceIdleSleepAfter = 30 * time.Minute
)

type puaNotice struct {
	Source string        `json:"source"`
	Type   string        `json:"type"`
	Time   string        `json:"time"`
	Data   puaNoticeData `json:"data"`
}

type puaNoticeData struct {
	Level      string `json:"level"`
	Method     string `json:"method"`
	Text       string `json:"text"`
	Kind       string `json:"kind,omitempty"`
	Lifecycle  string `json:"lifecycle,omitempty"`
	ResourceID string `json:"resourceId,omitempty"`
}

type agentStreamMessage struct {
	Notice *puaNotice
}

type agentUploadResponse struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type agentApprovalRequest struct {
	RequestID string `json:"requestId"`
	Decision  string `json:"decision"`
	OptionID  string `json:"optionId"`
	Text      string `json:"text"`
}

type agentRuntime struct {
	mu                    sync.Mutex
	turnActionMu          sync.Mutex
	retirementMu          sync.Mutex
	workspace             serveWorkspace
	manager               *agentManager
	record                generationRecord
	agentHub              *agentHubClient
	agentHubState         string
	agentHubStopRequested bool
	lifecycleStopInFlight bool
	archivedProofFailed   bool
}

type agentManager struct {
	server                *server
	mu                    sync.Mutex
	backgroundWork        sync.WaitGroup
	resourceControllersMu sync.Mutex
	resourceControllers   map[resourceControllerKey]*resourceController
	workspaceBarriersMu   sync.Mutex
	workspaceBarriers     map[string]*workspaceHandoffBarrier
	runtimes              map[string]*agentRuntime
	subscribers           map[string]map[chan agentStreamMessage]bool
	reconcileWake         chan struct{}
	reconcilePending      reconcileRequest
	now                   func() time.Time
	idleSleepAfter        time.Duration
	activePollInterval    time.Duration
	stablePollInterval    time.Duration
	coldAuditInterval     time.Duration
	mailboxRetryInterval  time.Duration
	notificationInterval  time.Duration
	schedulerFallback     time.Duration
}

// runBackground tracks short-lived work started by an HTTP request. The
// service keeps the work asynchronous for callers, while tests and orderly
// shutdown paths can wait until the manager has stopped touching its
// Workspace.
func (m *agentManager) runBackground(fn func()) {
	if fn == nil {
		return
	}
	m.backgroundWork.Add(1)
	go func() {
		defer m.backgroundWork.Done()
		fn()
	}()
}

func (m *agentManager) waitBackground() {
	m.backgroundWork.Wait()
}

func newAgentManager(s *server) *agentManager {
	return &agentManager{
		server:               s,
		resourceControllers:  make(map[resourceControllerKey]*resourceController),
		workspaceBarriers:    make(map[string]*workspaceHandoffBarrier),
		runtimes:             make(map[string]*agentRuntime),
		subscribers:          make(map[string]map[chan agentStreamMessage]bool),
		reconcileWake:        make(chan struct{}, 1),
		now:                  time.Now,
		idleSleepAfter:       defaultResourceIdleSleepAfter,
		activePollInterval:   2 * time.Second,
		stablePollInterval:   10 * time.Second,
		coldAuditInterval:    30 * time.Second,
		mailboxRetryInterval: 10 * time.Second,
		notificationInterval: 30 * time.Second,
		schedulerFallback:    30 * time.Second,
	}
}

func storeAgentUpload(w http.ResponseWriter, r *http.Request, workspacePath, cwd string) {
	r.Body = http.MaxBytesReader(w, r.Body, agentUploadMaxBytes)
	file, header, err := r.FormFile("file")
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, fmt.Errorf("file exceeds the %d MiB upload limit", agentUploadMaxBytes/(1024*1024)), http.StatusRequestEntityTooLarge)
			return
		}
		writeError(w, errors.New("multipart field file is required"), http.StatusBadRequest)
		return
	}
	defer file.Close()

	uploadDir, err := secureAgentUploadDir(workspacePath, cwd)
	if err != nil {
		writeError(w, fmt.Errorf("prepare upload directory: %w", err), http.StatusBadRequest)
		return
	}
	name := safeUploadName(header.Filename)
	destination, storedName, output, err := createUniqueUpload(uploadDir, name)
	if err != nil {
		writeError(w, fmt.Errorf("create upload: %w", err), http.StatusInternalServerError)
		return
	}
	written, copyErr := io.Copy(output, file)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		if copyErr != nil {
			writeError(w, fmt.Errorf("store upload: %w", copyErr), http.StatusInternalServerError)
		} else {
			writeError(w, fmt.Errorf("store upload: %w", closeErr), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, agentUploadResponse{
		Path: path.Join("artifacts", "upload", storedName),
		Name: storedName,
		Size: written,
	})
}

func secureAgentUploadDir(workspacePath, cwd string) (string, error) {
	workspaceAbs, err := filepath.Abs(workspacePath)
	if err != nil {
		return "", err
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	if err := ensurePathInside(workspaceAbs, cwdAbs); err != nil {
		return "", err
	}
	workspaceEval, err := filepath.EvalSymlinks(workspaceAbs)
	if err != nil {
		return "", err
	}
	cwdEval, err := filepath.EvalSymlinks(cwdAbs)
	if err != nil {
		return "", err
	}
	if err := ensurePathInside(workspaceEval, cwdEval); err != nil {
		return "", err
	}
	uploadDir := filepath.Join(cwdAbs, "artifacts", "upload")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return "", err
	}
	uploadEval, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		return "", err
	}
	if err := ensurePathInside(cwdEval, uploadEval); err != nil {
		return "", errors.New("upload directory escapes the agent session")
	}
	return uploadEval, nil
}

func safeUploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return "upload"
	}
	return name
}

func createUniqueUpload(dir, name string) (string, string, *os.File, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	for index := 1; ; index++ {
		candidate := name
		if index > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, index, ext)
		}
		destination := filepath.Join(dir, candidate)
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return destination, candidate, file, nil
		}
		if !os.IsExist(err) {
			return "", "", nil, err
		}
	}
}

func isAgentHubGeneration(record generationRecord) bool {
	return strings.TrimSpace(record.AgentHubSessionID) != "" || strings.TrimSpace(record.SourceExternalID) != ""
}

func generationMatchesResource(record generationRecord, resourceID string) bool {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return true
	}
	if resourceID == "workspace" {
		stored := strings.TrimSpace(record.ResourceID)
		return stored == "" || stored == "workspace"
	}
	return record.ResourceID == resourceID
}

func (m *agentManager) generationCwd(ctx context.Context, workspace serveWorkspace, resourceID, requested string) (string, error) {
	if strings.TrimSpace(requested) != "" {
		return agentCwd(workspace.Path, requested)
	}
	if strings.TrimSpace(resourceID) == "" || strings.TrimSpace(resourceID) == "workspace" {
		return agentCwd(workspace.Path, "")
	}
	return m.resourceDir(ctx, workspace, resourceID)
}

func (m *agentManager) resourceDir(ctx context.Context, workspace serveWorkspace, resourceID string) (string, error) {
	_ = ctx
	resourceID = strings.TrimSpace(resourceID)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return "", err
	}
	if resourceID == app.SchedulerResourceID {
		return safeWorkspacePath(workspace.Path, app.SchedulerResourceID)
	}
	detail, err := puaWorkspace.Resource(resourceID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(detail.Path) == "" {
		return "", fmt.Errorf("resource %s returned an empty path", resourceID)
	}
	dirAbs, err := safeWorkspacePath(workspace.Path, filepath.FromSlash(detail.Path))
	if err != nil {
		return "", err
	}
	return dirAbs, nil
}

func (m *agentManager) registerRuntime(rt *agentRuntime) {
	m.mu.Lock()
	m.runtimes[rt.record.ID] = rt
	m.mu.Unlock()
	m.requestReconcile(reconcileAgentHub)
}

func (m *agentManager) removeRuntime(recordID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runtimes, recordID)
}

func (m *agentManager) resolveApproval(w http.ResponseWriter, r *http.Request, workspaceID, recordID string) {
	_, rt, err := m.workspaceRuntime(workspaceID, recordID)
	if err != nil || rt == nil {
		writeError(w, errors.New("run is not active"), http.StatusBadRequest)
		return
	}
	var req agentApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	rt.mu.Lock()
	sessionID := strings.TrimSpace(rt.record.AgentHubSessionID)
	rt.mu.Unlock()
	if sessionID == "" {
		writeError(w, errors.New("run is not attached to AgentHub"), http.StatusBadRequest)
		return
	}
	m.resolveAgentHubApproval(w, r, rt, req)
}

func (m *agentManager) workspaceRuntime(workspaceID, recordID string) (serveWorkspace, *agentRuntime, error) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		return serveWorkspace{}, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[recordID]
	if rt != nil && rt.workspace.ID != workspaceID {
		return serveWorkspace{}, nil, errors.New("run belongs to another workspace")
	}
	return workspace, rt, nil
}

func (m *agentManager) subscribe(recordID string, ch chan agentStreamMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subscribers[recordID] == nil {
		m.subscribers[recordID] = make(map[chan agentStreamMessage]bool)
	}
	m.subscribers[recordID][ch] = true
}

func (m *agentManager) unsubscribe(recordID string, ch chan agentStreamMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subscribers[recordID], ch)
	close(ch)
}

func (m *agentManager) publishNotice(recordID string, notice puaNotice) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subscribers[recordID] {
		noticeCopy := notice
		select {
		case ch <- agentStreamMessage{Notice: &noticeCopy}:
		default:
		}
	}
}

func (m *agentManager) runtimeByID(recordID string) *agentRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimes[recordID]
}

// handleTurnFinished records the durable terminal event through one idempotent
// path shared by the poller, recovery, and direct action responses.
func (rt *agentRuntime) handleTurnFinished(m *agentManager, session agentHubSession) {
	rt.prepareTurnCompletion(session)
	rt.markTurnCompletionPending()
	rt.recordTurnCompletion(session)
}

func (rt *agentRuntime) markTurnCompletionPending() {
	_, _ = rt.mutateGeneration(func(record *generationRecord) { record.CompletionPending = true })
}

func (rt *agentRuntime) prepareTurnCompletion(session agentHubSession) {
	sessionID := strings.TrimSpace(session.ID)
	if sessionID == "" {
		return
	}
	_, _ = rt.mutateGeneration(func(record *generationRecord) {
		if record.CompletionSessionID == sessionID {
			return
		}
		// This path is entered only after an active -> ready/stopped edge, so
		// inspect the new session from its beginning instead of baselining away
		// the just-finished turn.
		record.CompletionSessionID = sessionID
		record.CompletionCursor = 0
		record.CompletionEventID = 0
		record.CompletionMarker = ""
		record.CompletionState = ""
		record.CompletionHasFinalReply = false
		record.CompletionTurnID = ""
		record.CompletionAt = ""
		record.CompletionPending = false
		record.AgentHubSessionID = sessionID
	})
}

func (rt *agentRuntime) recordTurnCompletion(session agentHubSession) {
	sessionID := strings.TrimSpace(session.ID)
	if sessionID == "" {
		return
	}
	rt.mu.Lock()
	client := rt.agentHub
	record := rt.record
	rt.mu.Unlock()
	if client == nil || strings.TrimSpace(record.AgentHubSessionID) != sessionID {
		return
	}

	// A resumed generation may be attached to a fresh AgentHub session whose event
	// cursor starts at one again. The first observation is a baseline, never a
	// historical notification.
	if record.CompletionSessionID != sessionID {
		_, _ = rt.mutateGeneration(func(record *generationRecord) {
			record.CompletionSessionID = sessionID
			record.CompletionCursor = session.LastEventID
			record.CompletionEventID = 0
			record.CompletionMarker = ""
			record.CompletionState = ""
			record.CompletionHasFinalReply = false
			record.CompletionTurnID = ""
			record.CompletionAt = ""
			record.CompletionPending = false
		})
		return
	}
	if session.LastEventID <= record.CompletionCursor {
		// The session projection already covers the durable event cursor. This
		// keeps terminal/stopped recovery lightweight while still retrying a
		// completion whose cursor advanced before a prior history read failed.
		if record.CompletionPending {
			_, _ = rt.mutateGeneration(func(record *generationRecord) { record.CompletionPending = false })
		}
		return
	}

	cursor := record.CompletionCursor
	history := make([]agentHubEvent, 0)
	latestCursor := cursor
	for {
		frames, durableCursor, err := client.SessionFrames(context.Background(), sessionID, cursor, 500)
		if err != nil {
			// The next poll/reconcile retries from the same durable cursor. A
			// transient history failure must not invent a completion or advance
			// the marker past an unexamined event.
			return
		}
		if durableCursor > latestCursor {
			latestCursor = durableCursor
		}
		previousCursor := cursor
		for _, frame := range frames {
			if frame.Cursor <= cursor {
				continue
			}
			if frame.Cursor != cursor+1 {
				// Do not advance over a cursor gap. AgentHub promises lossless
				// replay; retaining the old cursor lets a later reconcile retry
				// instead of manufacturing a marker from incomplete history.
				return
			}
			cursor = frame.Cursor
			for _, event := range frame.Events {
				history = append(history, semanticAgentHubEvent(event))
			}
		}
		if cursor == previousCursor && cursor < durableCursor {
			// A lossless replay must make progress. Keep the durable cursor
			// unchanged when an upstream response violates that contract so a
			// later poll can retry instead of skipping an unexamined event.
			return
		}
		if len(frames) < 500 || cursor >= durableCursor {
			break
		}
	}
	if latestCursor < cursor {
		latestCursor = cursor
	}
	// The history is applied from the current runtime snapshot so a duplicate
	// poll/reconcile cannot overwrite a newer marker discovered concurrently.
	rt.recordTurnCompletionHistory(session, history, latestCursor)
}

func (rt *agentRuntime) recordTurnCompletionHistory(session agentHubSession, history []agentHubEvent, latestCursor int64) {
	sessionID := strings.TrimSpace(session.ID)
	if sessionID == "" {
		return
	}
	previous := rt.snapshotGeneration().CompletionMarker
	updated, _ := rt.mutateGeneration(func(record *generationRecord) {
		if strings.TrimSpace(record.AgentHubSessionID) != sessionID {
			return
		}
		if record.CompletionSessionID != sessionID {
			record.CompletionSessionID = sessionID
			record.CompletionCursor = 0
			record.CompletionEventID = 0
			record.CompletionMarker = ""
			record.CompletionState = ""
			record.CompletionHasFinalReply = false
			record.CompletionTurnID = ""
			record.CompletionAt = ""
		}
		cursor := record.CompletionCursor
		latestTerminal := agentHubEvent{}
		finalReplyByTurn := make(map[string]bool)
		for _, event := range history {
			if event.ID <= cursor {
				continue
			}
			cursor = event.ID
			if event.Type == "message.assistant.delta" && strings.TrimSpace(event.TurnID) != "" {
				var data struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(event.Data, &data) == nil && strings.TrimSpace(data.Text) != "" {
					finalReplyByTurn[event.TurnID] = true
				}
			}
			if isAgentHubTurnTerminal(event.Type) && event.ID > latestTerminal.ID {
				latestTerminal = event
			}
		}
		if latestCursor > cursor {
			cursor = latestCursor
		}
		record.CompletionCursor = cursor
		if latestTerminal.ID > record.CompletionEventID {
			record.CompletionEventID = latestTerminal.ID
			record.CompletionMarker = sessionID + ":" + strconv.FormatInt(latestTerminal.ID, 10)
			record.CompletionState = strings.TrimPrefix(latestTerminal.Type, "turn.")
			record.CompletionHasFinalReply = finalReplyByTurn[latestTerminal.TurnID]
			record.CompletionTurnID = latestTerminal.TurnID
			record.CompletionAt = latestTerminal.Time
			if session.State == "ready" && !record.IdleSleepStopRequested {
				boundary := generationTime(latestTerminal.Time)
				if boundary.IsZero() {
					boundary = generationTime(record.CompletionAt)
				}
				if boundary.IsZero() {
					boundary = generationTime(session.UpdatedAt)
				}
				if !boundary.IsZero() {
					record.IdleSinceAt = boundary.Format(time.RFC3339Nano)
					record.IdleDeadlineAt = boundary.Add(rt.manager.resourceIdleSleepAfter()).Format(time.RFC3339Nano)
				}
			}
		}
		record.CompletionPending = false
	})
	if updated.CompletionMarker != "" && updated.CompletionMarker != previous && rt.manager != nil {
		rt.manager.scheduleTaskTurnCompletion(rt, updated)
		rt.manager.requestReconcile(reconcileNotifications | reconcileScheduler | reconcileAgentHub)
	}
}

func (rt *agentRuntime) completionHistoryPending(session agentHubSession) bool {
	sessionID := strings.TrimSpace(session.ID)
	if sessionID == "" || session.LastEventID <= 0 {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.record.CompletionPending && rt.record.CompletionSessionID == sessionID && session.LastEventID > rt.record.CompletionCursor
}

func isAgentHubTurnTerminal(eventType string) bool {
	switch eventType {
	case "turn.completed", "turn.failed", "turn.cancelled":
		return true
	default:
		return false
	}
}

// mutateGeneration is the single serialized persistence boundary for an existing
// generation. The in-memory projection is published only after the complete
// generation (including its mailbox) has been atomically replaced on disk;
// a write failure restores the previous in-memory value so retry remains
// possible after the process continues.
func (rt *agentRuntime) mutateGeneration(mutate func(*generationRecord)) (generationRecord, error) {
	return rt.mutateRuntime(func(runtime *agentRuntime) { mutate(&runtime.record) })
}

func (rt *agentRuntime) mutateRuntime(mutate func(*agentRuntime)) (generationRecord, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	previous := cloneGenerationRecord(rt.record)
	previousState := rt.agentHubState
	previousStopRequested := rt.agentHubStopRequested
	previousLifecycleStopInFlight := rt.lifecycleStopInFlight
	mutate(rt)
	updated := cloneGenerationRecord(rt.record)
	if reflect.DeepEqual(previous, updated) {
		return updated, nil
	}
	if err := saveGenerationRecord(rt.workspace.Path, updated); err != nil {
		rt.record = previous
		rt.agentHubState = previousState
		rt.agentHubStopRequested = previousStopRequested
		rt.lifecycleStopInFlight = previousLifecycleStopInFlight
		return previous, err
	}
	return updated, nil
}

func cloneGenerationRecord(record generationRecord) generationRecord {
	cloned := record
	if record.LifecycleReceipt != nil {
		receipt := *record.LifecycleReceipt
		cloned.LifecycleReceipt = &receipt
	}
	if record.StallWatchdog != nil {
		watchdog := *record.StallWatchdog
		cloned.StallWatchdog = &watchdog
	}
	return cloned
}

func isLiveAgentStatus(status string) bool {
	return status == "starting" || status == "running" || status == "waiting_approval" ||
		status == "idle" || status == "stopping" || status == "recovering"
}

func generationHasActiveTurn(record generationRecord) bool {
	return record.Status == "running" || record.Status == "waiting_approval"
}

func (rt *agentRuntime) snapshotGeneration() generationRecord {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.record
}

func agentStreamAfterID(r *http.Request) int64 {
	afterID, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("after")), 10, 64)
	lastEventID, _ := strconv.ParseInt(strings.TrimSpace(r.Header.Get("Last-Event-ID")), 10, 64)
	if lastEventID > afterID {
		return lastEventID
	}
	return afterID
}

func loadGenerationRecords(workspacePath string) ([]generationRecord, error) {
	store, err := openGenerationStore(workspacePath, "")
	if err != nil {
		return nil, err
	}
	storeRecords, err := store.List()
	if err != nil {
		return nil, err
	}
	records, err := fromStoreRecords(storeRecords)
	if err != nil {
		return nil, err
	}
	sortGenerationRecordsNewestFirst(records)
	return records, nil
}

func loadCurrentGenerationRecords(workspacePath string) ([]generationRecord, error) {
	store, err := openGenerationStore(workspacePath, "")
	if err != nil {
		return nil, err
	}
	storeRecords, err := store.ListCurrent()
	if err != nil {
		return nil, err
	}
	records, err := fromStoreRecords(storeRecords)
	if err != nil {
		return nil, err
	}
	sortGenerationRecordsNewestFirst(records)
	return records, nil
}

func saveGenerationRecord(workspacePath string, record generationRecord) error {
	// New in-process callers always create generation-addressed records. Keep
	// hand-built test/compatibility projections usable by deriving an address
	// for projections that predate explicit generation IDs.
	if strings.TrimSpace(record.GenerationID) == "" && strings.TrimSpace(record.SourceExternalID) != "" && strings.TrimSpace(record.ID) != "" && isAgentHubGeneration(record) {
		// Compatibility callers may hand us a projection that predates explicit
		// generation IDs. Derive one from its stable record ID so a later projection
		// update addresses the same current file instead of creating a new owner.
		record.GenerationID = "gen-" + strings.TrimSpace(record.ID)
		if record.Generation == 0 {
			record.Generation = 1
		}
	}
	store, err := openGenerationStore(workspacePath, record.SourceInstanceID)
	if err != nil {
		return err
	}
	storeRecord, err := toStoreRecord(record)
	if err != nil {
		return err
	}
	if storeRecord.Retired {
		return store.SaveRetired(storeRecord, storeRecord.RetireReason)
	}
	return store.SaveCurrent(storeRecord)
}

func sortGenerationRecordsNewestFirst(records []generationRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if normalizedResourceID(records[i].ResourceID) == normalizedResourceID(records[j].ResourceID) &&
			records[i].Generation > 0 && records[j].Generation > 0 && records[i].Generation != records[j].Generation {
			return records[i].Generation > records[j].Generation
		}
		left := generationRecordRecency(records[i])
		right := generationRecordRecency(records[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		leftCreated := generationTime(records[i].CreatedAt)
		rightCreated := generationTime(records[j].CreatedAt)
		if !leftCreated.Equal(rightCreated) {
			return leftCreated.After(rightCreated)
		}
		return records[i].ID > records[j].ID
	})
}

func generationRecordRecency(record generationRecord) time.Time {
	if parsed := generationTime(record.UpdatedAt); !parsed.IsZero() {
		return parsed
	}
	return generationTime(record.CreatedAt)
}

func generationTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func loadGenerationRecord(workspacePath, recordID string) (generationRecord, error) {
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return generationRecord{}, err
	}
	for _, item := range records {
		if item.ID == recordID {
			return item, nil
		}
	}
	return generationRecord{}, fmt.Errorf("run not found: %s", recordID)
}

func ensureAgentDirs(workspacePath string) error {
	return os.MkdirAll(agentRoot(workspacePath), 0o700)
}

func agentRoot(workspacePath string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "runtime")
}

func agentCwd(workspacePath, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return filepath.Abs(workspacePath)
	}
	if filepath.IsAbs(requested) {
		abs, err := filepath.Abs(requested)
		if err != nil {
			return "", err
		}
		if err := ensurePathInside(filepath.Clean(workspacePath), abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	return safeWorkspacePath(workspacePath, requested)
}

func newGenerationRecordID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}

func writePUANoticeSSE(w http.ResponseWriter, notice puaNotice) {
	data, _ := json.Marshal(notice)
	_, _ = fmt.Fprint(w, "event: pua.notice\n")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}
