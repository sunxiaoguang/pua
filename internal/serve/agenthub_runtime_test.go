package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

type runtimeFakeAgentHub struct {
	mu                   sync.Mutex
	sessions             map[string]agentHubSession
	events               map[string][]agentHubEvent
	turns                map[string]map[string]agentHubTurn
	nextSession          int
	abortNextCreate      bool
	rejectIdempotencyKey string
	duplicateSource      bool
	gapAfter             int64
	failEvents           bool
	failGetSessionID     string
	stopAtStopping       bool
	failNextStop         bool
	failNextInterrupt    bool
	failNextResume       bool
	resumeBeforeFailure  bool
	resumeErrorStatus    int
	resumeErrorCode      string
	resumeErrorMessage   string
	resumeUpdatesAt      bool
	failNextMessage      bool
	enforceMessageIDs    bool
	rejectAgentName      string
	extraAgents          []string
	stopHook             func(string)
	resumeHook           func(string)
	messageHook          func(string, agentHubInboundMessage)
	turnHook             func(string, string)
	messageSteers        []bool
	messageRoles         []string
	messageSenders       []*agentHubMessageSender
	messageIDs           []string
	messageInputs        map[string]agentHubInboundMessage
	actions              []string
	resumeEnvironments   []map[string]string
	listCalls            int
	getSessionCalls      int
	stopCalls            int
	eventsAttempts       int
	eventsCalls          int
	streamCalls          int
}

func newRuntimeFakeAgentHub() *runtimeFakeAgentHub {
	return &runtimeFakeAgentHub{
		sessions:      make(map[string]agentHubSession),
		events:        make(map[string][]agentHubEvent),
		turns:         make(map[string]map[string]agentHubTurn),
		messageInputs: make(map[string]agentHubInboundMessage),
	}
}

func (f *runtimeFakeAgentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/agents" && r.Method == http.MethodGet {
		f.mu.Lock()
		rejected := f.rejectAgentName
		extraAgents := append([]string(nil), f.extraAgents...)
		f.mu.Unlock()
		agents := []map[string]any{{"name": "fake-agent", "providerId": "fake", "available": rejected != "fake-agent"}}
		if rejected != "" && rejected != "fake-agent" {
			agents = append(agents, map[string]any{"name": rejected, "providerId": "fake", "available": false, "unavailableReason": "configured AgentHub agent is unavailable"})
		}
		for _, extra := range extraAgents {
			extra = strings.TrimSpace(extra)
			if extra == "" || extra == "fake-agent" || extra == rejected {
				continue
			}
			agents = append(agents, map[string]any{"name": extra, "providerId": "fake", "available": true})
		}
		writeRuntimeFakeJSON(w, map[string]any{
			"providers": []any{map[string]any{"id": "fake", "name": "Fake", "type": "fake", "enabled": true}},
			"agents":    agents, "probes": []any{},
		})
		return
	}
	if r.URL.Path == "/v1/sessions" {
		if r.Method == http.MethodGet {
			f.list(w, r)
			return
		}
		if r.Method == http.MethodPost {
			f.create(w, r)
			return
		}
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "sessions" {
		http.NotFound(w, r)
		return
	}
	id, _ := url.PathUnescape(parts[2])
	if len(parts) == 3 && r.Method == http.MethodGet {
		f.mu.Lock()
		f.getSessionCalls++
		fail := f.failGetSessionID == id
		session, ok := f.sessions[id]
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "runtime_unavailable", "message": "synthetic transient Session read failure", "retryable": true,
			}})
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRuntimeFakeJSON(w, map[string]any{"session": session})
		return
	}
	if len(parts) == 3 && r.Method == http.MethodDelete {
		f.mu.Lock()
		session, ok := f.sessions[id]
		if ok && session.State == "stopped" {
			session.State = "archived"
			f.sessions[id] = session
		}
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRuntimeFakeJSON(w, map[string]any{"session": session})
		return
	}
	if len(parts) == 4 && parts[3] == "events" {
		f.serveEvents(w, r, id)
		return
	}
	if len(parts) == 5 && parts[3] == "event" && r.Method == http.MethodGet {
		eventID, _ := strconv.ParseInt(parts[4], 10, 64)
		f.mu.Lock()
		all := append([]agentHubEvent(nil), f.events[id]...)
		f.mu.Unlock()
		for _, event := range all {
			if event.ID == eventID {
				writeRuntimeFakeJSON(w, map[string]any{"schema": "agenthub.event-detail.v1", "sourceEvent": event, "frame": runtimeFakeSemanticFrame(event)})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{"code": "event_not_found", "message": "event not found"}})
		return
	}
	if len(parts) == 4 && parts[3] == "turns" && r.Method == http.MethodGet {
		f.mu.Lock()
		turnMap := f.turns[id]
		session := f.sessions[id]
		turns := make([]agentHubTurn, 0, len(turnMap))
		for _, turn := range turnMap {
			turns = append(turns, turn)
		}
		f.mu.Unlock()
		sort.Slice(turns, func(i, j int) bool { return turns[i].FirstEventID < turns[j].FirstEventID })
		writeRuntimeFakeJSON(w, map[string]any{
			"turns":         turns,
			"page":          map[string]any{"nextBefore": 0, "hasMoreBefore": false},
			"latestCursor":  session.LastEventID,
			"latestEventId": session.LastEventID,
		})
		return
	}
	if len(parts) == 5 && parts[3] == "turns" && r.Method == http.MethodGet {
		turnID, _ := url.PathUnescape(parts[4])
		f.mu.Lock()
		turn, ok := f.turns[id][turnID]
		turnHook := f.turnHook
		f.mu.Unlock()
		if turnHook != nil {
			turnHook(id, turnID)
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRuntimeFakeJSON(w, map[string]any{"turn": turn, "latestEventId": turn.LastEventID})
		return
	}
	if len(parts) == 4 && parts[3] == "messages" {
		var body agentHubInboundMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		messageHook := f.messageHook
		f.mu.Unlock()
		if messageHook != nil {
			messageHook(id, body)
		}
		f.mu.Lock()
		f.messageSteers = append(f.messageSteers, body.Steer)
		_, presentationRole, presentationSender := puaMessagePresentation(body.Text, body.Role, body.Sender, body.Payload)
		f.messageRoles = append(f.messageRoles, presentationRole)
		f.messageSenders = append(f.messageSenders, presentationSender)
		f.messageIDs = append(f.messageIDs, body.MessageID)
		if f.enforceMessageIDs && body.MessageID != "" {
			if previous, exists := f.messageInputs[body.MessageID]; exists {
				session := f.sessions[id]
				f.mu.Unlock()
				if !reflect.DeepEqual(previous, body) {
					w.WriteHeader(http.StatusConflict)
					writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
						"code": "runtime_operation_failed", "message": "message id conflicts with an existing input",
					}})
					return
				}
				writeRuntimeFakeJSON(w, map[string]any{"session": session})
				return
			}
			f.messageInputs[body.MessageID] = body
		}
		inputData := fakeMessageInputData(body)
		f.appendLocked(id, "message.input", inputData)
		f.appendLocked(id, "turn.started", map[string]any{"text": body.Text})
		session := f.sessions[id]
		session.State = "running"
		f.sessions[id] = session
		fail := f.failNextMessage
		f.failNextMessage = false
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "message_outcome_unknown", "message": "synthetic ambiguous message failure",
			}})
			return
		}
		writeRuntimeFakeJSON(w, map[string]any{"session": session})
		return
	}
	if len(parts) == 5 && parts[3] == "approvals" {
		approvalID, _ := url.PathUnescape(parts[4])
		var body agentHubApprovalReply
		_ = json.NewDecoder(r.Body).Decode(&body)
		answer := body.Decision
		if body.OptionID != "" {
			answer = "option=" + body.OptionID
		}
		if body.Text != "" {
			answer = "text=" + body.Text
		}
		f.mu.Lock()
		f.actions = append(f.actions, "approval:"+approvalID+":"+answer)
		f.appendLocked(id, "approval.resolved", map[string]any{
			"approvalId": approvalID,
			"decision":   body.Decision,
			"optionId":   body.OptionID,
			"text":       body.Text,
		})
		session := f.sessions[id]
		session.State = "running"
		session.PendingApprovalIDs = nil
		f.sessions[id] = session
		f.mu.Unlock()
		writeRuntimeFakeJSON(w, map[string]any{"session": session})
		return
	}
	if len(parts) == 4 && r.Method == http.MethodPost {
		action := parts[3]
		var stopHook func(string)
		var resumeHook func(string)
		var resumeRequest agentHubResumeRequest
		if action == "resume" {
			_ = json.NewDecoder(r.Body).Decode(&resumeRequest)
		}
		f.mu.Lock()
		if action == "resume" {
			resumeHook = f.resumeHook
			f.resumeEnvironments = append(f.resumeEnvironments, resumeRequest.LaunchEnvironment)
			if f.failNextResume {
				f.failNextResume = false
				status := f.resumeErrorStatus
				code := f.resumeErrorCode
				message := f.resumeErrorMessage
				if f.resumeBeforeFailure {
					session := f.sessions[id]
					session.State = "ready"
					f.sessions[id] = session
				}
				f.mu.Unlock()
				if status == 0 {
					status = http.StatusInternalServerError
				}
				if code == "" {
					code = "resume_failed"
				}
				if message == "" {
					message = "synthetic resume failure"
				}
				w.WriteHeader(status)
				writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
					"code": code, "message": message,
				}})
				return
			}
		}
		if action == "interrupt" && f.failNextInterrupt {
			f.failNextInterrupt = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "interrupt_unknown", "message": "synthetic interrupt failure",
			}})
			return
		}
		if action == "stop" {
			f.stopCalls++
			if f.failNextStop {
				f.failNextStop = false
				f.mu.Unlock()
				w.WriteHeader(http.StatusBadGateway)
				writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
					"code": "stop_outcome_unknown", "message": "synthetic ambiguous stop failure",
				}})
				return
			}
		}
		f.actions = append(f.actions, action)
		session := f.sessions[id]
		switch action {
		case "interrupt":
			turnID := strings.TrimSpace(session.CurrentTurnID)
			f.appendLocked(id, "turn.cancelled", map[string]any{"reason": "requested"})
			f.appendLocked(id, "session.state", map[string]any{"state": "ready"})
			if turnID != "" {
				if turn, found := f.turns[id][turnID]; found {
					turn.Status = "cancelled"
					turn.Closed = true
					turn.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
					f.turns[id][turnID] = turn
				}
			}
			session.State = "ready"
			session.CurrentTurnID = ""
		case "stop":
			stopHook = f.stopHook
			f.appendLocked(id, "session.state", map[string]any{"state": "stopping"})
			session.State = "stopping"
			if !f.stopAtStopping {
				f.appendLocked(id, "session.state", map[string]any{"state": "stopped", "reason": "requested"})
				session.State = "stopped"
				session.StopReason = "requested"
			}
		case "resume":
			if len(resumeRequest.LaunchEnvironment) > 0 {
				if session.LaunchEnvironment == nil {
					session.LaunchEnvironment = make(map[string]string, len(resumeRequest.LaunchEnvironment))
				}
				for key, value := range resumeRequest.LaunchEnvironment {
					session.LaunchEnvironment[key] = value
				}
			}
			f.appendLocked(id, "session.state", map[string]any{"state": "starting"})
			f.appendLocked(id, "session.state", map[string]any{"state": "ready"})
			session.State = "ready"
			if f.resumeUpdatesAt {
				session.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
		}
		session.LastEventID = int64(len(f.events[id]))
		f.sessions[id] = session
		f.mu.Unlock()
		if stopHook != nil {
			stopHook(id)
		}
		if resumeHook != nil {
			resumeHook(id)
		}
		writeRuntimeFakeJSON(w, map[string]any{"session": session})
		return
	}
	http.NotFound(w, r)
}

func writeRuntimeFakeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// fakeMessageInputData mirrors the canonical message.input event a real
// AgentHub daemon persists. Schema v2 is opaque; legacy inputs still receive
// AgentHub's historical default role.
func fakeMessageInputData(message agentHubInboundMessage) map[string]any {
	encoded, _ := json.Marshal(message)
	data := make(map[string]any)
	_ = json.Unmarshal(encoded, &data)
	data["steer"] = message.Steer
	if message.SchemaVersion == 0 && message.Role == "" {
		data["role"] = "user"
	}
	return data
}

func (f *runtimeFakeAgentHub) create(w http.ResponseWriter, r *http.Request) {
	var request agentHubCreateSessionRequest
	_ = json.NewDecoder(r.Body).Decode(&request)
	f.mu.Lock()
	if f.rejectIdempotencyKey != "" && request.IdempotencyKey == f.rejectIdempotencyKey {
		f.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
			"code": "idempotency_conflict", "message": "session idempotency key conflicts with an existing session",
		}})
		return
	}
	if f.rejectAgentName != "" && request.AgentName == f.rejectAgentName {
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
			"code": "agent_unavailable", "message": "configured AgentHub agent is unavailable",
		}})
		return
	}
	f.nextSession++
	id := fmt.Sprintf("ses_%d", f.nextSession)
	session := agentHubSession{
		ID: id, Title: request.Title, Cwd: request.Cwd, AgentName: request.AgentName,
		LaunchEnvironment: request.LaunchEnvironment, Source: request.Source, Provider: "fake",
		State: "ready", CreatedAt: time.Now().Format(time.RFC3339), UpdatedAt: time.Now().Format(time.RFC3339),
	}
	f.sessions[id] = session
	f.appendLocked(id, "session.created", session)
	f.appendLocked(id, "session.state", map[string]any{"state": "ready"})
	if request.InitialMessage != nil {
		f.appendLocked(id, "message.input", fakeMessageInputData(*request.InitialMessage))
		f.appendLocked(id, "turn.started", map[string]any{"text": request.InitialMessage.Text})
		session.State = "running"
	}
	session.LastEventID = int64(len(f.events[id]))
	f.sessions[id] = session
	abort := f.abortNextCreate
	f.abortNextCreate = false
	f.mu.Unlock()
	if abort {
		panic(http.ErrAbortHandler)
	}
	w.WriteHeader(http.StatusCreated)
	writeRuntimeFakeJSON(w, map[string]any{"session": session})
}

func (f *runtimeFakeAgentHub) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	var sessions []agentHubSession
	if f.duplicateSource {
		source := &agentHubSource{
			App: r.URL.Query().Get("sourceApp"), InstanceID: r.URL.Query().Get("sourceInstanceId"),
			ExternalID: r.URL.Query().Get("sourceExternalId"),
		}
		sessions = append(sessions,
			agentHubSession{ID: "ses_duplicate_1", Source: source},
			agentHubSession{ID: "ses_duplicate_2", Source: source},
		)
		writeRuntimeFakeJSON(w, map[string]any{"sessions": sessions})
		return
	}
	// Mirror real AgentHub semantics: archived sessions are hidden from the
	// default list and only returned when includeArchived is requested.
	includeArchived := r.URL.Query().Get("includeArchived") == "true" || r.URL.Query().Get("archived") == "true"
	for _, session := range f.sessions {
		if session.State == "archived" && !includeArchived {
			continue
		}
		if sourceMatchesQuery(session.Source, r.URL.Query()) {
			sessions = append(sessions, session)
		}
	}
	writeRuntimeFakeJSON(w, map[string]any{"sessions": sessions})
}

func sourceMatchesQuery(source *agentHubSource, query url.Values) bool {
	if query.Get("sourceApp") == "" && query.Get("sourceInstanceId") == "" && query.Get("sourceExternalId") == "" {
		return true
	}
	return source != nil &&
		(query.Get("sourceApp") == "" || source.App == query.Get("sourceApp")) &&
		(query.Get("sourceInstanceId") == "" || source.InstanceID == query.Get("sourceInstanceId")) &&
		(query.Get("sourceExternalId") == "" || source.ExternalID == query.Get("sourceExternalId"))
}

func (f *runtimeFakeAgentHub) serveEvents(w http.ResponseWriter, r *http.Request, id string) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	f.mu.Lock()
	f.eventsAttempts++
	if f.failEvents {
		f.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
			"code": "events_unavailable", "message": "synthetic events failure",
		}})
		return
	}
	all := append([]agentHubEvent(nil), f.events[id]...)
	gapAfter := f.gapAfter
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || r.URL.Query().Get("stream") == "true" {
		f.streamCalls++
	} else {
		f.eventsCalls++
	}
	f.mu.Unlock()
	var events []agentHubEvent
	for _, event := range all {
		if event.ID <= after {
			continue
		}
		if gapAfter > 0 && after < gapAfter && event.ID == gapAfter {
			continue
		}
		events = append(events, event)
		if len(events) == limit {
			break
		}
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || r.URL.Query().Get("stream") == "true" {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			data, _ := json.Marshal(runtimeFakeSemanticFrame(event))
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, data)
		}
		return
	}
	next := after
	if len(events) > 0 {
		next = events[len(events)-1].ID
	}
	writeRuntimeFakeJSON(w, map[string]any{
		"schema": "agenthub.semantic-events.v1",
		"frames": func() []agentHubSemanticFrame {
			frames := make([]agentHubSemanticFrame, 0, len(events))
			for _, event := range events {
				frames = append(frames, runtimeFakeSemanticFrame(event))
			}
			return frames
		}(),
		"page": map[string]any{
			"after": after, "limit": limit, "nextAfter": next, "hasMore": next < int64(len(all)),
		},
		"latestCursor": len(all),
	})
}

func runtimeFakeSemanticFrame(event agentHubEvent) agentHubSemanticFrame {
	var frame agentHubSemanticFrame
	frame.Schema = "agenthub.semantic-events.v1"
	frame.Cursor = event.ID
	frame.Mode = "replace"
	frame.Source.EventID = event.ID
	frame.Source.Type = event.Type
	frame.Source.SessionID = event.SessionID
	frame.Source.TurnID = event.TurnID
	frame.Source.Time = event.Time
	frame.Events = []agentHubSemanticEvent{{
		ID: fmt.Sprintf("sem_%d_0", event.ID), SourceEventID: event.ID, Type: event.Type,
		Time: event.Time, SessionID: event.SessionID, TurnID: event.TurnID, Data: event.Data,
	}}
	return frame
}

func (f *runtimeFakeAgentHub) appendLocked(sessionID, eventType string, data any) agentHubEvent {
	raw, _ := json.Marshal(data)
	event := agentHubEvent{
		ID: int64(len(f.events[sessionID]) + 1), Time: time.Now().Format(time.RFC3339),
		Type: eventType, SessionID: sessionID, Data: raw,
	}
	f.events[sessionID] = append(f.events[sessionID], event)
	session := f.sessions[sessionID]
	session.LastEventID = event.ID
	session.UpdatedAt = event.Time
	f.sessions[sessionID] = session
	return event
}

func newRuntimeTestManager(t *testing.T, hubURL string) (*agentManager, serveWorkspace, string) {
	t.Helper()
	workspacePath := t.TempDir()
	puaWorkspace, err := app.Initialize(workspacePath, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Runtime test project", "runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Runtime test task", Slug: "runtime-test"}); err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-test", InstanceID: runtimeConfig.InstanceID, Name: "Test", Path: workspacePath}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	configData, _ := json.Marshal(agentHubServeConfig{
		Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace},
		AgentHubEndpoint: hubURL, AgentHubInstanceID: "pua-runtime-test",
		AgentProfiles: []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}},
	})
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &server{config: configPath, addr: "127.0.0.1:4936"}
	manager := newAgentManager(server)
	manager.now = func() time.Time { return time.Date(2026, 8, 1, 0, 0, 20, 0, time.UTC) }
	server.agents = manager
	// Some recovery tests replace the manager to model a daemon restart. Resolve
	// the active manager at cleanup time so its queued controller work finishes
	// before TempDir removal begins.
	t.Cleanup(func() { server.agents.waitBackground() })
	return manager, workspace, configPath
}

func TestResourceGenerationTitleUsesResourceTitleAndGeneration(t *testing.T) {
	_, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")

	tests := []struct {
		resourceID string
		generation int
		want       string
	}{
		{resourceID: "workspace", generation: 2, want: "Test (gen #2)"},
		{resourceID: "project1", generation: 4, want: "Runtime test project (gen #4)"},
		{resourceID: "project1.task1", generation: 10, want: "Runtime test task (gen #10)"},
	}
	for _, test := range tests {
		t.Run(test.resourceID, func(t *testing.T) {
			got, err := resourceGenerationTitle(workspace, test.resourceID, test.generation)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("resource generation title = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCreateResourceGenerationPersistsAgentHubCatalogSnapshot(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents" && r.Method == http.MethodGet {
			writeRuntimeFakeJSON(w, map[string]any{
				"providers": []any{map[string]any{"id": "codex", "name": "Codex Cloud", "type": "cloud", "enabled": true}},
				"agents":    []any{map[string]any{"name": "fake-agent", "providerId": "codex", "options": map[string]string{"model": "gpt-history"}, "available": true}},
				"probes":    []any{},
			})
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.resolveResourceAgent(workspace, "project1.task1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.createResourceGeneration(context.Background(), workspace, "project1.task1", workspace.Path, cfg, client, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if created.AgentHubProviderID != "codex" || created.AgentHubProviderName != "Codex Cloud" || created.AgentHubModel != "gpt-history" {
		t.Fatalf("catalog snapshot was not persisted in the generation: %#v", created)
	}
	persisted, err := loadGenerationRecord(workspace.Path, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	history := historyGeneration(persisted)
	if history.Provider != "Codex Cloud" || history.ProviderID != "codex" || history.Model != "gpt-history" {
		t.Fatalf("history projection lost the immutable catalog snapshot: %#v", history)
	}
}

func startRuntimeTestGeneration(t *testing.T, manager *agentManager, workspace serveWorkspace, body string) (*httptest.ResponseRecorder, generationRecord) {
	t.Helper()
	var request struct {
		ResourceID string `json:"resourceId"`
		Prompt     string `json:"prompt"`
		UserName   string `json:"userName"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	resourceID := normalizedResourceID(request.ResourceID)
	if resourceID == "" {
		resourceID = "workspace"
	}
	role, sender := agentHubMessageProvenance(request.UserName)
	recorder := httptest.NewRecorder()
	message, err := manager.acceptResourceMessage(context.Background(), workspace, resourceID, resourceMessageRequest{
		Text: request.Prompt, Mode: resourceMessageModeSteer, Role: role, Sender: sender,
	})
	if err != nil {
		writeError(recorder, err, resourceErrorStatus(err))
		return recorder, generationRecord{}
	}
	writeJSON(recorder, map[string]any{"messageId": message.ID, "status": message.Status, "resourceId": message.ResourceID})
	records, err := loadGenerationRecords(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if generationMatchesResource(record, resourceID) {
			return recorder, record
		}
	}
	return recorder, generationRecord{}
}

func TestResourceEndTurnCancelsQueuedSteersBeforeNextTurn(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	started, record := startRuntimeTestGeneration(t, manager, workspace, `{"resourceId":"project1.task1","prompt":"long running turn","userName":"Ada"}`)
	if started.Code != http.StatusOK || record.GenerationID == "" {
		t.Fatalf("failed to start resource turn: code=%d body=%s run=%#v", started.Code, started.Body.String(), record)
	}

	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "turn-active"
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	pending, err := acceptMailboxMessage(workspace.Path, "project1.task1", resourceMessageRequest{
		Text: "stale steer", Mode: resourceMessageModeSteer, Role: "user",
	})
	if err != nil || pending.Status != resourceMessageQueued {
		t.Fatalf("failed to stage pending steer: err=%v message=%#v", err, pending)
	}

	endRecorder := httptest.NewRecorder()
	endRequest := httptest.NewRequest(http.MethodPost, "/resources/project1.task1/turn/end?generationId="+record.GenerationID, nil)
	manager.handleResourceEndTurn(endRecorder, endRequest, workspace.ID, "project1.task1")
	if endRecorder.Code != http.StatusOK {
		t.Fatalf("end turn failed: %d %s", endRecorder.Code, endRecorder.Body.String())
	}
	var endResponse interruptGenerationResponse
	if err := json.Unmarshal(endRecorder.Body.Bytes(), &endResponse); err != nil {
		t.Fatal(err)
	}
	if endResponse.Status != "interrupted" || endResponse.PendingSteerPolicy != "cancel" || endResponse.CancelledPendingSteerCount != 1 || endResponse.TaskState != app.TaskStatePaused || endResponse.TaskStateError != "" {
		t.Fatalf("stop policy response mismatch: %#v", endResponse)
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStatePaused || detail.StateNote != "Current Turn ended by user" {
		t.Fatalf("manually stopped Task was not paused: detail=%#v err=%v", detail, err)
	}

	cancelled, found, err := mailboxMessageByID(workspace.Path, pending.ID)
	if err != nil || !found || cancelled.Status != resourceMessageCancelled || cancelled.LastErrorCode != resourceMessageReasonTurnStopped {
		t.Fatalf("pending steer was not durably cancelled: found=%v err=%v message=%#v", found, err, cancelled)
	}
	statusRecorder := httptest.NewRecorder()
	statusRequest := httptest.NewRequest(http.MethodGet, "/resources/project1.task1/status", nil)
	manager.handleResourceStatus(statusRecorder, statusRequest, workspace.ID, "project1.task1")
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("resource status failed after stop: %d %s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status resourceStatusResponse
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Messages.Waiting != 0 || len(status.WaitingMessages) != 0 || status.Messages.Cancelled != 1 {
		t.Fatalf("cancelled steer remained visible as pending: %#v", status)
	}

	fresh, err := manager.acceptResourceMessage(context.Background(), workspace, "project1.task1", resourceMessageRequest{
		Text: "fresh turn only", Mode: resourceMessageModeSteer, Role: "user",
	})
	if err != nil || fresh.Status != resourceMessageDelivered {
		t.Fatalf("fresh message was not delivered after stop: err=%v message=%#v", err, fresh)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, input := range fake.messageInputs {
		if input.MessageID == pending.ID || input.Text == pending.Text {
			t.Fatalf("cancelled steer crossed the AgentHub input boundary: %#v", input)
		}
	}
}

func TestResourceEndTurnFailureDoesNotPauseTask(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	started, record := startRuntimeTestGeneration(t, manager, workspace, `{"resourceId":"project1.task1","prompt":"long running turn"}`)
	if started.Code != http.StatusOK || record.GenerationID == "" {
		t.Fatalf("failed to start resource turn: code=%d body=%s run=%#v", started.Code, started.Body.String(), record)
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "turn-active"
	fake.sessions[session.ID] = session
	fake.failNextInterrupt = true
	fake.mu.Unlock()

	endRecorder := httptest.NewRecorder()
	endRequest := httptest.NewRequest(http.MethodPost, "/resources/project1.task1/turn/end?generationId="+record.GenerationID, nil)
	manager.handleResourceEndTurn(endRecorder, endRequest, workspace.ID, "project1.task1")
	if endRecorder.Code == http.StatusOK {
		t.Fatalf("ambiguous interrupt unexpectedly succeeded: %s", endRecorder.Body.String())
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource("project1.task1")
	if err != nil || detail.State != app.TaskStateInProgress {
		t.Fatalf("failed interrupt changed Task state: detail=%#v err=%v", detail, err)
	}
}

func TestResourceGenerationCreatesLazilyAndRecoversQueuedMessage(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	firstRecorder, first := startRuntimeTestGeneration(t, manager, workspace, `{"resourceId":"project1.task1","title":"Resource chat","prompt":"first","userName":"Ada"}`)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first resource message failed: %d %s", firstRecorder.Code, firstRecorder.Body.String())
	}
	if first.Generation != 1 || first.GenerationID == "" || first.BindingKind != "profile" || first.BindingName != "default" || first.SourceInstanceID == "" {
		t.Fatalf("resource generation metadata mismatch: %#v", first)
	}
	if first.Title != "Runtime test task (gen #1)" {
		t.Fatalf("resource generation title = %q", first.Title)
	}
	secondRecorder, second := startRuntimeTestGeneration(t, manager, workspace, `{"resourceId":"project1.task1","title":"Resource chat","prompt":"second","userName":"Ada"}`)
	if secondRecorder.Code != http.StatusOK || second.ID != first.ID {
		t.Fatalf("running non-steer generation did not use the Workspace mailbox: code=%d run=%#v", secondRecorder.Code, second)
	}
	mailbox, err := loadResourceMailbox(workspace.Path)
	if err != nil || len(mailbox.Messages) != 2 || mailbox.Messages[1].Status != resourceMessageQueued ||
		mailbox.Messages[1].ActualMode != resourceMessageModeEnqueue || mailbox.Messages[1].DowngradeReason != resourceMessageReasonSteerUnsupported {
		t.Fatalf("running non-steer message was not durably queued: mailbox=%#v err=%v", mailbox, err)
	}
	fake.mu.Lock()
	if len(fake.messageIDs) != 1 || fake.messageIDs[0] == "" {
		fake.mu.Unlock()
		t.Fatalf("first resource message lacked a stable id: %#v", fake.messageIDs)
	}
	session := fake.sessions[first.AgentHubSessionID]
	if session.Title != first.Title {
		fake.mu.Unlock()
		t.Fatalf("AgentHub session title = %q, want %q", session.Title, first.Title)
	}
	session.State = "ready"
	session.CurrentTurnID = ""
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	if err := restarted.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recovered resourceMailboxMessage
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recovered, _, err = mailboxMessageByID(workspace.Path, mailbox.Messages[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		if recovered.Status == resourceMessageDelivered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if recovered.Status != resourceMessageDelivered {
		t.Fatalf("restart did not drain queued message: %#v", recovered)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageIDs) != 2 || fake.messageIDs[1] == "" || fake.messageIDs[1] == fake.messageIDs[0] || fake.messageSteers[1] ||
		recovered.ActualMode != resourceMessageModeEnqueue || recovered.DowngradeReason != resourceMessageReasonSteerUnsupported {
		t.Fatalf("queued message delivery metadata mismatch: ids=%#v steers=%#v", fake.messageIDs, fake.messageSteers)
	}
}

func TestResourceMessageRetryKeepsPersistedSteerAfterUnknownOutcomeAndRestart(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.resolveResourceAgent(workspace, "project1.task1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.createResourceGeneration(context.Background(), workspace, "project1.task1", workspace.Path, cfg, client, resolved)
	if err != nil {
		t.Fatal(err)
	}
	rt := manager.runtimeByID(created.ID)
	if rt == nil {
		t.Fatal("resource runtime missing")
	}
	fake.mu.Lock()
	fake.failNextMessage = true
	fake.mu.Unlock()
	if err := rt.enqueueResourceMessage(newResourceMessage("ambiguous", "Ada")); err != nil {
		t.Fatal(err)
	}
	if err := rt.deliverPendingResourceMessages(context.Background(), manager); err == nil {
		fake.mu.Lock()
		ids, steers, fail := append([]string(nil), fake.messageIDs...), append([]bool(nil), fake.messageSteers...), fake.failNextMessage
		fake.mu.Unlock()
		t.Fatalf("expected the synthetic unknown delivery outcome: ids=%#v steers=%#v fail=%v", ids, steers, fail)
	}
	mailbox, err := loadResourceMailbox(workspace.Path)
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("ambiguous delivery was not retained: mailbox=%#v err=%v", mailbox, err)
	}
	pending := mailbox.Messages[0]
	if pending.Status != resourceMessageDelivering || pending.ActualMode != resourceMessageModeEnqueue {
		t.Fatalf("first delivery mode was not durably frozen as enqueue: %#v", pending)
	}

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	if err := restarted.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := mailboxMessageByID(workspace.Path, pending.ID)
	if err != nil || !found || recovered.Status != resourceMessageDelivered {
		t.Fatalf("restart did not accept the stable retry: found=%v err=%v message=%#v", found, err, recovered)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageIDs) != 2 || fake.messageIDs[0] == "" || fake.messageIDs[0] != fake.messageIDs[1] {
		t.Fatalf("retry did not reuse the stable message id: %#v", fake.messageIDs)
	}
	if len(fake.messageSteers) != 2 || fake.messageSteers[0] || fake.messageSteers[1] {
		t.Fatalf("retry changed the canonical steer value: %#v", fake.messageSteers)
	}
}

func TestLegacyResourceMessageRetryRepairsSteerFromCanonicalEvent(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.resolveResourceAgent(workspace, "project1.task1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.createResourceGeneration(context.Background(), workspace, "project1.task1", workspace.Path, cfg, client, resolved)
	if err != nil {
		t.Fatal(err)
	}
	rt := manager.runtimeByID(created.ID)
	if rt == nil {
		t.Fatal("resource runtime missing")
	}
	fake.mu.Lock()
	fake.failNextMessage = true
	fake.mu.Unlock()
	if err := rt.enqueueResourceMessage(newResourceMessage("legacy", "Ada")); err != nil {
		t.Fatal(err)
	}
	if err := rt.deliverPendingResourceMessages(context.Background(), manager); err == nil {
		fake.mu.Lock()
		ids, steers, fail := append([]string(nil), fake.messageIDs...), append([]bool(nil), fake.messageSteers...), fake.failNextMessage
		fake.mu.Unlock()
		t.Fatalf("expected the synthetic unknown delivery outcome: ids=%#v steers=%#v fail=%v", ids, steers, fail)
	}
	mailbox, err := loadResourceMailbox(workspace.Path)
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("ambiguous delivery was not retained: mailbox=%#v err=%v", mailbox, err)
	}
	legacy := mailbox.Messages[0]
	if _, err := updateMailboxMessage(workspace.Path, legacy.ID, func(message *resourceMailboxMessage) {
		message.ActualMode = resourceMessageModeSteer
		message.DowngradeReason = ""
	}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[created.AgentHubSessionID]
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	if err := restarted.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
	if err != nil || !found || recovered.Status != resourceMessageDelivered || recovered.ActualMode != resourceMessageModeEnqueue ||
		recovered.DowngradeReason != resourceMessageReasonRecoveredCanonical {
		t.Fatalf("legacy retry did not recover canonical delivery: found=%v err=%v message=%#v", found, err, recovered)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageIDs) != 2 || fake.messageIDs[0] == "" || fake.messageIDs[0] != fake.messageIDs[1] {
		t.Fatalf("legacy recovery did not preserve the stable message id: ids=%#v steers=%#v", fake.messageIDs, fake.messageSteers)
	}
	if !reflect.DeepEqual(fake.messageSteers, []bool{false, true}) {
		t.Fatalf("legacy recovery did not restore canonical steer: %#v", fake.messageSteers)
	}
}

func TestGenerationMutationSerializesMailboxWithConcurrentStateUpdates(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{ID: "gen-atomic", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1, GenerationID: "gen-atomic", Status: "idle", CreatedAt: now, UpdatedAt: now}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	manager.registerRuntime(rt)
	const messages = 12
	var group sync.WaitGroup
	for index := 0; index < messages; index++ {
		index := index
		group.Add(2)
		go func() {
			defer group.Done()
			if err := rt.enqueueResourceMessage(resourceMailboxMessage{ID: fmt.Sprintf("msg-%03d", index), Text: "queued"}); err != nil {
				t.Errorf("enqueue %d: %v", index, err)
			}
		}()
		go func() {
			defer group.Done()
			if _, err := rt.mutateGeneration(func(record *generationRecord) { record.CompletionCursor++ }); err != nil {
				t.Errorf("state update %d: %v", index, err)
			}
		}()
	}
	group.Wait()
	persisted, err := loadGenerationRecord(workspace.Path, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailbox(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != messages || persisted.CompletionCursor != messages {
		t.Fatalf("serialized runtime lost an update: mailbox=%d cursor=%d", len(mailbox.Messages), persisted.CompletionCursor)
	}
}

func TestGenerationMutationRollsBackStateWhenDiskWriteFails(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{ID: "gen-disk-failure", GenerationID: "gen-run-disk-failure", WorkspaceID: workspace.ID, Generation: 1, Status: "idle", CreatedAt: now, UpdatedAt: now, CompletionCursor: 1}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	runtimeDir := agentRoot(workspace.Path)
	backupDir := runtimeDir + "-backup"
	if err := os.Rename(runtimeDir, backupDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeDir, []byte("blocks directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.mutateGeneration(func(record *generationRecord) { record.CompletionCursor = 2 }); err == nil {
		t.Fatal("expected generation persistence failure")
	}
	if got := rt.snapshotGeneration().CompletionCursor; got != 1 {
		t.Fatalf("failed write advanced in-memory state: %d", got)
	}
	if err := os.Remove(runtimeDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupDir, runtimeDir); err != nil {
		t.Fatal(err)
	}
	persisted, err := loadGenerationRecord(workspace.Path, record.ID)
	if err != nil || persisted.CompletionCursor != 1 {
		t.Fatalf("failed write changed durable state: %#v, %v", persisted, err)
	}
}

func TestResourceBindingChangeWaitsForTurnBoundaryAndUsesWorkspaceMailbox(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.extraAgents = []string{"replacement-agent"}
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	firstRecorder, first := startRuntimeTestGeneration(t, manager, workspace, `{"resourceId":"project1.task1","prompt":"first"}`)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first message failed: %d %s", firstRecorder.Code, firstRecorder.Body.String())
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	binding := app.AgentBinding{Kind: "agent", Name: "replacement-agent"}
	if _, err := puaWorkspace.SetResourceAgentBinding("project1.task1", binding); err != nil {
		t.Fatal(err)
	}
	if err := manager.resourceBindingChanged(context.Background(), workspace, "project1.task1", binding); err != nil {
		t.Fatal(err)
	}
	queuedRecorder, queued := startRuntimeTestGeneration(t, manager, workspace, `{"resourceId":"project1.task1","prompt":"after binding change"}`)
	mailbox, mailboxErr := loadResourceMailbox(workspace.Path)
	if queuedRecorder.Code != http.StatusOK || queued.ID != first.ID || !queued.ReplacementPending ||
		mailboxErr != nil || len(mailbox.Messages) != 2 || mailbox.Messages[1].Status != resourceMessageQueued ||
		mailbox.Messages[1].DowngradeReason != resourceMessageReasonGenerationReplacing {
		t.Fatalf("message crossed the replacement boundary early: code=%d run=%#v", queuedRecorder.Code, queued)
	}
	replacementMessageID := mailbox.Messages[1].ID
	fake.mu.Lock()
	oldSession := fake.sessions[first.AgentHubSessionID]
	oldSession.State = "ready"
	fake.sessions[oldSession.ID] = oldSession
	fake.mu.Unlock()
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var records []generationRecord
	var replacementMessage resourceMailboxMessage
	var found bool
	var messageErr error
	for time.Now().Before(deadline) {
		records, err = loadGenerationRecords(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		replacementMessage, found, messageErr = mailboxMessageByID(workspace.Path, replacementMessageID)
		if messageErr == nil && found && replacementMessage.Status == resourceMessageDelivered && len(records) >= 2 && records[0].Generation == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(records) < 2 || records[0].Generation != 2 || records[0].AgentHubAgentName != "replacement-agent" {
		t.Fatalf("replacement generation mismatch: %#v", records)
	}
	if records[1].Status != "stopped" {
		t.Fatalf("old generation status mismatch: %#v", records[1])
	}
	if replacementMessage.ActualMode != resourceMessageModeEnqueue ||
		replacementMessage.DowngradeReason != resourceMessageReasonGenerationReplacing {
		t.Fatalf("replacement downgrade decision drifted: %#v", replacementMessage)
	}
	late := sendRuntimeAgentInput(t, manager, workspace, "project1.task1", `{"text":"late old-run input"}`)
	var lateResult resourceMessageResponse
	if err := json.Unmarshal(late.Body.Bytes(), &lateResult); err != nil {
		t.Fatal(err)
	}
	if late.Code != http.StatusOK || lateResult.ResourceID != "project1.task1" || lateResult.Status != "accepted" {
		t.Fatalf("late old-generation input was not redirected to the resource mailbox (runs=%#v): %d %s", records, late.Code, late.Body.String())
	}
	redirected, err := loadGenerationRecords(workspace.Path)
	mailbox, mailboxErr = loadResourceMailbox(workspace.Path)
	if err != nil || mailboxErr != nil || len(redirected) < 2 || len(mailbox.Messages) != 3 || mailbox.Messages[2].Status != resourceMessageQueued {
		t.Fatalf("redirected queue mismatch: runs=%#v err=%v", redirected, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions[first.AgentHubSessionID].State != "archived" || len(fake.messageIDs) != 2 {
		t.Fatalf("replacement lifecycle mismatch: old=%#v messageIDs=%#v", fake.sessions[first.AgentHubSessionID], fake.messageIDs)
	}
}

func sendRuntimeAgentInput(t *testing.T, manager *agentManager, workspace serveWorkspace, resourceID, body string) *httptest.ResponseRecorder {
	t.Helper()
	var request struct {
		Text     string `json:"text"`
		UserName string `json:"userName"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	role, sender := agentHubMessageProvenance(request.UserName)
	recorder := httptest.NewRecorder()
	message, sendErr := manager.acceptResourceMessage(context.Background(), workspace, normalizedResourceID(resourceID), resourceMessageRequest{
		Text: request.Text, Mode: resourceMessageModeSteer, Role: role, Sender: sender,
	})
	if sendErr != nil {
		writeError(recorder, sendErr, resourceErrorStatus(sendErr))
		return recorder
	}
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspace.ID, message.ID)
	writeJSON(recorder, map[string]any{
		"status": "accepted", "deliveryStatus": response.Status,
		"messageId": response.MessageID, "resourceId": response.ResourceID,
		"requestedMode": response.RequestedMode, "actualMode": response.ActualMode,
		"downgradeReason": response.DowngradeReason, "reference": response.Reference,
		"generationId": response.GenerationID, "agentHubSessionId": response.AgentHubSessionID,
		"turnId": response.TurnID, "lastError": response.LastError, "lastErrorCode": response.LastErrorCode,
	})
	return recorder
}

func fakeEventText(event agentHubEvent) string {
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return ""
	}
	text, _ := data["text"].(string)
	return text
}

func fakeEventRole(event agentHubEvent) string {
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return ""
	}
	role, _ := data["role"].(string)
	return role
}

func fakeEventSenderName(event agentHubEvent) string {
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return ""
	}
	sender, _ := data["sender"].(map[string]any)
	name, _ := sender["name"].(string)
	return name
}

func fakeEventMessage(event agentHubEvent) (agentHubInboundMessage, puaMessagePayload, bool) {
	var message agentHubInboundMessage
	if err := json.Unmarshal(event.Data, &message); err != nil {
		return agentHubInboundMessage{}, puaMessagePayload{}, false
	}
	payload, ok := decodePUAMessagePayload(message.Payload)
	return message, payload, ok
}

func TestAgentHubManualMessagesCarryBrowserUserProvenance(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	recorder, detail := startRuntimeTestGeneration(t, manager, workspace, `{"agentName":"fake-agent","resourceId":"project1.task1","prompt":"hello","userName":"  Ada Lovelace  "}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start failed: %d %s", recorder.Code, recorder.Body.String())
	}

	fake.mu.Lock()
	initialEvents := append([]agentHubEvent(nil), fake.events[detail.AgentHubSessionID]...)
	fake.mu.Unlock()
	var initial agentHubEvent
	var initialMessage agentHubInboundMessage
	var initialPayload puaMessagePayload
	for _, event := range initialEvents {
		message, payload, ok := fakeEventMessage(event)
		if event.Type == "message.input" && ok && payload.Text == "hello" {
			initial = event
			initialMessage = message
			initialPayload = payload
			break
		}
	}
	if initial.Type == "" || initialMessage.SchemaVersion != agentHubOpaqueMessageSchema ||
		initialMessage.Text != "Message from user \"Ada Lovelace\" [sender receives: progress + final reply]:\nhello" || initialPayload.Role != "user" ||
		initialPayload.Sender == nil || initialPayload.Sender.Name != "Ada Lovelace" {
		t.Fatalf("initial opaque message = message %#v payload %#v; events=%#v", initialMessage, initialPayload, initialEvents)
	}
	storedRecords, err := loadGenerationRecords(workspace.Path)
	if err != nil || len(storedRecords) != 1 {
		t.Fatalf("load stored runs: runs=%#v err=%v", storedRecords, err)
	}
	storedJSON, err := json.Marshal(storedRecords[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedJSON), "Ada Lovelace") {
		t.Fatalf("browser-local user name leaked into persisted run: %s", storedJSON)
	}
}

func TestNormalizeAgentHubUserName(t *testing.T) {
	longName := strings.Repeat("名", agentHubUserNameMaxLength+5)
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{name: "missing", want: "User"},
		{name: "blank", in: "  \n ", want: "User"},
		{name: "trim", in: "  Ada Lovelace  ", want: "Ada Lovelace"},
		{name: "unicode limit", in: longName, want: strings.Repeat("名", agentHubUserNameMaxLength)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeAgentHubUserName(test.in); got != test.want {
				t.Fatalf("normalizeAgentHubUserName(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestNormalizeAgentHubApprovalReply(t *testing.T) {
	tests := []struct {
		name    string
		request agentApprovalRequest
		want    agentHubApprovalReply
		wantErr bool
	}{
		{name: "decision", request: agentApprovalRequest{Decision: "accept"}, want: agentHubApprovalReply{Decision: "accept"}},
		{name: "option", request: agentApprovalRequest{OptionID: " option-a "}, want: agentHubApprovalReply{OptionID: "option-a"}},
		{name: "text", request: agentApprovalRequest{Text: " another answer "}, want: agentHubApprovalReply{Text: "another answer"}},
		{name: "missing", request: agentApprovalRequest{}, wantErr: true},
		{name: "combined", request: agentApprovalRequest{Decision: "accept", OptionID: "option-a"}, wantErr: true},
		{name: "unknown decision", request: agentApprovalRequest{Decision: "yes"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeAgentHubApprovalReply(test.request)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %#v", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("got %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
}

func waitForRuntimeTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not reached before timeout")
}
