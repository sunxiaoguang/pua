package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/schedulerapi"
)

type schedulerV1TestDefinition struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Condition   string `json:"condition"`
	Target      string `json:"target"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func migrateSchedulerV1ForTest(t *testing.T, workspace *app.Workspace, definitions ...schedulerV1TestDefinition) []app.Schedule {
	t.Helper()
	legacy := struct {
		SchemaVersion       int                         `json:"schemaVersion"`
		AgentBinding        map[string]string           `json:"agentBinding"`
		WakeIntervalMinutes int                         `json:"wakeIntervalMinutes"`
		Schedules           []schedulerV1TestDefinition `json:"schedules"`
	}{
		SchemaVersion:       1,
		AgentBinding:        map[string]string{"kind": "profile", "name": "default"},
		WakeIntervalMinutes: 30,
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
	if config.SchemaVersion != 2 || len(config.Schedules) != len(definitions) {
		t.Fatalf("migrated Scheduler config = %#v", config)
	}
	return config.Schedules
}

func schedulerTestScheduleByID(t *testing.T, workspace *app.Workspace, id string) app.Schedule {
	t.Helper()
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	for _, schedule := range config.Schedules {
		if schedule.ID == id {
			return schedule
		}
	}
	t.Fatalf("schedule %q not found in %#v", id, config.Schedules)
	return app.Schedule{}
}

func rewriteSchedulerTestOneTimeDeadline(t *testing.T, workspace *app.Workspace, id string, at time.Time) {
	t.Helper()
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index := range config.Schedules {
		schedule := &config.Schedules[index]
		if schedule.ID != id {
			continue
		}
		if schedule.Trigger == nil || schedule.Trigger.Type != app.ScheduleTriggerAt {
			t.Fatalf("schedule %q is not one-time: %#v", id, schedule)
		}
		schedule.Trigger.At = at.Format(time.RFC3339Nano)
		found = true
		break
	}
	if !found {
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
}

func rewriteSchedulerTestIntervalBoundary(t *testing.T, workspace *app.Workspace, id string, anchor, updatedAt time.Time) {
	t.Helper()
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index := range config.Schedules {
		schedule := &config.Schedules[index]
		if schedule.ID != id {
			continue
		}
		if schedule.Trigger == nil || schedule.Trigger.Type != app.ScheduleTriggerInterval {
			t.Fatalf("schedule %q is not an interval: %#v", id, schedule)
		}
		schedule.Trigger.AnchorAt = anchor.Format(time.RFC3339Nano)
		if !updatedAt.IsZero() {
			schedule.UpdatedAt = updatedAt.Format(time.RFC3339Nano)
		}
		found = true
		break
	}
	if !found {
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
}

func TestSchedulerHTTPAPIRoutesNaturalLanguageAndNativeChanges(t *testing.T) {
	root := t.TempDir()
	puaWorkspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler", Name: "Scheduler", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	t.Cleanup(s.agents.waitBackground)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set(workspaceUserHeader, "Ada")
		s.handleWorkspace(recorder, request)
		return recorder
	}

	natural := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler", `{"description":"Review","condition":"tomorrow morning","target":"workspace"}`)
	if natural.Code != http.StatusAccepted {
		t.Fatalf("natural-language request = %d %s", natural.Code, natural.Body.String())
	}
	mailbox, err := loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].Role != "user" || mailbox.Messages[0].Sender == nil || mailbox.Messages[0].Sender.Name != "Ada" || !strings.Contains(mailbox.Messages[0].Text, "IANA timezone") {
		t.Fatalf("Scheduler compilation mailbox = %#v, %v", mailbox.Messages, err)
	}
	revisionedCreate := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler", `{"expectedRevision":"1","description":"Review","condition":"tomorrow morning","target":"workspace"}`)
	if revisionedCreate.Code != http.StatusBadRequest || !strings.Contains(revisionedCreate.Body.String(), "expectedRevision is only valid") {
		t.Fatalf("natural-language create with revision = %d %s", revisionedCreate.Code, revisionedCreate.Body.String())
	}
	selfTargetNatural := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler", `{"description":"Review","condition":"tomorrow morning","target":"scheduler"}`)
	if selfTargetNatural.Code != http.StatusBadRequest || !strings.Contains(selfTargetNatural.Body.String(), `"code":"schedule_target_invalid"`) || !strings.Contains(selfTargetNatural.Body.String(), app.ErrScheduleTargetScheduler.Error()) {
		t.Fatalf("natural-language self-target = %d %s", selfTargetNatural.Code, selfTargetNatural.Body.String())
	}
	mailbox, err = loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("rejected self-target mutated Scheduler mailbox: %#v, %v", mailbox.Messages, err)
	}

	at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	invalid := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"restart"}`)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `unsupported Scheduler change \"restart\"`) {
		t.Fatalf("invalid native change = %d %s", invalid.Code, invalid.Body.String())
	}
	selfTargetCreate := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"create","description":"Review","condition":"tomorrow at 09:00 UTC","target":"scheduler","trigger":{"type":"at","at":"`+at+`"}}`)
	if selfTargetCreate.Code != http.StatusBadRequest || !strings.Contains(selfTargetCreate.Body.String(), `"code":"schedule_target_invalid"`) || !strings.Contains(selfTargetCreate.Body.String(), app.ErrScheduleTargetScheduler.Error()) {
		t.Fatalf("native self-target create = %d %s", selfTargetCreate.Code, selfTargetCreate.Body.String())
	}
	unsafeIntervalCreate := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"create","description":"Review","condition":"every minute","target":"workspace","trigger":{"type":"interval","everySeconds":60,"anchorAt":"9999-12-31T23:59:59Z"}}`)
	if unsafeIntervalCreate.Code != http.StatusBadRequest || !strings.Contains(unsafeIntervalCreate.Body.String(), `"code":"schedule_occurrence_out_of_range"`) {
		t.Fatalf("unpersistable interval create = %d %s", unsafeIntervalCreate.Code, unsafeIntervalCreate.Body.String())
	}
	createdResponse := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"create","description":"Review","condition":"tomorrow at 09:00 UTC","target":"workspace","trigger":{"type":"at","at":"`+at+`"}}`)
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("native create = %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var createdResponseSchedule schedulerapi.Schedule
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &createdResponseSchedule); err != nil || createdResponseSchedule.Revision != "1" || createdResponseSchedule.Trigger == nil {
		t.Fatalf("created schedule = %#v, %v", createdResponseSchedule, err)
	}
	created := schedulerTestScheduleByID(t, puaWorkspace, createdResponseSchedule.ID)
	selfTargetUpdateAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	selfTargetUpdate := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"update","id":"`+created.ID+`","expectedRevision":"1","target":"scheduler","trigger":{"type":"at","at":"`+selfTargetUpdateAt+`"}}`)
	if selfTargetUpdate.Code != http.StatusBadRequest || !strings.Contains(selfTargetUpdate.Body.String(), `"code":"schedule_target_invalid"`) {
		t.Fatalf("native self-target update = %d %s", selfTargetUpdate.Code, selfTargetUpdate.Body.String())
	}
	if got := schedulerTestScheduleByID(t, puaWorkspace, created.ID); !reflect.DeepEqual(got, created) {
		t.Fatalf("rejected self-target update changed schedule: got %#v, want %#v", got, created)
	}
	for _, invalidUpdate := range []string{
		`{"description":"Review later","condition":"next week","target":"workspace"}`,
		`{"expectedRevision":"0","description":"Review later","condition":"next week","target":"workspace"}`,
	} {
		rejected := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, invalidUpdate)
		if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "expectedRevision") {
			t.Fatalf("natural-language update without revision = %d %s", rejected.Code, rejected.Body.String())
		}
	}
	mailbox, err = loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 1 {
		t.Fatalf("rejected revision mutated Scheduler mailbox: %#v, %v", mailbox.Messages, err)
	}
	naturalUpdate := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, `{"expectedRevision":"1","description":"Review later","condition":"next week","target":"workspace"}`)
	if naturalUpdate.Code != http.StatusAccepted {
		t.Fatalf("natural-language update = %d %s", naturalUpdate.Code, naturalUpdate.Body.String())
	}
	mailbox, err = loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 2 || mailbox.Messages[1].Sender == nil || mailbox.Messages[1].Sender.Name != "Ada" || !strings.Contains(mailbox.Messages[1].Text, "Please update a native schedule for \""+created.ID+"\"") || !strings.Contains(mailbox.Messages[1].Text, "Pass exactly `--revision=1`") {
		t.Fatalf("Scheduler update compilation mailbox = %#v, %v", mailbox.Messages, err)
	}
	conflict := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"update","id":"`+created.ID+`","expectedRevision":"9","description":"Changed","trigger":{"type":"at","at":"`+at+`"}}`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "schedule_revision_conflict") {
		t.Fatalf("revision conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	paused := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID+"/pause", "")
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), `"state": "paused"`) {
		t.Fatalf("pause = %d %s", paused.Code, paused.Body.String())
	}
	read := request(http.MethodGet, "/api/workspaces/workspace-scheduler/scheduler", "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"effectiveState": "paused"`) || strings.Contains(read.Body.String(), "wakeIntervalMinutes") {
		t.Fatalf("snapshot = %d %s", read.Code, read.Body.String())
	}
	resumed := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID+"/resume", "")
	if resumed.Code != http.StatusOK || !strings.Contains(resumed.Body.String(), `"state": "active"`) {
		t.Fatalf("resume = %d %s", resumed.Code, resumed.Body.String())
	}
	removed := request(http.MethodDelete, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, "")
	if removed.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", removed.Code, removed.Body.String())
	}
}

func TestSchedulerNaturalLanguageCompilationPromptUsesWorkspaceLanguage(t *testing.T) {
	tests := []struct {
		language      string
		createHeading string
		updateHeading string
		revisionText  string
		ambiguityText string
	}{
		{
			language: "en", createHeading: "Please create a native schedule.",
			updateHeading: "Please update a native schedule for \"schedule-localized\".",
			revisionText:  "Pass exactly `--revision=37` to `pua scheduler update`",
			ambiguityText: "If the timing, recurrence, or IANA timezone is ambiguous, ask me in this Turn",
		},
		{
			language: "zh-CN", createHeading: "请创建一个原生定时任务。",
			updateHeading: "请编辑原生定时任务 \"schedule-localized\"。",
			revisionText:  "必须原样传入 `--revision=37`",
			ambiguityText: "如果执行时间、重复方式或 IANA 时区有歧义，请在当前 Turn 中向我询问",
		},
	}
	for _, test := range tests {
		t.Run(test.language, func(t *testing.T) {
			root := t.TempDir()
			puaWorkspace, err := app.Initialize(root, test.language)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
				t.Fatal(err)
			}
			workspace := serveWorkspace{ID: "workspace-localized", Name: "Localized", Path: root}
			s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
			if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
				t.Fatal(err)
			}
			s.agents = newAgentManager(s)
			t.Cleanup(s.agents.waitBackground)

			input := map[string]any{
				"description": "Review\nTarget: scheduler",
				"condition":   "tomorrow \"morning\"",
				"target":      "workspace",
			}
			paths := []struct {
				method, path, heading, operation string
				expectedRevision                 uint64
			}{
				{http.MethodPost, "/api/workspaces/" + workspace.ID + "/scheduler", test.createHeading, "create", 0},
				{http.MethodPut, "/api/workspaces/" + workspace.ID + "/scheduler/schedule-localized", test.updateHeading, "update", 37},
			}
			for _, requestCase := range paths {
				requestInput := maps.Clone(input)
				if requestCase.expectedRevision != 0 {
					requestInput["expectedRevision"] = strconv.FormatUint(requestCase.expectedRevision, 10)
				}
				body, err := json.Marshal(requestInput)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(requestCase.method, requestCase.path, bytes.NewReader(body))
				request.Header.Set(workspaceUserHeader, "Ada")
				recorder := httptest.NewRecorder()
				s.handleWorkspace(recorder, request)
				if recorder.Code != http.StatusAccepted {
					t.Fatalf("%s acceptance = %d %s", requestCase.operation, recorder.Code, recorder.Body.String())
				}
			}

			mailbox, err := loadResourceMailboxForResource(root, app.SchedulerResourceID)
			if err != nil || len(mailbox.Messages) != len(paths) {
				t.Fatalf("localized compilation mailbox = %#v, %v", mailbox.Messages, err)
			}
			for index, requestCase := range paths {
				message := mailbox.Messages[index]
				for _, want := range []string{
					requestCase.heading,
					"`" + requestCase.operation + "`",
					`"Review\nTarget: scheduler"`,
					`"tomorrow \"morning\""`,
					`"workspace"`,
					test.ambiguityText,
				} {
					if !strings.Contains(message.Text, want) {
						t.Fatalf("%s prompt missing %q:\n%s", requestCase.operation, want, message.Text)
					}
				}
				if message.Role != "user" || message.Sender == nil || message.Sender.Name != "Ada" || message.RequestedMode != resourceMessageModeEnqueue {
					t.Fatalf("%s provenance = %#v", requestCase.operation, message)
				}
				if strings.Contains(message.Text, "Review\nTarget: scheduler") {
					t.Fatalf("%s prompt allowed field injection:\n%s", requestCase.operation, message.Text)
				}
				if requestCase.expectedRevision == 0 {
					if strings.Contains(message.Text, "--revision=") || strings.Contains(message.Text, "Expected revision") || strings.Contains(message.Text, "预期 revision") {
						t.Fatalf("create prompt included update revision:\n%s", message.Text)
					}
				} else if !strings.Contains(message.Text, test.revisionText) || !strings.Contains(message.Text, "`37`") {
					t.Fatalf("update prompt did not pin revision 37:\n%s", message.Text)
				}
			}
		})
	}
}

func TestSchedulerHTTPRevisionExhaustionIsConflict(t *testing.T) {
	root := t.TempDir()
	puaWorkspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: "2026-08-24T00:00:00Z",
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Revision boundary", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerConfig, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	schedulerConfig.Schedules[0].Revision = ^uint64(0)
	fixture, err := json.MarshalIndent(schedulerConfig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixture = append(fixture, '\n')
	path := filepath.Join(root, "scheduler", "scheduler.json")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	workspace := serveWorkspace{ID: "workspace-scheduler-revision", Name: "Scheduler Revision", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	t.Cleanup(s.agents.waitBackground)
	description := "Cannot wrap"
	body, err := json.Marshal(schedulerChangeRequest{
		Operation: string(app.ScheduleChangeUpdate), ID: created.ID, ExpectedRevision: schedulerExpectedRevision(schedulerapi.RevisionFromUint64(^uint64(0))),
		Description: &description, Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost, "/api/workspaces/workspace-scheduler-revision/scheduler/changes", bytes.NewReader(body),
	)
	s.handleWorkspace(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("revision exhaustion status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "schedule_revision_exhausted" || !strings.Contains(response["error"], app.ErrScheduleRevisionExhausted.Error()) {
		t.Fatalf("revision exhaustion response = %#v", response)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatalf("HTTP revision exhaustion changed scheduler.json:\nbefore=%s\nafter=%s", fixture, after)
	}
	loaded, err := puaWorkspace.Scheduler()
	if err != nil || len(loaded.Schedules) != 1 || loaded.Schedules[0].Revision != ^uint64(0) {
		t.Fatalf("Scheduler after HTTP revision exhaustion = %#v, %v", loaded.Schedules, err)
	}
}

func TestSchedulerHTTPExpectedRevisionErrorsNameField(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler-revision-errors", Name: "Scheduler Revision Errors", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	t.Cleanup(s.agents.waitBackground)

	const (
		scheduleID             = "schedule-111111111111111111111111"
		invalidRevisionMessage = "expectedRevision: schedule revision must be a canonical decimal string between 1 and 18446744073709551615"
		missingRevisionMessage = "expectedRevision is required for schedule updates"
		extraRevisionMessage   = "expectedRevision is only valid for schedule updates"
	)
	for _, test := range []struct {
		name, method, path, body, wantError string
	}{
		{name: "structured zero", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + scheduleID + `","expectedRevision":"0"}`, wantError: invalidRevisionMessage},
		{name: "structured number", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + scheduleID + `","expectedRevision":1}`, wantError: invalidRevisionMessage},
		{name: "structured noncanonical", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + scheduleID + `","expectedRevision":"01"}`, wantError: invalidRevisionMessage},
		{name: "structured overflow", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + scheduleID + `","expectedRevision":"18446744073709551616"}`, wantError: invalidRevisionMessage},
		{name: "structured missing update revision", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + scheduleID + `"}`, wantError: missingRevisionMessage},
		{name: "structured create extra revision", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"create","expectedRevision":"1"}`, wantError: extraRevisionMessage},
		{name: "natural zero", method: http.MethodPut, path: "/scheduler/" + scheduleID, body: `{"expectedRevision":"0","description":"Review","condition":"later","target":"workspace"}`, wantError: invalidRevisionMessage},
		{name: "natural number", method: http.MethodPut, path: "/scheduler/" + scheduleID, body: `{"expectedRevision":1,"description":"Review","condition":"later","target":"workspace"}`, wantError: invalidRevisionMessage},
		{name: "natural noncanonical", method: http.MethodPut, path: "/scheduler/" + scheduleID, body: `{"expectedRevision":"+1","description":"Review","condition":"later","target":"workspace"}`, wantError: invalidRevisionMessage},
		{name: "natural overflow", method: http.MethodPut, path: "/scheduler/" + scheduleID, body: `{"expectedRevision":"18446744073709551616","description":"Review","condition":"later","target":"workspace"}`, wantError: invalidRevisionMessage},
		{name: "natural missing update revision", method: http.MethodPut, path: "/scheduler/" + scheduleID, body: `{"description":"Review","condition":"later","target":"workspace"}`, wantError: missingRevisionMessage},
		{name: "natural create extra revision", method: http.MethodPost, path: "/scheduler", body: `{"expectedRevision":"1","description":"Review","condition":"later","target":"workspace"}`, wantError: extraRevisionMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, "/api/workspaces/"+workspace.ID+test.path, strings.NewReader(test.body))
			s.handleWorkspace(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			want := map[string]any{"error": test.wantError}
			if !reflect.DeepEqual(payload, want) {
				t.Fatalf("error response = %#v, want %#v", payload, want)
			}
		})
	}
}

func TestSchedulerHTTPRevisionTransportPreservesUint64(t *testing.T) {
	root := t.TempDir()
	puaWorkspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
		t.Fatal(err)
	}
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: "2026-08-24T00:00:00Z",
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Revision boundary", Condition: "every minute", Target: "workspace", Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	setRevision := func(revision uint64) {
		t.Helper()
		config, readErr := puaWorkspace.Scheduler()
		if readErr != nil {
			t.Fatal(readErr)
		}
		config.Schedules[0].Revision = revision
		data, marshalErr := json.MarshalIndent(config, "", "  ")
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(filepath.Join(root, "scheduler", "scheduler.json"), append(data, '\n'), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	workspace := serveWorkspace{ID: "workspace-scheduler-transport", Name: "Scheduler Revision", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	t.Cleanup(s.agents.waitBackground)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest(method, path, strings.NewReader(body))
		httpRequest.Header.Set(workspaceUserHeader, "Ada")
		s.handleWorkspace(recorder, httpRequest)
		return recorder
	}

	const maxSafeRevision = uint64(1<<53 - 1)
	const firstUnsafeRevision = "9007199254740992"
	const maximumRevision = "18446744073709551615"
	setRevision(maxSafeRevision)
	updated := request(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler/changes",
		`{"operation":"update","id":"`+created.ID+`","expectedRevision":"9007199254740991","description":"Exact revision","trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"revision": "`+firstUnsafeRevision+`"`) {
		t.Fatalf("unsafe revision update = %d %s", updated.Code, updated.Body.String())
	}

	for name, path := range map[string]string{
		"snapshot":        "/api/workspaces/" + workspace.ID + "/scheduler",
		"resource detail": "/api/workspaces/" + workspace.ID + "/resources/scheduler",
	} {
		t.Run(name, func(t *testing.T) {
			response := request(http.MethodGet, path, "")
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision": "`+firstUnsafeRevision+`"`) || strings.Contains(response.Body.String(), `"revision": 9007199254740992`) {
				t.Fatalf("%s revision response = %d %s", name, response.Code, response.Body.String())
			}
		})
	}

	natural := request(http.MethodPut, "/api/workspaces/"+workspace.ID+"/scheduler/"+created.ID,
		`{"expectedRevision":"`+firstUnsafeRevision+`","description":"Review later","condition":"next week","target":"workspace"}`)
	if natural.Code != http.StatusAccepted {
		t.Fatalf("natural update = %d %s", natural.Code, natural.Body.String())
	}
	mailbox, err := loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 1 || !strings.Contains(mailbox.Messages[0].Text, "Pass exactly `--revision="+firstUnsafeRevision+"`") {
		t.Fatalf("unsafe revision compilation prompt = %#v, %v", mailbox.Messages, err)
	}

	conflict := request(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler/changes",
		`{"operation":"update","id":"`+created.ID+`","expectedRevision":"9007199254740991","description":"Stale","trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "revision is "+firstUnsafeRevision+", expected 9007199254740991") {
		t.Fatalf("exact revision conflict = %d %s", conflict.Code, conflict.Body.String())
	}

	setRevision(^uint64(0))
	maximumPrompt := request(http.MethodPut, "/api/workspaces/"+workspace.ID+"/scheduler/"+created.ID,
		`{"expectedRevision":"`+maximumRevision+`","description":"Maximum review","condition":"next week","target":"workspace"}`)
	if maximumPrompt.Code != http.StatusAccepted {
		t.Fatalf("maximum revision natural update = %d %s", maximumPrompt.Code, maximumPrompt.Body.String())
	}
	mailbox, err = loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 2 || !strings.Contains(mailbox.Messages[1].Text, "Pass exactly `--revision="+maximumRevision+"`") {
		t.Fatalf("maximum revision compilation prompt = %#v, %v", mailbox.Messages, err)
	}
	maximumConflict := request(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler/changes",
		`{"operation":"update","id":"`+created.ID+`","expectedRevision":"`+firstUnsafeRevision+`","description":"Stale maximum","trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`)
	if maximumConflict.Code != http.StatusConflict || !strings.Contains(maximumConflict.Body.String(), "revision is "+maximumRevision+", expected "+firstUnsafeRevision) {
		t.Fatalf("maximum revision conflict = %d %s", maximumConflict.Code, maximumConflict.Body.String())
	}
	exhausted := request(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler/changes",
		`{"operation":"update","id":"`+created.ID+`","expectedRevision":"`+maximumRevision+`","description":"Cannot wrap","trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`)
	if exhausted.Code != http.StatusConflict || !strings.Contains(exhausted.Body.String(), `"code":"schedule_revision_exhausted"`) {
		t.Fatalf("maximum revision exhaustion = %d %s", exhausted.Code, exhausted.Body.String())
	}

	for _, test := range []struct {
		name, method, path, body string
	}{
		{name: "structured number", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + created.ID + `","expectedRevision":9007199254740992,"trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`},
		{name: "structured leading zero", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + created.ID + `","expectedRevision":"01","trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`},
		{name: "structured overflow", method: http.MethodPost, path: "/scheduler/changes", body: `{"operation":"update","id":"` + created.ID + `","expectedRevision":"18446744073709551616","trigger":{"type":"interval","everySeconds":60,"anchorAt":"2026-08-24T00:00:00Z"}}`},
		{name: "natural number", method: http.MethodPut, path: "/scheduler/" + created.ID, body: `{"expectedRevision":9007199254740992,"description":"Review","condition":"later","target":"workspace"}`},
		{name: "natural zero", method: http.MethodPut, path: "/scheduler/" + created.ID, body: `{"expectedRevision":"0","description":"Review","condition":"later","target":"workspace"}`},
		{name: "natural noncanonical", method: http.MethodPut, path: "/scheduler/" + created.ID, body: `{"expectedRevision":"+1","description":"Review","condition":"later","target":"workspace"}`},
		{name: "natural overflow", method: http.MethodPut, path: "/scheduler/" + created.ID, body: `{"expectedRevision":"18446744073709551616","description":"Review","condition":"later","target":"workspace"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(test.method, "/api/workspaces/"+workspace.ID+test.path, test.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "revision") {
				t.Fatalf("invalid revision response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestNativeSchedulerMissingMutationsReturnTypedNotFound(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(nil, serveWorkspace{Path: root})
	missingID := "schedule-ffffffffffffffffffffffff"
	description := "Missing"
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-08-30T12:00:00Z"}
	changes := []NativeSchedulerChange{
		{Operation: app.ScheduleChangeUpdate, ID: missingID, ExpectedRevision: ^uint64(0), Description: &description, Trigger: &trigger},
		{Operation: app.ScheduleChangePause, ID: missingID},
		{Operation: app.ScheduleChangeResume, ID: missingID},
		{Operation: app.ScheduleChangeRemove, ID: missingID},
	}
	for _, change := range changes {
		t.Run(string(change.Operation), func(t *testing.T) {
			_, err := native.Change(context.Background(), change)
			var notFound *app.ScheduleNotFoundError
			if !errors.Is(err, app.ErrScheduleNotFound) || !errors.As(err, &notFound) || notFound.ScheduleID != missingID {
				t.Fatalf("%s error = %#v, %v", change.Operation, notFound, err)
			}
			var conflict *app.ScheduleRevisionConflictError
			if errors.As(err, &conflict) || errors.Is(err, app.ErrScheduleRevisionExhausted) {
				t.Fatalf("missing %s was reported as a revision failure: %v", change.Operation, err)
			}
		})
	}
}

func TestSchedulerHTTPMissingMutationsAreNotFound(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler-missing", Name: "Scheduler Missing", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	t.Cleanup(s.agents.waitBackground)
	path := filepath.Join(root, "scheduler", "scheduler.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	missingID := "schedule-ffffffffffffffffffffffff"
	description := "Missing"
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-08-30T12:00:00Z"}
	requests := []struct {
		name, method, path string
		body               *schedulerChangeRequest
	}{
		{name: "changes update", method: http.MethodPost, path: "/scheduler/changes", body: &schedulerChangeRequest{Operation: string(app.ScheduleChangeUpdate), ID: missingID, ExpectedRevision: schedulerExpectedRevision(schedulerapi.RevisionFromUint64(^uint64(0))), Description: &description, Trigger: &trigger}},
		{name: "changes pause", method: http.MethodPost, path: "/scheduler/changes", body: &schedulerChangeRequest{Operation: string(app.ScheduleChangePause), ID: missingID}},
		{name: "changes resume", method: http.MethodPost, path: "/scheduler/changes", body: &schedulerChangeRequest{Operation: string(app.ScheduleChangeResume), ID: missingID}},
		{name: "changes remove", method: http.MethodPost, path: "/scheduler/changes", body: &schedulerChangeRequest{Operation: string(app.ScheduleChangeRemove), ID: missingID}},
		{name: "web pause", method: http.MethodPost, path: "/scheduler/" + missingID + "/pause"},
		{name: "web resume", method: http.MethodPost, path: "/scheduler/" + missingID + "/resume"},
		{name: "web remove", method: http.MethodDelete, path: "/scheduler/" + missingID},
	}
	for _, requestCase := range requests {
		t.Run(requestCase.name, func(t *testing.T) {
			var body []byte
			if requestCase.body != nil {
				var marshalErr error
				body, marshalErr = json.Marshal(requestCase.body)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				requestCase.method,
				"/api/workspaces/"+workspace.ID+requestCase.path,
				bytes.NewReader(body),
			)
			s.handleWorkspace(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, body %s", requestCase.name, recorder.Code, recorder.Body.String())
			}
			var response map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["code"] != "schedule_not_found" || !strings.Contains(response["error"], missingID) {
				t.Fatalf("%s response = %#v", requestCase.name, response)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("missing %s changed scheduler.json:\nbefore=%s\nafter=%s", requestCase.name, before, after)
			}
		})
	}
}

func TestSchedulerNaturalLanguageAPIRequiresSelectedUserBeforeAcceptance(t *testing.T) {
	root := t.TempDir()
	puaWorkspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler-user", Name: "Scheduler User", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	t.Cleanup(s.agents.waitBackground)

	tests := []struct {
		name, user, code string
	}{
		{name: "missing", code: "user_required"},
		{name: "invalid", user: "bad/name", code: "invalid_request"},
		{name: "unknown", user: "Grace", code: "user_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler", strings.NewReader(`{"description":"Review","condition":"tomorrow morning","target":"workspace"}`))
			if test.user != "" {
				request.Header.Set(workspaceUserHeader, test.user)
			}
			recorder := httptest.NewRecorder()
			s.handleWorkspace(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
			mailbox, err := loadResourceMailboxForResource(root, app.SchedulerResourceID)
			if err != nil || len(mailbox.Messages) != 0 {
				t.Fatalf("rejected request mutated the Scheduler mailbox: %#v, %v", mailbox.Messages, err)
			}
		})
	}
}

func TestSchedulerNaturalLanguageAPIReturnsBeforeAgentHubReconciliation(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	reconciliationStarted := make(chan struct{})
	releaseReconciliation := make(chan struct{})
	var releaseOnce sync.Once
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents" {
			select {
			case <-reconciliationStarted:
			default:
				close(reconciliationStarted)
			}
			<-releaseReconciliation
		}
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	defer releaseOnce.Do(func() { close(releaseReconciliation) })
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler", strings.NewReader(`{"description":"Review","condition":"tomorrow morning","target":"workspace"}`))
	request.Header.Set(workspaceUserHeader, "Ada")
	recorder := httptest.NewRecorder()
	responseDone := make(chan struct{})
	go func() {
		manager.server.handleWorkspace(recorder, request)
		close(responseDone)
	}()
	select {
	case <-responseDone:
	case <-time.After(time.Second):
		t.Fatal("Scheduler acceptance waited for AgentHub reconciliation")
	}
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("acceptance = %d %s", recorder.Code, recorder.Body.String())
	}
	var response resourceMessageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	wantReference := "/api/workspaces/" + workspace.ID + "/messages/" + response.MessageID
	if response.MessageID == "" || response.ResourceID != app.SchedulerResourceID || response.Status != "waiting" || response.Reference != wantReference || response.GenerationID != "" || response.AgentHubSessionID != "" || response.TurnID != "" || response.LastError != "" || response.LastErrorCode != "" {
		t.Fatalf("response was not the initial durable acceptance: %#v", response)
	}
	message, found, err := mailboxMessageByID(workspace.Path, response.MessageID)
	if err != nil || !found || message.Status != resourceMessageQueued || message.Sender == nil || message.Sender.Name != "Ada" {
		t.Fatalf("durable acceptance = found %v, message %#v, error %v", found, message, err)
	}
	select {
	case <-reconciliationStarted:
	case <-time.After(time.Second):
		t.Fatal("queued reconciliation did not reach AgentHub")
	}
	releaseOnce.Do(func() { close(releaseReconciliation) })
}

func TestSchedulerNaturalLanguageAPIRetainsAcceptanceAfterBackgroundFailure(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.failNextMessage = true
	fake.mu.Unlock()

	request := httptest.NewRequest(http.MethodPut, "/api/workspaces/"+workspace.ID+"/scheduler/schedule-222222222222222222222222", strings.NewReader(`{"expectedRevision":"1","description":"Review later","condition":"next week","target":"workspace"}`))
	request.Header.Set(workspaceUserHeader, "Ada")
	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("background failure changed acceptance to %d: %s", recorder.Code, recorder.Body.String())
	}
	var response resourceMessageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	wantReference := "/api/workspaces/" + workspace.ID + "/messages/" + response.MessageID
	if response.Status != "waiting" || response.Reference != wantReference || response.GenerationID != "" || response.AgentHubSessionID != "" || response.LastError != "" || response.LastErrorCode != "" {
		t.Fatalf("acceptance exposed background state: %#v", response)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.withResourceController(ctx, workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatalf("wait for queued reconciliation: %v", err)
	}
	message, found, err := mailboxMessageByID(workspace.Path, response.MessageID)
	if err != nil || !found || message.Status != resourceMessageDelivering || message.LastError == "" || message.LastErrorCode == "" {
		t.Fatalf("background failure was not recorded: found %v, message %#v, error %v", found, message, err)
	}
}

func TestSchedulerHTTPStructuredUpdateRequiresCompleteTrigger(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler-update", Name: "Scheduler Update", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	puaWorkspace, err := app.OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	migrated := migrateSchedulerV1ForTest(t, puaWorkspace, schedulerV1TestDefinition{
		ID:          "schedule-222222222222222222222222",
		Description: "Migrated",
		Condition:   "ambiguous legacy rule",
		Target:      "workspace",
		CreatedAt:   "2026-08-01T00:00:00Z",
		UpdatedAt:   "2026-08-01T00:00:00Z",
	})
	compiled := migrated[0]
	originalAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	original, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Review", Condition: "at the original time", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: originalAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-scheduler-update/scheduler/changes", strings.NewReader(body))
		s.handleWorkspace(recorder, r)
		return recorder
	}
	assertUnchanged := func(want app.Schedule) {
		t.Helper()
		if got := schedulerTestScheduleByID(t, puaWorkspace, want.ID); !reflect.DeepEqual(got, want) {
			t.Fatalf("schedule changed after rejected update: got %#v, want %#v", got, want)
		}
	}

	descriptionOnly := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":"1","description":"Changed"}`)
	if descriptionOnly.Code != http.StatusBadRequest {
		t.Fatalf("description-only update = %d %s", descriptionOnly.Code, descriptionOnly.Body.String())
	}
	var triggerRequired map[string]string
	if err := json.Unmarshal(descriptionOnly.Body.Bytes(), &triggerRequired); err != nil {
		t.Fatal(err)
	}
	if triggerRequired["code"] != "schedule_trigger_required" || triggerRequired["error"] != "update requires a complete trigger" {
		t.Fatalf("description-only error = %#v", triggerRequired)
	}
	assertUnchanged(original)

	partialCompilation := request(`{"operation":"update","id":"` + compiled.ID + `","expectedRevision":"1","condition":"still ambiguous"}`)
	if partialCompilation.Code != http.StatusBadRequest || !strings.Contains(partialCompilation.Body.String(), "schedule_trigger_required") {
		t.Fatalf("partial compilation = %d %s", partialCompilation.Code, partialCompilation.Body.String())
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != 2 || !reflect.DeepEqual(schedulerTestScheduleByID(t, puaWorkspace, compiled.ID), compiled) {
		t.Fatalf("needs-compilation schedule changed after partial update: %#v", config.Schedules)
	}

	replacementAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	triggerOnly := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":"1","trigger":{"type":"at","at":"` + replacementAt + `"}}`)
	if triggerOnly.Code != http.StatusOK {
		t.Fatalf("trigger-only update = %d %s", triggerOnly.Code, triggerOnly.Body.String())
	}
	var updatedResponse schedulerapi.Schedule
	if err := json.Unmarshal(triggerOnly.Body.Bytes(), &updatedResponse); err != nil {
		t.Fatal(err)
	}
	if updatedResponse.Revision != "2" || updatedResponse.Description != original.Description || updatedResponse.Condition != original.Condition || updatedResponse.Target != original.Target || updatedResponse.Trigger == nil || updatedResponse.Trigger.At != replacementAt {
		t.Fatalf("trigger-only schedule = %#v", updatedResponse)
	}
	updated := schedulerTestScheduleByID(t, puaWorkspace, updatedResponse.ID)

	staleAt := time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339Nano)
	stale := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":"1","description":"Stale","condition":"stale rule","target":"scheduler","trigger":{"type":"at","at":"` + staleAt + `"}}`)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "schedule_revision_conflict") {
		t.Fatalf("full stale update = %d %s", stale.Code, stale.Body.String())
	}
	config, err = puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if got := schedulerTestScheduleByID(t, puaWorkspace, updated.ID); !reflect.DeepEqual(got, updated) {
		t.Fatalf("schedule changed after stale update: got %#v, want %#v", got, updated)
	}

	malformed := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":"2","trigger":{"type":"at"}}`)
	if malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), "at trigger must contain only at") {
		t.Fatalf("malformed trigger update = %d %s", malformed.Code, malformed.Body.String())
	}
	config, err = puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if got := schedulerTestScheduleByID(t, puaWorkspace, updated.ID); !reflect.DeepEqual(got, updated) {
		t.Fatalf("schedule changed after malformed trigger: got %#v, want %#v", got, updated)
	}
}

func TestNativeSchedulerStructuredUpdateRequiresCompleteTrigger(t *testing.T) {
	fixture := func(t *testing.T, trigger *app.ScheduleTrigger) (*NativeScheduler, *app.Workspace, app.Schedule) {
		t.Helper()
		root := t.TempDir()
		if _, err := app.Initialize(root, "en"); err != nil {
			t.Fatal(err)
		}
		puaWorkspace, err := app.OpenWorkspace(root)
		if err != nil {
			t.Fatal(err)
		}
		var created app.Schedule
		if trigger == nil {
			migrated := migrateSchedulerV1ForTest(t, puaWorkspace, schedulerV1TestDefinition{
				ID:          "schedule-333333333333333333333333",
				Description: "Review",
				Condition:   "at the original time",
				Target:      "workspace",
				CreatedAt:   "2026-08-01T00:00:00Z",
				UpdatedAt:   "2026-08-01T00:00:00Z",
			})
			created = migrated[0]
			if created.Revision != 1 || created.State != app.ScheduleStateNeedsCompilation || created.Trigger != nil {
				t.Fatalf("migrated fixture = %#v", created)
			}
		} else {
			created, err = puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Review", Condition: "at the original time", Target: "workspace", Trigger: trigger,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return newNativeScheduler(nil, serveWorkspace{Path: root}), puaWorkspace, created
	}
	readSchedule := func(t *testing.T, workspace *app.Workspace) app.Schedule {
		t.Helper()
		config, err := workspace.Scheduler()
		if err != nil {
			t.Fatal(err)
		}
		if len(config.Schedules) != 1 {
			t.Fatalf("schedules = %#v", config.Schedules)
		}
		return config.Schedules[0]
	}

	t.Run("description-only current revision is rejected atomically", func(t *testing.T) {
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		native, workspace, original := fixture(t, &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at})
		description := "Changed"
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Description: &description,
		})
		if err == nil || !errors.Is(err, errNativeSchedulerUpdateTriggerRequired) || err.Error() != "update requires a complete trigger" {
			t.Fatalf("description-only error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) {
			t.Fatalf("schedule changed after description-only update: got %#v, want %#v", got, original)
		}
	})

	t.Run("needs-compilation partial update is rejected", func(t *testing.T) {
		native, workspace, original := fixture(t, nil)
		condition := "still ambiguous"
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Condition: &condition,
		})
		if !errors.Is(err, errNativeSchedulerUpdateTriggerRequired) {
			t.Fatalf("partial compilation error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) || got.State != app.ScheduleStateNeedsCompilation {
			t.Fatalf("needs-compilation schedule changed: got %#v, want %#v", got, original)
		}
	})

	t.Run("historical Scheduler self-target requires retargeting", func(t *testing.T) {
		root := t.TempDir()
		if _, err := app.Initialize(root, "en"); err != nil {
			t.Fatal(err)
		}
		workspace, err := app.OpenWorkspace(root)
		if err != nil {
			t.Fatal(err)
		}
		migrated := migrateSchedulerV1ForTest(t, workspace, schedulerV1TestDefinition{
			ID:          "schedule-444444444444444444444444",
			Description: "Historical self-target",
			Condition:   "at the original time",
			Target:      app.SchedulerResourceID,
			CreatedAt:   "2026-08-01T00:00:00Z",
			UpdatedAt:   "2026-08-01T00:00:00Z",
		})[0]
		native := newNativeScheduler(nil, serveWorkspace{Path: root})
		replacement := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
		_, err = native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: migrated.ID, ExpectedRevision: migrated.Revision, Trigger: &replacement,
		})
		if !errors.Is(err, app.ErrScheduleTargetScheduler) {
			t.Fatalf("self-target compilation error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, migrated) {
			t.Fatalf("rejected self-target compilation changed schedule: got %#v, want %#v", got, migrated)
		}
		target := "workspace"
		compiled, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: migrated.ID, ExpectedRevision: migrated.Revision,
			Target: &target, Trigger: &replacement,
		})
		if err != nil {
			t.Fatal(err)
		}
		if compiled.State != app.ScheduleStateActive || compiled.Target != target || compiled.Trigger == nil || *compiled.Trigger != replacement {
			t.Fatalf("retargeted compilation = %#v", compiled)
		}
	})

	t.Run("trigger-only current revision succeeds", func(t *testing.T) {
		native, _, original := fixture(t, nil)
		replacement := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
		updated, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Trigger: &replacement,
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Revision != original.Revision+1 || updated.State != app.ScheduleStateActive || updated.Description != original.Description || updated.Condition != original.Condition || updated.Target != original.Target || updated.Trigger == nil || *updated.Trigger != replacement {
			t.Fatalf("trigger-only schedule = %#v", updated)
		}
	})

	t.Run("valid full replacement preserves revision conflict", func(t *testing.T) {
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		native, workspace, original := fixture(t, &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at})
		description, condition, target := "Changed", "at the new time", "scheduler"
		replacement := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision + 1,
			Description: &description, Condition: &condition, Target: &target, Trigger: &replacement,
		})
		var conflict *app.ScheduleRevisionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale full replacement error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) {
			t.Fatalf("schedule changed after stale full replacement: got %#v, want %#v", got, original)
		}
	})

	t.Run("malformed replacement is rejected atomically", func(t *testing.T) {
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		native, workspace, original := fixture(t, &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at})
		malformed := app.ScheduleTrigger{Type: app.ScheduleTriggerAt}
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Trigger: &malformed,
		})
		if err == nil || !strings.Contains(err.Error(), "at trigger must contain only at") {
			t.Fatalf("malformed replacement error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) {
			t.Fatalf("schedule changed after malformed replacement: got %#v, want %#v", got, original)
		}
	})
}

func TestSchedulerNativeChangeValidatesTrailingData(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler", Name: "Scheduler", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)

	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{name: "whitespace", body: "{\"operation\":\"restart\"} \n\t", wantError: `unsupported Scheduler change "restart"`},
		{name: "second value", body: `{"operation":"restart"} {}`, wantError: "request body must contain exactly one JSON value"},
		{name: "malformed bytes", body: `{"operation":"restart"} trailing`, wantError: "invalid character"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", strings.NewReader(test.body))
			s.handleWorkspace(recorder, request)

			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response = %d %q: %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			var response map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if !strings.Contains(response["error"], test.wantError) {
				t.Fatalf("error = %q, want substring %q", response["error"], test.wantError)
			}
		})
	}
}

func scheduleOccurrenceMessages(t *testing.T, workspacePath, resourceID string) []resourceMailboxMessage {
	t.Helper()
	mailbox, err := loadResourceMailboxForResource(workspacePath, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	var result []resourceMailboxMessage
	for _, message := range mailbox.Messages {
		if message.Type == resourceMessageTypeScheduleOccurrence {
			result = append(result, message)
		}
	}
	return result
}

func schedulerAgentHubInputCount(fake *runtimeFakeAgentHub) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	count := 0
	for _, events := range fake.events {
		for _, event := range events {
			if event.Type == "message.input" {
				count++
			}
		}
	}
	return count
}

func TestNativeSchedulerCoalescesDowntimeAndUsesStableOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Check release", Condition: "every minute", Guard: "the release branch is green", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return anchor.Add(3*time.Minute + 10*time.Second) }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].Causation == nil {
		t.Fatalf("occurrence messages = %#v", messages)
	}
	cause := messages[0].Causation
	if cause.ScheduleID != created.ID || cause.ScheduleRevision != 1 || cause.CoalescedCount != 4 || cause.Reason != schedulerOccurrenceReasonCoalesced {
		t.Fatalf("occurrence causation = %#v\n%s", cause, messages[0].Text)
	}
	protocol, err := newNativeScheduler(manager, workspace).prepareOccurrence(created, anchor, anchor.Add(3*time.Minute), anchor.Add(4*time.Minute), 4, false, time.Time{}, schedulerOccurrenceReasonCoalesced)
	if err != nil || !strings.Contains(protocol.Text, "Action: Check release") || !strings.Contains(protocol.Text, "Guard: the release branch is green") || !strings.Contains(protocol.Text, "Next occurrence: "+anchor.Add(4*time.Minute).Format(time.RFC3339Nano)) || !strings.Contains(protocol.Text, cause.OccurrenceID) {
		t.Fatalf("occurrence guard protocol is incomplete: %v\n%s", err, protocol.Text)
	}
	firstMessageID := messages[0].ID
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	messages = scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].ID != firstMessageID {
		t.Fatalf("reconcile duplicated occurrence = %#v", messages)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(manager.now())
	if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].NextRunAt != anchor.Add(4*time.Minute).Format(time.RFC3339Nano) || snapshot.Schedules[0].LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("runtime snapshot = %#v, %v", snapshot, err)
	}
	if deadline := manager.nextSchedulerReconcileDeadline(manager.now()); !deadline.Equal(anchor.Add(4 * time.Minute)) {
		t.Fatalf("dynamic Scheduler deadline = %s", deadline)
	}
}

func TestNativeSchedulerCappedCronFreezesTruthfulRecoveryBounds(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()

	tests := []struct {
		language       string
		capText        string
		recoveryText   string
		nonNominalText string
		ordinaryText   string
	}{
		{
			language: "en", capText: "Cron enumeration cap reached: yes",
			recoveryText: "Recovery cutoff:", nonNominalText: "not asserted to be a nominal occurrence",
			ordinaryText: "Coalesced through:",
		},
		{
			language: "zh-CN", capText: "Cron 枚举已达到上限：是",
			recoveryText: "恢复截点：", nonNominalText: "不表示该截点本身是 nominal occurrence",
			ordinaryText: "合并截至：",
		},
	}
	for _, test := range tests {
		t.Run(test.language, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			if test.language != "en" {
				if err := puaWorkspace.Migrate(test.language); err != nil {
					t.Fatal(err)
				}
			}
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Recover capped cron work", Condition: "every minute", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"},
			})
			if err != nil {
				t.Fatal(err)
			}

			first := time.Date(2026, time.January, 1, 0, 1, 0, 0, time.UTC)
			recoveryCutoff := first.Add(100_001*time.Minute + 30*time.Second)
			enumeratedThrough := first.Add(99_999 * time.Minute)
			native := newNativeScheduler(manager, workspace)
			runtime := schedulerScheduleRuntime{
				Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger),
				EffectiveState: app.ScheduleStateActive, NextRunAt: first.Format(time.RFC3339Nano),
			}
			if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
				t.Fatal(err)
			}
			if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
				t.Fatal(err)
			}
			manager.now = func() time.Time { return recoveryCutoff }
			if _, err := native.Reconcile(context.Background(), recoveryCutoff); err != nil {
				t.Fatal(err)
			}

			persisted, err := native.schedulerRuntime(schedule.ID)
			if err != nil || persisted.Prepared == nil {
				t.Fatalf("capped occurrence checkpoint = %#v, %v", persisted, err)
			}
			prepared := *persisted.Prepared
			cause := prepared.Causation
			if !prepared.CronEnumerationCapped || prepared.EnumeratedThrough != enumeratedThrough.Format(time.RFC3339Nano) ||
				prepared.EnumeratedCount != 100_000 || prepared.RecoveryCutoff != recoveryCutoff.Format(time.RFC3339Nano) ||
				cause == nil || !cause.CronEnumerationCapped || cause.EnumeratedThrough != prepared.EnumeratedThrough ||
				cause.EnumeratedCount != prepared.EnumeratedCount || cause.RecoveryCutoff != prepared.RecoveryCutoff {
				t.Fatalf("capped occurrence metadata = prepared %#v, causation %#v", prepared, cause)
			}
			if prepared.CoalescedThrough != prepared.EnumeratedThrough || prepared.CoalescedCount != prepared.EnumeratedCount ||
				cause.CoalescedThrough != cause.EnumeratedThrough || cause.CoalescedCount != cause.EnumeratedCount {
				t.Fatalf("enumerated lower bound changed: prepared %#v, causation %#v", prepared, cause)
			}
			next, err := time.Parse(time.RFC3339Nano, prepared.NextRunAt)
			if err != nil || !next.After(recoveryCutoff) {
				t.Fatalf("next occurrence = %q after %s, %v", prepared.NextRunAt, recoveryCutoff, err)
			}
			instanceID, err := workspaceInstanceID(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			revision := strconv.FormatUint(schedule.Revision, 10)
			if want := notificationMessageID("schedule-occurrence", instanceID, schedule.ID, revision, first.Format(time.RFC3339Nano)); prepared.OccurrenceID != want {
				t.Fatalf("occurrence ID = %q, want first-keyed %q", prepared.OccurrenceID, want)
			}
			if want := notificationMessageID(resourceMessageTypeScheduleOccurrence, instanceID, schedule.ID, revision, first.Format(time.RFC3339Nano)); prepared.MessageID != want {
				t.Fatalf("message ID = %q, want first-keyed %q", prepared.MessageID, want)
			}
			for _, want := range []string{test.capText, test.recoveryText, test.nonNominalText, prepared.EnumeratedThrough, prepared.RecoveryCutoff} {
				if !strings.Contains(prepared.Text, want) {
					t.Fatalf("capped occurrence prompt missing %q:\n%s", want, prepared.Text)
				}
			}
			causationJSON, err := json.Marshal(cause)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`"cronEnumerationCapped":true`, `"enumeratedThrough":"` + prepared.EnumeratedThrough + `"`, `"enumeratedCount":100000`, `"recoveryCutoff":"` + prepared.RecoveryCutoff + `"`} {
				if !strings.Contains(string(causationJSON), want) {
					t.Fatalf("public causation JSON missing %s: %s", want, causationJSON)
				}
			}

			restartedManager := newAgentManager(manager.server)
			restartedManager.now = func() time.Time { return recoveryCutoff }
			manager.server.agents = restartedManager
			restarted := newNativeScheduler(restartedManager, workspace)
			frozen, err := restarted.schedulerRuntime(schedule.ID)
			if err != nil || !reflect.DeepEqual(frozen.Prepared, &prepared) {
				t.Fatalf("restarted checkpoint = %#v, want %#v, %v", frozen.Prepared, prepared, err)
			}
			if _, err := restarted.Reconcile(context.Background(), recoveryCutoff); err != nil {
				t.Fatal(err)
			}
			replayed, err := restarted.schedulerRuntime(schedule.ID)
			if err != nil || !reflect.DeepEqual(replayed.Prepared, &prepared) {
				t.Fatalf("replayed checkpoint changed = %#v, want %#v, %v", replayed.Prepared, prepared, err)
			}

			ordinary, err := restarted.prepareOccurrence(schedule, first, first, first.Add(time.Minute), 1, false, time.Time{}, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			if ordinary.OccurrenceID != prepared.OccurrenceID || ordinary.MessageID != prepared.MessageID ||
				ordinary.CronEnumerationCapped || strings.Contains(ordinary.Text, test.capText) ||
				strings.Contains(ordinary.Text, test.recoveryText) || !strings.Contains(ordinary.Text, test.ordinaryText) {
				t.Fatalf("non-truncated occurrence protocol changed: %#v\n%s", ordinary, ordinary.Text)
			}
			ordinaryJSON, err := json.Marshal(ordinary)
			if err != nil {
				t.Fatal(err)
			}
			for _, omitted := range []string{"cronEnumerationCapped", "enumeratedThrough", "enumeratedCount", "recoveryCutoff"} {
				if strings.Contains(string(ordinaryJSON), omitted) {
					t.Fatalf("non-truncated checkpoint includes %q: %s", omitted, ordinaryJSON)
				}
			}
		})
	}
}

func TestNativeSchedulerSkipsBusyRepeatingTargetButQueuesOneTime(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	tests := []struct {
		name     string
		wantBusy bool
		seed     func(*resourceMailboxStore, string)
	}{
		{
			name:     "queued message",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "queued-message", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "already waiting", Role: "user", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageQueued,
					AcceptedAt: stamp, UpdatedAt: stamp,
				})
			},
		},
		{
			name:     "unresolved result subscription",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "pending-result", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "awaiting result", Role: "agent", Sender: &agentHubMessageSender{ID: "project1", Name: "Sender"},
					SenderWorkspaceInstanceID: "sender-instance", SubscribeResult: true,
					ResultSubscriptionStatus: resourceResultSubscriptionPending,
					RequestedMode:            resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
					Status: resourceMessageDelivered, AcceptedAt: stamp, UpdatedAt: stamp,
					DeliveredAt: stamp, TerminalAt: stamp, GenerationID: "generation-1", TurnID: "turn-1",
				})
			},
		},
		{
			name:     "unresolved notification",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "pending-notification", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "notify sender", Role: "agent", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered,
					AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
					Notification: &resourceNotificationReceipt{
						ID: "notification-1", Type: resourceMessageTypeDeliveryTerminal, Status: resourceNotificationWaiting,
						TargetWorkspaceInstanceID: "sender-instance", TargetResourceID: "project1",
						CreatedAt: stamp, UpdatedAt: stamp,
					},
				})
			},
		},
		{
			name:     "pending notification outbox",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "outbox-source", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "completed source", Role: "agent", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered,
					AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
				})
				store.Outbox.Operations = append(store.Outbox.Operations, resourceMailboxNotificationOp{
					ID: "outbox-operation", Type: resourceMessageTypeDeliveryTerminal,
					SourceMessageID: "outbox-source", SourceResourceID: "project1.task1",
					SourceWorkspaceInstanceID: "target-instance", TargetWorkspaceInstanceID: "sender-instance",
					TargetResourceID: "project1", GeneratedMessageID: "generated-notification",
					Status: resourceNotificationWaiting, UpdatedAt: stamp,
				})
			},
		},
		{
			name: "cold terminal receipt",
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "cold-receipt", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "finished", Role: "user", SubscribeResult: false,
					ResultSubscriptionStatus: resourceResultSubscriptionDisabled,
					RequestedMode:            resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
					Status: resourceMessageDelivered, AcceptedAt: stamp, UpdatedAt: stamp,
					DeliveredAt: stamp, TerminalAt: stamp,
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			repeating, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Repeated", Condition: "each minute", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			at := anchor.Add(10 * time.Second)
			oneTime, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "One time", Condition: "once", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			stamp := anchor.Add(-time.Minute).Format(time.RFC3339Nano)
			if _, err := mutateResourceMailboxStoreForResource(workspace.Path, "project1.task1", func(store *resourceMailboxStore) error {
				test.seed(store, stamp)
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			hasHotWork, err := resourceMailboxHasHotWork(workspace.Path, "project1.task1")
			if err != nil || hasHotWork != test.wantBusy {
				t.Fatalf("hot mailbox ownership = %v, want %v: %v", hasHotWork, test.wantBusy, err)
			}
			if test.name == "cold terminal receipt" {
				stored, found, err := mailboxMessageByID(workspace.Path, "cold-receipt")
				if err != nil || !found || !stored.receipt {
					t.Fatalf("terminal message did not leave hot storage: found=%v err=%v message=%#v", found, err, stored)
				}
			}

			manager.now = func() time.Time { return at.Add(time.Second) }
			if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
				t.Fatal(err)
			}
			snapshot, err := newNativeScheduler(manager, workspace).Snapshot(manager.now())
			if err != nil || len(snapshot.Schedules) != 2 {
				t.Fatalf("snapshot = %#v, %v", snapshot, err)
			}
			wantRepeatOutcome := schedulerOutcomeAccepted
			if test.wantBusy {
				wantRepeatOutcome = schedulerOutcomeBusy
			}
			if snapshot.Schedules[0].ID != repeating.ID || snapshot.Schedules[0].LastOutcome != wantRepeatOutcome {
				t.Fatalf("repeating target outcome = %#v, want %q", snapshot.Schedules[0], wantRepeatOutcome)
			}
			if snapshot.Schedules[1].ID != oneTime.ID || snapshot.Schedules[1].LastOutcome != schedulerOutcomeAccepted {
				t.Fatalf("one-time target outcome = %#v", snapshot.Schedules[1])
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, "project1.task1")
			wantIDs := map[string]bool{oneTime.ID: true}
			if !test.wantBusy {
				wantIDs[repeating.ID] = true
			}
			if len(messages) != len(wantIDs) {
				t.Fatalf("occurrence messages = %#v, want schedule ids %#v", messages, wantIDs)
			}
			for _, message := range messages {
				if message.Causation == nil || !wantIDs[message.Causation.ScheduleID] {
					t.Fatalf("unexpected occurrence message = %#v, want schedule ids %#v", message, wantIDs)
				}
				delete(wantIDs, message.Causation.ScheduleID)
			}
			if len(wantIDs) != 0 {
				t.Fatalf("missing occurrence schedule ids = %#v", wantIDs)
			}
		})
	}
}

func TestNativeSchedulerCrashWindowReplayMatrix(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	type crashWindow int
	const (
		crashBeforePrepared crashWindow = iota
		crashWithPrepared
		crashWithAcceptedMessage
		crashAfterCheckpoint
	)
	tests := []struct {
		name   string
		window crashWindow
	}{
		{name: "before prepared persistence", window: crashBeforePrepared},
		{name: "after prepared persistence", window: crashWithPrepared},
		// No durable write separates prepared persistence from the mailbox
		// acceptance call, so these two crash boundaries restart from the same
		// checkpoint and must have the same outcome.
		{name: "before mailbox acceptance", window: crashWithPrepared},
		{name: "after mailbox acceptance", window: crashWithAcceptedMessage},
		// Likewise, an accepted mailbox message plus the still-prepared source
		// checkpoint is the durable state immediately before checkpoint commit.
		{name: "before checkpoint commit", window: crashWithAcceptedMessage},
		{name: "after checkpoint commit", window: crashAfterCheckpoint},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Replay safely", Condition: "every minute", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := at.Add(time.Second)
			next := at.Add(time.Minute)
			native := newNativeScheduler(manager, workspace)
			prepared, err := native.prepareOccurrence(schedule, at, at, next, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			initial := schedulerScheduleRuntime{
				Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger), EffectiveState: app.ScheduleStateActive,
				NextRunAt: at.Format(time.RFC3339Nano),
			}
			if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
				t.Fatal(err)
			}

			switch test.window {
			case crashBeforePrepared:
				// The due cursor is durable, but no immutable occurrence has been
				// prepared. Restart must derive the same stable identifiers.
			case crashWithPrepared:
				initial.Prepared = &prepared
				if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
					t.Fatal(err)
				}
			case crashWithAcceptedMessage:
				initial.Prepared = &prepared
				if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
					t.Fatal(err)
				}
				if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
					t.Fatal(err)
				}
			case crashAfterCheckpoint:
				initial.Prepared = &prepared
				if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
					t.Fatal(err)
				}
				if err := native.deliverPrepared(context.Background(), schedule, initial, now); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown crash window %d", test.window)
			}

			wantBefore := 0
			if test.window >= crashWithAcceptedMessage {
				wantBefore = 1
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, wantBefore)

			// A fresh manager has no in-memory resource controller or Scheduler
			// instance, so only the durable workspace state crosses this boundary.
			restartedManager := newAgentManager(manager.server)
			restartedManager.now = func() time.Time { return now }
			manager.server.agents = restartedManager
			restarted := newNativeScheduler(restartedManager, workspace)
			recomputed, err := restarted.prepareOccurrence(schedule, at, at, next, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			if recomputed.OccurrenceID != prepared.OccurrenceID || recomputed.MessageID != prepared.MessageID {
				t.Fatalf("stable identifiers changed after restart: got %s/%s, want %s/%s", recomputed.OccurrenceID, recomputed.MessageID, prepared.OccurrenceID, prepared.MessageID)
			}
			if test.window == crashWithAcceptedMessage {
				busy, err := restarted.targetBusy(prepared.Target)
				if err != nil || !busy {
					t.Fatalf("accepted prepared occurrence was not hot before replay: busy=%v err=%v", busy, err)
				}
			}
			if _, err := restarted.Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)

			runtime, err := restarted.schedulerRuntime(schedule.ID)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != at.Format(time.RFC3339Nano) || runtime.NextRunAt != next.Format(time.RFC3339Nano) {
				t.Fatalf("replayed checkpoint = %#v", runtime)
			}

			// A second restart proves a committed replay cannot append either a
			// hot mailbox duplicate or an additional compacted receipt.
			secondManager := newAgentManager(manager.server)
			secondManager.now = func() time.Time { return now }
			manager.server.agents = secondManager
			if _, err := newNativeScheduler(secondManager, workspace).Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
		})
	}
}

func preparedOccurrenceMessage(prepared schedulerPreparedOccurrence) resourceMailboxMessage {
	return resourceMailboxMessage{
		ID: prepared.MessageID, ResourceID: prepared.Target, Text: prepared.Text,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeScheduleOccurrence, Causation: cloneMailboxCausation(prepared.Causation),
		SenderWorkspaceInstanceID: prepared.Causation.SourceWorkspaceInstanceID,
	}
}

func mustSchedulerTriggerDigest(t *testing.T, trigger *app.ScheduleTrigger) string {
	t.Helper()
	digest, err := schedulerTriggerDigest(trigger)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertPreparedOccurrenceEqual(t *testing.T, got *schedulerPreparedOccurrence, want schedulerPreparedOccurrence) {
	t.Helper()
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("prepared occurrence = %#v, want %#v", got, want)
	}
}

func assertDeliveredPreparedOccurrence(t *testing.T, messages []resourceMailboxMessage, prepared schedulerPreparedOccurrence) {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("delivered occurrence count = %d in %#v, want 1", len(messages), messages)
	}
	message := messages[0]
	if message.ID != prepared.MessageID || !reflect.DeepEqual(message.Causation, prepared.Causation) {
		t.Fatalf("delivered occurrence changed identity or causation: %#v, want %s/%#v", message, prepared.MessageID, prepared.Causation)
	}
}

func assertSingleDurableOccurrence(t *testing.T, workspacePath string, prepared schedulerPreparedOccurrence, want int) {
	t.Helper()
	messages := scheduleOccurrenceMessages(t, workspacePath, prepared.Target)
	matching := 0
	for _, message := range messages {
		if message.ID == prepared.MessageID {
			matching++
			if message.Causation == nil || message.Causation.OccurrenceID != prepared.OccurrenceID {
				t.Fatalf("occurrence causation changed: %#v", message)
			}
		}
	}
	if matching != want || len(messages) != want {
		t.Fatalf("durable occurrence copies = %d in %#v, want %d", matching, messages, want)
	}

	store, err := loadResourceMailboxStoreForRead(workspacePath, prepared.Target)
	if err != nil {
		t.Fatal(err)
	}
	hotCopies := 0
	for _, message := range store.Mailbox.Messages {
		if !message.receipt && message.ID == prepared.MessageID {
			hotCopies++
		}
	}
	receiptCopies := 0
	for _, receipt := range store.Receipts.Receipts {
		if receipt.ID == prepared.MessageID {
			receiptCopies++
		}
	}
	if hotCopies+receiptCopies != want {
		t.Fatalf("physical occurrence copies = hot %d + receipts %d, want %d", hotCopies, receiptCopies, want)
	}
}

func setPreparedReceiptRetentionForTest(t *testing.T, receiptCount int, receiptWindow time.Duration, expiredCount int, expiredWindow time.Duration) {
	t.Helper()
	previousReceiptCount, previousReceiptWindow := resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow
	previousExpiredCount, previousExpiredWindow := resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow
	resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = receiptCount, receiptWindow
	resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow = expiredCount, expiredWindow
	t.Cleanup(func() {
		resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = previousReceiptCount, previousReceiptWindow
		resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow = previousExpiredCount, previousExpiredWindow
	})
}

func TestNativeSchedulerPreparedPinsAcceptedReceipt(t *testing.T) {
	setPreparedReceiptRetentionForTest(t, 2, time.Hour, 2, 24*time.Hour)
	for _, targetKind := range []string{"workspace", "task"} {
		t.Run(targetKind, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			target := "workspace"
			if targetKind == "task" {
				project, err := puaWorkspace.CreateProject("Receipt target", "receipt-target")
				if err != nil {
					t.Fatal(err)
				}
				task, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Receipt target", Slug: "receipt-target"})
				if err != nil {
					t.Fatal(err)
				}
				target = task.ID
			}
			at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Retain accepted evidence", Condition: "once", Target: target,
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			native := newNativeScheduler(manager, workspace)
			prepared, err := native.prepareOccurrence(schedule, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := initialScheduleRuntime(schedule, at.Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			runtime.Prepared = &prepared
			if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
				t.Fatal(err)
			}
			if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
				t.Fatal(err)
			}

			oldStamp := time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano)
			recent := time.Now().Add(-time.Minute)
			if _, err := mutateResourceMailboxStoreForResource(workspace.Path, target, func(store *resourceMailboxStore) error {
				found := false
				for index := range store.Mailbox.Messages {
					message := &store.Mailbox.Messages[index]
					if message.ID != prepared.MessageID {
						continue
					}
					found = true
					message.Status = resourceMessageDelivered
					message.AcceptedAt, message.UpdatedAt = oldStamp, oldStamp
					message.DeliveredAt, message.TerminalAt = oldStamp, oldStamp
				}
				if !found {
					return errors.New("accepted occurrence fixture is missing")
				}
				for index := 0; index < 3; index++ {
					stamp := recent.Add(-time.Duration(index) * time.Second).Format(time.RFC3339Nano)
					store.Mailbox.NextSequence++
					store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
						ID: fmt.Sprintf("unrelated-receipt-%s-%d", target, index), Sequence: store.Mailbox.NextSequence,
						ResourceID: target, Role: "system", RequestedMode: resourceMessageModeEnqueue,
						ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered,
						AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
					})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			store, err := loadResourceMailboxStoreForRead(workspace.Path, target)
			if err != nil {
				t.Fatal(err)
			}
			pinnedReceipts, unrelatedReceipts := 0, 0
			for _, receipt := range store.Receipts.Receipts {
				switch {
				case receipt.ID == prepared.MessageID:
					pinnedReceipts++
				case strings.HasPrefix(receipt.ID, "unrelated-receipt-"):
					unrelatedReceipts++
				}
			}
			if pinnedReceipts != 1 || unrelatedReceipts != 2 || len(store.Receipts.Expired) != 1 {
				t.Fatalf("prepared receipt retention = pinned %d unrelated %d expired %d", pinnedReceipts, unrelatedReceipts, len(store.Receipts.Expired))
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)

			// A daemon restart reconstructs both the Prepared reference and the
			// target receipt solely from the atomically renamed JSON documents.
			restarted := newNativeScheduler(newAgentManager(manager.server), workspace)
			persisted, err := restarted.schedulerRuntime(schedule.ID)
			if err != nil || persisted.Prepared == nil || persisted.Prepared.MessageID != prepared.MessageID {
				t.Fatalf("restarted Prepared checkpoint = %#v, %v", persisted, err)
			}
			accepted, found, err := mailboxMessageByID(workspace.Path, prepared.MessageID)
			if err != nil || !found || !accepted.receipt || accepted.ID != prepared.MessageID {
				t.Fatalf("restarted accepted evidence = %#v, found=%v err=%v", accepted, found, err)
			}
		})
	}
}

func TestNativeSchedulerExpiredPreparedReceiptReplaysAccepted(t *testing.T) {
	setPreparedReceiptRetentionForTest(t, 1, time.Hour, 2, time.Hour)
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Replay expired acceptance", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	prepared, err := native.prepareOccurrence(schedule, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := initialScheduleRuntime(schedule, at.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runtime.Prepared = &prepared
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}

	oldStamp := time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	recent := time.Now().Add(-time.Minute)
	if _, err := mutateResourceMailboxStoreForResource(workspace.Path, prepared.Target, func(store *resourceMailboxStore) error {
		store.Receipts.Expired = append(store.Receipts.Expired, resourceMailboxExpiredEntry{ID: prepared.MessageID, ExpiredAt: oldStamp})
		for index := 0; index < 4; index++ {
			store.Receipts.Expired = append(store.Receipts.Expired, resourceMailboxExpiredEntry{
				ID:        fmt.Sprintf("unrelated-expired-%d", index),
				ExpiredAt: recent.Add(-time.Duration(index) * time.Second).Format(time.RFC3339Nano),
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store, err := loadResourceMailboxStoreForRead(workspace.Path, prepared.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Mailbox.Messages) != 0 || len(store.Receipts.Receipts) != 0 || len(store.Receipts.Expired) != 3 {
		t.Fatalf("pinned expired fixture = hot/receipts %d/%d expired %d", len(store.Mailbox.Messages), len(store.Receipts.Receipts), len(store.Receipts.Expired))
	}
	if _, found, err := mailboxMessageByID(workspace.Path, prepared.MessageID); found {
		t.Fatalf("expired prepared lookup unexpectedly found a message")
	} else {
		var apiErr *resourceAPIError
		if !errors.As(err, &apiErr) || apiErr.Code != "message_receipt_expired" {
			t.Fatalf("expired prepared lookup error = %v", err)
		}
	}

	// Restart with only the persisted Prepared cursor and expired tombstone.
	// Replay must commit acceptance without appending or waking the target.
	restartedManager := newAgentManager(manager.server)
	restartedManager.now = func() time.Time { return at.Add(time.Second) }
	manager.server.agents = restartedManager
	fake.mu.Lock()
	beforeActions, beforeMessages, beforeSessions := len(fake.actions), len(fake.messageIDs), len(fake.sessions)
	fake.mu.Unlock()
	if _, err := newNativeScheduler(restartedManager, workspace).Reconcile(context.Background(), at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	persisted, err := newNativeScheduler(restartedManager, workspace).schedulerRuntime(schedule.ID)
	if err != nil || persisted.Prepared != nil || persisted.LastOutcome != schedulerOutcomeAccepted || persisted.EffectiveState != app.ScheduleStateCompleted || persisted.RetryAt != "" {
		t.Fatalf("expired evidence replay checkpoint = %#v, %v", persisted, err)
	}
	store, err = loadResourceMailboxStoreForRead(workspace.Path, prepared.Target)
	if err != nil {
		t.Fatal(err)
	}
	if store.Mailbox.NextSequence != 0 || len(store.Mailbox.Messages) != 0 || len(store.Receipts.Receipts) != 0 || len(store.Receipts.Expired) != 3 {
		t.Fatalf("expired replay appended a physical copy: next=%d hot/receipts=%d/%d expired=%d", store.Mailbox.NextSequence, len(store.Mailbox.Messages), len(store.Receipts.Receipts), len(store.Receipts.Expired))
	}
	fake.mu.Lock()
	afterActions, afterMessages, afterSessions := len(fake.actions), len(fake.messageIDs), len(fake.sessions)
	fake.mu.Unlock()
	if afterActions != beforeActions || afterMessages != beforeMessages || afterSessions != beforeSessions {
		t.Fatalf("expired replay woke target: actions=%d/%d messages=%d/%d sessions=%d/%d", afterActions, beforeActions, afterMessages, beforeMessages, afterSessions, beforeSessions)
	}

	// Once Prepared is durably clear, the target's next ordinary compaction can
	// release its evidence while the two unrelated tombstones remain bounded.
	if _, err := mutateResourceMailboxStoreForResource(workspace.Path, prepared.Target, func(*resourceMailboxStore) error { return nil }); err != nil {
		t.Fatal(err)
	}
	store, err = loadResourceMailboxStoreForRead(workspace.Path, prepared.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Receipts.Expired) != 2 {
		t.Fatalf("released Prepared evidence left expired count %d, want 2", len(store.Receipts.Expired))
	}
	for _, entry := range store.Receipts.Expired {
		if entry.ID == prepared.MessageID {
			t.Fatalf("cleared Prepared still pinned %s", entry.ID)
		}
	}
	if _, found, err := mailboxMessageByID(workspace.Path, prepared.MessageID); err != nil || found {
		t.Fatalf("released Prepared evidence lookup = found=%v err=%v", found, err)
	}
}

func TestNativeSchedulerAcceptedPreparedReplayPrecedesTargetAvailability(t *testing.T) {
	for _, targetState := range []string{"archived", "missing"} {
		for _, evidence := range []string{"accepted message", "expired receipt"} {
			t.Run(targetState+"/"+evidence, func(t *testing.T) {
				fake, manager, workspace, puaWorkspace, native, schedule, at := newOneTimeRetargetFixture(t, "project1.task1")
				prepared := seedOneTimePreparedRetarget(t, native, schedule, at)
				switch evidence {
				case "accepted message":
					if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
						t.Fatal(err)
					}
				case "expired receipt":
					if _, err := mutateResourceMailboxStoreForResource(workspace.Path, prepared.Target, func(store *resourceMailboxStore) error {
						store.Receipts.Expired = append(store.Receipts.Expired, resourceMailboxExpiredEntry{
							ID: prepared.MessageID, ExpiredAt: time.Now().Format(time.RFC3339Nano),
						})
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown evidence %q", evidence)
				}

				target, err := puaWorkspace.ResourceValue(prepared.Target)
				if err != nil || target.Path == "" {
					t.Fatalf("prepared target = %#v, %v", target, err)
				}
				switch targetState {
				case "archived":
					if _, err := puaWorkspace.ArchiveResource(prepared.Target); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Rename(filepath.Join(workspace.Path, filepath.FromSlash(target.Path)), filepath.Join(t.TempDir(), "removed-target")); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown target state %q", targetState)
				}

				// Model a daemon restart after mailbox acceptance but before the
				// Scheduler source checkpoint committed. The portable target is no
				// longer deliverable, but that cannot revoke the durable handoff.
				restartedManager := newAgentManager(manager.server)
				restartedManager.now = manager.now
				manager.server.agents = restartedManager
				restarted := newNativeScheduler(restartedManager, workspace)
				deadline, err := restarted.Reconcile(context.Background(), manager.now())
				if err != nil || !deadline.IsZero() {
					t.Fatalf("accepted replay deadline = %s, %v", deadline, err)
				}
				persisted, err := restarted.schedulerRuntime(schedule.ID)
				if err != nil || persisted.Prepared != nil || persisted.EffectiveState != app.ScheduleStateCompleted ||
					persisted.LastOutcome != schedulerOutcomeAccepted || persisted.LastOccurrenceAt != at.Format(time.RFC3339Nano) ||
					persisted.LastError != "" || persisted.AttentionTarget != "" || persisted.NextRunAt != "" || persisted.RetryAt != "" {
					t.Fatalf("accepted unavailable-target checkpoint = %#v, %v", persisted, err)
				}
				if got := schedulerAgentHubInputCount(fake); got != 0 {
					t.Fatalf("accepted unavailable-target replay external inputs = %d, want 0", got)
				}

				store, err := loadResourceMailboxStoreForRead(workspace.Path, prepared.Target)
				if err != nil {
					t.Fatal(err)
				}
				copies := 0
				for _, message := range store.Mailbox.Messages {
					if !message.receipt && message.ID == prepared.MessageID {
						copies++
					}
				}
				for _, receipt := range store.Receipts.Receipts {
					if receipt.ID == prepared.MessageID {
						copies++
					}
				}
				for _, expired := range store.Receipts.Expired {
					if expired.ID == prepared.MessageID {
						copies++
					}
				}
				if copies != 1 {
					t.Fatalf("accepted unavailable-target evidence copies = %d, want 1", copies)
				}

				secondManager := newAgentManager(manager.server)
				secondManager.now = manager.now
				manager.server.agents = secondManager
				if deadline, err := newNativeScheduler(secondManager, workspace).Reconcile(context.Background(), manager.now()); err != nil || !deadline.IsZero() {
					t.Fatalf("second accepted replay deadline = %s, %v", deadline, err)
				}
				second, err := newNativeScheduler(secondManager, workspace).schedulerRuntime(schedule.ID)
				if err != nil || second.Prepared != nil || second.EffectiveState != app.ScheduleStateCompleted || second.LastOutcome != schedulerOutcomeAccepted {
					t.Fatalf("second accepted unavailable-target checkpoint = %#v, %v", second, err)
				}
				if got := schedulerAgentHubInputCount(fake); got != 0 {
					t.Fatalf("second accepted unavailable-target replay external inputs = %d, want 0", got)
				}
			})
		}
	}
}

func TestNativeSchedulerHonorsPersistedDeliveryBackoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Retry safely", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	now := anchor.Add(time.Second)
	runtime, err := initialScheduleRuntime(schedule, now)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := native.prepareOccurrence(schedule, anchor, anchor, anchor.Add(time.Minute), 1, false, time.Time{}, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(5 * time.Second)
	runtime.Prepared = &prepared
	runtime.RetryAt = retryAt.Format(time.RFC3339Nano)
	runtime.RetryCount = 1
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}
	deadline, err := native.Reconcile(context.Background(), now)
	if err != nil || !deadline.Equal(retryAt) {
		t.Fatalf("persisted retry deadline = %s, %v", deadline, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("occurrence delivered before retry: %#v", messages)
	}
	if _, err := native.Reconcile(context.Background(), retryAt); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("occurrence was not delivered at retry: %#v", messages)
	}
}

func TestNativeSchedulerMetadataEditPreservesPreparedRetry(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	failCatalog := true
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failCatalog && r.Method == http.MethodGet && r.URL.Path == "/v1/agents" {
			failCatalog = false
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "runtime_unavailable", "message": "synthetic catalog outage", "retryable": true,
			}})
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Retry immutable work", Condition: "every minute", Guard: "only when ready", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := anchor.Add(time.Second)
	native := newNativeScheduler(manager, workspace)
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	before, err := native.schedulerRuntime(created.ID)
	if err != nil || before.Prepared == nil || before.RetryAt == "" || before.RetryCount != 1 {
		t.Fatalf("transient prepared retry = %#v, %v", before, err)
	}
	if before.Target != created.Target {
		t.Fatalf("initial runtime target = %q, want %q", before.Target, created.Target)
	}
	prepared := *before.Prepared
	if prepared.ScheduleRevision != created.Revision || prepared.Causation == nil || prepared.Causation.ScheduleRevision != created.Revision {
		t.Fatalf("prepared retry causation = %#v", prepared)
	}
	// Model a checkpoint written before runtime target identity was added. A
	// frozen prepared target is sufficient to establish sameness safely.
	before.Target = ""
	if err := native.storeSchedulerRuntime(created.ID, before); err != nil {
		t.Fatal(err)
	}

	description := "Retry immutable work with clearer wording"
	condition := "every minute after metadata review"
	guard := "only when reviewed and ready"
	trigger := *created.Trigger
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision,
		Description: &description, Condition: &condition, Guard: &guard, Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	after, err := native.schedulerRuntime(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.Revision = updated.Revision
	want.TriggerDigest = mustSchedulerTriggerDigest(t, updated.Trigger)
	want.Target = updated.Target
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("metadata edit runtime = %#v, want %#v", after, want)
	}
	assertPreparedOccurrenceEqual(t, after.Prepared, prepared)

	retryAt := generationTime(before.RetryAt)
	if _, err := native.Reconcile(context.Background(), retryAt); err != nil {
		t.Fatal(err)
	}
	assertDeliveredPreparedOccurrence(t, scheduleOccurrenceMessages(t, workspace.Path, created.Target), prepared)
	if _, err := native.Reconcile(context.Background(), retryAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
}

func TestNativeSchedulerSnapshotProjectsMetadataRevisionLag(t *testing.T) {
	t.Run("completed one-time", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2099, time.January, 2, 3, 4, 5, 0, time.UTC)
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Run once", Condition: "at the configured time", Target: "workspace",
			Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
		})
		if err != nil {
			t.Fatal(err)
		}
		native := newNativeScheduler(manager, workspace)
		runtime := schedulerScheduleRuntime{
			Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
			EffectiveState: app.ScheduleStateCompleted, LastOccurrenceAt: at.Format(time.RFC3339Nano),
			LastOutcome: schedulerOutcomeAccepted, LastError: "preserved completion detail",
		}
		if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
			t.Fatal(err)
		}
		description := "Run once with clearer wording"
		trigger := *created.Trigger
		updated, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
			Description: &description, Trigger: &trigger,
		})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := native.Snapshot(at.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		got := snapshot.Schedules[0]
		if got.Revision != updated.Revision || got.Description != description || got.EffectiveState != app.ScheduleStateCompleted ||
			got.LastOccurrenceAt != runtime.LastOccurrenceAt || got.LastOutcome != runtime.LastOutcome || got.LastError != runtime.LastError ||
			got.NextRunAt != "" || snapshot.NextWakeAt != "" {
			t.Fatalf("completed metadata-lag snapshot = %#v", snapshot)
		}
	})

	t.Run("attention", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		anchor := time.Date(2099, time.February, 3, 4, 5, 6, 0, time.UTC)
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Inspect target", Condition: "every five minutes", Target: "project1.task1",
			Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 300, AnchorAt: anchor.Format(time.RFC3339Nano)},
		})
		if err != nil {
			t.Fatal(err)
		}
		native := newNativeScheduler(manager, workspace)
		prepared := schedulerPreparedOccurrence{ScheduleID: created.ID, ScheduleRevision: created.Revision, Target: created.Target}
		runtime := schedulerScheduleRuntime{
			Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger),
			EffectiveState: schedulerOutcomeAttention, LastOutcome: schedulerOutcomeAttention,
			LastError: "target resource is archived", AttentionTarget: created.Target, Prepared: &prepared,
		}
		if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
			t.Fatal(err)
		}
		condition := "every five minutes after review"
		trigger := *created.Trigger
		if _, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
			Condition: &condition, Trigger: &trigger,
		}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := native.Snapshot(anchor)
		if err != nil {
			t.Fatal(err)
		}
		got := snapshot.Schedules[0]
		if got.EffectiveState != schedulerOutcomeAttention || got.LastOutcome != schedulerOutcomeAttention ||
			got.LastError != runtime.LastError || got.NextRunAt != "" || snapshot.NextWakeAt != "" {
			t.Fatalf("attention metadata-lag snapshot = %#v", snapshot)
		}
	})

	for _, test := range []struct {
		name       string
		retryDelay time.Duration
	}{
		{name: "prepared deadline"},
		{name: "retry deadline", retryDelay: 45 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			anchor := time.Date(2099, time.March, 4, 5, 6, 7, 0, time.UTC)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Retry work", Condition: "every minute", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := anchor.Add(10 * time.Minute)
			prepared := schedulerPreparedOccurrence{ScheduleID: created.ID, ScheduleRevision: created.Revision, Target: created.Target}
			runtime := schedulerScheduleRuntime{
				Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
				EffectiveState: app.ScheduleStateActive, NextRunAt: anchor.Add(11 * time.Minute).Format(time.RFC3339Nano),
				LastOccurrenceAt: anchor.Add(9 * time.Minute).Format(time.RFC3339Nano), LastOutcome: schedulerOutcomeBusy,
				LastError: "preserved retry detail", Prepared: &prepared,
			}
			wantWake := now
			if test.retryDelay != 0 {
				wantWake = now.Add(test.retryDelay)
				runtime.RetryAt = wantWake.Format(time.RFC3339Nano)
				runtime.RetryCount = 2
			}
			native := newNativeScheduler(manager, workspace)
			if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
				t.Fatal(err)
			}
			guard := "only after metadata review"
			trigger := *created.Trigger
			if _, err := native.Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
				Guard: &guard, Trigger: &trigger,
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := native.Snapshot(now)
			if err != nil {
				t.Fatal(err)
			}
			got := snapshot.Schedules[0]
			if got.EffectiveState != app.ScheduleStateActive || got.NextRunAt != runtime.NextRunAt ||
				got.LastOccurrenceAt != runtime.LastOccurrenceAt || got.LastOutcome != runtime.LastOutcome || got.LastError != runtime.LastError ||
				snapshot.NextWakeAt != wantWake.Format(time.RFC3339Nano) {
				t.Fatalf("%s metadata-lag snapshot = %#v", test.name, snapshot)
			}
		})
	}
}

func TestNativeSchedulerSnapshotRejectsUnsafeRevisionLag(t *testing.T) {
	t.Run("target edit", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
		at := time.Date(2099, time.April, 5, 6, 7, 8, 0, time.UTC)
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Run on Task", Condition: "once", Target: "project1.task1",
			Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
		})
		if err != nil {
			t.Fatal(err)
		}
		prepared := schedulerPreparedOccurrence{ScheduleID: created.ID, ScheduleRevision: created.Revision, Target: created.Target}
		runtime := schedulerScheduleRuntime{
			Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
			EffectiveState: schedulerOutcomeAttention, LastOutcome: schedulerOutcomeAttention, LastError: "old target unavailable",
			AttentionTarget: created.Target, Prepared: &prepared,
		}
		native := newNativeScheduler(manager, workspace)
		if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
			t.Fatal(err)
		}
		target := "workspace"
		trigger := *created.Trigger
		if _, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
			Target: &target, Trigger: &trigger,
		}); err != nil {
			t.Fatal(err)
		}
		assertPortableSchedulerSnapshot(t, native, at, app.ScheduleStateActive)
	})

	t.Run("trigger edit", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
		at := time.Date(2099, time.May, 6, 7, 8, 9, 0, time.UTC)
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Run once", Condition: "once", Target: "workspace",
			Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
		})
		if err != nil {
			t.Fatal(err)
		}
		runtime := schedulerScheduleRuntime{
			Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
			EffectiveState: app.ScheduleStateCompleted, LastOccurrenceAt: at.Format(time.RFC3339Nano), LastOutcome: schedulerOutcomeAccepted,
		}
		native := newNativeScheduler(manager, workspace)
		if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
			t.Fatal(err)
		}
		trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Add(time.Hour).Format(time.RFC3339Nano)}
		if _, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Trigger: &trigger,
		}); err != nil {
			t.Fatal(err)
		}
		assertPortableSchedulerSnapshot(t, native, at, app.ScheduleStateActive)
	})

	for _, test := range []struct {
		name      string
		configure func(*testing.T, app.Schedule) schedulerScheduleRuntime
		wantState string
	}{
		{
			name: "runtime newer",
			configure: func(t *testing.T, schedule app.Schedule) schedulerScheduleRuntime {
				return schedulerScheduleRuntime{
					Revision: schedule.Revision + 2, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger), Target: schedule.Target,
					EffectiveState: app.ScheduleStateCompleted, LastOccurrenceAt: schedule.Trigger.At, LastOutcome: schedulerOutcomeAccepted,
				}
			},
			wantState: app.ScheduleStateActive,
		},
		{
			name: "unknown target",
			configure: func(t *testing.T, schedule app.Schedule) schedulerScheduleRuntime {
				return schedulerScheduleRuntime{
					Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger),
					EffectiveState: app.ScheduleStateCompleted, LastOccurrenceAt: schedule.Trigger.At, LastOutcome: schedulerOutcomeAccepted,
				}
			},
			wantState: app.ScheduleStateActive,
		},
		{
			name: "corrupt completion",
			configure: func(t *testing.T, schedule app.Schedule) schedulerScheduleRuntime {
				return schedulerScheduleRuntime{
					Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger), Target: schedule.Target,
					EffectiveState: app.ScheduleStateCompleted, LastOccurrenceAt: generationTime(schedule.Trigger.At).Add(time.Minute).Format(time.RFC3339Nano),
					LastOutcome: schedulerOutcomeAccepted,
				}
			},
			wantState: app.ScheduleStateActive,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
			at := time.Date(2099, time.June, 7, 8, 9, 10, 0, time.UTC)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Run once", Condition: "once", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			native := newNativeScheduler(manager, workspace)
			if err := native.storeSchedulerRuntime(created.ID, test.configure(t, created)); err != nil {
				t.Fatal(err)
			}
			description := "Run once after metadata review"
			trigger := *created.Trigger
			if _, err := native.Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision,
				Description: &description, Trigger: &trigger,
			}); err != nil {
				t.Fatal(err)
			}
			assertPortableSchedulerSnapshot(t, native, at, test.wantState)
		})
	}
}

func assertPortableSchedulerSnapshot(t *testing.T, native *NativeScheduler, now time.Time, wantState string) {
	t.Helper()
	snapshot, err := native.Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Schedules) != 1 {
		t.Fatalf("portable snapshot schedule count = %d, want 1", len(snapshot.Schedules))
	}
	got := snapshot.Schedules[0]
	if got.EffectiveState != wantState || got.NextRunAt != "" || got.LastOccurrenceAt != "" ||
		got.LastOutcome != "" || got.LastError != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("unsafe revision lag exposed runtime = %#v", snapshot)
	}
}

func TestNativeSchedulerMetadataEditReplaysAcceptedPreparedOneTime(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Accept exactly once", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(time.Second)
	native := newNativeScheduler(manager, workspace)
	runtime, err := initialScheduleRuntime(created, now)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := native.prepareOccurrence(created, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Prepared = &prepared
	if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
		t.Fatal(err)
	}
	assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)

	description := "Accept exactly once with clearer wording"
	trigger := *created.Trigger
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision, Description: &description, Trigger: &trigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
	after, err := native.schedulerRuntime(created.ID)
	if err != nil || after.Revision != updated.Revision || after.Prepared != nil || after.EffectiveState != app.ScheduleStateCompleted || after.LastOutcome != schedulerOutcomeAccepted || after.LastOccurrenceAt != prepared.ScheduledFor {
		t.Fatalf("accepted prepared replay checkpoint = %#v, %v", after, err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target)
	assertDeliveredPreparedOccurrence(t, messages, prepared)
}

func TestNativeSchedulerBindingAttentionRecoversPreparedOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	rewriteSchedulerTestProfiles(t, configPath, nil)
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Recover binding", Condition: "every minute", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := anchor.Add(3*time.Minute + 10*time.Second)
	native := newNativeScheduler(manager, workspace)
	deadline, err := native.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.IsZero() {
		t.Fatalf("binding attention deadline = %s, want none", deadline)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.LastOccurrenceAt != "" {
		t.Fatalf("binding attention advanced the occurrence: %#v", runtime)
	}
	prepared := *runtime.Prepared
	if prepared.ScheduledFor != anchor.Format(time.RFC3339Nano) || prepared.CoalescedThrough != anchor.Add(3*time.Minute).Format(time.RFC3339Nano) || prepared.CoalescedCount != 4 || prepared.NextRunAt != anchor.Add(4*time.Minute).Format(time.RFC3339Nano) || prepared.Reason != schedulerOccurrenceReasonCoalesced {
		t.Fatalf("binding attention did not freeze the coalesced occurrence: %#v", prepared)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("binding attention accepted mailbox work: %#v", messages)
	}
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Revision != schedule.Revision || resumed.State != app.ScheduleStateActive {
		t.Fatalf("attention retry mutated the portable definition: %#v", resumed)
	}
	runtime, err = native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.NextRunAt != "" || runtime.RetryAt != "" {
		t.Fatalf("resume discarded binding attention occurrence: %#v, %v", runtime, err)
	}
	assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
	if snapshot, snapshotErr := native.Snapshot(now); snapshotErr != nil || snapshot.NextWakeAt != "" {
		t.Fatalf("binding attention retry acquired a deadline: %#v, %v", snapshot, snapshotErr)
	}

	rewriteSchedulerTestProfiles(t, configPath, []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}})
	if _, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID}); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	assertDeliveredPreparedOccurrence(t, messages, prepared)
	runtime, err = native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateActive || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != prepared.CoalescedThrough || runtime.NextRunAt != prepared.NextRunAt {
		t.Fatalf("recovered occurrence checkpoint = %#v, %v", runtime, err)
	}
	if _, err := native.Reconcile(context.Background(), now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("recovery duplicated occurrence: %#v", messages)
	}
}

func TestNativeSchedulerLegacyIntervalCompletesAtPersistenceBoundary(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Deliver the final valid occurrence", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{
			Type: app.ScheduleTriggerInterval, EverySeconds: 60,
			AnchorAt: latest.Add(-time.Minute).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model a definition accepted by an older release, before interval
	// mutations required a persistable successor.
	rewriteSchedulerTestIntervalBoundary(t, puaWorkspace, schedule.ID, latest, time.Time{})
	native := newNativeScheduler(manager, workspace)
	deadline, err := native.Reconcile(context.Background(), latest)
	if err != nil || !deadline.IsZero() {
		t.Fatalf("terminal reconcile deadline = %s, %v", deadline, err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	if len(messages) != 1 || messages[0].Causation == nil || messages[0].Causation.ScheduledFor != latest.Format(time.RFC3339Nano) {
		t.Fatalf("terminal occurrence messages = %#v", messages)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.Prepared != nil || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.LastOccurrenceAt != latest.Format(time.RFC3339Nano) || runtime.LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("terminal interval runtime = %#v, %v", runtime, err)
	}
	if _, err := native.Reconcile(context.Background(), latest.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if messages = scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 {
		t.Fatalf("terminal interval replayed occurrence: %#v", messages)
	}

	exhausted, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Recover after the final valid occurrence", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{
			Type: app.ScheduleTriggerInterval, EverySeconds: 60,
			AnchorAt: latest.Add(-time.Minute).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rewriteSchedulerTestIntervalBoundary(t, puaWorkspace, exhausted.ID, latest, latest)
	if deadline, err = native.Reconcile(context.Background(), latest); err != nil || !deadline.IsZero() {
		t.Fatalf("exhausted recovery deadline = %s, %v", deadline, err)
	}
	exhaustedRuntime, err := native.schedulerRuntime(exhausted.ID)
	if err != nil || exhaustedRuntime.EffectiveState != app.ScheduleStateCompleted || exhaustedRuntime.LastOutcome != schedulerOutcomeRangeExhausted || exhaustedRuntime.NextRunAt != "" || exhaustedRuntime.RetryAt != "" || !strings.Contains(exhaustedRuntime.LastError, app.ErrScheduleOccurrenceOutOfRange.Error()) {
		t.Fatalf("exhausted interval runtime = %#v, %v", exhaustedRuntime, err)
	}
	if messages = scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 {
		t.Fatalf("already exhausted interval delivered work: %#v", messages)
	}
}

func TestNativeSchedulerBusyRepeatingBoundaryCompletesWithoutSuccessor(t *testing.T) {
	tests := []struct {
		name          string
		trigger       app.ScheduleTrigger
		due           time.Time
		wantState     string
		wantNextRunAt string
	}{
		{
			name: "final interval occurrence",
			trigger: app.ScheduleTrigger{
				Type: app.ScheduleTriggerInterval, EverySeconds: 60,
				AnchorAt: time.Date(9999, time.December, 31, 23, 58, 59, 999999999, time.UTC).Format(time.RFC3339Nano),
			},
			due:       time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC),
			wantState: app.ScheduleStateCompleted,
		},
		{
			name: "final sparse cron occurrence",
			trigger: app.ScheduleTrigger{
				Type: app.ScheduleTriggerCron, Cron: "0 0 0 29 2 *", TimeZone: "UTC",
			},
			due:       time.Date(9996, time.February, 29, 0, 0, 0, 0, time.UTC),
			wantState: app.ScheduleStateCompleted,
		},
		{
			name: "ordinary occurrence with future successor",
			trigger: app.ScheduleTrigger{
				Type: app.ScheduleTriggerInterval, EverySeconds: 60,
				AnchorAt: time.Date(2030, time.January, 2, 3, 4, 5, 123456789, time.UTC).Format(time.RFC3339Nano),
			},
			due:           time.Date(2030, time.January, 2, 3, 4, 5, 123456789, time.UTC),
			wantState:     app.ScheduleStateActive,
			wantNextRunAt: time.Date(2030, time.January, 2, 3, 5, 5, 123456789, time.UTC).Format(time.RFC3339Nano),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Skip work on the busy target", Condition: "repeat at the configured time",
				Target: "project1.task1", Trigger: &test.trigger,
			})
			if err != nil {
				t.Fatal(err)
			}

			native := newNativeScheduler(manager, workspace)
			runtime := schedulerScheduleRuntime{
				Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger), Target: schedule.Target,
				EffectiveState: app.ScheduleStateActive, NextRunAt: test.due.Format(time.RFC3339Nano),
			}
			if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
				t.Fatal(err)
			}
			stamp := test.due.Add(-time.Minute).Format(time.RFC3339Nano)
			if _, err := mutateResourceMailboxStoreForResource(workspace.Path, schedule.Target, func(store *resourceMailboxStore) error {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "busy-target-message", Sequence: store.Mailbox.NextSequence, ResourceID: schedule.Target,
					Text: "already waiting", Role: "user", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageQueued,
					AcceptedAt: stamp, UpdatedAt: stamp,
				})
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			deadline, err := native.Reconcile(context.Background(), test.due)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := native.schedulerRuntime(schedule.ID)
			if err != nil || persisted.EffectiveState != test.wantState || persisted.Prepared != nil ||
				persisted.NextRunAt != test.wantNextRunAt || persisted.RetryAt != "" || persisted.RetryCount != 0 ||
				persisted.AttentionTarget != "" || persisted.LastOccurrenceAt != test.due.Format(time.RFC3339Nano) ||
				persisted.LastOutcome != schedulerOutcomeBusy || persisted.LastError != "" {
				t.Fatalf("busy skip runtime = %#v, %v", persisted, err)
			}
			wantDeadline := generationTime(test.wantNextRunAt)
			if !deadline.Equal(wantDeadline) {
				t.Fatalf("busy skip deadline = %s, want %s", deadline, wantDeadline)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
				t.Fatalf("busy skip appended Scheduler occurrence: %#v", messages)
			}

			restartedManager := newAgentManager(manager.server)
			restartedManager.now = func() time.Time { return test.due }
			manager.server.agents = restartedManager
			restarted := newNativeScheduler(restartedManager, workspace)
			snapshot, err := restarted.Snapshot(test.due)
			if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != test.wantState ||
				snapshot.Schedules[0].NextRunAt != test.wantNextRunAt || snapshot.Schedules[0].LastOccurrenceAt != test.due.Format(time.RFC3339Nano) ||
				snapshot.Schedules[0].LastOutcome != schedulerOutcomeBusy || snapshot.Schedules[0].LastError != "" ||
				snapshot.NextWakeAt != test.wantNextRunAt {
				t.Fatalf("restarted busy skip snapshot = %#v, %v", snapshot, err)
			}
			if _, err := restarted.Reconcile(context.Background(), test.due); err != nil {
				t.Fatal(err)
			}
			replayed, err := restarted.schedulerRuntime(schedule.ID)
			if err != nil || !reflect.DeepEqual(replayed, persisted) {
				t.Fatalf("busy skip changed after restart: before=%#v after=%#v err=%v", persisted, replayed, err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
				t.Fatalf("restarted busy skip appended Scheduler occurrence: %#v", messages)
			}
		})
	}
}

func TestNativeSchedulerIntervalRangeFailureDoesNotRetry(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Stop outside the persistence range", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{
			Type: app.ScheduleTriggerInterval, EverySeconds: 60,
			AnchorAt: latest.Add(-time.Minute).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rewriteSchedulerTestIntervalBoundary(t, puaWorkspace, schedule.ID, latest, time.Time{})
	beyondPersistence := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	native := newNativeScheduler(manager, workspace)
	deadline, err := native.Reconcile(context.Background(), beyondPersistence)
	if err != nil || !deadline.IsZero() {
		t.Fatalf("range failure deadline = %s, %v", deadline, err)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.LastOutcome != schedulerOutcomeAttention || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.RetryCount != 0 || !strings.Contains(runtime.LastError, app.ErrScheduleOccurrenceOutOfRange.Error()) {
		t.Fatalf("non-retryable range failure = %#v, %v", runtime, err)
	}
	if _, err := native.Reconcile(context.Background(), beyondPersistence.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, err := native.schedulerRuntime(schedule.ID)
	if err != nil || !reflect.DeepEqual(after, runtime) {
		t.Fatalf("range failure changed on audit: before=%#v after=%#v err=%v", runtime, after, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("range failure delivered work: %#v", messages)
	}
}

func TestNativeSchedulerCronSuccessorFailureDoesNotRetry(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	native := newNativeScheduler(manager, workspace)
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	runtime := schedulerScheduleRuntime{
		Revision:       1,
		EffectiveState: app.ScheduleStateActive,
		NextRunAt:      now.Format(time.RFC3339Nano),
	}
	if err := native.storeSchedulerRuntime("schedule-cron-contract", runtime); err != nil {
		t.Fatal(err)
	}
	if err := native.recordScheduleError("schedule-cron-contract", runtime, now, app.ErrScheduleCronSuccessorUnavailable); err != nil {
		t.Fatal(err)
	}
	persisted, err := native.schedulerRuntime("schedule-cron-contract")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.EffectiveState != schedulerOutcomeAttention || persisted.LastOutcome != schedulerOutcomeAttention || persisted.NextRunAt != "" || persisted.RetryAt != "" || persisted.RetryCount != 0 || persisted.LastError != app.ErrScheduleCronSuccessorUnavailable.Error() {
		t.Fatalf("cron successor failure runtime = %#v", persisted)
	}
}

func prepareResourceBindingAttention(t *testing.T) (*agentManager, serveWorkspace, app.Schedule, schedulerPreparedOccurrence) {
	t.Helper()
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	t.Cleanup(hub.Close)
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	rewriteSchedulerTestProfiles(t, configPath, nil)
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Wake restored binding", Condition: "every minute", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := anchor.Add(3*time.Minute + 10*time.Second)
	manager.now = func() time.Time { return now }
	native := newNativeScheduler(manager, workspace)
	if deadline, reconcileErr := native.Reconcile(context.Background(), now); reconcileErr != nil || !deadline.IsZero() {
		t.Fatalf("create binding attention: deadline=%s err=%v", deadline, reconcileErr)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil {
		t.Fatalf("binding attention runtime = %#v, %v", runtime, err)
	}
	return manager, workspace, schedule, *runtime.Prepared
}

func TestSchedulerActiveResumeRefreshesRecoveredAttentionDeadline(t *testing.T) {
	for _, test := range []struct {
		name string
		now  func(schedulerPreparedOccurrence) time.Time
	}{
		{
			name: "future next occurrence",
			now: func(prepared schedulerPreparedOccurrence) time.Time {
				return generationTime(prepared.NextRunAt).Add(-time.Second)
			},
		},
		{
			name: "already due next occurrence",
			now: func(prepared schedulerPreparedOccurrence) time.Time {
				return generationTime(prepared.NextRunAt).Add(time.Second)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, schedule, prepared := prepareResourceBindingAttention(t)
			// Recover the binding which originally held the immutable prepared
			// occurrence in attention_required.
			rewriteSchedulerTestProfiles(t, manager.server.config, []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}})
			now := test.now(prepared)
			manager.now = func() time.Time { return now }
			_ = manager.takeReconcileRequests()
			select {
			case <-manager.reconcileWake:
			default:
			}

			resumed, err := manager.server.changeNativeSchedule(context.Background(), workspace,
				newNativeScheduler(manager, workspace), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Revision != schedule.Revision || resumed.State != app.ScheduleStateActive {
				t.Fatalf("active attention resume changed portable definition: %#v", resumed)
			}
			runtime, err := newNativeScheduler(manager, workspace).schedulerRuntime(schedule.ID)
			if err != nil || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateActive || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.NextRunAt != prepared.NextRunAt {
				t.Fatalf("active attention resume runtime = %#v, %v", runtime, err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
			if len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("active attention resume messages = %#v, want %s", messages, prepared.MessageID)
			}
			if request := manager.takeReconcileRequests(); request&reconcileScheduler == 0 {
				t.Fatalf("active attention resume reconcile request = %08b, want Scheduler", request)
			}
			select {
			case <-manager.reconcileWake:
			default:
				t.Fatal("active attention resume did not wake the reconcile loop")
			}
			select {
			case <-manager.reconcileWake:
				t.Fatal("active attention resume woke the reconcile loop twice")
			default:
			}
			wantDeadline := generationTime(prepared.NextRunAt)
			if wantDeadline.Before(now) {
				wantDeadline = now
			}
			if deadline := manager.nextSchedulerReconcileDeadline(now); !deadline.Equal(wantDeadline) {
				t.Fatalf("recovered Scheduler deadline = %s, want %s", deadline, wantDeadline)
			}
			if _, err := newNativeScheduler(manager, workspace).Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
		})
	}
}

func TestSchedulerActiveResumeNoOpAndFailureDoNotWake(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	manager.now = func() time.Time { return now }
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Remain active", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(time.Minute).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	before, err := native.schedulerRuntime(schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = manager.takeReconcileRequests()
	select {
	case <-manager.reconcileWake:
	default:
	}

	resumed, err := manager.server.changeNativeSchedule(context.Background(), workspace, native,
		NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID})
	if err != nil || resumed.Revision != schedule.Revision {
		t.Fatalf("active no-op resume = %#v, %v", resumed, err)
	}
	after, err := native.schedulerRuntime(schedule.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("active no-op resume runtime changed: before=%#v after=%#v err=%v", before, after, err)
	}
	if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
		t.Fatalf("active no-op resume requested Scheduler reconciliation: %08b", request)
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("active no-op resume woke the reconcile loop")
	default:
	}

	_, err = manager.server.changeNativeSchedule(context.Background(), workspace, native,
		NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: "schedule-ffffffffffffffffffffffff"})
	if !errors.Is(err, app.ErrScheduleNotFound) {
		t.Fatalf("missing active resume error = %v", err)
	}
	if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
		t.Fatalf("failed active resume requested Scheduler reconciliation: %08b", request)
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("failed active resume woke the reconcile loop")
	default:
	}
}

func TestSchedulerActiveResumeOwnershipLossRejectsQueuedChange(t *testing.T) {
	manager, workspace, schedule, prepared := prepareResourceBindingAttention(t)
	rewriteSchedulerTestProfiles(t, manager.server.config, []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}})
	manager.server.locks = newWorkspaceLockManager("127.0.0.1:4936", manager.server.config)
	t.Cleanup(manager.server.locks.closeAll)
	if _, err := manager.server.locks.acquire(workspace.Path); err != nil {
		t.Fatal(err)
	}
	_ = manager.takeReconcileRequests()
	select {
	case <-manager.reconcileWake:
	default:
	}
	controller, release := holdResourceController(t, manager, workspace, schedule.Target)
	done := make(chan error, 1)
	go func() {
		_, err := manager.server.changeNativeSchedule(context.Background(), workspace,
			newNativeScheduler(manager, workspace), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID})
		done <- err
	}()
	waitForResourceControllerQueue(t, controller, 1)
	manager.server.locks.release(workspace.Path)
	release()
	if err := <-done; err == nil {
		t.Fatal("attention recovery crossed ownership loss")
	} else {
		requireWorkspaceOwnershipError(t, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("ownership-lost attention recovery messages = %#v, want none for %s", messages, prepared.MessageID)
	}
	if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
		t.Fatalf("ownership-lost active resume requested Scheduler reconciliation: %08b", request)
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("rejected active resume woke reconciliation")
	default:
	}
}

func TestResourceBindingChangeWakesAttentionHeldScheduler(t *testing.T) {
	manager, workspace, schedule, prepared := prepareResourceBindingAttention(t)
	_ = manager.takeReconcileRequests()
	select {
	case <-manager.reconcileWake:
	default:
	}

	body, err := json.Marshal(app.AgentBinding{Kind: "agent", Name: "fake-agent"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	manager.server.updateResourceAgentBinding(recorder, httptest.NewRequest(http.MethodPut, "/agent-binding", bytes.NewReader(body)), workspace.ID, schedule.Target)
	if recorder.Code != http.StatusOK {
		t.Fatalf("binding restoration returned %d: %s", recorder.Code, recorder.Body.String())
	}
	request := manager.takeReconcileRequests()
	if request&reconcileScheduler == 0 {
		t.Fatalf("binding restoration reconcile request = %08b, want Scheduler", request)
	}
	select {
	case <-manager.reconcileWake:
	default:
		t.Fatal("binding restoration did not wake the reconcile loop")
	}
	if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	if len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("binding wake delivered occurrences = %#v, want %s", messages, prepared.MessageID)
	}
	if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("binding wake duplicated occurrence: %#v", messages)
	}
}

func TestResourceBindingControllerJobCancellationBoundaries(t *testing.T) {
	t.Run("cancelled before start", func(t *testing.T) {
		manager, workspace, schedule, prepared := prepareResourceBindingAttention(t)
		_ = manager.takeReconcileRequests()
		select {
		case <-manager.reconcileWake:
		default:
		}
		controller, release := holdResourceController(t, manager, workspace, schedule.Target)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, _, err := manager.updateResourceAgentBinding(ctx, workspace, schedule.Target, app.AgentBinding{Kind: "agent", Name: "fake-agent"})
			done <- err
		}()
		waitForResourceControllerQueue(t, controller, 1)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled queued binding mutation = %v", err)
		}
		release()
		if err := manager.withResourceController(context.Background(), workspace, schedule.Target, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := puaWorkspace.ResourceAgentBinding(schedule.Target)
		wantBinding := app.AgentBinding{Kind: "profile", Name: "default"}
		if err != nil || binding != wantBinding {
			t.Fatalf("cancelled queued binding = %#v, %v; want %#v", binding, err, wantBinding)
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
			t.Fatalf("cancelled queued binding requested Scheduler reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("cancelled queued binding woke the reconcile loop")
		default:
		}
		runtime, err := newNativeScheduler(manager, workspace).schedulerRuntime(schedule.ID)
		if err != nil || runtime.Prepared == nil || runtime.Prepared.MessageID != prepared.MessageID || runtime.EffectiveState != schedulerOutcomeAttention {
			t.Fatalf("cancelled queued binding attention = %#v, %v", runtime, err)
		}
		if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
			t.Fatalf("cancelled queued binding delivered occurrences: %#v", messages)
		}
	})

	t.Run("cancelled after start", func(t *testing.T) {
		manager, workspace, schedule, prepared := prepareResourceBindingAttention(t)
		_ = manager.takeReconcileRequests()
		select {
		case <-manager.reconcileWake:
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseMutation := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseMutation()
		done := make(chan resourceBindingMutationOutcome, 1)
		go func() {
			done <- manager.runResourceBindingControllerJob(ctx, workspace, schedule.Target, func(jobCtx context.Context) resourceBindingMutationOutcome {
				close(started)
				<-release
				return manager.updateResourceAgentBindingLocked(jobCtx, workspace, schedule.Target, app.AgentBinding{Kind: "agent", Name: "fake-agent"})
			})
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("binding mutation did not start")
		}
		cancel()
		if outcome := <-done; !errors.Is(outcome.err, context.Canceled) || outcome.persisted {
			t.Fatalf("cancelled running binding caller = %#v", outcome)
		}
		if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
			t.Fatalf("blocked binding mutation delivered occurrences: %#v", messages)
		}
		releaseMutation()
		if err := manager.withResourceController(context.Background(), workspace, schedule.Target, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := puaWorkspace.ResourceAgentBinding(schedule.Target)
		wantBinding := app.AgentBinding{Kind: "agent", Name: "fake-agent"}
		if err != nil || binding != wantBinding {
			t.Fatalf("cancelled running binding = %#v, %v; want %#v", binding, err, wantBinding)
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler == 0 {
			t.Fatalf("completed running binding reconcile request = %08b, want Scheduler", request)
		}
		if request := manager.takeReconcileRequests(); request != 0 {
			t.Fatalf("completed running binding requested duplicate reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
		default:
			t.Fatal("completed running binding did not wake the reconcile loop")
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("completed running binding woke the reconcile loop twice")
		default:
		}
		if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
			t.Fatal(err)
		}
		messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
		if len(messages) != 1 || messages[0].ID != prepared.MessageID {
			t.Fatalf("cancelled binding wake delivered occurrences = %#v, want %s", messages, prepared.MessageID)
		}
		runtime, err := newNativeScheduler(manager, workspace).schedulerRuntime(schedule.ID)
		if err != nil || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted {
			t.Fatalf("cancelled binding acceptance checkpoint = %#v, %v", runtime, err)
		}
		if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
			t.Fatal(err)
		}
		if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
			t.Fatalf("cancelled binding wake duplicated occurrence: %#v", messages)
		}
	})
}

func TestResourceBindingMutationRequestsSchedulerOnlyAfterChangedReconciliation(t *testing.T) {
	t.Run("no-op", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		_ = manager.takeReconcileRequests()
		body := bytes.NewBufferString(`{"kind":"profile","name":"default"}`)
		recorder := httptest.NewRecorder()
		manager.server.updateResourceAgentBinding(recorder, httptest.NewRequest(http.MethodPut, "/agent-binding", body), workspace.ID, "project1.task1")
		if recorder.Code != http.StatusOK {
			t.Fatalf("no-op binding update returned %d: %s", recorder.Code, recorder.Body.String())
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
			t.Fatalf("no-op binding update requested Scheduler reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("no-op binding update woke the reconcile loop")
		default:
		}
	})

	t.Run("reconciliation failure after persistence", func(t *testing.T) {
		manager, workspace, configPath := newRuntimeTestManager(t, "http://127.0.0.1:1")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		record := generationRecord{
			ID: "gen-binding-wake-failure", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
			Generation: 1, GenerationID: "gen-binding-wake-failure", AgentHubSessionID: "ses-binding-wake-failure",
			BindingKind: "profile", BindingName: "default", AgentHubAgentName: "fake-agent", Status: "idle",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := saveGenerationRecord(workspace.Path, record); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		var cfg agentHubServeConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.AgentHubInstanceID = ""
		data, err = json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = manager.takeReconcileRequests()
		body := bytes.NewBufferString(`{"kind":"agent","name":"replacement-agent"}`)
		recorder := httptest.NewRecorder()
		manager.server.updateResourceAgentBinding(recorder, httptest.NewRequest(http.MethodPut, "/agent-binding", body), workspace.ID, record.ResourceID)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("failed binding reconciliation returned %d: %s", recorder.Code, recorder.Body.String())
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := puaWorkspace.ResourceAgentBinding(record.ResourceID)
		if err != nil || binding != (app.AgentBinding{Kind: "agent", Name: "replacement-agent"}) {
			t.Fatalf("failed reconciliation lost durable binding: %#v, %v", binding, err)
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
			t.Fatalf("failed binding reconciliation requested Scheduler reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("failed binding reconciliation woke the reconcile loop")
		default:
		}
	})

	t.Run("ownership missing before start", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		manager.server.locks = newWorkspaceLockManager("127.0.0.1:4936", manager.server.config)
		t.Cleanup(manager.server.locks.closeAll)
		_ = manager.takeReconcileRequests()
		select {
		case <-manager.reconcileWake:
		default:
		}
		_, persisted, err := manager.updateResourceAgentBinding(context.Background(), workspace, "project1.task1", app.AgentBinding{Kind: "agent", Name: "replacement-agent"})
		if err == nil || persisted {
			t.Fatalf("unowned binding mutation = persisted %v, err %v", persisted, err)
		}
		puaWorkspace, openErr := app.OpenWorkspace(workspace.Path)
		if openErr != nil {
			t.Fatal(openErr)
		}
		binding, bindingErr := puaWorkspace.ResourceAgentBinding("project1.task1")
		wantBinding := app.AgentBinding{Kind: "profile", Name: "default"}
		if bindingErr != nil || binding != wantBinding {
			t.Fatalf("unowned binding = %#v, %v; want %#v", binding, bindingErr, wantBinding)
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
			t.Fatalf("unowned binding requested Scheduler reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("unowned binding woke the reconcile loop")
		default:
		}
	})

	t.Run("ownership lost after material change", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		manager.server.locks = newWorkspaceLockManager("127.0.0.1:4936", manager.server.config)
		t.Cleanup(manager.server.locks.closeAll)
		if _, err := manager.server.locks.acquire(workspace.Path); err != nil {
			t.Fatal(err)
		}
		_ = manager.takeReconcileRequests()
		select {
		case <-manager.reconcileWake:
		default:
		}
		outcome := manager.runResourceBindingControllerJob(context.Background(), workspace, "project1.task1", func(jobCtx context.Context) resourceBindingMutationOutcome {
			outcome := manager.updateResourceAgentBindingLocked(jobCtx, workspace, "project1.task1", app.AgentBinding{Kind: "agent", Name: "replacement-agent"})
			manager.server.locks.release(workspace.Path)
			return outcome
		})
		if outcome.err != nil || !outcome.persisted || !outcome.material {
			t.Fatalf("binding mutation before ownership loss = %#v", outcome)
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := puaWorkspace.ResourceAgentBinding("project1.task1")
		wantBinding := app.AgentBinding{Kind: "agent", Name: "replacement-agent"}
		if err != nil || binding != wantBinding {
			t.Fatalf("binding before ownership loss = %#v, %v; want %#v", binding, err, wantBinding)
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
			t.Fatalf("ownership-lost binding requested Scheduler reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("ownership-lost binding woke the reconcile loop")
		default:
		}
	})
}

func TestWorkspaceDefaultsMutationWakesAttentionHeldScheduler(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		defaults app.ResourceAgentDefaults
	}{
		{
			name:   "project fallback",
			target: "project1",
			defaults: app.ResourceAgentDefaults{
				Project: app.AgentBinding{Kind: "agent", Name: "fake-agent"},
				Task:    app.AgentBinding{Kind: "profile", Name: "fallback-missing"},
			},
		},
		{
			name:   "task fallback",
			target: "project1.task1",
			defaults: app.ResourceAgentDefaults{
				Project: app.AgentBinding{Kind: "profile", Name: "fallback-missing"},
				Task:    app.AgentBinding{Kind: "agent", Name: "fake-agent"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			missingFallback := app.AgentBinding{Kind: "profile", Name: "fallback-missing"}
			if _, err := puaWorkspace.SetResourceAgentDefaults(app.ResourceAgentDefaults{
				Project: missingFallback,
				Task:    missingFallback,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := puaWorkspace.SetResourceAgentBinding(test.target, app.AgentBinding{
				Kind: "profile", Name: "explicit-missing",
			}); err != nil {
				t.Fatal(err)
			}
			rewriteSchedulerTestProfiles(t, configPath, []agentHubProfileRoute{
				{Key: "fallback-missing", AgentName: "missing-agent"},
			})

			at := time.Date(2032, time.March, 4, 5, 6, 7, 0, time.UTC)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Recover through Workspace defaults", Condition: "once", Target: test.target,
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := at.Add(time.Second)
			manager.now = func() time.Time { return now }
			native := newNativeScheduler(manager, workspace)
			if deadline, reconcileErr := native.Reconcile(context.Background(), now); reconcileErr != nil || !deadline.IsZero() {
				t.Fatalf("create defaults attention: deadline=%s err=%v", deadline, reconcileErr)
			}
			runtime, err := native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil {
				t.Fatalf("defaults attention runtime = %#v, %v", runtime, err)
			}
			prepared := *runtime.Prepared
			_ = manager.takeReconcileRequests()
			select {
			case <-manager.reconcileWake:
			default:
			}

			body, err := json.Marshal(test.defaults)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			manager.server.updateWorkspaceDefaults(recorder, httptest.NewRequest(http.MethodPut, "/defaults", bytes.NewReader(body)), workspace.ID)
			if recorder.Code != http.StatusOK {
				t.Fatalf("defaults restoration returned %d: %s", recorder.Code, recorder.Body.String())
			}
			request := manager.takeReconcileRequests()
			if request&reconcileScheduler == 0 {
				t.Fatalf("defaults restoration reconcile request = %08b, want Scheduler", request)
			}
			select {
			case <-manager.reconcileWake:
			default:
				t.Fatal("defaults restoration did not wake the reconcile loop")
			}

			if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
				t.Fatal(err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
			if len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("defaults wake delivered occurrences = %#v, want %s", messages, prepared.MessageID)
			}
			runtime, err = native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted {
				t.Fatalf("defaults wake acceptance checkpoint = %#v, %v", runtime, err)
			}
			if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
				t.Fatal(err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("defaults wake duplicated occurrence: %#v", messages)
			}
		})
	}
}

func TestWorkspaceDefaultsMutationRequestsSchedulerOnlyAfterMaterialSuccess(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		prepare    func(*testing.T, *agentManager, serveWorkspace) *http.Request
		wantStatus int
	}{
		{
			name:       "normalized no-op",
			body:       `{"project":{"kind":"PROFILE","name":"DEFAULT"},"task":{"kind":"profile","name":"default"}}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "validation failure",
			body:       `{"project":{"kind":"profile","name":"missing"},"task":{"kind":"profile","name":"default"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "cancelled request",
			body: `{"project":{"kind":"agent","name":"replacement"},"task":{"kind":"profile","name":"default"}}`,
			prepare: func(t *testing.T, _ *agentManager, _ serveWorkspace) *http.Request {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return httptest.NewRequest(http.MethodPut, "/defaults", nil).WithContext(ctx)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "stale Workspace ownership",
			body: `{"project":{"kind":"agent","name":"replacement"},"task":{"kind":"profile","name":"default"}}`,
			prepare: func(t *testing.T, manager *agentManager, _ serveWorkspace) *http.Request {
				t.Helper()
				manager.server.locks = newWorkspaceLockManager("127.0.0.1:4936", manager.server.config)
				return httptest.NewRequest(http.MethodPut, "/defaults", nil)
			},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			_ = manager.takeReconcileRequests()
			select {
			case <-manager.reconcileWake:
			default:
			}
			request := httptest.NewRequest(http.MethodPut, "/defaults", strings.NewReader(test.body))
			if test.prepare != nil {
				prepared := test.prepare(t, manager, workspace)
				prepared.Body = request.Body
				request = prepared
			}
			recorder := httptest.NewRecorder()
			manager.server.updateWorkspaceDefaults(recorder, request, workspace.ID)
			manager.waitBackground()
			if recorder.Code != test.wantStatus {
				t.Fatalf("defaults mutation returned %d: %s", recorder.Code, recorder.Body.String())
			}
			if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
				t.Fatalf("rejected defaults mutation requested Scheduler reconciliation: %08b", request)
			}
			select {
			case <-manager.reconcileWake:
				t.Fatal("rejected defaults mutation woke the reconcile loop")
			default:
			}
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			runtimeConfig, err := puaWorkspace.RuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			want := app.AgentBinding{Kind: "profile", Name: "default"}
			if runtimeConfig.ResourceDefaults.Project != want || runtimeConfig.ResourceDefaults.Task != want {
				t.Fatalf("rejected defaults mutation persisted %#v", runtimeConfig.ResourceDefaults)
			}
		})
	}
}

type schedulerSettingsFakeAgentHub struct {
	base                *runtimeFakeAgentHub
	mu                  sync.Mutex
	config              agentHubConfiguredConfig
	mutationAttempts    atomic.Int32
	failMutations       bool
	afterMutationCommit func(*http.Request, string)
}

func newSchedulerSettingsFakeAgentHub(config agentHubConfiguredConfig) *schedulerSettingsFakeAgentHub {
	return &schedulerSettingsFakeAgentHub{base: newRuntimeFakeAgentHub(), config: config}
}

func (f *schedulerSettingsFakeAgentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		writeRuntimeFakeJSON(w, map[string]any{
			"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "test",
		})
		return
	case r.URL.Path == "/v1/config":
		if r.Method == http.MethodGet {
			f.mu.Lock()
			configured := f.config
			f.mu.Unlock()
			writeRuntimeFakeJSON(w, map[string]any{"config": configured})
			return
		}
		if r.Method == http.MethodPut {
			f.mutationAttempts.Add(1)
			var request struct {
				Config agentHubConfiguredConfig `json:"config"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f.failMutations {
				w.WriteHeader(http.StatusServiceUnavailable)
				writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
					"code": "runtime_unavailable", "message": "synthetic Settings mutation failure", "retryable": true,
				}})
				return
			}
			f.mu.Lock()
			f.config = request.Config
			configured := f.config
			hook := f.afterMutationCommit
			f.mu.Unlock()
			if hook != nil {
				hook(r, "config")
			}
			writeRuntimeFakeJSON(w, map[string]any{"config": configured})
			return
		}
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/config/providers/"):
		f.mutationAttempts.Add(1)
		providerID, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/v1/config/providers/"))
		var request struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.failMutations {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "runtime_unavailable", "message": "synthetic Settings mutation failure", "retryable": true,
			}})
			return
		}
		f.mu.Lock()
		for index := range f.config.AgentProviders {
			if f.config.AgentProviders[index].ID == providerID {
				f.config.AgentProviders[index].Enabled = request.Enabled
				provider := f.config.AgentProviders[index]
				hook := f.afterMutationCommit
				f.mu.Unlock()
				if hook != nil {
					hook(r, "provider")
				}
				writeRuntimeFakeJSON(w, map[string]any{"provider": provider})
				return
			}
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
		f.mu.Lock()
		configured := f.config
		f.mu.Unlock()
		providers := make([]agentHubProvider, 0, len(configured.AgentProviders))
		enabled := make(map[string]bool, len(configured.AgentProviders))
		for _, provider := range configured.AgentProviders {
			providers = append(providers, agentHubProvider{
				ID: provider.ID, Name: provider.Name, Type: provider.Type, Enabled: provider.Enabled,
			})
			enabled[provider.ID] = provider.Enabled
		}
		agents := make([]agentHubAgent, 0, len(configured.Agents))
		for _, agent := range configured.Agents {
			available := enabled[agent.ProviderID]
			reason := ""
			if !available {
				reason = "provider disabled"
			}
			agents = append(agents, agentHubAgent{
				Name: agent.Name, ProviderID: agent.ProviderID, Available: available, UnavailableReason: reason,
			})
		}
		writeRuntimeFakeJSON(w, agentHubCatalog{Providers: providers, Agents: agents, Probes: []agentHubProbe{}})
		return
	}
	f.base.ServeHTTP(w, r)
}

type agentHubSettingsRemoteMutationCase struct {
	name          string
	initial       agentHubConfiguredConfig
	request       func(string, bool) *http.Request
	invoke        func(*server, *httptest.ResponseRecorder, *http.Request)
	remoteChanged func(agentHubConfiguredConfig) bool
	localChanged  func(agentHubServeConfig) bool
}

func agentHubSettingsRemoteMutationCases() []agentHubSettingsRemoteMutationCase {
	provider := agentHubConfiguredProvider{ID: "fake", Name: "Fake", Type: "fake", Enabled: true}
	return []agentHubSettingsRemoteMutationCase{
		{
			name: "agent config and profile",
			initial: agentHubConfiguredConfig{
				Version: 1, AgentProviders: []agentHubConfiguredProvider{provider},
				Agents: []agentHubConfiguredAgent{{Name: "fake-agent", ProviderID: provider.ID}},
			},
			request: func(endpoint string, changed bool) *http.Request {
				agentName := "fake-agent"
				if changed {
					agentName = "settings-agent"
				}
				body, _ := json.Marshal(updateAgentHubSettingsRequest{
					Endpoint: endpoint,
					AgentProfiles: []agentHubProfileRoute{
						{Key: "default", AgentName: agentName},
					},
					AgentProviders: []agentHubConfiguredProvider{provider},
					Agents:         []agentHubConfiguredAgent{{Name: agentName, ProviderID: provider.ID}},
				})
				return httptest.NewRequest(http.MethodPut, "/api/settings/agenthub", bytes.NewReader(body))
			},
			invoke: func(server *server, recorder *httptest.ResponseRecorder, request *http.Request) {
				server.handleAgentHubSettings(recorder, request)
			},
			remoteChanged: func(config agentHubConfiguredConfig) bool {
				return len(config.Agents) == 1 && config.Agents[0].Name == "settings-agent"
			},
			localChanged: func(config agentHubServeConfig) bool {
				return configuredAgentHubProfileTarget(config.AgentProfiles, "default") == "settings-agent"
			},
		},
		{
			name: "provider",
			initial: agentHubConfiguredConfig{
				Version: 1,
				AgentProviders: []agentHubConfiguredProvider{
					{ID: provider.ID, Name: provider.Name, Type: provider.Type, Enabled: false},
				},
				Agents: []agentHubConfiguredAgent{{Name: "fake-agent", ProviderID: provider.ID}},
			},
			request: func(_ string, changed bool) *http.Request {
				return httptest.NewRequest(http.MethodPut, "/api/settings/agenthub/providers/fake", strings.NewReader(fmt.Sprintf(`{"enabled":%t}`, changed)))
			},
			invoke: func(server *server, recorder *httptest.ResponseRecorder, request *http.Request) {
				server.handleAgentHubProviderSettings(recorder, request, "fake")
			},
			remoteChanged: func(config agentHubConfiguredConfig) bool {
				return len(config.AgentProviders) == 1 && config.AgentProviders[0].Enabled
			},
		},
	}
}

func schedulerSettingsConfigSnapshot(fake *schedulerSettingsFakeAgentHub) agentHubConfiguredConfig {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.config
}

func prepareAgentHubSettingsMutationTest(
	t *testing.T,
	test agentHubSettingsRemoteMutationCase,
) (*schedulerSettingsFakeAgentHub, *httptest.Server, *agentManager, serveWorkspace, string) {
	t.Helper()
	fake := newSchedulerSettingsFakeAgentHub(test.initial)
	hub := httptest.NewServer(fake)
	t.Cleanup(hub.Close)
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	rewriteSchedulerTestProfiles(t, configPath, []agentHubProfileRoute{{
		Key: "default", Description: systemAgentProfileDefinitions[0].Description, AgentName: "fake-agent",
	}})
	manager.reconcileWake = make(chan struct{}, 4)
	_ = manager.takeReconcileRequests()
	return fake, hub, manager, workspace, configPath
}

func assertAgentHubSettingsSchedulerWake(t *testing.T, manager *agentManager, want bool) {
	t.Helper()
	request := manager.takeReconcileRequests()
	if got := request&reconcileScheduler != 0; got != want {
		t.Fatalf("Settings Scheduler reconcile request = %08b, want wake %v", request, want)
	}
	wakes := len(manager.reconcileWake)
	wantWakes := 0
	if want {
		wantWakes = 1
	}
	if wakes != wantWakes {
		t.Fatalf("Settings reconcile wake count = %d, want %d", wakes, wantWakes)
	}
}

func assertAgentHubSettingsLocalChange(t *testing.T, test agentHubSettingsRemoteMutationCase, configPath string) {
	t.Helper()
	if test.localChanged == nil {
		return
	}
	config, err := readAgentHubConfigFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !test.localChanged(config) {
		t.Fatalf("confirmed remote mutation did not persist required local config: %#v", config)
	}
}

func TestAgentHubSettingsRemoteMutationCancellationBoundaries(t *testing.T) {
	for _, test := range agentHubSettingsRemoteMutationCases() {
		t.Run(test.name+" cancelled before start", func(t *testing.T) {
			fake, hub, manager, _, _ := prepareAgentHubSettingsMutationTest(t, test)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			request := test.request(hub.URL, true).WithContext(ctx)
			recorder := httptest.NewRecorder()
			test.invoke(manager.server, recorder, request)
			if recorder.Code == http.StatusOK {
				t.Fatalf("cancelled Settings mutation unexpectedly succeeded: %s", recorder.Body.String())
			}
			if attempts := fake.mutationAttempts.Load(); attempts != 0 {
				t.Fatalf("cancelled Settings mutation reached AgentHub %d times", attempts)
			}
			if test.remoteChanged(schedulerSettingsConfigSnapshot(fake)) {
				t.Fatal("cancelled Settings mutation changed durable AgentHub config")
			}
			assertAgentHubSettingsSchedulerWake(t, manager, false)
		})

		t.Run(test.name+" cancelled after remote commit", func(t *testing.T) {
			fake, hub, manager, _, configPath := prepareAgentHubSettingsMutationTest(t, test)
			committed := make(chan struct{}, 1)
			release := make(chan struct{})
			requestCancelled := make(chan struct{}, 1)
			fake.afterMutationCommit = func(request *http.Request, _ string) {
				committed <- struct{}{}
				select {
				case <-release:
				case <-request.Context().Done():
					requestCancelled <- struct{}{}
					<-release
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			request := test.request(hub.URL, true).WithContext(ctx)
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				test.invoke(manager.server, recorder, request)
				close(done)
			}()
			select {
			case <-committed:
			case <-time.After(time.Second):
				t.Fatal("Settings mutation did not reach its remote commit")
			}
			cancel()
			select {
			case <-requestCancelled:
				t.Fatal("committed AgentHub mutation retained the cancelled HTTP context")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("confirmed Settings mutation did not finish")
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("confirmed Settings mutation returned %d: %s", recorder.Code, recorder.Body.String())
			}
			if attempts := fake.mutationAttempts.Load(); attempts != 1 {
				t.Fatalf("confirmed Settings mutation attempts = %d, want 1", attempts)
			}
			if !test.remoteChanged(schedulerSettingsConfigSnapshot(fake)) {
				t.Fatal("confirmed Settings mutation was not durable in AgentHub")
			}
			assertAgentHubSettingsLocalChange(t, test, configPath)
			assertAgentHubSettingsSchedulerWake(t, manager, true)
		})
	}
}

func TestAgentHubSettingsRemoteMutationQuietOutcomes(t *testing.T) {
	for _, test := range agentHubSettingsRemoteMutationCases() {
		t.Run(test.name+" remote failure", func(t *testing.T) {
			fake, hub, manager, _, _ := prepareAgentHubSettingsMutationTest(t, test)
			fake.failMutations = true
			recorder := httptest.NewRecorder()
			test.invoke(manager.server, recorder, test.request(hub.URL, true))
			if recorder.Code == http.StatusOK {
				t.Fatalf("failed Settings mutation unexpectedly succeeded: %s", recorder.Body.String())
			}
			if attempts := fake.mutationAttempts.Load(); attempts != 1 {
				t.Fatalf("failed Settings mutation attempts = %d, want 1", attempts)
			}
			if test.remoteChanged(schedulerSettingsConfigSnapshot(fake)) {
				t.Fatal("failed Settings mutation changed durable AgentHub config")
			}
			assertAgentHubSettingsSchedulerWake(t, manager, false)
		})

		t.Run(test.name+" no-op", func(t *testing.T) {
			fake, hub, manager, _, _ := prepareAgentHubSettingsMutationTest(t, test)
			recorder := httptest.NewRecorder()
			test.invoke(manager.server, recorder, test.request(hub.URL, false))
			if recorder.Code != http.StatusOK {
				t.Fatalf("no-op Settings mutation returned %d: %s", recorder.Code, recorder.Body.String())
			}
			if attempts := fake.mutationAttempts.Load(); attempts != 0 {
				t.Fatalf("no-op Settings mutation reached AgentHub %d times", attempts)
			}
			if test.remoteChanged(schedulerSettingsConfigSnapshot(fake)) {
				t.Fatal("no-op Settings mutation changed durable AgentHub config")
			}
			assertAgentHubSettingsSchedulerWake(t, manager, false)
		})

		t.Run(test.name+" ownership lost", func(t *testing.T) {
			fake, hub, manager, workspace, configPath := prepareAgentHubSettingsMutationTest(t, test)
			manager.server.locks = newWorkspaceLockManager("127.0.0.1:4936", manager.server.config)
			t.Cleanup(manager.server.locks.closeAll)
			if _, err := manager.server.locks.acquire(workspace.Path); err != nil {
				t.Fatal(err)
			}
			fake.afterMutationCommit = func(_ *http.Request, _ string) {
				manager.server.locks.release(workspace.Path)
			}
			recorder := httptest.NewRecorder()
			test.invoke(manager.server, recorder, test.request(hub.URL, true))
			if recorder.Code != http.StatusOK {
				t.Fatalf("ownership-lost Settings mutation returned %d: %s", recorder.Code, recorder.Body.String())
			}
			if !test.remoteChanged(schedulerSettingsConfigSnapshot(fake)) {
				t.Fatal("ownership-lost Settings mutation was not durable in AgentHub")
			}
			assertAgentHubSettingsLocalChange(t, test, configPath)
			assertAgentHubSettingsSchedulerWake(t, manager, false)
		})
	}
}

func TestAgentHubSettingsMutationWakesAttentionHeldScheduler(t *testing.T) {
	provider := agentHubConfiguredProvider{ID: "fake", Name: "Fake", Type: "fake", Enabled: true}
	agent := agentHubConfiguredAgent{Name: "settings-agent", ProviderID: "fake"}
	tests := []struct {
		name           string
		initialConfig  agentHubConfiguredConfig
		initialProfile string
		mutate         func(*testing.T, *server, string)
	}{
		{
			name: "agent profile and catalog",
			initialConfig: agentHubConfiguredConfig{
				Version: 1, AgentProviders: []agentHubConfiguredProvider{provider}, Agents: []agentHubConfiguredAgent{},
			},
			initialProfile: "missing-agent",
			mutate: func(t *testing.T, server *server, endpoint string) {
				t.Helper()
				body, err := json.Marshal(updateAgentHubSettingsRequest{
					Endpoint: endpoint,
					AgentProfiles: []agentHubProfileRoute{
						{Key: "default", AgentName: agent.Name},
					},
					AgentProviders: []agentHubConfiguredProvider{provider},
					Agents:         []agentHubConfiguredAgent{agent},
				})
				if err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				server.handleAgentHubSettings(recorder, httptest.NewRequest(http.MethodPut, "/api/settings/agenthub", bytes.NewReader(body)))
				if recorder.Code != http.StatusOK {
					t.Fatalf("settings save returned %d: %s", recorder.Code, recorder.Body.String())
				}
			},
		},
		{
			name: "provider toggle",
			initialConfig: agentHubConfiguredConfig{
				Version: 1,
				AgentProviders: []agentHubConfiguredProvider{
					{ID: provider.ID, Name: provider.Name, Type: provider.Type, Enabled: false},
				},
				Agents: []agentHubConfiguredAgent{agent},
			},
			initialProfile: agent.Name,
			mutate: func(t *testing.T, server *server, _ string) {
				t.Helper()
				recorder := httptest.NewRecorder()
				server.handleAgentHubProviderSettings(recorder, httptest.NewRequest(
					http.MethodPut, "/api/settings/agenthub/providers/fake", strings.NewReader(`{"enabled":true}`),
				), provider.ID)
				if recorder.Code != http.StatusOK {
					t.Fatalf("provider toggle returned %d: %s", recorder.Code, recorder.Body.String())
				}
			},
		},
		{
			name: "catalog normalization",
			initialConfig: agentHubConfiguredConfig{
				Version: 1, AgentProviders: []agentHubConfiguredProvider{provider}, Agents: []agentHubConfiguredAgent{agent},
			},
			initialProfile: "",
			mutate: func(t *testing.T, server *server, _ string) {
				t.Helper()
				recorder := httptest.NewRecorder()
				server.handleAgentHubSettings(recorder, httptest.NewRequest(http.MethodGet, "/api/settings/agenthub", nil))
				if recorder.Code != http.StatusOK {
					t.Fatalf("catalog refresh returned %d: %s", recorder.Code, recorder.Body.String())
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newSchedulerSettingsFakeAgentHub(test.initialConfig)
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
			rewriteSchedulerTestProfiles(t, configPath, []agentHubProfileRoute{{Key: "default", AgentName: test.initialProfile}})
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2031, time.February, 3, 4, 5, 6, 0, time.UTC)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Recover through Settings", Condition: "once", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := at.Add(time.Second)
			manager.now = func() time.Time { return now }
			native := newNativeScheduler(manager, workspace)
			if deadline, reconcileErr := native.Reconcile(context.Background(), now); reconcileErr != nil || !deadline.IsZero() {
				t.Fatalf("create Settings attention: deadline=%s err=%v", deadline, reconcileErr)
			}
			runtime, err := native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil {
				t.Fatalf("Settings attention runtime = %#v, %v", runtime, err)
			}
			prepared := *runtime.Prepared
			_ = manager.takeReconcileRequests()
			select {
			case <-manager.reconcileWake:
			default:
			}

			test.mutate(t, manager.server, hub.URL)
			request := manager.takeReconcileRequests()
			if request&reconcileScheduler == 0 {
				t.Fatalf("Settings mutation reconcile request = %08b, want Scheduler", request)
			}
			select {
			case <-manager.reconcileWake:
			default:
				t.Fatal("Settings mutation did not wake the reconcile loop")
			}
			if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
				t.Fatal(err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
			if len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("Settings wake delivered occurrences = %#v, want %s", messages, prepared.MessageID)
			}
			runtime, err = native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted {
				t.Fatalf("Settings wake acceptance checkpoint = %#v, %v", runtime, err)
			}

			_ = manager.takeReconcileRequests()
			select {
			case <-manager.reconcileWake:
			default:
			}
			test.mutate(t, manager.server, hub.URL)
			if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
				t.Fatalf("no-op Settings mutation requested Scheduler reconciliation: %08b", request)
			}
			select {
			case <-manager.reconcileWake:
				t.Fatal("no-op Settings mutation woke the reconcile loop")
			default:
			}
			if err := manager.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
				t.Fatal(err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("Settings wake duplicated occurrence: %#v", messages)
			}
		})
	}
}

func TestNativeSchedulerTransientBindingPreflightUsesBackoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	failCatalog := true
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failCatalog && r.Method == http.MethodGet && r.URL.Path == "/v1/agents" {
			failCatalog = false
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "runtime_unavailable", "message": "synthetic catalog outage", "retryable": true,
			}})
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Retry transiently", Condition: "once", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(time.Second)
	native := newNativeScheduler(manager, workspace)
	deadline, err := native.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.EffectiveState == schedulerOutcomeAttention || runtime.Prepared == nil || runtime.RetryAt == "" || runtime.NextRunAt != at.Format(time.RFC3339Nano) || runtime.LastOccurrenceAt != "" {
		t.Fatalf("transient preflight classification = %#v", runtime)
	}
	retryAt := generationTime(runtime.RetryAt)
	if retryAt.IsZero() || !deadline.Equal(retryAt) {
		t.Fatalf("transient retry deadline = %s, want %s", deadline, retryAt)
	}
	prepared := *runtime.Prepared
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("transient preflight accepted mailbox work: %#v", messages)
	}
	if _, err := native.Reconcile(context.Background(), retryAt); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	if len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("transient retry lost stable occurrence: %#v, want %s", messages, prepared.MessageID)
	}
}

func rewriteSchedulerTestProfiles(t *testing.T, configPath string, profiles []agentHubProfileRoute) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg agentHubServeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.AgentProfiles = profiles
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSchedulerDoesNotReplayCompletedOneTimeOnSemanticEdit(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run once", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := at.Add(time.Second)
	manager.now = func() time.Time { return current }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	description := "Clarified completed action"
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 1 {
		t.Fatalf("semantic edit replayed completed one-time occurrence: %#v", messages)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(current)
	if err != nil || snapshot.Schedules[0].Revision != updated.Revision || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted {
		t.Fatalf("completed semantic edit snapshot = %#v, %v", snapshot, err)
	}
	newAt := at.Add(time.Hour)
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: newAt.Format(time.RFC3339Nano)}
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: updated.Revision, Trigger: &trigger}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = newNativeScheduler(manager, workspace).Snapshot(current)
	if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStateActive || snapshot.Schedules[0].NextRunAt != newAt.Format(time.RFC3339Nano) {
		t.Fatalf("new one-time trigger did not reactivate schedule: %#v, %v", snapshot, err)
	}
}

func TestNativeSchedulerRejectsPauseAfterOneTimeAcceptance(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run exactly once", Condition: "at the configured time", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(time.Second)
	manager.now = func() time.Time { return now }
	native := newNativeScheduler(manager, workspace)
	if _, err := native.Reconcile(context.Background(), now); err != nil {
		t.Fatal(err)
	}

	assertAcceptedCompletion := func(t *testing.T, scheduler *NativeScheduler) {
		t.Helper()
		runtime, err := scheduler.schedulerRuntime(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("completed one-time runtime = %#v", runtime)
		}
	}
	assertAcceptedCompletion(t, native)

	before, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID}); !errors.Is(err, errNativeSchedulerPauseCompleted) || err.Error() != "completed schedule cannot be paused" {
		t.Fatalf("pause completed one-time error = %v", err)
	}
	after, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || after.Schedules[0].State != app.ScheduleStateActive || after.Schedules[0].Revision != created.Revision {
		t.Fatalf("pause mutated portable schedule: before=%#v after=%#v", before.Schedules[0], after.Schedules[0])
	}
	if _, err := native.Reconcile(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertAcceptedCompletion(t, native)

	restartedManager := newAgentManager(manager.server)
	restartedManager.now = func() time.Time { return now.Add(2 * time.Minute) }
	manager.server.agents = restartedManager
	restarted := newNativeScheduler(restartedManager, workspace)
	if _, err := restarted.Reconcile(context.Background(), restartedManager.now()); err != nil {
		t.Fatal(err)
	}
	assertAcceptedCompletion(t, restarted)
	snapshot, err := restarted.Snapshot(restartedManager.now())
	if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomeAccepted || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("restarted completed snapshot = %#v, %v", snapshot, err)
	}
}

func TestNativeSchedulerPauseRecoversOneTimeDueAtPersistenceBoundary(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run before pause", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	rewriteSchedulerTestOneTimeDeadline(t, puaWorkspace, created.ID, at)
	before := schedulerTestScheduleByID(t, puaWorkspace, created.ID)

	// The injected clock is deliberately before the occurrence. Only the
	// timestamp captured inside the persisted app transition can close this
	// deadline-crossing window deterministically.
	manager.now = func() time.Time { return at.Add(-time.Nanosecond) }
	native := newNativeScheduler(manager, workspace)
	if _, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID}); !errors.Is(err, errNativeSchedulerPauseCompleted) {
		t.Fatalf("pause due one-time error = %v", err)
	}
	after := schedulerTestScheduleByID(t, puaWorkspace, created.ID)
	if !reflect.DeepEqual(after, before) || after.State != app.ScheduleStateActive || after.Revision != created.Revision {
		t.Fatalf("due pause mutated portable definition: before=%#v after=%#v", before, after)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target)
	if len(messages) != 1 || messages[0].Causation == nil || messages[0].Causation.ScheduledFor != at.Format(time.RFC3339Nano) {
		t.Fatalf("due occurrence mailbox = %#v", messages)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != created.Revision || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("due occurrence runtime = %#v, %v", runtime, err)
	}
}

func TestNativeSchedulerPausePreservesClaimedOneTimeOccurrence(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		accepted  bool
		configure func(*testing.T, *NativeScheduler, string, app.Schedule, schedulerPreparedOccurrence, schedulerScheduleRuntime) schedulerScheduleRuntime
		assert    func(*testing.T, *NativeScheduler, serveWorkspace, app.Schedule, schedulerPreparedOccurrence, schedulerScheduleRuntime)
	}{
		{
			name:   "prepared",
			target: "workspace",
			configure: func(_ *testing.T, _ *NativeScheduler, _ string, _ app.Schedule, prepared schedulerPreparedOccurrence, runtime schedulerScheduleRuntime) schedulerScheduleRuntime {
				runtime.Prepared = &prepared
				return runtime
			},
			assert: func(t *testing.T, native *NativeScheduler, workspace serveWorkspace, schedule app.Schedule, prepared schedulerPreparedOccurrence, _ schedulerScheduleRuntime) {
				runtime, err := native.schedulerRuntime(schedule.ID)
				if err != nil || runtime.Revision != schedule.Revision || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != prepared.ScheduledFor {
					t.Fatalf("prepared pause recovery runtime = %#v, %v", runtime, err)
				}
				assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
			},
		},
		{
			name:   "retry backoff",
			target: "workspace",
			configure: func(_ *testing.T, _ *NativeScheduler, _ string, _ app.Schedule, prepared schedulerPreparedOccurrence, runtime schedulerScheduleRuntime) schedulerScheduleRuntime {
				runtime.Prepared = &prepared
				runtime.RetryAt = generationTime(prepared.ScheduledFor).Add(time.Minute).Format(time.RFC3339Nano)
				runtime.RetryCount = 2
				return runtime
			},
			assert: func(t *testing.T, native *NativeScheduler, workspace serveWorkspace, schedule app.Schedule, prepared schedulerPreparedOccurrence, want schedulerScheduleRuntime) {
				runtime, err := native.schedulerRuntime(schedule.ID)
				if err != nil || runtime.Revision != schedule.Revision || runtime.EffectiveState != app.ScheduleStateActive || runtime.RetryAt != want.RetryAt || runtime.RetryCount != want.RetryCount {
					t.Fatalf("retry pause preservation runtime = %#v, %v", runtime, err)
				}
				assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
				assertSingleDurableOccurrence(t, workspace.Path, prepared, 0)
			},
		},
		{
			name:   "attention",
			target: "project1.task1",
			configure: func(t *testing.T, _ *NativeScheduler, configPath string, _ app.Schedule, prepared schedulerPreparedOccurrence, runtime schedulerScheduleRuntime) schedulerScheduleRuntime {
				rewriteSchedulerTestProfiles(t, configPath, nil)
				runtime.Prepared = &prepared
				runtime.EffectiveState = schedulerOutcomeAttention
				runtime.LastOutcome = schedulerOutcomeAttention
				runtime.LastError = "binding unavailable"
				runtime.AttentionTarget = prepared.Target
				return runtime
			},
			assert: func(t *testing.T, native *NativeScheduler, workspace serveWorkspace, schedule app.Schedule, prepared schedulerPreparedOccurrence, _ schedulerScheduleRuntime) {
				runtime, err := native.schedulerRuntime(schedule.ID)
				if err != nil || runtime.Revision != schedule.Revision || runtime.EffectiveState != schedulerOutcomeAttention || runtime.LastOutcome != schedulerOutcomeAttention || runtime.AttentionTarget != prepared.Target {
					t.Fatalf("attention pause preservation runtime = %#v, %v", runtime, err)
				}
				assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
				assertSingleDurableOccurrence(t, workspace.Path, prepared, 0)
			},
		},
		{
			name:     "mailbox accepted before checkpoint",
			target:   "workspace",
			accepted: true,
			configure: func(_ *testing.T, _ *NativeScheduler, _ string, _ app.Schedule, prepared schedulerPreparedOccurrence, runtime schedulerScheduleRuntime) schedulerScheduleRuntime {
				runtime.Prepared = &prepared
				return runtime
			},
			assert: func(t *testing.T, native *NativeScheduler, workspace serveWorkspace, schedule app.Schedule, prepared schedulerPreparedOccurrence, _ schedulerScheduleRuntime) {
				runtime, err := native.schedulerRuntime(schedule.ID)
				if err != nil || runtime.Revision != schedule.Revision || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != prepared.ScheduledFor {
					t.Fatalf("accepted-window pause recovery runtime = %#v, %v", runtime, err)
				}
				assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Finish claimed work", Condition: "once", Target: test.target,
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			manager.now = func() time.Time { return at.Add(-time.Minute) }
			native := newNativeScheduler(manager, workspace)
			prepared, err := native.prepareOccurrence(schedule, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			runtime := schedulerScheduleRuntime{
				Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger),
				EffectiveState: app.ScheduleStateActive, NextRunAt: at.Format(time.RFC3339Nano),
			}
			runtime = test.configure(t, native, configPath, schedule, prepared, runtime)
			if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
				t.Fatal(err)
			}
			if test.accepted {
				if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: schedule.ID}); !errors.Is(err, errNativeSchedulerPauseCompleted) {
				t.Fatalf("pause claimed one-time error = %v", err)
			}
			after := schedulerTestScheduleByID(t, puaWorkspace, schedule.ID)
			if !reflect.DeepEqual(after, schedule) || after.State != app.ScheduleStateActive || after.Revision != schedule.Revision {
				t.Fatalf("claimed pause mutated portable definition: before=%#v after=%#v", schedule, after)
			}
			test.assert(t, native, workspace, schedule, prepared, runtime)
		})
	}
}

func TestNativeSchedulerPauseStillWorksBeforeCompletion(t *testing.T) {
	tests := []struct {
		name    string
		trigger *app.ScheduleTrigger
	}{
		{
			name: "repeating",
			trigger: &app.ScheduleTrigger{
				Type: app.ScheduleTriggerInterval, EverySeconds: 60,
				AnchorAt: time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano),
			},
		},
		{
			name: "pending one-time",
			trigger: &app.ScheduleTrigger{
				Type: app.ScheduleTriggerAt,
				At:   time.Date(2030, time.January, 2, 4, 4, 5, 0, time.UTC).Format(time.RFC3339Nano),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			manager.now = func() time.Time { return time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC) }
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Pause safely", Condition: "before completion", Target: "workspace", Trigger: test.trigger,
			})
			if err != nil {
				t.Fatal(err)
			}
			paused, err := newNativeScheduler(manager, workspace).Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID})
			if err != nil || paused.State != app.ScheduleStatePaused || paused.Revision != created.Revision+1 {
				t.Fatalf("pause active schedule = %#v, %v", paused, err)
			}
			pausedAgain, err := newNativeScheduler(manager, workspace).Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID})
			if err != nil || !reflect.DeepEqual(pausedAgain, paused) {
				t.Fatalf("idempotent pause = %#v, want %#v, err=%v", pausedAgain, paused, err)
			}
		})
	}
}

func TestSchedulerHTTPRejectsPauseAfterOneTimeAcceptance(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run exactly once", Condition: "at the configured time", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(time.Second) }
	if _, err := newNativeScheduler(manager, workspace).Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost,
		"/api/workspaces/"+workspace.ID+"/scheduler/"+created.ID+"/pause", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("pause completed one-time HTTP status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "schedule_state_conflict" || response["error"] != "completed schedule cannot be paused" {
		t.Fatalf("pause completed one-time HTTP error = %#v", response)
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if config.Schedules[0].State != app.ScheduleStateActive || config.Schedules[0].Revision != created.Revision {
		t.Fatalf("HTTP pause mutated completed one-time definition = %#v", config.Schedules[0])
	}
}

func TestNativeSchedulerMigrationMessagesProgressByDigest(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	legacy, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "msg-legacy-scheduler-tick", ResourceID: app.SchedulerResourceID, Text: "legacy tick",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type:      resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	migrated := migrateSchedulerV1ForTest(t, puaWorkspace,
		schedulerV1TestDefinition{
			ID:          "schedule-444444444444444444444444",
			Description: "First",
			Condition:   "tomorrow",
			Target:      "workspace",
			CreatedAt:   "2026-08-01T00:00:00Z",
			UpdatedAt:   "2026-08-01T00:00:00Z",
		},
		schedulerV1TestDefinition{
			ID:          "schedule-555555555555555555555555",
			Description: "Second",
			Condition:   "next week",
			Target:      app.SchedulerResourceID,
			CreatedAt:   "2026-08-02T00:00:00Z",
			UpdatedAt:   "2026-08-02T00:00:00Z",
		},
	)
	first := migrated[0]
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		// The compilation message is accepted before the intentionally absent
		// AgentHub endpoint fails to wake; that wake error is recoverable.
		t.Logf("initial migration wake: %v", err)
	}
	countMigrationMessages := func() int {
		mailbox, loadErr := loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		count := 0
		for _, message := range mailbox.Messages {
			if message.Type == resourceMessageTypeScheduleMigration {
				count++
			}
		}
		return count
	}
	if got := countMigrationMessages(); got != 1 {
		t.Fatalf("migration messages = %d, want 1", got)
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	foundRetargetGuidance := false
	for _, message := range mailbox.Messages {
		if message.Type == resourceMessageTypeScheduleMigration && strings.Contains(message.Text, "cannot be an execution target") {
			foundRetargetGuidance = true
		}
	}
	if !foundRetargetGuidance {
		t.Fatalf("migration message omitted Scheduler self-target guidance: %#v", mailbox.Messages)
	}
	cancelled, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
	if err != nil || !found || cancelled.Status != resourceMessageUndeliverable || cancelled.LastErrorCode != "scheduler_v1_retired" {
		t.Fatalf("legacy Scheduler tick was not cancelled: %#v, found=%v, err=%v", cancelled, found, err)
	}
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: first.ID, ExpectedRevision: first.Revision, Trigger: &trigger}); err != nil {
		t.Fatal(err)
	}
	_ = manager.reconcileSchedulerLocked(context.Background(), workspace)
	if got := countMigrationMessages(); got != 2 {
		t.Fatalf("partial compilation did not advance digest: %d", got)
	}
	_ = manager.reconcileSchedulerLocked(context.Background(), workspace)
	if got := countMigrationMessages(); got != 2 {
		t.Fatalf("unchanged migration digest spun another message: %d", got)
	}
}

func TestNativeSchedulerRetiresLegacyTickStates(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	retirementTime := time.Now().UTC().Truncate(time.Second)
	manager.now = func() time.Time { return retirementTime }
	const retiredError = "Legacy scheduler tick was retired before AgentHub delivery"
	tests := []struct {
		name        string
		messageType string
		status      string
		wantStatus  string
		wantRetired bool
	}{
		{name: "queued", messageType: resourceMessageTypeSchedulerTick, status: resourceMessageQueued, wantStatus: resourceMessageUndeliverable, wantRetired: true},
		{name: "delivered terminal", messageType: resourceMessageTypeSchedulerTick, status: resourceMessageDelivered, wantStatus: resourceMessageDelivered},
		{name: "cancelled", messageType: resourceMessageTypeSchedulerTick, status: resourceMessageCancelled, wantStatus: resourceMessageCancelled},
		{name: "undeliverable", messageType: resourceMessageTypeSchedulerTick, status: resourceMessageUndeliverable, wantStatus: resourceMessageUndeliverable},
		{name: "ordinary queued", status: resourceMessageQueued, wantStatus: resourceMessageQueued},
		{name: "native occurrence", messageType: resourceMessageTypeScheduleOccurrence, status: resourceMessageQueued, wantStatus: resourceMessageQueued},
		{name: "native migration", messageType: resourceMessageTypeScheduleMigration, status: resourceMessageDelivering, wantStatus: resourceMessageDelivering},
	}
	stamp := manager.now().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := mutateResourceMailboxStoreForResource(workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		for index, test := range tests {
			store.Mailbox.NextSequence++
			message := resourceMailboxMessage{
				ID: "msg-legacy-state-" + strconv.Itoa(index), Sequence: store.Mailbox.NextSequence,
				ResourceID: app.SchedulerResourceID, Text: test.name, Role: "system",
				SubscribeResult: false, ResultSubscriptionStatus: resourceResultSubscriptionDisabled,
				Type: test.messageType, RequestedMode: resourceMessageModeEnqueue,
				ActualMode: resourceMessageModeEnqueue, ModeFrozen: true, Status: test.status,
				AcceptedAt: stamp, UpdatedAt: stamp,
			}
			switch test.status {
			case resourceMessageDelivered:
				message.DeliveredAt = stamp
				message.TerminalAt = stamp
				message.TurnTerminalAt = stamp
			case resourceMessageCancelled, resourceMessageUndeliverable, resourceMessageDeliveryUnknown:
				message.TerminalAt = stamp
				message.LastErrorCode = "existing_" + strings.ReplaceAll(test.name, " ", "_")
				message.LastError = "existing terminal state"
			}
			store.Mailbox.Messages = append(store.Mailbox.Messages, message)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := make(map[string]resourceMailboxMessage, len(tests))
	for index := range tests {
		message, found, err := mailboxMessageByID(workspace.Path, "msg-legacy-state-"+strconv.Itoa(index))
		if err != nil || !found {
			t.Fatalf("load initial state %d: found=%v err=%v", index, found, err)
		}
		before[message.ID] = message
	}

	native := newNativeScheduler(manager, workspace)
	if err := native.cancelLegacyTicks(context.Background()); err != nil {
		t.Fatal(err)
	}
	for index, test := range tests {
		id := "msg-legacy-state-" + strconv.Itoa(index)
		message, found, err := mailboxMessageByID(workspace.Path, id)
		if err != nil || !found {
			t.Fatalf("load %s state: found=%v err=%v", test.name, found, err)
		}
		if message.Status != test.wantStatus {
			t.Errorf("%s status = %q, want %q", test.name, message.Status, test.wantStatus)
		}
		if test.wantRetired {
			if message.LastErrorCode != "scheduler_v1_retired" || message.LastError != retiredError ||
				message.TerminalAt == "" || message.UpdatedAt != message.TerminalAt || !message.receipt {
				t.Errorf("%s retirement receipt = %#v", test.name, message)
			}
		} else if !reflect.DeepEqual(message, before[id]) {
			t.Errorf("%s changed: before=%#v after=%#v", test.name, before[id], message)
		}
	}

	store, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	hotBeforeRestart, err := os.ReadFile(resourceMailboxHotPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	receiptsBeforeRestart, err := os.ReadFile(resourceMailboxReceiptPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	restartedManager := newAgentManager(manager.server)
	restartedManager.now = func() time.Time { return manager.now().Add(time.Hour) }
	manager.server.agents = restartedManager
	if err := newNativeScheduler(restartedManager, workspace).cancelLegacyTicks(context.Background()); err != nil {
		t.Fatal(err)
	}
	hotAfterRestart, err := os.ReadFile(resourceMailboxHotPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	receiptsAfterRestart, err := os.ReadFile(resourceMailboxReceiptPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	if string(hotAfterRestart) != string(hotBeforeRestart) || string(receiptsAfterRestart) != string(receiptsBeforeRestart) {
		t.Fatal("repeated legacy tick cancellation rewrote stable mailbox documents")
	}
}

func TestSchedulerMailboxRecoveryRetiresLegacyTickBeforeNativeReconcile(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	legacy, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "msg-legacy-recovery-first", ResourceID: app.SchedulerResourceID, Text: "legacy tick",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Model startup recovery winning the race with NativeScheduler.Reconcile.
	restarted := newAgentManager(manager.server)
	restarted.now = manager.now
	manager.server.agents = restarted
	if err := restarted.reconcileWorkspaceMailboxes(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	retired, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
	if err != nil || !found || retired.Status != resourceMessageUndeliverable ||
		retired.LastErrorCode != "scheduler_v1_retired" || retired.TerminalAt == "" || !retired.receipt {
		t.Fatalf("recovery-first retired receipt = %#v, found=%v err=%v", retired, found, err)
	}
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if len(messageIDs) != 0 {
		t.Fatalf("startup mailbox recovery dispatched a legacy tick: %#v", messageIDs)
	}

	store, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	hotBeforeRepeat, err := os.ReadFile(resourceMailboxHotPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	receiptsBeforeRepeat, err := os.ReadFile(resourceMailboxReceiptPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileWorkspaceMailboxes(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	hotAfterRepeat, err := os.ReadFile(resourceMailboxHotPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	receiptsAfterRepeat, err := os.ReadFile(resourceMailboxReceiptPath(store.Directory))
	if err != nil {
		t.Fatal(err)
	}
	if string(hotAfterRepeat) != string(hotBeforeRepeat) || string(receiptsAfterRepeat) != string(receiptsBeforeRepeat) {
		t.Fatal("repeated startup recovery rewrote the retired Scheduler mailbox")
	}
}

func TestSchedulerNaturalLanguageMessageRetiresLegacyTickBeforeDelivery(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Ada"); err != nil {
		t.Fatal(err)
	}
	legacy, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "msg-legacy-before-web-chat", ResourceID: app.SchedulerResourceID, Text: "legacy tick",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	type deliveryObservation struct {
		messageID  string
		tickStatus string
		tickCode   string
	}
	observed := make(chan deliveryObservation, 2)
	fake.mu.Lock()
	fake.messageHook = func(_ string, input agentHubInboundMessage) {
		tick, _, loadErr := mailboxMessageByID(workspace.Path, legacy.ID)
		if loadErr != nil {
			observed <- deliveryObservation{messageID: input.MessageID, tickCode: loadErr.Error()}
			return
		}
		observed <- deliveryObservation{messageID: input.MessageID, tickStatus: tick.Status, tickCode: tick.LastErrorCode}
	}
	fake.mu.Unlock()

	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/scheduler", strings.NewReader(`{"description":"Review","condition":"tomorrow morning","target":"workspace"}`))
	request.Header.Set(workspaceUserHeader, "Ada")
	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("natural Scheduler message = %d %s", recorder.Code, recorder.Body.String())
	}
	var response resourceMessageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	var observation deliveryObservation
	select {
	case observation = <-observed:
	default:
		t.Fatal("Scheduler chat never reached the AgentHub message boundary")
	}
	if observation.messageID != response.MessageID || observation.tickStatus != resourceMessageUndeliverable || observation.tickCode != "scheduler_v1_retired" {
		t.Fatalf("delivery boundary observed %#v, want retired tick before chat %q", observation, response.MessageID)
	}
	retired, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
	if err != nil || !found || retired.Status != resourceMessageUndeliverable || retired.LastErrorCode != "scheduler_v1_retired" {
		t.Fatalf("Web-path retired tick = %#v, found=%v err=%v", retired, found, err)
	}
	chat, found, err := mailboxMessageByID(workspace.Path, response.MessageID)
	if err != nil || !found || chat.Status != resourceMessageDelivered {
		t.Fatalf("Web Scheduler chat = %#v, found=%v err=%v", chat, found, err)
	}
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if !reflect.DeepEqual(messageIDs, []string{response.MessageID}) {
		t.Fatalf("AgentHub messages = %#v, want only new Scheduler chat %q", messageIDs, response.MessageID)
	}
}

func TestSchedulerMailboxRecoveryRetiresDeliveringTickBeforeNativeReconcile(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	retirementTime := time.Now().UTC().Truncate(time.Second)
	manager.now = func() time.Time { return retirementTime }
	opener := acceptTestResourceMessage(t, manager, workspace, app.SchedulerResourceID, "open scheduler session", resourceMessageModeEnqueue, nil)
	if opener.Status != resourceMessageDelivered {
		t.Fatalf("Scheduler opener = %#v", opener)
	}
	record, found, err := currentResourceGeneration(workspace.Path, app.SchedulerResourceID)
	if err != nil || !found {
		t.Fatalf("Scheduler generation missing: found=%v err=%v", found, err)
	}
	fake.mu.Lock()
	fake.messageIDs = nil
	fake.messageSteers = nil
	fake.messageRoles = nil
	fake.messageSenders = nil
	fake.mu.Unlock()
	stamp := manager.now().Add(-time.Minute).Format(time.RFC3339Nano)
	const tickID = "msg-legacy-delivering-crash"
	if _, err := mutateResourceMailboxStoreForResource(workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		store.Mailbox.NextSequence++
		store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
			ID: tickID, Sequence: store.Mailbox.NextSequence, ResourceID: app.SchedulerResourceID,
			Text: "legacy tick crash window", Role: "system", SubscribeResult: false,
			ResultSubscriptionStatus: resourceResultSubscriptionDisabled,
			Type:                     resourceMessageTypeSchedulerTick, RequestedMode: resourceMessageModeEnqueue,
			ActualMode: resourceMessageModeEnqueue, ModeFrozen: true, Status: resourceMessageDelivering,
			AcceptedAt: stamp, UpdatedAt: stamp, GenerationID: record.GenerationID,
			AgentHubSessionID: record.AgentHubSessionID, AttemptCount: 1, LastAttemptAt: stamp,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hot, err := loadHotResourceMailbox(workspace.Path, app.SchedulerResourceID)
	if err != nil || len(hot.Messages) != 1 || hot.Messages[0].ID != tickID {
		t.Fatalf("durable delivering fixture = %#v, %v", hot.Messages, err)
	}

	if err := manager.reconcileWorkspaceMailboxes(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	retired, found, err := mailboxMessageByID(workspace.Path, tickID)
	if err != nil || !found || retired.Status != resourceMessageUndeliverable ||
		retired.LastErrorCode != "scheduler_v1_retired" || retired.TerminalAt == "" || !retired.receipt {
		t.Fatalf("retired delivering receipt = %#v, found=%v err=%v", retired, found, err)
	}
	if hotWork, err := resourceMailboxHasHotWork(workspace.Path, app.SchedulerResourceID); err != nil || hotWork {
		t.Fatalf("retired delivering tick left hot work: hot=%v err=%v", hotWork, err)
	}
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	actions := append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if len(messageIDs) != 0 {
		t.Fatalf("ordinary mailbox recovery resubmitted retired tick: %#v", messageIDs)
	}
	for _, action := range actions {
		if action == "interrupt" {
			t.Fatalf("canonical absence interrupted the unrelated active Scheduler Turn: %#v", actions)
		}
	}
}

func seedCanonicalLegacyTick(t *testing.T, fake *runtimeFakeAgentHub, workspace serveWorkspace, messageID, status, sessionState, currentTurnID string, turn agentHubTurn, canonical bool) resourceMailboxMessage {
	t.Helper()
	generationID := "gen-" + messageID
	sessionID := "ses-" + messageID
	recordStatus := "idle"
	if sessionState == "running" || sessionState == "waiting_approval" {
		recordStatus = sessionState
	}
	record := generationRecord{
		ID: "record-" + messageID, WorkspaceID: workspace.ID, ResourceID: app.SchedulerResourceID, Generation: 1,
		GenerationID: generationID, AgentHubSessionID: sessionID, AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/" + generationID, Status: recordStatus,
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: sessionID, State: sessionState, CurrentTurnID: currentTurnID,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	})
	message, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: messageID, ResourceID: app.SchedulerResourceID, Text: "legacy tick " + messageID,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		current.Status = status
		current.GenerationID = generationID
		current.AgentHubSessionID = sessionID
		current.AttemptCount = 1
		current.LastAttemptAt = stamp
		if status == resourceMessageDelivered {
			current.DeliveredAt = stamp
			current.TerminalAt = stamp
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !canonical {
		return message
	}
	wire, err := agentHubMailboxMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	event := fake.appendLocked(sessionID, "message.input", fakeMessageInputData(wire))
	turnID := firstNonEmpty(strings.TrimSpace(turn.TurnID), strings.TrimSpace(turn.ID))
	fake.events[sessionID][len(fake.events[sessionID])-1].TurnID = turnID
	if fake.turns[sessionID] == nil {
		fake.turns[sessionID] = make(map[string]agentHubTurn)
	}
	turn.ID, turn.TurnID = turnID, turnID
	turn.TriggerMessageID = message.ID
	turn.FirstEventID = event.ID
	turn.TriggerEventID = event.ID
	fake.turns[sessionID][turnID] = turn
	fake.mu.Unlock()
	return message
}

func TestNativeSchedulerProvesUncertainLegacyTicksWereNotAccepted(t *testing.T) {
	for _, status := range []string{resourceMessageDelivering, resourceMessageDeliveryUnknown} {
		t.Run(status, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			message := seedCanonicalLegacyTick(t, fake, workspace, "msg-uncertain-"+status, status,
				"running", "turn-current-compilation", agentHubTurn{}, false)
			fake.mu.Lock()
			fake.turns[message.AgentHubSessionID] = map[string]agentHubTurn{
				"turn-current-compilation": {
					ID: "turn-current-compilation", TurnID: "turn-current-compilation", Status: "running",
					TriggerMessageID: "msg-current-compilation",
				},
			}
			fake.mu.Unlock()

			if err := newNativeScheduler(manager, workspace).cancelLegacyTicks(context.Background()); err != nil {
				t.Fatal(err)
			}
			retired, found, err := mailboxMessageByID(workspace.Path, message.ID)
			if err != nil || !found || retired.Status != resourceMessageUndeliverable ||
				retired.LastErrorCode != "scheduler_v1_retired" || retired.TerminalAt == "" {
				t.Fatalf("uncertain legacy tick = %#v, found=%v err=%v", retired, found, err)
			}
			fake.mu.Lock()
			actions := append([]string(nil), fake.actions...)
			fake.mu.Unlock()
			if len(actions) != 0 {
				t.Fatalf("canonical absence affected the current Scheduler Turn: %#v", actions)
			}
		})
	}
}

func TestNativeSchedulerWaitsForCanonicalActiveLegacyTick(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	const turnID = "turn-active-legacy-tick"
	message := seedCanonicalLegacyTick(t, fake, workspace, "msg-active-legacy-tick", resourceMessageDelivered,
		"running", turnID, agentHubTurn{TurnID: turnID, Status: "running"}, true)

	if err := newNativeScheduler(manager, workspace).cancelLegacyTicks(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no conditional Turn interrupt") {
		t.Fatalf("active legacy tick error = %v", err)
	}
	pending, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || pending.Status != resourceMessageDelivered || pending.TurnID != turnID ||
		pending.InterruptTurnID != "" || pending.InterruptAt != "" || pending.TurnTerminalAt != "" {
		t.Fatalf("active tick checkpoint = %#v, found=%v err=%v", pending, found, err)
	}
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	active := fake.turns[message.AgentHubSessionID][turnID]
	fake.mu.Unlock()
	if len(actions) != 0 || active.Closed || active.Status != "running" {
		t.Fatalf("active tick AgentHub state: actions=%#v turn=%#v", actions, active)
	}

	// Model a checkpoint written by the previous implementation immediately
	// before its unsafe call. A restart must not replay that Session-wide
	// interrupt now that the API is known to lack a Turn precondition.
	if _, err := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		current.InterruptTurnID = turnID
		current.InterruptAt = "2026-08-01T00:00:02Z"
		current.UpdatedAt = current.InterruptAt
	}); err != nil {
		t.Fatal(err)
	}
	restarted := newAgentManager(manager.server)
	restarted.now = manager.now
	manager.server.agents = restarted
	if err := newNativeScheduler(restarted, workspace).cancelLegacyTicks(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no conditional Turn interrupt") {
		t.Fatalf("restarted active legacy tick error = %v", err)
	}
	fake.mu.Lock()
	actions = append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if len(actions) != 0 {
		t.Fatalf("restart interrupted an active legacy tick: %#v", actions)
	}
	pending, found, err = mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || pending.InterruptTurnID != turnID ||
		pending.InterruptAt != "2026-08-01T00:00:02Z" || pending.TurnTerminalAt != "" {
		t.Fatalf("restarted legacy intent checkpoint = %#v, found=%v err=%v", pending, found, err)
	}
}

func TestNativeSchedulerNeverInterruptsTurnThatReplacesLegacyTick(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	const oldTurnID = "turn-racing-legacy-tick"
	const newTurnID = "turn-new-scheduler-chat"
	message := seedCanonicalLegacyTick(t, fake, workspace, "msg-racing-legacy-tick", resourceMessageDelivered,
		"running", oldTurnID, agentHubTurn{TurnID: oldTurnID, Status: "running"}, true)

	// The Turn endpoint returns the running snapshot, then deterministically
	// closes that Turn and starts an unrelated one. This is the exact window in
	// which a Session-scoped interrupt would cancel the replacement Turn.
	fake.mu.Lock()
	fake.turnHook = func(sessionID, turnID string) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.turnHook = nil
		if sessionID != message.AgentHubSessionID || turnID != oldTurnID {
			return
		}
		old := fake.turns[sessionID][oldTurnID]
		old.Status = "completed"
		old.Closed = true
		old.EndedAt = "2026-08-01T00:00:04Z"
		fake.turns[sessionID][oldTurnID] = old
		fake.turns[sessionID][newTurnID] = agentHubTurn{
			ID: newTurnID, TurnID: newTurnID, Status: "running", TriggerMessageID: "msg-new-scheduler-chat",
		}
		session := fake.sessions[sessionID]
		session.State = "running"
		session.CurrentTurnID = newTurnID
		fake.sessions[sessionID] = session
	}
	fake.mu.Unlock()

	if err := newNativeScheduler(manager, workspace).cancelLegacyTicks(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no conditional Turn interrupt") {
		t.Fatalf("racing legacy tick error = %v", err)
	}
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	session := fake.sessions[message.AgentHubSessionID]
	replacement := fake.turns[message.AgentHubSessionID][newTurnID]
	fake.mu.Unlock()
	if len(actions) != 0 || session.CurrentTurnID != newTurnID || session.State != "running" ||
		replacement.Closed || replacement.Status != "running" {
		t.Fatalf("replacement Turn was disturbed: actions=%#v session=%#v turn=%#v", actions, session, replacement)
	}
	pending, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || pending.TurnTerminalAt != "" || pending.InterruptTurnID != "" || pending.InterruptAt != "" {
		t.Fatalf("racing tick checkpoint = %#v, found=%v err=%v", pending, found, err)
	}

	// The next pass observes the exact legacy Turn's terminal record and may
	// retire it without touching the unrelated active chat.
	if err := newNativeScheduler(manager, workspace).cancelLegacyTicks(context.Background()); err != nil {
		t.Fatal(err)
	}
	retired, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || retired.TurnTerminalAt != "2026-08-01T00:00:04Z" {
		t.Fatalf("terminal legacy tick = %#v, found=%v err=%v", retired, found, err)
	}
	fake.mu.Lock()
	actions = append([]string(nil), fake.actions...)
	session = fake.sessions[message.AgentHubSessionID]
	replacement = fake.turns[message.AgentHubSessionID][newTurnID]
	fake.mu.Unlock()
	if len(actions) != 0 || session.CurrentTurnID != newTurnID || replacement.Closed || replacement.Status != "running" {
		t.Fatalf("terminal proof disturbed replacement Turn: actions=%#v session=%#v turn=%#v", actions, session, replacement)
	}
}

func TestNativeSchedulerLeavesCurrentSchedulerChatAfterTerminalLegacyTick(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	const tickTurnID = "turn-terminal-legacy-tick"
	message := seedCanonicalLegacyTick(t, fake, workspace, "msg-terminal-legacy-tick", resourceMessageDelivered,
		"running", "turn-current-compilation", agentHubTurn{
			TurnID: tickTurnID, Status: "completed", Closed: true, EndedAt: "2026-08-01T00:00:03Z",
		}, true)
	fake.mu.Lock()
	fake.turns[message.AgentHubSessionID]["turn-current-compilation"] = agentHubTurn{
		ID: "turn-current-compilation", TurnID: "turn-current-compilation", Status: "running",
		TriggerMessageID: "msg-current-compilation",
	}
	fake.mu.Unlock()

	if err := newNativeScheduler(manager, workspace).cancelLegacyTicks(context.Background()); err != nil {
		t.Fatal(err)
	}
	retired, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || retired.TurnTerminalAt != "2026-08-01T00:00:03Z" || retired.InterruptAt != "" {
		t.Fatalf("terminal legacy tick = %#v, found=%v err=%v", retired, found, err)
	}
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	session := fake.sessions[message.AgentHubSessionID]
	fake.mu.Unlock()
	if len(actions) != 0 || session.State != "running" || session.CurrentTurnID != "turn-current-compilation" {
		t.Fatalf("terminal tick disturbed current Scheduler chat: actions=%#v session=%#v", actions, session)
	}
}

func TestNativeSchedulerLegacyTickIdentityAmbiguityFailsClosedAcrossRestart(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	message := seedCanonicalLegacyTick(t, fake, workspace, "msg-ambiguous-legacy-tick", resourceMessageDeliveryUnknown,
		"running", "turn-current-compilation", agentHubTurn{}, false)
	if _, err := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		current.AgentHubSessionID = "ses-unrelated"
	}); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		activeManager := manager
		if attempt == 1 {
			activeManager = newAgentManager(manager.server)
			activeManager.now = manager.now
			manager.server.agents = activeManager
		}
		err := newNativeScheduler(activeManager, workspace).cancelLegacyTicks(context.Background())
		if err == nil || !strings.Contains(err.Error(), "identity is ambiguous") {
			t.Fatalf("attempt %d ambiguity error = %v", attempt, err)
		}
	}
	unchanged, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || unchanged.Status != resourceMessageDeliveryUnknown || unchanged.TurnTerminalAt != "" {
		t.Fatalf("ambiguous tick was terminalized: %#v, found=%v err=%v", unchanged, found, err)
	}
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if len(actions) != 0 {
		t.Fatalf("ambiguous tick affected AgentHub: %#v", actions)
	}
}

func TestNativeSchedulerBlocksMigrationUntilLegacyTickTurnIsTerminal(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	migrateSchedulerV1ForTest(t, puaWorkspace, schedulerV1TestDefinition{
		ID: "schedule-666666666666666666666666", Description: "Compile me", Condition: "daily", Target: "workspace",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:00:00Z",
	})
	const turnID = "turn-blocking-legacy-tick"
	message := seedCanonicalLegacyTick(t, fake, workspace, "msg-blocking-legacy-tick", resourceMessageDelivered,
		"running", turnID, agentHubTurn{TurnID: turnID, Status: "running"}, true)
	native := newNativeScheduler(manager, workspace)
	if _, err := native.Reconcile(context.Background(), manager.now()); err == nil ||
		!strings.Contains(err.Error(), "no conditional Turn interrupt") {
		t.Fatalf("active legacy Turn migration error = %v", err)
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range mailbox.Messages {
		if candidate.Type == resourceMessageTypeScheduleMigration {
			t.Fatalf("migration message was accepted before tick terminal proof: %#v", candidate)
		}
	}
	pending, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || pending.TurnTerminalAt != "" || pending.InterruptTurnID != "" || pending.InterruptAt != "" {
		t.Fatalf("pending legacy retirement = %#v, found=%v err=%v", pending, found, err)
	}
	fake.mu.Lock()
	terminal := fake.turns[message.AgentHubSessionID][turnID]
	terminal.Status = "completed"
	terminal.Closed = true
	terminal.EndedAt = "2026-08-01T00:00:05Z"
	fake.turns[message.AgentHubSessionID][turnID] = terminal
	session := fake.sessions[message.AgentHubSessionID]
	session.State = "ready"
	session.CurrentTurnID = ""
	fake.sessions[message.AgentHubSessionID] = session
	fake.mu.Unlock()

	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	mailbox, err = loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	migrationCount := 0
	for _, candidate := range mailbox.Messages {
		if candidate.Type == resourceMessageTypeScheduleMigration {
			migrationCount++
		}
	}
	if migrationCount != 1 {
		t.Fatalf("terminal legacy tick produced %d migration messages, want 1", migrationCount)
	}
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if len(actions) != 0 {
		t.Fatalf("migration issued a Session action: %#v", actions)
	}
}

func TestSchedulerMailboxRecoveryLeavesOtherResourcesUnaffected(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	legacy, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "msg-legacy-with-task-work", ResourceID: app.SchedulerResourceID, Text: "legacy tick",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	taskMessage, err := acceptMailboxMessage(workspace.Path, "project1.task1", resourceMessageRequest{
		Text: "unrelated task work", Mode: resourceMessageModeEnqueue, Role: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.reconcileWorkspaceMailboxes(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	retired, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
	if err != nil || !found || retired.Status != resourceMessageUndeliverable || retired.LastErrorCode != "scheduler_v1_retired" {
		t.Fatalf("Scheduler tick = %#v, found=%v err=%v", retired, found, err)
	}
	delivered, found, err := mailboxMessageByID(workspace.Path, taskMessage.ID)
	if err != nil || !found || delivered.Status != resourceMessageDelivered {
		t.Fatalf("unrelated task message = %#v, found=%v err=%v", delivered, found, err)
	}
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if !reflect.DeepEqual(messageIDs, []string{taskMessage.ID}) {
		t.Fatalf("AgentHub messages = %#v, want only unrelated task %q", messageIDs, taskMessage.ID)
	}
}

func TestSchedulerMailboxRecoveryDeliversNativeMessagesAfterTickRetirement(t *testing.T) {
	for _, messageType := range []string{resourceMessageTypeScheduleOccurrence, resourceMessageTypeScheduleMigration} {
		t.Run(messageType, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			legacy, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
				ID: "msg-legacy-before-" + messageType, ResourceID: app.SchedulerResourceID, Text: "legacy tick",
				RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
				Type: resourceMessageTypeSchedulerTick,
				Causation: &resourceMessageCausation{
					Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			native, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
				ID: "msg-native-after-" + messageType, ResourceID: app.SchedulerResourceID, Text: "native Scheduler work",
				RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
				Type: messageType,
				Causation: &resourceMessageCausation{
					Type: messageType, SourceResourceID: app.SchedulerResourceID, Reason: "native",
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			if err := manager.reconcileWorkspaceMailboxes(context.Background(), workspace); err != nil {
				t.Fatal(err)
			}
			retired, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
			if err != nil || !found || retired.Status != resourceMessageUndeliverable || retired.LastErrorCode != "scheduler_v1_retired" {
				t.Fatalf("Scheduler tick = %#v, found=%v err=%v", retired, found, err)
			}
			delivered, found, err := mailboxMessageByID(workspace.Path, native.ID)
			if err != nil || !found || delivered.Status != resourceMessageDelivered {
				t.Fatalf("native Scheduler message = %#v, found=%v err=%v", delivered, found, err)
			}
			fake.mu.Lock()
			messageIDs := append([]string(nil), fake.messageIDs...)
			fake.mu.Unlock()
			if !reflect.DeepEqual(messageIDs, []string{native.ID}) {
				t.Fatalf("AgentHub messages = %#v, want only native message %q", messageIDs, native.ID)
			}
		})
	}
}

func TestNativeSchedulerPauseResumeSkipsPausedOccurrences(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	repeating, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Repeat", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(repeating.ID); err != nil {
		t.Fatal(err)
	}
	oneTime, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "in the past", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(oneTime.ID); err != nil {
		t.Fatal(err)
	}
	// Mutation validation requires a future one-time trigger. Rewrite the
	// already-paused persisted fixture to model a deadline expiring while paused.
	rewriteSchedulerTestOneTimeDeadline(t, puaWorkspace, oneTime.ID, anchor)
	current := time.Now().UTC()
	manager.now = func() time.Time { return current }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(current)
	if err != nil || len(snapshot.Schedules) != 2 || snapshot.Schedules[0].EffectiveState != app.ScheduleStatePaused || snapshot.Schedules[1].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[1].LastOutcome != schedulerOutcomePaused {
		t.Fatalf("paused snapshot = %#v, %v", snapshot, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("paused occurrences were delivered: %#v", messages)
	}
	if _, err := newNativeScheduler(manager, workspace).Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: repeating.ID}); err != nil {
		t.Fatal(err)
	}
	current = time.Now().UTC()
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = newNativeScheduler(manager, workspace).Snapshot(current)
	next := generationTime(snapshot.Schedules[0].NextRunAt)
	if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStateActive || !next.After(current) {
		t.Fatalf("resume caught up paused occurrences: %#v, %v", snapshot.Schedules[0], err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("resume delivered paused backlog: %#v", messages)
	}
}

func TestNativeSchedulerPausePreservesRepeatingHistory(t *testing.T) {
	tests := []struct {
		name        string
		lastOutcome string
		lastError   string
	}{
		{name: "accepted", lastOutcome: schedulerOutcomeAccepted},
		{name: "busy", lastOutcome: schedulerOutcomeBusy},
		{name: "transient error", lastOutcome: schedulerOutcomeAccepted, lastError: "temporary checkpoint failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Remember recent work", Condition: "every minute", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(-time.Hour).Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			lastOccurrenceAt := now.Add(-time.Minute).Format(time.RFC3339Nano)
			runtime := schedulerScheduleRuntime{
				Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
				EffectiveState: app.ScheduleStateActive, NextRunAt: now.Add(time.Minute).Format(time.RFC3339Nano),
				LastOccurrenceAt: lastOccurrenceAt, LastOutcome: test.lastOutcome, LastError: test.lastError,
				RetryAt: now.Add(time.Second).Format(time.RFC3339Nano), RetryCount: 3,
			}
			native := newNativeScheduler(manager, workspace)
			if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
				t.Fatal(err)
			}
			manager.now = func() time.Time { return now }
			paused, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID})
			if err != nil {
				t.Fatal(err)
			}

			persisted, err := native.schedulerRuntime(created.ID)
			if err != nil || persisted.Revision != paused.Revision || persisted.EffectiveState != app.ScheduleStatePaused ||
				persisted.LastOccurrenceAt != lastOccurrenceAt || persisted.LastOutcome != test.lastOutcome || persisted.LastError != test.lastError ||
				persisted.NextRunAt != "" || persisted.Prepared != nil || persisted.RetryAt != "" || persisted.RetryCount != 0 || persisted.AttentionTarget != "" {
				t.Fatalf("paused repeating history runtime = %#v, %v", persisted, err)
			}
			if deadline := schedulerRuntimeDeadline(persisted, now); !deadline.IsZero() {
				t.Fatalf("paused history deadline = %s, want zero", deadline)
			}

			restarted := newNativeScheduler(newAgentManager(manager.server), workspace)
			snapshot, err := restarted.Snapshot(now)
			if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStatePaused ||
				snapshot.Schedules[0].LastOccurrenceAt != lastOccurrenceAt || snapshot.Schedules[0].LastOutcome != test.lastOutcome ||
				snapshot.Schedules[0].LastError != test.lastError || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
				t.Fatalf("restarted paused history snapshot = %#v, %v", snapshot, err)
			}
		})
	}
}

func TestNativeSchedulerPauseClearsRepeatingDeliveryStateWithoutInventingHistory(t *testing.T) {
	tests := []struct {
		name           string
		effectiveState string
		lastOutcome    string
		lastError      string
		attention      bool
	}{
		{name: "prepared", effectiveState: app.ScheduleStateActive, lastOutcome: schedulerOutcomeBusy},
		{name: "attention", effectiveState: schedulerOutcomeAttention, lastOutcome: schedulerOutcomeAttention, lastError: "target is archived", attention: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Pause in-flight repeat", Condition: "every minute", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(-time.Hour).Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			lastOccurrenceAt := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
			prepared := schedulerPreparedOccurrence{
				ScheduleID: created.ID, ScheduleRevision: created.Revision, Target: created.Target,
				ScheduledFor: now.Add(-time.Minute).Format(time.RFC3339Nano), NextRunAt: now.Add(time.Minute).Format(time.RFC3339Nano),
			}
			runtime := schedulerScheduleRuntime{
				Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
				EffectiveState: test.effectiveState, NextRunAt: prepared.ScheduledFor,
				LastOccurrenceAt: lastOccurrenceAt, LastOutcome: test.lastOutcome, LastError: test.lastError,
				RetryAt: now.Add(time.Minute).Format(time.RFC3339Nano), RetryCount: 2, Prepared: &prepared,
			}
			if test.attention {
				runtime.AttentionTarget = created.Target
			}
			native := newNativeScheduler(manager, workspace)
			if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
				t.Fatal(err)
			}
			manager.now = func() time.Time { return now }
			if _, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID}); err != nil {
				t.Fatal(err)
			}

			persisted, err := native.schedulerRuntime(created.ID)
			if err != nil || persisted.EffectiveState != app.ScheduleStatePaused || persisted.LastOccurrenceAt != lastOccurrenceAt ||
				persisted.LastOutcome != test.lastOutcome || persisted.LastError != test.lastError || persisted.Prepared != nil ||
				persisted.NextRunAt != "" || persisted.RetryAt != "" || persisted.RetryCount != 0 || persisted.AttentionTarget != "" {
				t.Fatalf("paused %s runtime = %#v, %v", test.name, persisted, err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
				t.Fatalf("pause delivered discarded %s occurrence: %#v", test.name, messages)
			}
		})
	}
}

func TestNativeSchedulerPauseResumePreservesRepeatingHistory(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Resume without forgetting", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(-time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	lastOccurrenceAt := now.Add(-time.Minute).Format(time.RFC3339Nano)
	runtime := schedulerScheduleRuntime{
		Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
		EffectiveState: app.ScheduleStateActive, NextRunAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		LastOccurrenceAt: lastOccurrenceAt, LastOutcome: schedulerOutcomeBusy,
	}
	native := newNativeScheduler(manager, workspace)
	if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return now }
	paused, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := native.schedulerRuntime(created.ID)
	resumeBoundary := generationTime(resumed.UpdatedAt)
	if err != nil || paused.State != app.ScheduleStatePaused || resumed.State != app.ScheduleStateActive ||
		persisted.Revision != resumed.Revision || persisted.EffectiveState != app.ScheduleStateActive ||
		persisted.LastOccurrenceAt != lastOccurrenceAt || persisted.LastOutcome != schedulerOutcomeBusy || persisted.LastError != "" ||
		!generationTime(persisted.NextRunAt).After(resumeBoundary) || persisted.Prepared != nil || persisted.RetryAt != "" || persisted.RetryCount != 0 || persisted.AttentionTarget != "" {
		t.Fatalf("pause/resume history runtime = %#v, paused=%#v resumed=%#v err=%v", persisted, paused, resumed, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
		t.Fatalf("pause/resume caught up occurrences: %#v", messages)
	}
}

func TestNativeSchedulerSemanticEditResetsRepeatingHistory(t *testing.T) {
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
				target := "project1"
				trigger := *created.Trigger
				return NativeSchedulerChange{Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Target: &target, Trigger: &trigger}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Replace schedule semantics", Condition: "every minute", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: now.Add(-time.Hour).Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := schedulerScheduleRuntime{
				Revision: created.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, created.Trigger), Target: created.Target,
				EffectiveState: app.ScheduleStateActive, NextRunAt: now.Add(time.Minute).Format(time.RFC3339Nano),
				LastOccurrenceAt: now.Add(-time.Minute).Format(time.RFC3339Nano), LastOutcome: schedulerOutcomeAccepted, LastError: "old transient error",
			}
			native := newNativeScheduler(manager, workspace)
			if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
				t.Fatal(err)
			}
			updated, err := native.Change(context.Background(), test.change(created))
			if err != nil {
				t.Fatal(err)
			}
			mutationAt := generationTime(updated.UpdatedAt)
			if _, err := native.Reconcile(context.Background(), mutationAt); err != nil {
				t.Fatal(err)
			}
			persisted, err := native.schedulerRuntime(created.ID)
			if err != nil || persisted.Revision != updated.Revision || persisted.TriggerDigest != mustSchedulerTriggerDigest(t, updated.Trigger) ||
				persisted.Target != updated.Target || persisted.EffectiveState != app.ScheduleStateActive || persisted.LastOccurrenceAt != "" ||
				persisted.LastOutcome != "" || persisted.LastError != "" || persisted.Prepared != nil || persisted.AttentionTarget != "" ||
				persisted.RetryAt != "" || persisted.RetryCount != 0 || !generationTime(persisted.NextRunAt).After(mutationAt) {
				t.Fatalf("%s edit runtime = %#v, %v", test.name, persisted, err)
			}
		})
	}
}

func TestNativeSchedulerPausedOneTimeUsesExactDeadline(t *testing.T) {
	t.Run("pause through native scheduler", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
		before := at.Add(-time.Minute)
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Once", Condition: "at the configured time", Target: "workspace",
			Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
		})
		if err != nil {
			t.Fatal(err)
		}
		manager.now = func() time.Time { return before }
		native := newNativeScheduler(manager, workspace)
		paused, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID})
		if err != nil || paused.State != app.ScheduleStatePaused {
			t.Fatalf("pause = %#v, %v", paused, err)
		}

		snapshot, err := native.Snapshot(before)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStatePaused || snapshot.Schedules[0].NextRunAt != at.Format(time.RFC3339Nano) || snapshot.NextWakeAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("paused one-time snapshot = %#v", snapshot)
		}
		runtime, err := native.schedulerRuntime(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if deadline := schedulerRuntimeDeadline(runtime, before); !deadline.Equal(at) {
			t.Fatalf("paused one-time runtime deadline = %s, want %s; runtime=%#v", deadline, at, runtime)
		}

		justBefore := at.Add(-time.Nanosecond)
		deadline, err := native.Reconcile(context.Background(), justBefore)
		if err != nil || !deadline.Equal(at) {
			t.Fatalf("just-before reconcile deadline = %s, want %s; err=%v", deadline, at, err)
		}
		snapshot, err = native.Snapshot(justBefore)
		if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStatePaused || snapshot.Schedules[0].NextRunAt != at.Format(time.RFC3339Nano) || snapshot.NextWakeAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("just-before paused snapshot = %#v, %v", snapshot, err)
		}
		if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
			t.Fatalf("paused one-time occurrence was delivered before deadline: %#v", messages)
		}

		deadline, err = native.Reconcile(context.Background(), at)
		if err != nil || !deadline.IsZero() {
			t.Fatalf("boundary reconcile deadline = %s, want zero; err=%v", deadline, err)
		}
		snapshot, err = native.Snapshot(at)
		if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomePaused || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
			t.Fatalf("boundary paused snapshot = %#v, %v", snapshot, err)
		}
		runtime, err = native.schedulerRuntime(created.ID)
		if err != nil || !schedulerRuntimeDeadline(runtime, at).IsZero() {
			t.Fatalf("terminal paused runtime = %#v, %v", runtime, err)
		}
		if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
			t.Fatalf("paused one-time occurrence was delivered at deadline: %#v", messages)
		}
	})

	t.Run("restart reconstructs checkpoint", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Once", Condition: "across restart", Target: "workspace",
			Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
		})
		if err != nil {
			t.Fatal(err)
		}
		paused, err := puaWorkspace.PauseSchedule(created.ID)
		if err != nil {
			t.Fatal(err)
		}

		restartedManager := newAgentManager(manager.server)
		restarted := newNativeScheduler(restartedManager, workspace)
		before := at.Add(-time.Nanosecond)
		deadline, err := restarted.Reconcile(context.Background(), before)
		if err != nil || !deadline.Equal(at) {
			t.Fatalf("reconstructed deadline = %s, want %s; err=%v", deadline, at, err)
		}
		runtime, err := restarted.schedulerRuntime(created.ID)
		if err != nil || runtime.Revision != paused.Revision || runtime.EffectiveState != app.ScheduleStatePaused || runtime.NextRunAt != at.Format(time.RFC3339Nano) || !schedulerRuntimeDeadline(runtime, before).Equal(at) {
			t.Fatalf("reconstructed paused runtime = %#v, %v", runtime, err)
		}
		snapshot, err := restarted.Snapshot(before)
		if err != nil || snapshot.Schedules[0].NextRunAt != at.Format(time.RFC3339Nano) || snapshot.NextWakeAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("reconstructed paused snapshot = %#v, %v", snapshot, err)
		}
		if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
			t.Fatalf("reconstruction delivered paused occurrence: %#v", messages)
		}
	})
}

func TestNativeSchedulerPausedRepeatingHasNoDeadline(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC().Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Repeat", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return anchor.Add(time.Second) }
	native := newNativeScheduler(manager, workspace)
	paused, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangePause, ID: created.ID})
	if err != nil || paused.State != app.ScheduleStatePaused {
		t.Fatalf("pause repeating schedule = %#v, %v", paused, err)
	}
	deadline, err := native.Reconcile(context.Background(), anchor.Add(10*time.Minute))
	if err != nil || !deadline.IsZero() {
		t.Fatalf("paused repeating deadline = %s, want zero; err=%v", deadline, err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.EffectiveState != app.ScheduleStatePaused || runtime.NextRunAt != "" || !schedulerRuntimeDeadline(runtime, anchor).IsZero() {
		t.Fatalf("paused repeating runtime = %#v, %v", runtime, err)
	}
	snapshot, err := native.Snapshot(anchor.Add(10 * time.Minute))
	if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStatePaused || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("paused repeating snapshot = %#v, %v", snapshot, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("paused repeating occurrences were delivered: %#v", messages)
	}
}

func TestExpiredOneTimePauseRuntimePathsMatch(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	now := at
	schedule := app.Schedule{
		ID: "schedule-paused-expiry", Revision: 7, State: app.ScheduleStatePaused,
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	}
	digest := mustSchedulerTriggerDigest(t, schedule.Trigger)
	want := schedulerScheduleRuntime{
		Revision: schedule.Revision, ActivationRevision: schedule.Revision, TriggerDigest: digest, EffectiveState: app.ScheduleStateCompleted,
		LastOccurrenceAt: at.Format(time.RFC3339Nano), LastOutcome: schedulerOutcomePaused,
	}
	native := newNativeScheduler(manager, workspace)
	tests := []struct {
		name string
		run  func(*testing.T) schedulerScheduleRuntime
	}{
		{
			name: "initial runtime",
			run: func(t *testing.T) schedulerScheduleRuntime {
				runtime, err := initialScheduleRuntime(schedule, now)
				if err != nil {
					t.Fatal(err)
				}
				return runtime
			},
		},
		{
			name: "paused reconciliation",
			run: func(t *testing.T) schedulerScheduleRuntime {
				runtime := schedulerScheduleRuntime{
					Revision: schedule.Revision, TriggerDigest: digest, EffectiveState: app.ScheduleStateActive,
					NextRunAt: now.Add(time.Minute).Format(time.RFC3339Nano), LastOccurrenceAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
					LastOutcome: schedulerOutcomeAttention, LastError: "stale attention error", AttentionTarget: "workspace",
					RetryAt: now.Add(2 * time.Minute).Format(time.RFC3339Nano), RetryCount: 4, Prepared: &schedulerPreparedOccurrence{},
				}
				if err := native.reconcilePausedSchedule(schedule, runtime, now); err != nil {
					t.Fatal(err)
				}
				runtime, err := native.schedulerRuntime(schedule.ID)
				if err != nil {
					t.Fatal(err)
				}
				return runtime
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.run(t); !reflect.DeepEqual(got, want) {
				t.Fatalf("paused expiry runtime = %#v, want %#v", got, want)
			}
		})
	}
}

func TestNativeSchedulerResumeSkipsExpiredOneTimeWithoutReconcile(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "in one hour", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(created.ID); err != nil {
		t.Fatal(err)
	}

	// Model a stopped Server: the paused definition is not reconciled until a
	// resume request arrives after its one-time occurrence has elapsed.
	manager.now = func() time.Time { return at.Add(time.Minute) }
	native := newNativeScheduler(manager, workspace)
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != app.ScheduleStateActive {
		t.Fatalf("resumed schedule state = %q, want active", resumed.State)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err := native.Snapshot(manager.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomePaused || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("resumed expired one-time snapshot = %#v", snapshot)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("resume delivered an occurrence skipped while paused: %#v", messages)
	}
}

func TestNativeSchedulerResumeUsesPersistedTransitionBoundary(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "after resume", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(created.ID); err != nil {
		t.Fatal(err)
	}

	// Move the persisted deadline behind the wall clock without sleeping. The
	// Scheduler clock still reports an instant just before that deadline, so
	// the pre-resume paused reconciliation cannot classify the occurrence.
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	rewriteSchedulerTestOneTimeDeadline(t, puaWorkspace, created.ID, at)
	manager.now = func() time.Time { return at.Add(-time.Nanosecond) }

	native := newNativeScheduler(manager, workspace)
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	resumeBoundary := generationTime(resumed.UpdatedAt)
	if resumed.State != app.ScheduleStateActive || resumeBoundary.Before(at) {
		t.Fatalf("resume transition = %#v, want boundary at or after %s", resumed, at)
	}
	snapshot, err := native.Snapshot(resumeBoundary)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].State != app.ScheduleStateActive || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomePaused || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("resume race snapshot = %#v", snapshot)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("resume race delivered paused occurrence: %#v", messages)
	}
	if _, err := native.Reconcile(context.Background(), resumeBoundary); err != nil {
		t.Fatal(err)
	}
	afterReconcile, err := native.Snapshot(resumeBoundary)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterReconcile, snapshot) {
		t.Fatalf("post-resume reconcile changed terminal snapshot: before=%#v after=%#v", snapshot, afterReconcile)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("post-resume reconcile delivered paused occurrence: %#v", messages)
	}
}

func TestNativeSchedulerPersistedResumeBoundarySurvivesRestartWindow(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "across restart", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := puaWorkspace.PauseSchedule(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	rewriteSchedulerTestOneTimeDeadline(t, puaWorkspace, created.ID, at)
	beforeDeadline := at.Add(-time.Nanosecond)
	native := newNativeScheduler(manager, workspace)
	if _, err := native.Reconcile(context.Background(), beforeDeadline); err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != paused.Revision || runtime.EffectiveState != app.ScheduleStatePaused {
		t.Fatalf("pre-resume paused runtime = %#v, %v", runtime, err)
	}

	// Model a crash after the portable transition commits and before Change can
	// refresh the runtime checkpoint.
	resumed, err := puaWorkspace.ResumeSchedule(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumeBoundary := generationTime(resumed.UpdatedAt)
	if resumeBoundary.Before(at) {
		t.Fatalf("persisted resume boundary = %s, want at or after %s", resumeBoundary, at)
	}
	restartedManager := newAgentManager(manager.server)
	restarted := newNativeScheduler(restartedManager, workspace)
	if _, err := restarted.Reconcile(context.Background(), resumeBoundary); err != nil {
		t.Fatal(err)
	}
	snapshot, err := restarted.Snapshot(resumeBoundary)
	if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomePaused || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("restart-window snapshot = %#v, %v", snapshot, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("restart window delivered paused occurrence: %#v", messages)
	}
}

func TestNativeSchedulerResumeDeliversOneTimeStrictlyAfterTransition(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "after resume", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(created.ID); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(-time.Minute) }

	native := newNativeScheduler(manager, workspace)
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !at.After(generationTime(resumed.UpdatedAt)) {
		t.Fatalf("deadline %s is not strictly after resume transition %#v", at, resumed)
	}
	if _, err := native.Reconcile(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	snapshot, err := native.Snapshot(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomeAccepted || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) || snapshot.Schedules[0].NextRunAt != "" {
		t.Fatalf("future-after-resume snapshot = %#v", snapshot)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 1 {
		t.Fatalf("future-after-resume messages = %#v, want one", messages)
	}
}

func TestNativeSchedulerUnavailableTargetPreservesPreparedOccurrence(t *testing.T) {
	tests := []struct {
		name        string
		makeMissing func(*testing.T, *app.Workspace, string, string) func()
	}{
		{
			name: "archived",
			makeMissing: func(t *testing.T, workspace *app.Workspace, workspacePath, targetPath string) func() {
				archived, err := workspace.ArchiveResource("project1.task1")
				if err != nil {
					t.Fatal(err)
				}
				archivedPath := filepath.Join(workspacePath, filepath.FromSlash(archived.Path))
				return func() {
					if err := os.Rename(archivedPath, targetPath); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "missing",
			makeMissing: func(t *testing.T, _ *app.Workspace, _ string, targetPath string) func() {
				detachedPath := filepath.Join(t.TempDir(), "missing-target")
				if err := os.Rename(targetPath, detachedPath); err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := os.Rename(detachedPath, targetPath); err != nil {
						t.Fatal(err)
					}
				}
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
			target, err := puaWorkspace.ResourceValue("project1.task1")
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Recover target", Condition: "once", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(workspace.Path, filepath.FromSlash(target.Path))
			restore := test.makeMissing(t, puaWorkspace, workspace.Path, targetPath)
			now := at.Add(time.Second)
			native := newNativeScheduler(manager, workspace)
			deadline, err := native.Reconcile(context.Background(), now)
			if err != nil || !deadline.IsZero() {
				t.Fatalf("unavailable target deadline = %s, %v", deadline, err)
			}
			runtime, err := native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.LastOccurrenceAt != "" {
				t.Fatalf("unavailable target advanced the occurrence: %#v, %v", runtime, err)
			}
			prepared := *runtime.Prepared
			if prepared.Target != schedule.Target || prepared.ScheduledFor != at.Format(time.RFC3339Nano) || prepared.CoalescedThrough != at.Format(time.RFC3339Nano) {
				t.Fatalf("unavailable target prepared occurrence = %#v", prepared)
			}
			if deadline, err = native.Reconcile(context.Background(), now.Add(time.Minute)); err != nil || !deadline.IsZero() {
				t.Fatalf("repeated attention deadline = %s, %v", deadline, err)
			}
			runtime, err = native.schedulerRuntime(schedule.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
				t.Fatalf("unavailable target accepted mailbox work: %#v", messages)
			}

			restore()
			if _, err := native.Reconcile(context.Background(), now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
			assertDeliveredPreparedOccurrence(t, messages, prepared)
			runtime, err = native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != prepared.CoalescedThrough {
				t.Fatalf("restored target checkpoint = %#v, %v", runtime, err)
			}
			if _, err := native.Reconcile(context.Background(), now.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("restored target duplicated the occurrence: %#v", messages)
			}
		})
	}
}

func TestNativeSchedulerTargetEditReplacesPreparedOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Archived target", Condition: "once", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(time.Second) }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	snapshot, err := native.Snapshot(manager.now())
	if err != nil || snapshot.Schedules[0].EffectiveState != schedulerOutcomeAttention || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("archived target snapshot = %#v, %v", snapshot, err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Prepared == nil {
		t.Fatalf("archived target did not preserve prepared occurrence: %#v, %v", runtime, err)
	}
	if runtime.Target != created.Target || runtime.AttentionTarget != created.Target {
		t.Fatalf("attention runtime target identity = %#v", runtime)
	}
	prepared := *runtime.Prepared
	// Model a legacy attention checkpoint. Its consistent prepared and
	// attention identities can establish sameness, but cannot hide a retarget.
	runtime.Target = ""
	if err := native.storeSchedulerRuntime(created.ID, runtime); err != nil {
		t.Fatal(err)
	}
	description := "Still archived"
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = native.Snapshot(manager.now())
	if err != nil || snapshot.Schedules[0].EffectiveState != schedulerOutcomeAttention {
		t.Fatalf("unrelated edit cleared target attention: %#v, %v", snapshot, err)
	}
	runtime, err = native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != updated.Revision || runtime.Target != updated.Target {
		t.Fatalf("unrelated edit runtime = %#v, %v", runtime, err)
	}
	assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
	target := "workspace"
	retargeted, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: updated.Revision, Target: &target})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].ID == prepared.MessageID || messages[0].Causation == nil || messages[0].Causation.ScheduleRevision != retargeted.Revision {
		t.Fatalf("retargeted attention schedule did not prepare a new revision: %#v", messages)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
		t.Fatalf("retargeting delivered the discarded occurrence: %#v", messages)
	}
	runtime, err = native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != retargeted.Revision || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("retargeted attention checkpoint = %#v, %v", runtime, err)
	}
}

func newOneTimeRetargetFixture(t *testing.T, target string) (*runtimeFakeAgentHub, *agentManager, serveWorkspace, *app.Workspace, *NativeScheduler, app.Schedule, time.Time) {
	t.Helper()
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	t.Cleanup(hub.Close)
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2035, time.March, 14, 9, 26, 53, 123456789, time.UTC)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run exactly once", Condition: "at the fixed instant", Target: target,
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(time.Second) }
	return fake, manager, workspace, puaWorkspace, newNativeScheduler(manager, workspace), schedule, at
}

func seedOneTimePreparedRetarget(t *testing.T, native *NativeScheduler, schedule app.Schedule, at time.Time) schedulerPreparedOccurrence {
	t.Helper()
	prepared, err := native.prepareOccurrence(schedule, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := initialScheduleRuntime(schedule, at.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runtime.Prepared = &prepared
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestNativeSchedulerCompletedOneTimeRetargetStaysTerminal(t *testing.T) {
	fake, manager, workspace, puaWorkspace, native, schedule, at := newOneTimeRetargetFixture(t, "project1.task1")
	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("initial external occurrence inputs = %d, want 1", got)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 {
		t.Fatalf("initial one-time messages = %#v", messages)
	}

	newTarget := "workspace"
	retargeted, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: schedule.ID, ExpectedRevision: schedule.Revision, Target: &newTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeReconcile, err := native.Snapshot(manager.now())
	if err != nil || len(beforeReconcile.Schedules) != 1 || beforeReconcile.Schedules[0].Revision != retargeted.Revision ||
		beforeReconcile.Schedules[0].EffectiveState != app.ScheduleStateCompleted || beforeReconcile.Schedules[0].LastOutcome != schedulerOutcomeAccepted ||
		beforeReconcile.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) || beforeReconcile.Schedules[0].NextRunAt != "" || beforeReconcile.NextWakeAt != "" {
		t.Fatalf("retarget revision projection = %#v, %v", beforeReconcile, err)
	}
	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.Revision != retargeted.Revision || runtime.Target != newTarget || runtime.Prepared != nil ||
		runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("completed retarget checkpoint = %#v, %v", runtime, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, newTarget); len(messages) != 0 {
		t.Fatalf("completed retarget delivered to the new target: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("completed retarget external occurrence inputs = %d, want 1", got)
	}
}

func TestNativeSchedulerAcceptedPreparedRetargetCommitsOldReceipt(t *testing.T) {
	fake, manager, workspace, puaWorkspace, native, schedule, at := newOneTimeRetargetFixture(t, "project1.task1")
	prepared := seedOneTimePreparedRetarget(t, native, schedule, at)
	if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
		t.Fatal(err)
	}
	if got := schedulerAgentHubInputCount(fake); got != 0 {
		t.Fatalf("mailbox acceptance caused %d external inputs, want 0", got)
	}
	newTarget := "workspace"
	retargeted, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: schedule.ID, ExpectedRevision: schedule.Revision, Target: &newTarget,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Model a restart after the portable retarget committed, while the source
	// checkpoint still contains the accepted old-revision Prepared payload.
	restartedManager := newAgentManager(manager.server)
	restartedManager.now = manager.now
	manager.server.agents = restartedManager
	restarted := newNativeScheduler(restartedManager, workspace)
	if _, err := restarted.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	runtime, err := restarted.schedulerRuntime(schedule.ID)
	if err != nil || runtime.Revision != retargeted.Revision || runtime.Target != newTarget || runtime.Prepared != nil ||
		runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("accepted retarget checkpoint = %#v, %v", runtime, err)
	}
	assertDeliveredPreparedOccurrence(t, scheduleOccurrenceMessages(t, workspace.Path, schedule.Target), prepared)
	if messages := scheduleOccurrenceMessages(t, workspace.Path, newTarget); len(messages) != 0 {
		t.Fatalf("accepted retarget duplicated work on the new target: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 0 {
		t.Fatalf("receipt reconciliation woke a target: external inputs = %d", got)
	}

	if err := restartedManager.withResourceController(context.Background(), workspace, schedule.Target, func() error {
		return restartedManager.reconcileResourceMailboxLocked(context.Background(), workspace, schedule.Target)
	}); err != nil {
		t.Fatal(err)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("accepted old-target occurrence external inputs = %d, want 1", got)
	}
	secondManager := newAgentManager(manager.server)
	secondManager.now = manager.now
	manager.server.agents = secondManager
	if _, err := newNativeScheduler(secondManager, workspace).Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("restart duplicated the accepted occurrence externally: inputs = %d", got)
	}
}

func TestNativeSchedulerUnacceptedPreparedRetargetRedirects(t *testing.T) {
	fake, manager, workspace, puaWorkspace, native, schedule, at := newOneTimeRetargetFixture(t, "project1.task1")
	prepared := seedOneTimePreparedRetarget(t, native, schedule, at)
	newTarget := "workspace"
	retargeted, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: schedule.ID, ExpectedRevision: schedule.Revision, Target: &newTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("unaccepted old target received discarded work: %#v", messages)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, newTarget)
	if len(messages) != 1 || messages[0].ID == prepared.MessageID || messages[0].Causation == nil ||
		messages[0].Causation.ScheduleRevision != retargeted.Revision || messages[0].Causation.ScheduledFor != at.Format(time.RFC3339Nano) {
		t.Fatalf("redirected one-time occurrence = %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("redirected occurrence external inputs = %d, want 1", got)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.Revision != retargeted.Revision || runtime.Target != newTarget || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateCompleted {
		t.Fatalf("redirected retarget checkpoint = %#v, %v", runtime, err)
	}
	restartedManager := newAgentManager(manager.server)
	restartedManager.now = manager.now
	manager.server.agents = restartedManager
	if _, err := newNativeScheduler(restartedManager, workspace).Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("redirect restart duplicated external work: inputs = %d", got)
	}
}

func TestNativeSchedulerCompletedRetargetSurvivesRestartBeforeCheckpointPromotion(t *testing.T) {
	fake, manager, workspace, puaWorkspace, native, schedule, at := newOneTimeRetargetFixture(t, "project1.task1")
	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	newTarget := "workspace"
	retargeted, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: schedule.ID, ExpectedRevision: schedule.Revision, Target: &newTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedManager := newAgentManager(manager.server)
	restartedManager.now = manager.now
	manager.server.agents = restartedManager
	restarted := newNativeScheduler(restartedManager, workspace)
	snapshot, err := restarted.Snapshot(manager.now())
	if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].Revision != retargeted.Revision ||
		snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) || snapshot.NextWakeAt != "" {
		t.Fatalf("restart retarget projection = %#v, %v", snapshot, err)
	}
	if _, err := restarted.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, newTarget); len(messages) != 0 {
		t.Fatalf("restart retarget delivered to the new target: %#v", messages)
	}
	if got := schedulerAgentHubInputCount(fake); got != 1 {
		t.Fatalf("restart retarget external occurrence inputs = %d, want 1", got)
	}
}

func TestNativeSchedulerOneTimeTriggerEditCreatesNewOccurrence(t *testing.T) {
	fake, manager, workspace, puaWorkspace, native, schedule, at := newOneTimeRetargetFixture(t, "project1.task1")
	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	newAt := at.Add(time.Hour)
	newTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: newAt.Format(time.RFC3339Nano)}
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: schedule.ID, ExpectedRevision: schedule.Revision, Trigger: &newTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return newAt.Add(time.Second) }
	if _, err := native.Reconcile(context.Background(), manager.now()); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	if len(messages) != 2 || messages[0].Causation == nil || messages[1].Causation == nil ||
		messages[0].Causation.ScheduleRevision != schedule.Revision || messages[1].Causation.ScheduleRevision != updated.Revision ||
		messages[0].Causation.ScheduledFor != at.Format(time.RFC3339Nano) || messages[1].Causation.ScheduledFor != newAt.Format(time.RFC3339Nano) || messages[0].ID == messages[1].ID {
		t.Fatalf("trigger edit occurrences = %#v", messages)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.Revision != updated.Revision || runtime.EffectiveState != app.ScheduleStateCompleted ||
		runtime.LastOccurrenceAt != newAt.Format(time.RFC3339Nano) || runtime.LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("trigger edit checkpoint = %#v, %v", runtime, err)
	}
	beforeReplay := schedulerAgentHubInputCount(fake)
	if _, err := native.Reconcile(context.Background(), manager.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(got) != 2 {
		t.Fatalf("trigger edit replay duplicated durable work: %#v", got)
	}
	if afterReplay := schedulerAgentHubInputCount(fake); afterReplay != beforeReplay {
		t.Fatalf("trigger edit replay changed external input count: before=%d after=%d", beforeReplay, afterReplay)
	}
}

func TestNativeSchedulerArchiveWinsBeforePreparedMailboxAcceptance(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	const targetID = "project1.task1"
	target, err := puaWorkspace.ResourceValue(targetID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Do not lose this occurrence", Condition: "once", Target: targetID,
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	prepared, err := native.prepareOccurrence(schedule, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	runtime := schedulerScheduleRuntime{
		Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger),
		Target: schedule.Target, EffectiveState: app.ScheduleStateActive,
		NextRunAt: at.Format(time.RFC3339Nano), Prepared: &prepared,
	}
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}

	// Hold the target controller, queue archive first, then queue delivery. This
	// forces archive to complete after deliverPrepared's initial availability
	// check but before its mailbox acceptance on the unfixed implementation.
	controller, err := manager.controllerForResource(workspace, targetID)
	if err != nil {
		t.Fatal(err)
	}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	if err := manager.enqueueResourceController(workspace, targetID, func() error {
		close(blockerStarted)
		<-releaseBlocker
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-blockerStarted
	waitForQueuedJobs := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			controller.mu.Lock()
			queued := len(controller.jobs)
			controller.mu.Unlock()
			if queued >= want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("target controller queued jobs did not reach %d", want)
	}

	archiveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/archive", strings.NewReader(`{"resourceId":"`+targetID+`"}`))
		manager.server.archiveResource(recorder, request, workspace.ID)
		archiveDone <- recorder
	}()
	waitForQueuedJobs(1)
	deliveryDone := make(chan error, 1)
	go func() {
		deliveryDone <- native.deliverPrepared(context.Background(), schedule, runtime, at.Add(time.Second))
	}()
	waitForQueuedJobs(2)
	close(releaseBlocker)

	recorder := <-archiveDone
	if recorder.Code != http.StatusOK {
		t.Fatalf("archive prepared target = %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := <-deliveryDone; err != nil {
		t.Fatal(err)
	}
	persisted, err := native.schedulerRuntime(schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.EffectiveState != schedulerOutcomeAttention || persisted.LastOutcome != schedulerOutcomeAttention ||
		persisted.AttentionTarget != targetID || persisted.NextRunAt != "" || persisted.RetryAt != "" {
		t.Fatalf("archived target runtime = %#v", persisted)
	}
	assertPreparedOccurrenceEqual(t, persisted.Prepared, prepared)
	if messages := scheduleOccurrenceMessages(t, workspace.Path, targetID); len(messages) != 0 {
		t.Fatalf("archived target consumed prepared occurrence: %#v", messages)
	}
	if deadline, err := native.Reconcile(context.Background(), at.Add(2*time.Minute)); err != nil || !deadline.IsZero() {
		t.Fatalf("attention reconcile deadline = %s, %v", deadline, err)
	}

	var archived struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &archived); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(workspace.Path, filepath.FromSlash(archived.Path)), filepath.Join(workspace.Path, filepath.FromSlash(target.Path))); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Reconcile(context.Background(), at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertDeliveredPreparedOccurrence(t, scheduleOccurrenceMessages(t, workspace.Path, targetID), prepared)
	persisted, err = native.schedulerRuntime(schedule.ID)
	if err != nil || persisted.Prepared != nil || persisted.EffectiveState != app.ScheduleStateCompleted || persisted.LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("restored target runtime = %#v, %v", persisted, err)
	}
}

func TestNativeSchedulerProjectArchiveSerializesChildAcceptance(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	const (
		projectID = "project1"
		targetID  = "project1.task1"
	)
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Preserve child occurrence", Condition: "once", Target: targetID,
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	prepared, err := native.prepareOccurrence(schedule, at, at, time.Time{}, 1, false, time.Time{}, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	runtime := schedulerScheduleRuntime{
		Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger),
		Target: schedule.Target, EffectiveState: app.ScheduleStateActive,
		NextRunAt: at.Format(time.RFC3339Nano), Prepared: &prepared,
	}
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}

	projectController, err := manager.controllerForResource(workspace, projectID)
	if err != nil {
		t.Fatal(err)
	}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	if err := manager.enqueueResourceController(workspace, projectID, func() error {
		close(blockerStarted)
		<-releaseBlocker
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-blockerStarted
	waitForProjectQueue := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			projectController.mu.Lock()
			queued := len(projectController.jobs)
			projectController.mu.Unlock()
			if queued >= want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("Project controller queued jobs did not reach %d", want)
	}

	archiveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/archive", strings.NewReader(`{"resourceId":"`+projectID+`"}`))
		manager.server.archiveResource(recorder, request, workspace.ID)
		archiveDone <- recorder
	}()
	waitForProjectQueue(1)
	deliveryDone := make(chan error, 1)
	go func() {
		deliveryDone <- native.deliverPrepared(context.Background(), schedule, runtime, at.Add(time.Second))
	}()
	// Child delivery must also queue on its Project address. That makes a
	// Project subtree move and child mailbox acceptance one ordered operation.
	waitForProjectQueue(2)
	close(releaseBlocker)

	if recorder := <-archiveDone; recorder.Code != http.StatusOK {
		t.Fatalf("archive prepared target Project = %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := <-deliveryDone; err != nil {
		t.Fatal(err)
	}
	persisted, err := native.schedulerRuntime(schedule.ID)
	if err != nil || persisted.EffectiveState != schedulerOutcomeAttention || persisted.LastOutcome != schedulerOutcomeAttention || persisted.AttentionTarget != targetID {
		t.Fatalf("Project-archived target runtime = %#v, %v", persisted, err)
	}
	assertPreparedOccurrenceEqual(t, persisted.Prepared, prepared)
	if messages := scheduleOccurrenceMessages(t, workspace.Path, targetID); len(messages) != 0 {
		t.Fatalf("Project archive consumed child prepared occurrence: %#v", messages)
	}
}

func TestNativeSchedulerTriggerEditReplacesAttentionRuntime(t *testing.T) {
	tests := []struct {
		name         string
		oldTrigger   func(time.Time) *app.ScheduleTrigger
		overdueAt    func(app.Schedule) time.Time
		newTrigger   func(time.Time, app.Schedule) app.ScheduleTrigger
		changeNative bool
		restart      bool
	}{
		{
			name: "interval through native change",
			oldTrigger: func(base time.Time) *app.ScheduleTrigger {
				return &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: base.Add(-time.Hour).Format(time.RFC3339Nano)}
			},
			overdueAt: func(schedule app.Schedule) time.Time {
				return generationTime(schedule.UpdatedAt).Add(10 * time.Minute)
			},
			newTrigger: func(base time.Time, _ app.Schedule) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 300, AnchorAt: base.Add(-time.Hour).Format(time.RFC3339Nano)}
			},
			changeNative: true,
		},
		{
			name: "cron after restart audit",
			oldTrigger: func(time.Time) *app.ScheduleTrigger {
				return &app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"}
			},
			overdueAt: func(schedule app.Schedule) time.Time {
				return generationTime(schedule.UpdatedAt).Add(10 * time.Minute)
			},
			newTrigger: func(time.Time, app.Schedule) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 0 * * * *", TimeZone: "UTC"}
			},
			restart: true,
		},
		{
			name: "one-time through portable revision",
			oldTrigger: func(base time.Time) *app.ScheduleTrigger {
				return &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: base.Add(time.Minute).Format(time.RFC3339Nano)}
			},
			overdueAt: func(schedule app.Schedule) time.Time {
				return generationTime(schedule.Trigger.At).Add(time.Second)
			},
			newTrigger: func(_ time.Time, schedule app.Schedule) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: generationTime(schedule.Trigger.At).Add(2 * time.Hour).Format(time.RFC3339Nano)}
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
			const targetID = "project1.task1"
			target, err := puaWorkspace.ResourceValue(targetID)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Truncate(time.Second)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Replace frozen trigger", Condition: "old rule", Target: targetID,
				Trigger: test.oldTrigger(base),
			})
			if err != nil {
				t.Fatal(err)
			}
			archived, err := puaWorkspace.ArchiveResource(targetID)
			if err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(workspace.Path, filepath.FromSlash(target.Path))
			archivedPath := filepath.Join(workspace.Path, filepath.FromSlash(archived.Path))
			native := newNativeScheduler(manager, workspace)
			overdue := test.overdueAt(created)
			if _, err := native.Reconcile(context.Background(), overdue); err != nil {
				t.Fatal(err)
			}
			oldRuntime, err := native.schedulerRuntime(created.ID)
			if err != nil || oldRuntime.EffectiveState != schedulerOutcomeAttention || oldRuntime.Prepared == nil {
				t.Fatalf("overdue trigger attention runtime = %#v, %v", oldRuntime, err)
			}
			oldPrepared := *oldRuntime.Prepared

			trigger := test.newTrigger(base, created)
			var updated app.Schedule
			if test.changeNative {
				updated, err = native.Change(context.Background(), NativeSchedulerChange{
					Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Trigger: &trigger,
				})
			} else {
				// Model an audit discovering a portable definition revision that
				// was written while this Server was not processing mutations.
				updated, err = puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
					ID: created.ID, ExpectedRevision: created.Revision, Trigger: &trigger,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(archivedPath, targetPath); err != nil {
				t.Fatal(err)
			}
			mutationAt := generationTime(updated.UpdatedAt)
			if test.restart {
				manager = newAgentManager(manager.server)
				manager.server.agents = manager
				native = newNativeScheduler(manager, workspace)
			}
			if _, err := native.Reconcile(context.Background(), mutationAt); err != nil {
				t.Fatal(err)
			}

			runtime, err := native.schedulerRuntime(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			next := generationTime(runtime.NextRunAt)
			if runtime.Revision != updated.Revision || runtime.TriggerDigest != mustSchedulerTriggerDigest(t, updated.Trigger) || runtime.EffectiveState != app.ScheduleStateActive || runtime.Prepared != nil || runtime.AttentionTarget != "" || runtime.LastOccurrenceAt != "" || !next.After(mutationAt) {
				t.Fatalf("replacement trigger runtime = %#v; mutation = %s", runtime, mutationAt)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, targetID); len(messages) != 0 {
				t.Fatalf("trigger edit accepted old occurrence %s: %#v", oldPrepared.MessageID, messages)
			}
		})
	}
}

func newSchedulerOwnershipHandoffFixture(t *testing.T) (*server, *server, serveWorkspace, *app.Workspace) {
	t.Helper()
	root := t.TempDir()
	puaWorkspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	firstConfig := filepath.Join(t.TempDir(), "first-serve.json")
	first := &server{config: firstConfig, locks: newWorkspaceLockManager("127.0.0.1:4936", firstConfig)}
	first.agents = newAgentManager(first)
	workspace, err := first.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	secondConfig := filepath.Join(t.TempDir(), "second-serve.json")
	second := &server{config: secondConfig, locks: newWorkspaceLockManager("127.0.0.1:4999", secondConfig)}
	second.agents = newAgentManager(second)
	t.Cleanup(func() {
		first.agents.waitBackground()
		second.agents.waitBackground()
		first.locks.closeAll()
		second.locks.closeAll()
	})
	return first, second, workspace, puaWorkspace
}

func holdResourceController(t *testing.T, manager *agentManager, workspace serveWorkspace, resourceID string) (*resourceController, func()) {
	t.Helper()
	controller, err := manager.controllerForResource(workspace, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	if err := manager.enqueueResourceController(workspace, resourceID, func() error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("%s controller blocker did not start", resourceID)
	}
	closeBlocker := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeBlocker)
	return controller, closeBlocker
}

func holdSchedulerController(t *testing.T, manager *agentManager, workspace serveWorkspace) (*resourceController, func()) {
	t.Helper()
	return holdResourceController(t, manager, workspace, app.SchedulerResourceID)
}

func waitForResourceControllerQueue(t *testing.T, controller *resourceController, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		queued := len(controller.jobs)
		controller.mu.Unlock()
		if queued >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource controller queued jobs = %d, want at least %d", queued, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForSchedulerControllerQueue(t *testing.T, controller *resourceController, want int) {
	t.Helper()
	waitForResourceControllerQueue(t, controller, want)
}

func TestSchedulerControllerJobCancellationBoundaries(t *testing.T) {
	t.Run("cancelled before start", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		_ = manager.takeReconcileRequests()
		select {
		case <-manager.reconcileWake:
		default:
		}
		controller, release := holdSchedulerController(t, manager, workspace)
		ctx, cancel := context.WithCancel(context.Background())
		var started atomic.Bool
		done := make(chan error, 1)
		go func() {
			_, err := runSchedulerControllerJob(ctx, manager.server, workspace, func() schedulerControllerJobOutcome[string] {
				started.Store(true)
				return schedulerControllerJobOutcome[string]{Value: "persisted", Material: true}
			}, func(string) {
				manager.requestReconcile(reconcileScheduler)
			})
			done <- err
		}()
		waitForSchedulerControllerQueue(t, controller, 1)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled controller caller = %v", err)
		}
		release()
		if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if started.Load() {
			t.Fatal("cancelled queued Scheduler mutation started")
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
			t.Fatalf("cancelled queued mutation requested Scheduler reconciliation: %08b", request)
		}
		select {
		case <-manager.reconcileWake:
			t.Fatal("cancelled queued mutation woke the reconcile loop")
		default:
		}
	})

	t.Run("cancelled after start", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		_ = manager.takeReconcileRequests()
		select {
		case <-manager.reconcileWake:
		default:
		}
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseMutation := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseMutation()
		var followups atomic.Int32
		done := make(chan error, 1)
		go func() {
			_, err := runSchedulerControllerJob(ctx, manager.server, workspace, func() schedulerControllerJobOutcome[app.ResourceAgentDefaults] {
				close(started)
				<-release
				puaWorkspace, err := app.OpenWorkspace(workspace.Path)
				if err != nil {
					return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{Err: err}
				}
				updated, err := puaWorkspace.SetResourceAgentDefaults(app.ResourceAgentDefaults{
					Project: app.AgentBinding{Kind: "agent", Name: "replacement"},
					Task:    app.AgentBinding{Kind: "profile", Name: "default"},
				})
				return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{Value: updated, Material: err == nil, Err: err}
			}, func(app.ResourceAgentDefaults) {
				followups.Add(1)
				manager.requestReconcile(reconcileScheduler)
			})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("Scheduler defaults mutation did not start")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled running controller caller = %v", err)
		}
		releaseMutation()
		if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if got := followups.Load(); got != 1 {
			t.Fatalf("durable mutation follow-ups = %d, want 1", got)
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		runtimeConfig, err := puaWorkspace.RuntimeConfig()
		if err != nil || runtimeConfig.ResourceDefaults.Project != (app.AgentBinding{Kind: "agent", Name: "replacement"}) {
			t.Fatalf("cancelled caller durable defaults = %#v, %v", runtimeConfig.ResourceDefaults, err)
		}
		if request := manager.takeReconcileRequests(); request&reconcileScheduler == 0 {
			t.Fatalf("completed running mutation reconcile request = %08b, want Scheduler", request)
		}
		select {
		case <-manager.reconcileWake:
		default:
			t.Fatal("completed running mutation did not wake the reconcile loop")
		}
	})

	t.Run("natural acceptance after caller cancellation", func(t *testing.T) {
		manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseAcceptance := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseAcceptance()
		done := make(chan error, 1)
		go func() {
			_, err := runSchedulerControllerJob(ctx, manager.server, workspace, func() schedulerControllerJobOutcome[resourceMailboxMessage] {
				close(started)
				<-release
				message, acceptErr := manager.acceptResourceMessageDurable(context.Background(), workspace, app.SchedulerResourceID, resourceMessageRequest{
					Text: "compile after cancellation", Mode: resourceMessageModeEnqueue, Role: "user",
				})
				return schedulerControllerJobOutcome[resourceMailboxMessage]{Value: message, Material: acceptErr == nil, Err: acceptErr}
			}, func(message resourceMailboxMessage) {
				manager.server.enqueueSchedulerMailboxReconcile(workspace, message)
			})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("Scheduler natural-language acceptance did not start")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled natural-language caller = %v", err)
		}
		releaseAcceptance()
		if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		mailbox, err := loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
		if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].Text != "compile after cancellation" {
			t.Fatalf("cancelled caller Scheduler acceptances = %#v, %v", mailbox.Messages, err)
		}
	})

	for _, test := range []struct {
		name     string
		outcome  schedulerControllerJobOutcome[string]
		loseLock bool
		wantErr  bool
	}{
		{name: "normalized no-op", outcome: schedulerControllerJobOutcome[string]{Value: "unchanged"}},
		{name: "validation or write failure", outcome: schedulerControllerJobOutcome[string]{Material: true, Err: errors.New("write failed")}, wantErr: true},
		{name: "ownership lost", outcome: schedulerControllerJobOutcome[string]{Value: "persisted", Material: true}, loseLock: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
			if test.loseLock {
				manager.server.locks = newWorkspaceLockManager("127.0.0.1:4936", manager.server.config)
				t.Cleanup(manager.server.locks.closeAll)
			}
			_ = manager.takeReconcileRequests()
			select {
			case <-manager.reconcileWake:
			default:
			}
			var followups atomic.Int32
			_, err := runSchedulerControllerJob(context.Background(), manager.server, workspace, func() schedulerControllerJobOutcome[string] {
				return test.outcome
			}, func(string) {
				followups.Add(1)
				manager.requestReconcile(reconcileScheduler)
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("controller result error = %v, wantErr %v", err, test.wantErr)
			}
			if got := followups.Load(); got != 0 {
				t.Fatalf("rejected mutation follow-ups = %d, want 0", got)
			}
			if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
				t.Fatalf("rejected mutation reconcile request = %08b", request)
			}
			select {
			case <-manager.reconcileWake:
				t.Fatal("rejected mutation woke the reconcile loop")
			default:
			}
		})
	}
}

func TestSchedulerIdempotentLifecycleChangeDoesNotWakeReconcile(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Pause once", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{
			Type: app.ScheduleTriggerInterval, EverySeconds: 60,
			AnchorAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost,
			"/api/workspaces/"+workspace.ID+"/scheduler/"+created.ID+"/pause", nil))
		return recorder
	}
	if recorder := request(); recorder.Code != http.StatusOK {
		t.Fatalf("initial pause = %d %s", recorder.Code, recorder.Body.String())
	}
	_ = manager.takeReconcileRequests()
	select {
	case <-manager.reconcileWake:
	default:
	}
	if recorder := request(); recorder.Code != http.StatusOK {
		t.Fatalf("idempotent pause = %d %s", recorder.Code, recorder.Body.String())
	}
	if request := manager.takeReconcileRequests(); request&reconcileScheduler != 0 {
		t.Fatalf("idempotent pause reconcile request = %08b", request)
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("idempotent pause woke the reconcile loop")
	default:
	}
}

func addWorkspaceAfterSchedulerHandoff(t *testing.T, server *server, workspace serveWorkspace) serveWorkspace {
	t.Helper()
	added, err := server.addWorkspace(context.Background(), workspace.Path)
	if err != nil {
		t.Fatalf("new owner did not acquire Workspace: %v", err)
	}
	return added
}

func TestNativeSchedulerChangeCannotCrossWorkspaceOwnershipHandoff(t *testing.T) {
	first, second, workspace, puaWorkspace := newSchedulerOwnershipHandoffFixture(t)
	controller, release := holdSchedulerController(t, first.agents, workspace)

	removeDone := make(chan error, 1)
	go func() { removeDone <- first.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)

	description := "stale owner change"
	condition := "once in the future"
	target := "workspace"
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
	staleDone := make(chan error, 1)
	go func() {
		staleDone <- first.agents.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error {
			_, err := newNativeScheduler(first.agents, workspace).Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeCreate, Description: &description, Condition: &condition,
				Target: &target, Trigger: &trigger,
			})
			return err
		})
	}()
	waitForSchedulerControllerQueue(t, controller, 2)
	release()
	if err := <-removeDone; err != nil {
		t.Fatalf("remove Workspace: %v", err)
	}
	if err := <-staleDone; err == nil || !strings.Contains(err.Error(), "not owned by this pua serve instance") {
		t.Fatalf("stale Scheduler Change error = %v", err)
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != 0 {
		t.Fatalf("stale owner wrote schedules across handoff: %#v", config.Schedules)
	}

	workspace = addWorkspaceAfterSchedulerHandoff(t, second, workspace)
	newDescription := "new owner change"
	var created app.Schedule
	err = second.agents.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error {
		var changeErr error
		created, changeErr = newNativeScheduler(second.agents, workspace).Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeCreate, Description: &newDescription, Condition: &condition,
			Target: &target, Trigger: &trigger,
		})
		return changeErr
	})
	if err != nil || created.Description != newDescription {
		t.Fatalf("new owner Scheduler Change = %#v, %v", created, err)
	}
}

func TestNativeSchedulerReconcileCannotCrossWorkspaceOwnershipHandoff(t *testing.T) {
	first, second, workspace, puaWorkspace := newSchedulerOwnershipHandoffFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "future owner reconcile", Condition: "once in the future", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: now.Add(2 * time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller, release := holdSchedulerController(t, first.agents, workspace)

	removeDone := make(chan error, 1)
	go func() { removeDone <- first.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)
	staleDone := make(chan error, 1)
	go func() {
		staleDone <- first.agents.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error {
			_, err := newNativeScheduler(first.agents, workspace).Reconcile(context.Background(), now)
			return err
		})
	}()
	waitForSchedulerControllerQueue(t, controller, 2)
	release()
	if err := <-removeDone; err != nil {
		t.Fatalf("remove Workspace: %v", err)
	}
	if err := <-staleDone; err == nil || !strings.Contains(err.Error(), "not owned by this pua serve instance") {
		t.Fatalf("stale Scheduler Reconcile error = %v", err)
	}
	store, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := store.Scheduler.Schedules[schedule.ID]; found {
		t.Fatalf("stale owner wrote Scheduler runtime across handoff: %#v", store.Scheduler.Schedules[schedule.ID])
	}

	workspace = addWorkspaceAfterSchedulerHandoff(t, second, workspace)
	if err := second.agents.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatalf("new owner Scheduler Reconcile: %v", err)
	}
	runtime, err := newNativeScheduler(second.agents, workspace).schedulerRuntime(schedule.ID)
	if err != nil || runtime.Revision != schedule.Revision || runtime.NextRunAt == "" {
		t.Fatalf("new owner Scheduler runtime = %#v, %v", runtime, err)
	}
}

func TestCancelledSchedulerWorkDrainsBeforeCleanOwnershipRelease(t *testing.T) {
	first, second, workspace, puaWorkspace := newSchedulerOwnershipHandoffFixture(t)
	controller, release := holdSchedulerController(t, first.agents, workspace)
	ctx, cancel := context.WithCancel(context.Background())
	description := "cancelled stale change"
	condition := "once in the future"
	target := "workspace"
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
	done := make(chan error, 1)
	go func() {
		done <- first.agents.withResourceController(ctx, workspace, app.SchedulerResourceID, func() error {
			_, err := newNativeScheduler(first.agents, workspace).Change(ctx, NativeSchedulerChange{
				Operation: app.ScheduleChangeCreate, Description: &description, Condition: &condition,
				Target: &target, Trigger: &trigger,
			})
			return err
		})
	}()
	waitForSchedulerControllerQueue(t, controller, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Scheduler Change = %v", err)
	}
	release()
	shutdownDone := make(chan struct{})
	go func() {
		first.agents.waitBackground()
		first.locks.closeAll()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("clean shutdown did not drain the Scheduler controller")
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != 0 {
		t.Fatalf("cancelled Scheduler work wrote schedules: %#v", config.Schedules)
	}

	workspace = addWorkspaceAfterSchedulerHandoff(t, second, workspace)
	if err := second.agents.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error {
		_, err := newNativeScheduler(second.agents, workspace).Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeCreate, Description: &description, Condition: &condition,
			Target: &target, Trigger: &trigger,
		})
		return err
	}); err != nil {
		t.Fatalf("new owner did not proceed after clean shutdown: %v", err)
	}
}
