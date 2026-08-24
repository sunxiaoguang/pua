package pua

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/buildinfo"
	"github.com/disksing/pua/internal/schedulerapi"
)

const (
	testConfigFile       = "workspace.json"
	testReposDir         = "repos"
	testArchiveDir       = "archive"
	testWikiDir          = "wiki"
	testProjectJSONFile  = "project.json"
	testProjectMDFile    = "project.md"
	testTaskMDFile       = "task.md"
	testDefaultLanguage  = "en"
	testChineseLanguage  = "zh-CN"
	testDefaultWikiIndex = "# Workspace Wiki\n\n此索引是 workspace 长期知识的入口。随着 Wiki 内容增长，请在这里添加主题页面链接及简短摘要。\n"
	defaultWikiIndex     = "# Workspace Wiki\n\nThis index is the entry point for long-lived workspace knowledge. Add links to topic pages with short summaries as the Wiki grows.\n"
	puaPromptStart       = "<!-- managed by pua cli -->"
	puaPromptEnd         = "<!-- end of pua cli prompt -->"
)

func TestVersion(t *testing.T) {
	oldBranch := buildinfo.Branch
	oldSHA := buildinfo.SHA
	buildinfo.Branch = "task-branch"
	buildinfo.SHA = "abc123"
	t.Cleanup(func() {
		buildinfo.Branch = oldBranch
		buildinfo.SHA = oldSHA
	})

	out := run(t, "--version")
	if out != "pua branch=task-branch sha=abc123\n" {
		t.Fatalf("unexpected version output: %q", out)
	}
}

func TestSchedulerCommandsUseOwningServerForNativeSchedules(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		workspace, err := app.OpenWorkspace(root)
		if err != nil {
			t.Fatal(err)
		}
		changeRequests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/scheduler"):
				config, readErr := workspace.Scheduler()
				if readErr != nil {
					t.Error(readErr)
					http.Error(w, readErr.Error(), http.StatusInternalServerError)
					return
				}
				snapshot := app.SchedulerSnapshot{SchemaVersion: config.SchemaVersion, AgentBinding: config.AgentBinding, Schedules: make([]app.ScheduleSnapshot, 0, len(config.Schedules))}
				for _, schedule := range config.Schedules {
					snapshot.Schedules = append(snapshot.Schedules, app.ScheduleSnapshot{Schedule: schedule, EffectiveState: schedule.State})
				}
				_ = json.NewEncoder(w).Encode(schedulerapi.FromSnapshot(snapshot))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scheduler/changes"):
				changeRequests++
				var body schedulerChangePayload
				if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
					t.Error(decodeErr)
					http.Error(w, decodeErr.Error(), http.StatusBadRequest)
					return
				}
				var schedule app.Schedule
				var changeErr error
				expectedRevision := uint64(0)
				if body.ExpectedRevision != "" {
					expectedRevision, changeErr = body.ExpectedRevision.Uint64()
				}
				switch body.Operation {
				case app.ScheduleChangeCreate:
					schedule, changeErr = workspace.AddSchedule(app.CreateScheduleInput{Description: *body.Description, Condition: *body.Condition, Target: *body.Target, Trigger: body.Trigger})
				case app.ScheduleChangeUpdate:
					if changeErr == nil {
						schedule, changeErr = workspace.UpdateSchedule(app.UpdateScheduleInput{ID: body.ID, ExpectedRevision: expectedRevision, Description: body.Description, Condition: body.Condition, Guard: body.Guard, Target: body.Target, Trigger: body.Trigger})
					}
				case app.ScheduleChangePause:
					schedule, changeErr = workspace.PauseSchedule(body.ID)
				case app.ScheduleChangeResume:
					schedule, changeErr = workspace.ResumeSchedule(body.ID)
				case app.ScheduleChangeRemove:
					schedule, changeErr = workspace.RemoveSchedule(body.ID)
				}
				if changeErr != nil {
					http.Error(w, changeErr.Error(), http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(schedulerapi.FromSchedule(schedule))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		lock, err := json.Marshal(map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), lock, 0o600); err != nil {
			t.Fatal(err)
		}

		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		createdOutput := run(t, "scheduler", "add", "--description", "Review release", "--condition", "at the agreed review time", "--target", "workspace", "--at", at)
		var created schedulerapi.Schedule
		if err := json.Unmarshal([]byte(createdOutput), &created); err != nil {
			t.Fatal(err)
		}
		if created.ID == "" || created.Revision != "1" || created.Trigger == nil || created.Target != "workspace" {
			t.Fatalf("created schedule = %#v", created)
		}
		listed := run(t, "scheduler", "list")
		if !strings.Contains(listed, created.ID+"\t1\tactive\tat ") {
			t.Fatalf("schedule list = %q", listed)
		}
		for name, test := range map[string]struct {
			args []string
			want string
		}{
			"missing trigger": {
				args: []string{"scheduler", "update", "--id=" + created.ID, "--revision=1", "--condition=at the agreed time when the release branch is green"},
				want: schedulerUpdateUsage,
			},
			"incomplete trigger": {
				args: []string{"scheduler", "update", "--id=" + created.ID, "--revision=1", "--every=5m"},
				want: "--every and --anchor are required together",
			},
			"mixed triggers": {
				args: []string{"scheduler", "update", "--id=" + created.ID, "--revision=1", "--at=" + at, "--cron=0 0 9 * * *", "--timezone=UTC"},
				want: "exactly one trigger form is required",
			},
			"Scheduler self-target": {
				args: []string{"scheduler", "update", "--id=" + created.ID, "--revision=1", "--target=scheduler", "--at=" + at},
				want: app.ErrScheduleTargetScheduler.Error(),
			},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := runErr(t, test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("scheduler update error = %v, want %q", err, test.want)
				}
				if changeRequests != 1 {
					t.Fatalf("scheduler change requests = %d, want create only", changeRequests)
				}
			})
		}

		updatedAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
		updatedOutput := run(t, "scheduler", "update", "--id="+created.ID, "--revision=1", "--condition=at the agreed time when the release branch is green", "--target=workspace", "--at="+updatedAt)
		var updated schedulerapi.Schedule
		if err := json.Unmarshal([]byte(updatedOutput), &updated); err != nil {
			t.Fatal(err)
		}
		if updated.Revision != "2" || updated.Condition != "at the agreed time when the release branch is green" || updated.Target != "workspace" || updated.CreatedAt != created.CreatedAt || updated.Trigger == nil || updated.Trigger.At != updatedAt {
			t.Fatalf("updated schedule = %#v", updated)
		}
		triggerOnlyAt := time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339Nano)
		triggerOnlyOutput := run(t, "scheduler", "update", "--id="+created.ID, "--revision=2", "--at="+triggerOnlyAt)
		var triggerOnly schedulerapi.Schedule
		if err := json.Unmarshal([]byte(triggerOnlyOutput), &triggerOnly); err != nil {
			t.Fatal(err)
		}
		if triggerOnly.Revision != "3" || triggerOnly.Description != updated.Description || triggerOnly.Condition != updated.Condition || triggerOnly.Target != updated.Target || triggerOnly.Trigger == nil || triggerOnly.Trigger.At != triggerOnlyAt {
			t.Fatalf("trigger-only update changed unrelated fields = %#v", triggerOnly)
		}
		shown := run(t, "scheduler", "show", "--id", created.ID)
		if !strings.Contains(shown, `"target": "workspace"`) {
			t.Fatalf("schedule show = %s", shown)
		}
		jsonList := run(t, "scheduler", "list", "--json")
		if strings.Contains(jsonList, "wakeIntervalMinutes") || !strings.Contains(jsonList, created.ID) {
			t.Fatalf("JSON schedule list = %s", jsonList)
		}
		pausedOutput := run(t, "scheduler", "pause", "--id="+created.ID)
		var paused schedulerapi.Schedule
		if err := json.Unmarshal([]byte(pausedOutput), &paused); err != nil || paused.State != app.ScheduleStatePaused || paused.Revision != "4" {
			t.Fatalf("paused schedule = %#v, %v", paused, err)
		}
		resumedOutput := run(t, "scheduler", "resume", "--id="+created.ID)
		var resumed schedulerapi.Schedule
		if err := json.Unmarshal([]byte(resumedOutput), &resumed); err != nil || resumed.State != app.ScheduleStateActive || resumed.Revision != "5" {
			t.Fatalf("resumed schedule = %#v, %v", resumed, err)
		}
		removed := run(t, "scheduler", "remove", "--id="+created.ID)
		if !strings.Contains(removed, created.ID) || strings.TrimSpace(run(t, "scheduler", "list")) != "" {
			t.Fatalf("remove result = %s", removed)
		}
		if _, err := os.Stat(filepath.Join(root, "scheduler", "scheduler.json")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSchedulerCLIRevisionTransportPreservesUint64(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		const (
			firstUnsafeRevision = "9007199254740992"
			maximumRevision     = "18446744073709551615"
		)
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		trigger := &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at}
		schedules := []schedulerapi.ScheduleSnapshot{
			{Schedule: schedulerapi.Schedule{ID: "schedule-111111111111111111111111", Revision: firstUnsafeRevision, Description: "Unsafe", Condition: "later", Target: "workspace", State: app.ScheduleStateActive, Trigger: trigger}, EffectiveState: app.ScheduleStateActive},
			{Schedule: schedulerapi.Schedule{ID: "schedule-222222222222222222222222", Revision: maximumRevision, Description: "Maximum", Condition: "later", Target: "workspace", State: app.ScheduleStateActive, Trigger: trigger}, EffectiveState: app.ScheduleStateActive},
		}
		var received []schedulerapi.Revision
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/scheduler"):
				_ = json.NewEncoder(w).Encode(schedulerapi.Snapshot{SchemaVersion: 2, AgentBinding: app.AgentBinding{Kind: "profile", Name: "default"}, Schedules: schedules})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scheduler/changes"):
				var body schedulerChangePayload
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				received = append(received, body.ExpectedRevision)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				code, message := "schedule_revision_conflict", "revision conflict"
				if body.ExpectedRevision == maximumRevision {
					code, message = "schedule_revision_exhausted", app.ErrScheduleRevisionExhausted.Error()
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": message})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		lock, err := json.Marshal(map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), lock, 0o600); err != nil {
			t.Fatal(err)
		}

		listed := run(t, "scheduler", "list")
		if !strings.Contains(listed, schedules[0].ID+"\t"+firstUnsafeRevision+"\t") || !strings.Contains(listed, schedules[1].ID+"\t"+maximumRevision+"\t") {
			t.Fatalf("lossy Scheduler list:\n%s", listed)
		}
		jsonList := run(t, "scheduler", "list", "--json")
		if !strings.Contains(jsonList, `"revision": "`+firstUnsafeRevision+`"`) || !strings.Contains(jsonList, `"revision": "`+maximumRevision+`"`) {
			t.Fatalf("lossy JSON Scheduler list:\n%s", jsonList)
		}
		shown := run(t, "scheduler", "show", "--id="+schedules[1].ID)
		if !strings.Contains(shown, `"revision": "`+maximumRevision+`"`) {
			t.Fatalf("lossy Scheduler show:\n%s", shown)
		}
		for _, test := range []struct {
			revision, code string
		}{
			{revision: firstUnsafeRevision, code: "schedule_revision_conflict"},
			{revision: maximumRevision, code: "schedule_revision_exhausted"},
		} {
			_, err := runErr(t, "scheduler", "update", "--id="+schedules[0].ID, "--revision="+test.revision, "--at="+at)
			if err == nil || !strings.Contains(err.Error(), test.code) {
				t.Fatalf("revision %s error = %v, want %s", test.revision, err, test.code)
			}
		}
		if len(received) != 2 || received[0] != firstUnsafeRevision || received[1] != maximumRevision {
			t.Fatalf("Scheduler update revisions = %#v", received)
		}
	})
}

func TestSchedulerMutationCommandsSurfaceNotFoundCode(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		missingID := "schedule-ffffffffffffffffffffffff"
		var received []app.ScheduleChangeOperation
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/scheduler/changes") {
				http.NotFound(w, r)
				return
			}
			var body schedulerChangePayload
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body.ID != missingID {
				t.Errorf("schedule id = %q, want %q", body.ID, missingID)
			}
			received = append(received, body.Operation)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "schedule_not_found", "error": "schedule not found: " + missingID,
			})
		}))
		defer server.Close()
		lock, err := json.Marshal(map[string]any{
			"pid": os.Getpid(), "address": server.URL, "workspacePath": root,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), lock, 0o600); err != nil {
			t.Fatal(err)
		}

		operations := []struct {
			name string
			want app.ScheduleChangeOperation
			args []string
		}{
			{name: "update", want: app.ScheduleChangeUpdate, args: []string{"scheduler", "update", "--id=" + missingID, "--revision=18446744073709551615", "--at=9999-08-30T12:00:00Z"}},
			{name: "pause", want: app.ScheduleChangePause, args: []string{"scheduler", "pause", "--id=" + missingID}},
			{name: "resume", want: app.ScheduleChangeResume, args: []string{"scheduler", "resume", "--id=" + missingID}},
			{name: "remove", want: app.ScheduleChangeRemove, args: []string{"scheduler", "remove", "--id=" + missingID}},
		}
		for _, operation := range operations {
			t.Run(operation.name, func(t *testing.T) {
				_, err := runErr(t, operation.args...)
				want := "PUA Server schedule_not_found: schedule not found: " + missingID
				if err == nil || err.Error() != want {
					t.Fatalf("scheduler %s error = %v, want %q", operation.name, err, want)
				}
				if len(received) == 0 || received[len(received)-1] != operation.want {
					t.Fatalf("scheduler %s request operations = %#v", operation.name, received)
				}
			})
		}
	})
}

func TestSchedulerTriggerOptionsAreStructuredAndUnambiguous(t *testing.T) {
	interval, present, err := schedulerTriggerFromOptions(map[string]string{"every": "5m", "anchor": "2026-08-23T09:00:00+08:00"})
	if err != nil || !present || interval.Type != app.ScheduleTriggerInterval || interval.EverySeconds != 300 {
		t.Fatalf("interval trigger = %#v, present=%v, err=%v", interval, present, err)
	}
	cron, present, err := schedulerTriggerFromOptions(map[string]string{"cron": "0 0 9 * * *", "timezone": "Asia/Shanghai"})
	if err != nil || !present || cron.Type != app.ScheduleTriggerCron {
		t.Fatalf("cron trigger = %#v, present=%v, err=%v", cron, present, err)
	}
	for name, values := range map[string]map[string]string{
		"mixed forms":       {"at": "2026-08-23T09:00:00Z", "cron": "0 0 9 * * *", "timezone": "UTC"},
		"missing anchor":    {"every": "5m"},
		"sub-minute":        {"every": "59s", "anchor": "2026-08-23T09:00:00Z"},
		"implicit timezone": {"cron": "0 0 9 * * *", "timezone": "Local"},
	} {
		if _, _, err := schedulerTriggerFromOptions(values); err == nil {
			t.Fatalf("%s unexpectedly accepted", name)
		}
	}
	parsed, err := parseSchedulerOptions([]string{"--guard="}, map[string]bool{"guard": true})
	if err != nil || parsed["guard"] != "" {
		t.Fatalf("empty guard cannot clear optional predicate: %#v, %v", parsed, err)
	}
}

func TestSchedulerUpdateValidatesTriggerBeforeOwnerDiscovery(t *testing.T) {
	withTempCwd(t, func(_ string) {
		for name, test := range map[string]struct {
			args []string
			want string
		}{
			"missing trigger": {
				args: []string{"scheduler", "update", "--id=schedule-1", "--revision=1", "--condition=changed"},
				want: schedulerUpdateUsage,
			},
			"incomplete trigger": {
				args: []string{"scheduler", "update", "--id=schedule-1", "--revision=1", "--every=5m"},
				want: "--every and --anchor are required together",
			},
			"mixed triggers": {
				args: []string{"scheduler", "update", "--id=schedule-1", "--revision=1", "--at=2026-08-23T09:00:00Z", "--cron=0 0 9 * * *", "--timezone=UTC"},
				want: "exactly one trigger form is required",
			},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := runErr(t, test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("scheduler update error = %v, want %q", err, test.want)
				}
			})
		}
	})
}

func TestSchedulerValidatesCommandsBeforeOwnerDiscovery(t *testing.T) {
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "")
	withTempCwd(t, func(_ string) {
		validAt := "2030-08-23T09:00:00Z"
		for _, test := range []struct {
			name string
			args []string
			want string
		}{
			{name: "unknown subcommand", args: []string{"scheduler", "frobnicate"}, want: `unknown scheduler subcommand "frobnicate"`},
			{name: "unknown subcommand before server parsing", args: []string{"scheduler", "frobnicate", "--server"}, want: `unknown scheduler subcommand "frobnicate"`},
			{name: "duplicate list flag", args: []string{"scheduler", "list", "--json", "--json"}, want: "usage: pua scheduler list [--json] [--server=<url>]"},
			{name: "unknown list flag", args: []string{"scheduler", "list", "--yaml"}, want: "usage: pua scheduler list [--json] [--server=<url>]"},
			{name: "missing show id", args: []string{"scheduler", "show"}, want: schedulerShowUsage},
			{name: "unknown add option", args: []string{"scheduler", "add", "--description=Review", "--condition=At review time", "--target=workspace", "--at=" + validAt, "--yaml=true"}, want: schedulerAddUsage},
			{name: "Scheduler self-target add", args: []string{"scheduler", "add", "--description=Review", "--condition=At review time", "--target=scheduler", "--at=" + validAt}, want: app.ErrScheduleTargetScheduler.Error()},
			{name: "incomplete add trigger", args: []string{"scheduler", "add", "--description=Review", "--condition=At review time", "--target=workspace", "--every=5m"}, want: "--every and --anchor are required together"},
			{name: "missing update revision", args: []string{"scheduler", "update", "--id=schedule-1", "--at=" + validAt}, want: schedulerUpdateUsage},
			{name: "zero update revision", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=0", "--at=" + validAt}, want: schedulerUpdateUsage},
			{name: "leading-zero update revision", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=01", "--at=" + validAt}, want: schedulerUpdateUsage},
			{name: "overflowing update revision", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=18446744073709551616", "--at=" + validAt}, want: schedulerUpdateUsage},
			{name: "signed update revision", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=+1", "--at=" + validAt}, want: schedulerUpdateUsage},
			{name: "missing update trigger", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=1"}, want: schedulerUpdateUsage},
			{name: "Scheduler self-target update", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=1", "--target=scheduler", "--at=" + validAt}, want: app.ErrScheduleTargetScheduler.Error()},
			{name: "incomplete update trigger", args: []string{"scheduler", "update", "--id=schedule-1", "--revision=1", "--cron=0 0 9 * * *"}, want: "--cron and --timezone are required together"},
			{name: "missing pause id", args: []string{"scheduler", "pause"}, want: "usage: pua scheduler pause --id=<schedule> [--server=<url>]"},
			{name: "missing resume id", args: []string{"scheduler", "resume"}, want: "usage: pua scheduler resume --id=<schedule> [--server=<url>]"},
			{name: "missing remove id", args: []string{"scheduler", "remove"}, want: schedulerRemoveUsage},
			{name: "duplicate server", args: []string{"scheduler", "list", "--server=http://127.0.0.1:1", "--server", "http://127.0.0.1:2"}, want: "usage: pua scheduler list [--server=<url>]"},
			{name: "invalid server URL", args: []string{"scheduler", "list", "--server=ftp://127.0.0.1:1"}, want: `unsupported PUA Server URL scheme "ftp"`},
		} {
			t.Run(test.name, func(t *testing.T) {
				if _, err := runErr(t, test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("Run(%q) error = %v, want %q", test.args, err, test.want)
				}
			})
		}

		if _, err := runErr(t, "scheduler", "list"); err == nil || !strings.Contains(err.Error(), "could not find AgentWorkspace root; run pua init first") {
			t.Fatalf("valid scheduler list error = %v, want owner discovery failure", err)
		}
	})
}

func TestSchedulerHelpDescribesNativeCommandSurface(t *testing.T) {
	topLevelHelp := run(t, "help")
	for _, marker := range []string{
		"Manage native structured schedules through the owning pua serve process.",
		"Subcommands: list, show, add, update, pause, resume, remove.",
		"require --at, --every with --anchor, or --cron with --timezone",
		"UI or Scheduler Agent to compile natural-language requests",
	} {
		if !strings.Contains(topLevelHelp, marker) {
			t.Fatalf("top-level help is missing %q:\n%s", marker, topLevelHelp)
		}
	}

	schedulerHelp := run(t, "scheduler", "help")
	for _, command := range []string{"list", "show", "add", "update", "pause", "resume", "remove"} {
		if !strings.Contains(schedulerHelp, "pua scheduler "+command) {
			t.Fatalf("scheduler help is missing %q:\n%s", command, schedulerHelp)
		}
	}
	for _, marker := range []string{
		"pua scheduler update --id=<schedule> --revision=<n> [--description=<text>]",
		"complete replacement trigger",
		"CLI add and update accept complete structured triggers only.",
		"Scheduler Agent to compile natural-language requests",
	} {
		if !strings.Contains(schedulerHelp, marker) {
			t.Fatalf("scheduler help is missing %q:\n%s", marker, schedulerHelp)
		}
	}

	for name, help := range map[string]string{"top-level": topLevelHelp, "scheduler": schedulerHelp} {
		for _, obsolete := range []string{"Manage natural-language schedules", "wake interval", "wakeInterval"} {
			if strings.Contains(help, obsolete) {
				t.Fatalf("%s help contains obsolete wording %q:\n%s", name, obsolete, help)
			}
		}
	}
}

func TestUserListShowsWorkspaceProfiles(t *testing.T) {
	withTempCwd(t, func(_ string) {
		run(t, "init")
		workspace, err := openApplicationWorkspace()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.RegisterUser("alice_2"); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.UpdateUserPreference("alice_2", "Use concise replies"); err != nil {
			t.Fatal(err)
		}
		listed := run(t, "user", "list")
		if listed != "alice_2\tUse concise replies\n" {
			t.Fatalf("user list = %q", listed)
		}
		var result struct {
			Users []app.UserProfile `json:"users"`
		}
		if err := json.Unmarshal([]byte(run(t, "user", "list", "--json")), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Users) != 1 || result.Users[0].Name != "alice_2" {
			t.Fatalf("JSON users = %#v", result.Users)
		}
	})
}

func TestStatusAndMessageCommandsUseOwningServerAndProvenance(t *testing.T) {
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "")
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Mailbox project")
		if err := os.Chdir(filepath.Join(root, "project1")); err != nil {
			t.Fatal(err)
		}
		run(t, "task", "create", "Mailbox task")
		if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
			t.Fatal(err)
		}
		var requestBody map[string]any
		var taskStateBody map[string]any
		turnRef := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"k":"turn","w":"instance","r":"project1.task1","g":"gen-1","t":"turn-1"}`))
		eventRef := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"k":"event","w":"instance","r":"project1.task1","g":"gen-1","e":1}`))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/messages"):
				if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, `{"messageId":"msg-test","resourceId":"project1.task1","requestedMode":"interrupt","actualMode":"interrupt","status":"delivered","reference":"messages/msg-test"}`)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/resources/") && strings.HasSuffix(r.URL.Path, "/status"):
				_, _ = io.WriteString(w, `{"resourceId":"project1.task1","sessionState":"idle","exists":true,"acceptsMessages":true}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/task-state"):
				_, _ = io.WriteString(w, `{"resourceId":"project1.task1","state":"waiting","note":"CI is running"}`)
			case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/task-state"):
				if err := json.NewDecoder(r.Body).Decode(&taskStateBody); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, `{"resourceId":"project1.task1","state":"blocked","note":"Need approval"}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages/msg-test"):
				_, _ = io.WriteString(w, `{"messageId":"msg-test","status":"delivered"}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/workspace/history/turns"):
				_, _ = io.WriteString(w, `{"resourceId":"workspace","segments":[],"page":{"limit":20,"hasMore":false}}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/project1/history/turns"):
				_, _ = io.WriteString(w, `{"resourceId":"project1","segments":[],"page":{"limit":20,"hasMore":false}}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/history/turns"):
				if r.URL.Query().Get("cursor") != "cursor-test" || r.URL.Query().Get("limit") != "7" {
					t.Errorf("unexpected history query: %s", r.URL.RawQuery)
				}
				_, _ = io.WriteString(w, `{"resourceId":"project1.task1","segments":[{"generation":{"generation":1,"generationId":"gen-1","title":"Mailbox task (gen #1)","binding":{"kind":"profile","name":"default"},"agentName":"test-agent","status":"running","createdAt":"2026-08-13T00:00:00Z","updatedAt":"2026-08-13T00:00:01Z"},"turns":[{"reference":"`+turnRef+`","turnId":"turn-1","status":"completed","closed":true,"startedAt":"2026-08-13T00:00:00Z","endedAt":"2026-08-13T00:00:01Z","durationMs":1000,"triggerPreview":"coordinate now","triggerRole":"agent","finalReplyPreview":"done","eventCount":2,"toolEventCount":1,"startEventId":1,"lastEventId":2}]}],"page":{"limit":7,"hasMore":true,"nextCursor":"cursor-next"}}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/history/turns/"+turnRef):
				_, _ = io.WriteString(w, `{"turn":{"reference":"`+turnRef+`","turnId":"turn-1","status":"completed","closed":true,"startedAt":"2026-08-13T00:00:00Z","endedAt":"2026-08-13T00:00:01Z","durationMs":1000,"triggerPreview":"coordinate now","triggerRole":"agent","finalReplyPreview":"done","eventCount":2,"toolEventCount":1,"startEventId":1,"lastEventId":2,"generation":{"generation":1,"generationId":"gen-1","title":"Mailbox task (gen #1)","binding":{"kind":"profile","name":"default"},"agentName":"test-agent","status":"running","createdAt":"2026-08-13T00:00:00Z","updatedAt":"2026-08-13T00:00:01Z"}},"items":[{"type":"message","role":"agent","text":"coordinate now","startEventId":1,"endEventId":1,"startEventRef":"`+eventRef+`","endEventRef":"`+eventRef+`","startedAt":"2026-08-13T00:00:00Z","endedAt":"2026-08-13T00:00:00Z","durationMs":0,"count":1}],"deliveries":[],"latestEventId":2,"latestEventRef":"`+eventRef+`","turnStartedEventId":1,"completedAt":"2026-08-13T00:00:01Z"}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/history/events/"+eventRef):
				_, _ = io.WriteString(w, `{"reference":"`+eventRef+`","generation":{"generation":1,"generationId":"gen-1","title":"Mailbox task (gen #1)","binding":{"kind":"profile","name":"default"},"agentName":"test-agent","status":"running","createdAt":"2026-08-13T00:00:00Z","updatedAt":"2026-08-13T00:00:01Z"},"schema":"agenthub.event-detail.v1","sourceEvent":{"id":1,"time":"2026-08-13T00:00:00Z","type":"message.input","sessionId":"session-1","turnId":"turn-1","data":{"text":"coordinate now"}},"frame":{"schema":"agenthub.semantic-events.v1","cursor":1,"mode":"replace","events":[{"id":"sem_1_0","type":"message.input","data":{"text":"coordinate now"}}]}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		lock := map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root}
		data, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), data, 0o600); err != nil {
			t.Fatal(err)
		}

		sent := run(t, "message", "send", "--to=project1.task1", "--mode=interrupt", "coordinate now")
		if !strings.Contains(sent, `"messageId": "msg-test"`) {
			t.Fatalf("unexpected send response: %s", sent)
		}
		if requestBody["text"] != "coordinate now" || requestBody["mode"] != "interrupt" || requestBody["role"] != "agent" {
			t.Fatalf("unexpected resource message request: %#v", requestBody)
		}
		sender, _ := requestBody["sender"].(map[string]any)
		if sender["id"] != "project1.task1" || sender["name"] != "project1.task1" {
			t.Fatalf("sender provenance did not use current resource: %#v", sender)
		}
		var config app.Config
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if requestBody["senderWorkspaceInstanceId"] != config.InstanceID {
			t.Fatalf("sender Workspace provenance = %#v, want %q", requestBody["senderWorkspaceInstanceId"], config.InstanceID)
		}
		if status := run(t, "task", "status"); !strings.Contains(status, `"acceptsMessages": true`) {
			t.Fatalf("unexpected inferred status response: %s", status)
		}
		if status := run(t, "project", "status", "--project=1"); !strings.Contains(status, `"sessionState": "idle"`) {
			t.Fatalf("unexpected project status response: %s", status)
		}
		if status := run(t, "workspace", "status"); !strings.Contains(status, `"sessionState": "idle"`) {
			t.Fatalf("unexpected workspace status response: %s", status)
		}
		if state := run(t, "task", "state"); !strings.Contains(state, `"state": "waiting"`) || !strings.Contains(state, `"note": "CI is running"`) {
			t.Fatalf("unexpected task state response: %s", state)
		}
		if state := run(t, "task", "state", "set", "blocked", "--note", "Need approval"); !strings.Contains(state, `"state": "blocked"`) {
			t.Fatalf("unexpected task state update response: %s", state)
		}
		if taskStateBody["state"] != "blocked" || taskStateBody["note"] != "Need approval" {
			t.Fatalf("unexpected task state request: %#v", taskStateBody)
		}
		if message := run(t, "message", "show", "--id=msg-test"); !strings.Contains(message, `"status": "delivered"`) {
			t.Fatalf("unexpected message response: %s", message)
		}
		if history := run(t, "workspace", "history"); !strings.Contains(history, "Resource: workspace\nHistory: (empty)") {
			t.Fatalf("unexpected Workspace history text: %s", history)
		}
		var projectHistoryJSON map[string]any
		if err := json.Unmarshal([]byte(run(t, "project", "history", "--project=project1", "--json")), &projectHistoryJSON); err != nil || projectHistoryJSON["resourceId"] != "project1" {
			t.Fatalf("unexpected Project history JSON: %#v, %v", projectHistoryJSON, err)
		}
		history := run(t, "task", "history", "--cursor=cursor-test", "--limit=7")
		for _, want := range []string{"Resource: project1.task1", "Generation #1: Mailbox task (gen #1)", "Turn turn-1: completed", "Trigger: agent", "Next cursor: cursor-next"} {
			if !strings.Contains(history, want) {
				t.Fatalf("history text missing %q:\n%s", want, history)
			}
		}
		if json.Valid([]byte(history)) {
			t.Fatalf("default history output is still JSON: %s", history)
		}
		var historyJSON map[string]any
		if err := json.Unmarshal([]byte(run(t, "task", "history", "--cursor=cursor-test", "--limit=7", "--json")), &historyJSON); err != nil || historyJSON["resourceId"] != "project1.task1" {
			t.Fatalf("unexpected history JSON: %#v, %v", historyJSON, err)
		}

		turn := run(t, "history", "turn", "show", "--ref="+turnRef)
		for _, want := range []string{"Turn turn-1: completed", "Items:", "Text:\n       coordinate now", "Latest event reference: " + eventRef} {
			if !strings.Contains(turn, want) {
				t.Fatalf("Turn text missing %q:\n%s", want, turn)
			}
		}
		var turnJSON struct {
			Turn struct {
				TurnID string `json:"turnId"`
			} `json:"turn"`
		}
		if err := json.Unmarshal([]byte(run(t, "history", "turn", "show", "--ref="+turnRef, "--json")), &turnJSON); err != nil || turnJSON.Turn.TurnID != "turn-1" {
			t.Fatalf("unexpected Turn JSON: %#v, %v", turnJSON, err)
		}

		event := run(t, "history", "event", "show", "--ref="+eventRef)
		for _, want := range []string{"Event: 1", "Type: message.input", "Generation #1: Mailbox task (gen #1)", `"text": "coordinate now"`} {
			if !strings.Contains(event, want) {
				t.Fatalf("Event text missing %q:\n%s", want, event)
			}
		}
		var eventJSON struct {
			SourceEvent struct {
				ID int64 `json:"id"`
			} `json:"sourceEvent"`
		}
		if err := json.Unmarshal([]byte(run(t, "history", "event", "show", "--ref="+eventRef, "--json")), &eventJSON); err != nil || eventJSON.SourceEvent.ID != 1 {
			t.Fatalf("unexpected Event JSON: %#v, %v", eventJSON, err)
		}
		if _, err := runErr(t, "history", "turn", "show", "--ref="+turnRef, "--json", "--json"); err == nil || !strings.Contains(err.Error(), historyShowUsage) {
			t.Fatalf("duplicate history --json was accepted: %v", err)
		}
		for _, args := range [][]string{{"resource", "status"}, {"resource", "send", "--id=project1.task1", "legacy"}, {"resource", "message", "--id=msg-test"}} {
			if _, err := runErr(t, args...); err == nil || !strings.Contains(err.Error(), `unknown command "resource"`) {
				t.Fatalf("legacy resource command still exists: %v", args)
			}
		}
	})
}

func TestMessageSendToUserTarget(t *testing.T) {
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "")
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Mailbox project")
		if err := os.Chdir(filepath.Join(root, "project1")); err != nil {
			t.Fatal(err)
		}
		run(t, "task", "create", "Mailbox task")
		if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
			t.Fatal(err)
		}
		var requestBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users/disksing/messages") {
				if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, `{"messageId":"umsg-test","user":"disksing","sourceResourceId":"project1.task1","unread":true}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		lock := map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root}
		data, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), data, 0o600); err != nil {
			t.Fatal(err)
		}

		sent := run(t, "message", "send", "--to=disksing", "hello user")
		if !strings.Contains(sent, `"messageId": "umsg-test"`) {
			t.Fatalf("unexpected user send response: %s", sent)
		}
		if requestBody["text"] != "hello user" {
			t.Fatalf("unexpected user message request: %#v", requestBody)
		}
		if _, exists := requestBody["mode"]; exists {
			t.Fatalf("user target carried resource mailbox mode: %#v", requestBody)
		}
		sender, _ := requestBody["sender"].(map[string]any)
		if sender["id"] != "project1.task1" {
			t.Fatalf("sender provenance did not use current resource: %#v", sender)
		}
		var config app.Config
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if requestBody["senderWorkspaceInstanceId"] != config.InstanceID {
			t.Fatalf("sender Workspace provenance = %#v, want %q", requestBody["senderWorkspaceInstanceId"], config.InstanceID)
		}

		if _, err := runErr(t, "message", "send", "--to=disksing", "--mode=enqueue", "hello"); err == nil || !strings.Contains(err.Error(), "--mode applies only to resource targets") {
			t.Fatalf("user target accepted --mode: %v", err)
		}
		if _, err := runErr(t, "message", "send", "--to=project1", "hello"); err == nil {
			t.Fatal("resource-lookalike user name was treated as a user target")
		}
	})
}

func TestMessageSendStdinAndEscapeWarning(t *testing.T) {
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "")
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Mailbox project")
		if err := os.Chdir(filepath.Join(root, "project1")); err != nil {
			t.Fatal(err)
		}
		run(t, "task", "create", "Mailbox task")
		if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
			t.Fatal(err)
		}
		var requestBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources/project1.task1/messages") {
				if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
					t.Error(err)
				}
				_, _ = io.WriteString(w, `{"messageId":"msg-test","resourceId":"project1.task1","requestedMode":"steer","actualMode":"steer","status":"delivered"}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		lock := map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root}
		data, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), data, 0o600); err != nil {
			t.Fatal(err)
		}

		// A "-" message argument reads the body from stdin, preserving real
		// newlines.
		_, stderr, err := runWithStdin(t, "line1\nline2\n\n- item\n", "message", "send", "--to=project1.task1", "-")
		if err != nil {
			t.Fatalf("stdin send failed: %v", err)
		}
		if requestBody["text"] != "line1\nline2\n\n- item" {
			t.Fatalf("stdin message text = %#v", requestBody["text"])
		}
		if strings.Contains(stderr, "warning") {
			t.Fatalf("stdin message triggered an escape warning: %q", stderr)
		}

		// A literal \n sequence without any real newline hints at shell
		// quoting that kept the escape verbatim; the send still succeeds.
		_, stderr, err = runWithStdin(t, "", "message", "send", "--to=project1.task1", `failed:\n1. first\n2. second`)
		if err != nil {
			t.Fatalf("literal-escape send failed: %v", err)
		}
		if requestBody["text"] != `failed:\n1. first\n2. second` {
			t.Fatalf("literal-escape message text = %#v", requestBody["text"])
		}
		if !strings.Contains(stderr, "literal \\n sequences") {
			t.Fatalf("missing literal-escape warning, stderr = %q", stderr)
		}

		// Plain single-line text stays silent.
		_, stderr, err = runWithStdin(t, "", "message", "send", "--to=project1.task1", "hello")
		if err != nil {
			t.Fatalf("plain send failed: %v", err)
		}
		if strings.Contains(stderr, "warning") {
			t.Fatalf("plain message triggered an escape warning: %q", stderr)
		}
	})
}

func TestRemovedStartAndServeSubcommands(t *testing.T) {
	if _, err := runErr(t, "start"); err == nil || !strings.Contains(err.Error(), `unknown command "start"`) {
		t.Fatalf("expected pua start to be unknown, got %v", err)
	}
	for _, args := range [][]string{{"session"}, {"session", "list"}, {"session", "show", "--id=test"}} {
		if _, err := runErr(t, args...); err == nil || !strings.Contains(err.Error(), `unknown command "session"`) {
			t.Fatalf("expected removed pua session command to be unknown for %v, got %v", args, err)
		}
	}
	serveHelp := run(t, "serve", "--help")
	for _, marker := range []string{
		"usage: pua serve [--addr=<address>] [--workspace=<path>]",
		"--agenthub-mode=embedded|external",
		"--agenthub-endpoint=<url>",
		"/agenthub/v1/",
		"in-process application API",
		"PUA_SERVE_CONFIG",
	} {
		if !strings.Contains(serveHelp, marker) {
			t.Fatalf("expected pua serve help to contain %q, got:\n%s", marker, serveHelp)
		}
	}
	if _, err := runErr(t, "serve", "--bogus"); err == nil {
		t.Fatal("expected pua serve to reject unknown flags")
	}
	if _, err := runErr(t, "serve", "extra"); err == nil || !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected pua serve to reject positional arguments, got %v", err)
	}
	version := run(t, "serve", "--version")
	if !strings.HasPrefix(version, "pua branch=") {
		t.Fatalf("expected pua serve --version to print pua build info, got %q", version)
	}
}

func TestInitDefaultsToEnglishAndPersistsLanguage(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		var config app.Config
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if config.Language != testDefaultLanguage {
			t.Fatalf("expected default language %q, got %+v", testDefaultLanguage, config)
		}
		rootAgents := readFile(t, filepath.Join(root, "AGENTS.md"))
		if !strings.Contains(rootAgents, "# AgentWorkspace Agent Instructions") || strings.Contains(rootAgents, "## 1. 工作环境") {
			t.Fatalf("default Workspace prompt should be English:\n%s", rootAgents)
		}
	})
}

func TestSimplifiedChineseInitAndLanguageMigration(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init", "--language", "zh-CN")
		var config app.Config
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if config.Language != testChineseLanguage {
			t.Fatalf("expected zh-CN Workspace config, got %+v", config)
		}
		if got := readFile(t, filepath.Join(root, testWikiDir, "index.md")); got != testDefaultWikiIndex {
			t.Fatalf("unexpected Chinese Wiki index:\n%s", got)
		}
		rootAgentsPath := filepath.Join(root, "AGENTS.md")
		for _, want := range []string{"# AgentWorkspace Agent 工作指引", "## 5. PUA 资源管理", "有合适模板时优先使用，并保留模板中的规则"} {
			if got := readFile(t, rootAgentsPath); !strings.Contains(got, want) {
				t.Fatalf("Chinese Workspace prompt is missing %q:\n%s", want, got)
			}
		}

		run(t, "project", "create", "中文项目")
		run(t, "task", "create", "--project=project1", "中文任务")
		projectAgentsPath := filepath.Join(root, "project1", "AGENTS.md")
		taskAgentsPath := filepath.Join(root, "project1", "task1", "AGENTS.md")
		appendFile(t, projectAgentsPath, "\n# 团队说明\n\n保留这行。\n")
		chineseTaskMD := readFile(t, filepath.Join(root, "project1", "task1", testTaskMDFile))

		if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
			t.Fatal(err)
		}
		run(t, "migrate", "--language=en")
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if config.Language != testDefaultLanguage {
			t.Fatalf("expected migration to persist English, got %+v", config)
		}
		if got := readFile(t, rootAgentsPath); !strings.Contains(got, "# AgentWorkspace Agent Instructions") || strings.Contains(got, "## 1. 工作环境") {
			t.Fatalf("expected English Workspace prompt after migration, got:\n%s", got)
		}
		if got := readFile(t, projectAgentsPath); !strings.Contains(got, "# Project Agent Instructions") || !strings.Contains(got, "Resource ID: project1") || !strings.Contains(got, "保留这行。") {
			t.Fatalf("expected English Project card with manual content preserved, got:\n%s", got)
		}
		if got := readFile(t, taskAgentsPath); !strings.Contains(got, "# Task Agent Instructions") || !strings.Contains(got, "Resource ID: project1.task1") {
			t.Fatalf("expected English Task card after migration, got:\n%s", got)
		}
		if got := readFile(t, filepath.Join(root, "project1", "task1", testTaskMDFile)); got != chineseTaskMD {
			t.Fatalf("migration should not translate existing task.md\nbefore:\n%s\nafter:\n%s", chineseTaskMD, got)
		}

		run(t, "migrate", "--language=zh-CN")
		if got := readFile(t, taskAgentsPath); !strings.Contains(got, "# 任务 Agent 指引") || !strings.Contains(got, "当前资源 ID：project1.task1") {
			t.Fatalf("expected migration to switch Task card back to Chinese, got:\n%s", got)
		}
	})
}
func TestGeneratedAgentGuidanceSurvivesBilingualInitAndMigrate(t *testing.T) {
	cases := []struct {
		name             string
		language         string
		rootAnchors      []string
		projectAnchors   []string
		taskAnchors      []string
		wrongLanguage    string
		localOnlyHeading string
	}{
		{
			name:     "English",
			language: "en",
			rootAnchors: []string{
				"# AgentWorkspace Agent Instructions", "## 1. Environment", "## 2. Starting work",
				"## 3. Finding more information", "## 4. Permissions and PUA CLI",
				"## 5. Managing PUA resources", "## 6. Agent collaboration", "## 7. Scheduler",
				"adjust wording and tone", "[[project1.task2]]", "pua message send --to=<resource>",
				"A message delivered as steer into an already-running Turn does not subscribe",
				"Do not also send the same result with pua message send",
			},
			projectAnchors:   []string{"# Project Agent Instructions", "AgentWorkspace Project directory", "Resource ID: project1", "../AGENTS.md"},
			taskAnchors:      []string{"# Task Agent Instructions", "Resource ID: project1.task1", "../AGENTS.md", "../../AGENTS.md"},
			wrongLanguage:    "## 1. 工作环境",
			localOnlyHeading: "## 1. Environment",
		},
		{
			name:     "Simplified Chinese",
			language: "zh-CN",
			rootAnchors: []string{
				"# AgentWorkspace Agent 工作指引", "## 1. 工作环境", "## 2. 开始工作",
				"## 3. 查询更多信息", "## 4. 权限和 PUA CLI",
				"## 5. PUA 资源管理", "## 6. Agent 协作", "## 7. Scheduler",
				"调整表达方式和语气", "[[project1.task2]]", "pua message send --to=<resource>",
				"注入已有 Turn 的 steer 消息不订阅结果",
				"不要再用 pua message send 发送同一结果",
			},
			projectAnchors:   []string{"# 项目 Agent 指引", "Project 目录", "当前资源 ID：project1", "../AGENTS.md"},
			taskAnchors:      []string{"# 任务 Agent 指引", "当前资源 ID：project1.task1", "../AGENTS.md", "../../AGENTS.md"},
			wrongLanguage:    "## 1. Environment",
			localOnlyHeading: "## 1. 工作环境",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempCwd(t, func(root string) {
				run(t, "init", "--language", tc.language)
				run(t, "project", "create", "Guidance project")
				run(t, "task", "create", "--project=project1", "Guidance task")

				assertPrompts := func(stage string) {
					t.Helper()
					rootAgents := readFile(t, filepath.Join(root, "AGENTS.md"))
					for _, want := range tc.rootAnchors {
						if !strings.Contains(rootAgents, want) {
							t.Fatalf("Workspace prompt after %s is missing %q:\n%s", stage, want, rootAgents)
						}
					}
					if strings.Contains(rootAgents, tc.wrongLanguage) {
						t.Fatalf("Workspace prompt after %s contains the wrong language:\n%s", stage, rootAgents)
					}

					projectAgents := readFile(t, filepath.Join(root, "project1", "AGENTS.md"))
					for _, want := range tc.projectAnchors {
						if !strings.Contains(projectAgents, want) {
							t.Fatalf("Project card after %s is missing %q:\n%s", stage, want, projectAgents)
						}
					}
					taskAgents := readFile(t, filepath.Join(root, "project1", "task1", "AGENTS.md"))
					for _, want := range tc.taskAnchors {
						if !strings.Contains(taskAgents, want) {
							t.Fatalf("Task card after %s is missing %q:\n%s", stage, want, taskAgents)
						}
					}
					for label, content := range map[string]string{"Project": projectAgents, "Task": taskAgents} {
						if strings.Contains(content, tc.localOnlyHeading) || strings.Contains(content, "pua message send") || strings.Contains(content, "templates/") {
							t.Fatalf("%s card after %s copied Workspace guidance:\n%s", label, stage, content)
						}
					}
				}

				assertPrompts("init")
				if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
					t.Fatal(err)
				}
				run(t, "migrate", "--language", tc.language)
				assertPrompts("migrate")
			})
		})
	}
}
func TestLanguageValidationAndLegacyWorkspaceMigration(t *testing.T) {
	withTempCwd(t, func(root string) {
		if _, err := runErr(t, "init", "--language=fr"); err == nil || !strings.Contains(err.Error(), "unsupported language") {
			t.Fatalf("expected unsupported init language error, got %v", err)
		}
		assertMissing(t, filepath.Join(root, testConfigFile))

		run(t, "init", "--language=zh_CN")
		var config app.Config
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if config.Language != testChineseLanguage {
			t.Fatalf("expected language alias to normalize to zh-CN, got %+v", config)
		}
		if _, err := runErr(t, "migrate", "--language"); err == nil || !strings.Contains(err.Error(), "--language requires a value") {
			t.Fatalf("expected missing language value error, got %v", err)
		}
	})

	withTempCwd(t, func(root string) {
		if err := os.MkdirAll(filepath.Join(root, testReposDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, testArchiveDir), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, testConfigFile), `{"version":1}`+"\n")
		run(t, "migrate")
		var config app.Config
		if err := readJSON(filepath.Join(root, testConfigFile), &config); err != nil {
			t.Fatal(err)
		}
		if config.Language != testDefaultLanguage {
			t.Fatalf("expected legacy workspace to migrate to explicit English, got %+v", config)
		}
	})
}

func TestTaskLifecycle(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		assertDir(t, filepath.Join(root, testReposDir))
		assertDir(t, filepath.Join(root, testArchiveDir))
		assertFile(t, filepath.Join(root, testConfigFile))
		assertFile(t, filepath.Join(root, "AGENTS.md"))
		assertDir(t, filepath.Join(root, testWikiDir))
		assertFile(t, filepath.Join(root, testWikiDir, "index.md"))

		created := run(t, "project", "create", "Implement the pua MVP")
		if !strings.Contains(created, `"id": "project1"`) {
			t.Fatalf("expected project1 JSON, got:\n%s", created)
		}
		if strings.Contains(created, `"workflow"`) {
			t.Fatalf("project JSON should not contain workflow, got:\n%s", created)
		}
		if strings.Contains(created, `"repos"`) {
			t.Fatalf("expected project JSON not to include repos, got:\n%s", created)
		}
		assertFile(t, filepath.Join(root, "project1", "project.json"))
		assertFile(t, filepath.Join(root, "project1", "project.md"))
		assertMissing(t, filepath.Join(root, "project1", "task.json"))
		assertMissing(t, filepath.Join(root, "project1", "task.md"))
		assertMissing(t, filepath.Join(root, "project1", "work.md"))
		assertDir(t, filepath.Join(root, "project1", "artifacts"))
		assertDir(t, filepath.Join(root, "project1", "templates"))
		assertMissing(t, filepath.Join(root, "project1", "worktree"))
		projectAgents := readFile(t, filepath.Join(root, "project1", "AGENTS.md"))
		for _, want := range []string{"# Project Agent Instructions", "AgentWorkspace Project directory", "Resource ID: project1", "../AGENTS.md"} {
			if !strings.Contains(projectAgents, want) {
				t.Fatalf("Project AGENTS.md is missing %q:\n%s", want, projectAgents)
			}
		}
		if strings.Count(projectAgents, puaPromptStart) != 1 || strings.Count(projectAgents, puaPromptEnd) != 1 {
			t.Fatalf("expected Project AGENTS.md to contain one managed block, got:\n%s", projectAgents)
		}
		for _, copied := range []string{"## 1. Environment", "pua message send", "templates/", "project.md"} {
			if strings.Contains(projectAgents, copied) {
				t.Fatalf("Project AGENTS.md copied Workspace guidance %q:\n%s", copied, projectAgents)
			}
		}
		projectMDPath := filepath.Join(root, "project1", "project.md")
		projectMD := readFile(t, projectMDPath)
		if !strings.Contains(projectMD, "# Implement the pua MVP") || !strings.Contains(projectMD, "Implement the pua MVP") {
			t.Fatalf("expected project.md to contain project background, got:\n%s", projectMD)
		}
		if !strings.Contains(projectMD, "## Background") || !strings.Contains(projectMD, "## Scope") || !strings.Contains(projectMD, "## Acceptance Criteria") {
			t.Fatalf("expected project.md to include durable brief modules, got:\n%s", projectMD)
		}
		if strings.Contains(projectMD, "## Notes") {
			t.Fatalf("expected project.md to contain durable brief context only, got:\n%s", projectMD)
		}
		assertNoHan(t, projectMDPath)
		templateContent := "---\nschema-version: 2\ntitle: Daily inspection\nfields: []\n---\n# Daily inspection\n\nCheck current state.\n"
		if err := os.WriteFile(filepath.Join(root, "project1", "templates", "daily.md"), []byte(templateContent), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, "project1", "artifacts")); err != nil {
			t.Fatal(err)
		}
		projectDetailJSON := run(t, "workspace", "resource", "--id", "project1", "--json")
		var projectDetail app.ResourceDetailView
		if err := json.Unmarshal([]byte(projectDetailJSON), &projectDetail); err != nil {
			t.Fatalf("workspace project resource should print JSON, got error %v and output:\n%s", err, projectDetailJSON)
		}
		if projectDetail.Artifacts == nil || len(projectDetail.Artifacts) != 0 {
			t.Fatalf("expected missing project artifacts directory to return an empty list, got: %+v", projectDetail.Artifacts)
		}
		if len(projectDetail.Templates) != 1 {
			t.Fatalf("expected one task template, got: %+v", projectDetail.Templates)
		}
		template := projectDetail.Templates[0]
		if template.Name != "daily" || template.Title != "Daily inspection" || !strings.Contains(template.Detail, "Check current state.") {
			t.Fatalf("unexpected parsed task template: %+v", template)
		}

		listed := run(t, "project", "list")
		if !strings.Contains(listed, "project1\tImplement the pua MVP") {
			t.Fatalf("expected task list to include project1, got:\n%s", listed)
		}

		child := run(t, "task", "create", "--project=project1", "Add task commands")
		if !strings.Contains(child, `"id": "project1.task1"`) {
			t.Fatalf("expected project1.task1 JSON, got:\n%s", child)
		}
		assertFile(t, filepath.Join(root, "project1", "task1", "task.json"))
		assertFile(t, filepath.Join(root, "project1", "task1", "task.md"))
		taskMD := readFile(t, filepath.Join(root, "project1", "task1", "task.md"))
		assertMissing(t, filepath.Join(root, "project1", "task1", "work.md"))
		if !strings.Contains(taskMD, "Keep the durable task contract here") || !strings.Contains(taskMD, "when they affect the task contract") {
			t.Fatalf("expected task.md template to define the durable contract, got:\n%s", taskMD)
		}
		assertDir(t, filepath.Join(root, "project1", "task1", "worktree"))
		subtaskAgents := readFile(t, filepath.Join(root, "project1", "task1", "AGENTS.md"))
		for _, want := range []string{"# Task Agent Instructions", "Resource ID: project1.task1", "../AGENTS.md", "../../AGENTS.md"} {
			if !strings.Contains(subtaskAgents, want) {
				t.Fatalf("Task AGENTS.md is missing %q:\n%s", want, subtaskAgents)
			}
		}
		if strings.Count(subtaskAgents, puaPromptStart) != 1 || strings.Count(subtaskAgents, puaPromptEnd) != 1 {
			t.Fatalf("expected Task AGENTS.md to contain one managed block, got:\n%s", subtaskAgents)
		}
		for _, copied := range []string{"## 1. Environment", "pua message send", "templates/", "task.md", "git worktree add"} {
			if strings.Contains(subtaskAgents, copied) {
				t.Fatalf("Task AGENTS.md copied Workspace guidance %q:\n%s", copied, subtaskAgents)
			}
		}

		children := run(t, "task", "list", "--project=project1")
		if !strings.Contains(children, "task1\tAdd task commands") {
			t.Fatalf("expected subtask list to include task1, got:\n%s", children)
		}
		if strings.Contains(children, "project1.task1") {
			t.Fatalf("task list should display short task ids, got:\n%s", children)
		}
		if err := os.RemoveAll(filepath.Join(root, "project1", "task1", "artifacts")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, "project1", "task1", "worktree")); err != nil {
			t.Fatal(err)
		}
		emptyDetailJSON := run(t, "workspace", "resource", "--id", "project1.task1", "--json")
		var emptyDetail app.ResourceDetailView
		if err := json.Unmarshal([]byte(emptyDetailJSON), &emptyDetail); err != nil {
			t.Fatalf("workspace task resource should print JSON, got error %v and output:\n%s", err, emptyDetailJSON)
		}
		if emptyDetail.Artifacts == nil || len(emptyDetail.Artifacts) != 0 {
			t.Fatalf("expected missing task artifacts directory to return an empty list, got: %+v", emptyDetail.Artifacts)
		}
		if emptyDetail.Worktrees == nil || len(emptyDetail.Worktrees) != 0 {
			t.Fatalf("expected missing task worktree directory to return an empty list, got: %+v", emptyDetail.Worktrees)
		}
		if err := os.MkdirAll(filepath.Join(root, "project1", "task1", "artifacts"), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(root, "project1", "task1", "artifacts", "result.txt"), []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
		treeJSON := run(t, "workspace", "tree", "--json")
		var tree app.WorkspaceTree
		if err := json.Unmarshal([]byte(treeJSON), &tree); err != nil {
			t.Fatalf("workspace tree should print JSON, got error %v and output:\n%s", err, treeJSON)
		}
		if tree.Root != filepath.ToSlash(realPath(t, root)) || len(tree.Projects) != 1 {
			t.Fatalf("unexpected workspace tree root/projects: %+v", tree)
		}
		if tree.Projects[0].ID != "project1" || tree.Projects[0].Path != "project1" || len(tree.Projects[0].Children) != 1 {
			t.Fatalf("unexpected project tree item: %+v", tree.Projects[0])
		}
		taskItem := tree.Projects[0].Children[0]
		if taskItem.ID != "project1.task1" || taskItem.Path != "project1/task1" {
			t.Fatalf("unexpected task tree item: %+v", taskItem)
		}
		detailJSON := run(t, "workspace", "resource", "--id", "project1.task1", "--json")
		var detail app.ResourceDetailView
		if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
			t.Fatalf("workspace resource should print JSON, got error %v and output:\n%s", err, detailJSON)
		}
		if detail.ID != "project1.task1" || detail.Path != "project1/task1" || len(detail.Files) == 0 {
			t.Fatalf("unexpected task detail: %+v", detail)
		}
		if detail.Files[0].Name != "task.md" || detail.Files[0].Path != "project1/task1/task.md" {
			t.Fatalf("expected task file path in detail, got: %+v", detail.Files[0])
		}
		if detail.Files[0].ContentHash == "" {
			t.Fatal("expected task Markdown detail to include a content hash")
		}
		if len(detail.Artifacts) != 1 || detail.Artifacts[0].Name != "result.txt" {
			t.Fatalf("expected artifact file in task detail, got: %+v", detail.Artifacts)
		}

		shown := run(t, "task", "show", "--project=project1", "--task=task1")
		if !strings.Contains(shown, `"parent": "project1"`) {
			t.Fatalf("expected show to find subtask, got:\n%s", shown)
		}

		archivedTask := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(archivedTask, "project1/archive/task1") {
			t.Fatalf("expected task archive path before project archive, got:\n%s", archivedTask)
		}
		archived := run(t, "project", "archive", "--project=project1")
		if !strings.Contains(archived, "archive/project1") {
			t.Fatalf("expected archive path, got:\n%s", archived)
		}
		assertDir(t, filepath.Join(root, testArchiveDir, "project1"))
		if pathExists(filepath.Join(root, "project1")) {
			t.Fatal("project1 should have moved out of the open workspace")
		}
		openOnly := run(t, "project", "list")
		if strings.Contains(openOnly, "project1\tImplement the pua MVP") {
			t.Fatalf("archived task should not be listed by default, got:\n%s", openOnly)
		}
		allTasks := run(t, "project", "list", "--all")
		if !strings.Contains(allTasks, "project1\tImplement the pua MVP") {
			t.Fatalf("expected task list --all to include archived task, got:\n%s", allTasks)
		}

		next := run(t, "project", "create", "Second project")
		if !strings.Contains(next, `"id": "project2"`) {
			t.Fatalf("expected archived task ids not to be reused, got:\n%s", next)
		}
	})
}

func TestRemovedAutomationCommandsAreRejected(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Automation")
		if _, err := runErr(t, "task", "create", "--project=project1", "--non-interactive", "Task"); err == nil {
			t.Fatal("expected --non-interactive to be rejected")
		}
		if _, err := runErr(t, "task", "run", "queue", "--project=project1", "--task=task1"); err == nil {
			t.Fatal("expected task run command to be rejected")
		}
		if _, err := runErr(t, "task", "create", "--project=project1", "--autorun", "Task"); err == nil {
			t.Fatal("expected the retired creation flag to be rejected")
		}
		if _, err := runErr(t, "task", "autorun", "queue", "--project=project1", "--task=task1"); err == nil {
			t.Fatal("expected the retired task subcommand to be rejected")
		}
		if _, err := runErr(t, "task", "create", "--project=project1", "--self-driving", "Task"); err == nil {
			t.Fatal("expected the removed creation flag to be rejected")
		}
		if _, err := runErr(t, "task", "self-driving", "enable", "--project=project1", "--task=task1"); err == nil {
			t.Fatal("expected the removed task subcommand to be rejected")
		}
		if _, err := runErr(t, "task", "list", "--project=project1", "--runnable"); err == nil {
			t.Fatal("expected the removed runnable filter to be rejected")
		}
	})
}

func TestHelpGroupsCommandSections(t *testing.T) {
	help := run(t, "help")
	expected := []string{
		"How PUA works:",
		"All workspace data lives on the filesystem",
		"Agents may inspect\n  other resources, but write only the Workspace files owned by their starting\n  resource and its task worktrees.",
		"The web service is provided by pua serve.",
		"Usage:",
		"  pua --version\n  pua init [--language=<language>]",
		"  pua migrate [--language=<language>]",
		"  pua repo <command>",
		"  pua project <command>",
		"  pua task <command>",
		"  pua scheduler <command>",
		"  pua template <command>",
		"  pua workspace <command>",
		"  pua message <command>",
		"  pua history <command>",
		"  pua serve [--addr=<address>] [--workspace=<path>] [--version]",
		"  pua help [<command>]",
		"Commands:",
		"Use \"pua help <command>\" to see the subcommands of <command>.",
	}
	offset := 0
	for _, marker := range expected {
		index := strings.Index(help[offset:], marker)
		if index < 0 {
			t.Fatalf("expected help marker %q after offset %d, got:\n%s", marker, offset, help)
		}
		offset += index + len(marker)
	}
	// The top-level help lists only first-level subcommands; it must not expand
	// second-level command surfaces.
	for _, expanded := range []string{
		"pua repo add [--bare]",
		"pua project create [--slug <slug>] <description>",
		"pua task create [<title>]",
		"pua template list [--project=<project>]",
		"pua scheduler add",
		"pua workspace tree --json",
		"pua message send",
		"pua history turn show",
	} {
		if strings.Contains(help, expanded) {
			t.Fatalf("top-level help expands second-level command %q:\n%s", expanded, help)
		}
	}
	if strings.Contains(help, "pua session") {
		t.Fatalf("removed pua session command remains in help:\n%s", help)
	}
	messageHelp := run(t, "help", "message")
	for _, marker := range []string{
		"A message that actually opens a Turn",
		"a message delivered as steer into an\n    existing Turn does not",
		"--subscribe-result=false",
	} {
		if !strings.Contains(messageHelp, marker) {
			t.Fatalf("message help is missing %q:\n%s", marker, messageHelp)
		}
	}
}

func TestTaskHelpVariants(t *testing.T) {
	for _, args := range [][]string{
		{"help", "task"},
		{"task", "help"},
		{"task", "--help"},
		{"task", "-h"},
	} {
		out := run(t, args...)
		for _, marker := range []string{
			"pua task create [<title>]",
			"pua task list [--project=<project>] [--all]",
			"pua task show [--project=<project>] [--task=<task>]",
			"pua task archive [--project=<project>] [--task=<task>]",
			"pua task binding set [--project=<project>] [--task=<task>]",
			"pua task status [--project=<project>] [--task=<task>] [--server=<url>]",
			"pua task history [--project=<project>] [--task=<task>]",
			"Print a task's task.json as formatted JSON",
		} {
			if !strings.Contains(out, marker) {
				t.Fatalf("expected %v help to contain %q, got:\n%s", args, marker, out)
			}
		}
	}
}

func TestTaskCreateUsesTitleAndDetail(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")

		created := run(t, "task", "create", "Task title", "--project=project1", "--slug=task-title", "--detail=Line one\n\nLine two")
		var task app.Task
		if err := json.Unmarshal([]byte(created), &task); err != nil {
			t.Fatalf("task create should print task JSON, got error %v and output:\n%s", err, created)
		}
		if task.Title != "Task title" {
			t.Fatalf("expected task title in JSON, got: %+v", task)
		}
		if strings.Contains(created, `"description"`) || task.Description != "" {
			t.Fatalf("new task JSON should not include description, got:\n%s", created)
		}

		taskMD := readFile(t, filepath.Join(root, "project1", "task1-task-title", "task.md"))
		expectedTaskMD := `# Task title

## Background

Line one

Line two

## Scope

<!-- Define what is included. Add Out of Scope, Constraints, Decisions, or Open Questions when they affect the task contract. -->

## Acceptance Criteria

<!-- List observable results that mean this is done. -->
- TBD
`
		if taskMD != expectedTaskMD {
			t.Fatalf("expected detail to initialize task.md, got:\n%s", taskMD)
		}

		listed := run(t, "task", "list", "--project=project1")
		if !strings.Contains(listed, "task1\tTask title") {
			t.Fatalf("expected task list to show title, got:\n%s", listed)
		}
	})
}

func TestTaskCreateUsesCompleteTaskMarkdown(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")

		templateMarkdown := `# Template task

## Background

Template background.

## Scope

- Template scope.

## Acceptance Criteria

- Template result.
`
		run(t, "task", "create", "Template task", "--project=project1", "--slug=template-task", "--task-markdown", templateMarkdown)

		taskMD := readFile(t, filepath.Join(root, "project1", "task1-template-task", "task.md"))
		if taskMD != templateMarkdown {
			t.Fatalf("expected template markdown to be written exactly once, got:\n%s", taskMD)
		}
		for _, heading := range []string{"# Template task", "## Background", "## Scope", "## Acceptance Criteria"} {
			if count := strings.Count(taskMD, heading); count != 1 {
				t.Fatalf("expected %q exactly once, got %d in:\n%s", heading, count, taskMD)
			}
		}
		if strings.Contains(taskMD, "<!-- Define what is included.") || strings.Contains(taskMD, "- TBD") {
			t.Fatalf("complete template markdown should not include the default scaffold, got:\n%s", taskMD)
		}

		if _, err := runErr(t, "task", "create", "Ambiguous task", "--project=project1", "--detail=Background", "--task-markdown=# Full task"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected detail and complete markdown to be rejected together, got: %v", err)
		}
	})
}

func TestSluggedProjectAndTaskDirectories(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")

		created := run(t, "project", "create", "--slug", "pua-dev", "Develop pua")
		if !strings.Contains(created, `"id": "project1"`) {
			t.Fatalf("expected project id to remain project1, got:\n%s", created)
		}
		projectPath := filepath.Join(root, "project1-pua-dev")
		assertFile(t, filepath.Join(projectPath, "project.json"))
		assertMissing(t, filepath.Join(root, "project1", "project.json"))

		if err := os.Chdir(projectPath); err != nil {
			t.Fatal(err)
		}
		child := run(t, "task", "create", "develop pua", "--slug", "develop-pua")
		if err := os.Chdir(root); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(child, `"id": "project1.task1"`) {
			t.Fatalf("expected task id to remain project1.task1, got:\n%s", child)
		}
		taskPath := filepath.Join(projectPath, "task1-develop-pua")
		assertFile(t, filepath.Join(taskPath, "task.json"))
		assertMissing(t, filepath.Join(projectPath, "task1", "task.json"))

		listed := run(t, "project", "list")
		if !strings.Contains(listed, "project1\tDevelop pua") || strings.Contains(listed, "project1.task1") {
			t.Fatalf("expected project list to include only slugged project by stable id, got:\n%s", listed)
		}
		children := run(t, "task", "list", "--project=1")
		if !strings.Contains(children, "task1\tdevelop pua") {
			t.Fatalf("expected task list to include slugged task by short id, got:\n%s", children)
		}
		shown := run(t, "task", "show", "--project=project1", "--task=task1")
		if !strings.Contains(shown, `"parent": "project1"`) {
			t.Fatalf("expected show to resolve slugged task by id, got:\n%s", shown)
		}

		archivedTask := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(archivedTask, "project1-pua-dev/archive/task1-develop-pua") {
			t.Fatalf("expected task archive to preserve slugged directory name, got:\n%s", archivedTask)
		}
		assertDir(t, filepath.Join(projectPath, testArchiveDir, "task1-develop-pua"))

		nextChild := run(t, "task", "create", "--project=project1", "Next task")
		if !strings.Contains(nextChild, `"id": "project1.task2"`) {
			t.Fatalf("expected next task id to account for archived slugged task, got:\n%s", nextChild)
		}

		nextProject := run(t, "project", "create", "Next project")
		if !strings.Contains(nextProject, `"id": "project2"`) {
			t.Fatalf("expected next project id to account for slugged project, got:\n%s", nextProject)
		}

		archivedNextTask := run(t, "task", "archive", "--project=project1", "--task=task2")
		if !strings.Contains(archivedNextTask, "project1-pua-dev/archive/task2") {
			t.Fatalf("expected second task archive path before project archive, got:\n%s", archivedNextTask)
		}
		archivedProject := run(t, "project", "archive", "--project=project1")
		if !strings.Contains(archivedProject, "archive/project1-pua-dev") {
			t.Fatalf("expected project archive to preserve slugged directory name, got:\n%s", archivedProject)
		}
		assertDir(t, filepath.Join(root, testArchiveDir, "project1-pua-dev"))
	})
}

func TestMalformedSluggedDirectoriesAreIgnored(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		now := time.Now().Format(time.RFC3339)
		malformedProject := app.Project{ResourceMeta: app.ResourceMeta{SchemaVersion: 1, ID: "project9", Type: "project", Title: "Malformed project", CreatedAt: now, UpdatedAt: now}, Description: "Malformed project"}
		writeTestResourceJSON(t, filepath.Join(root, "project9--bad", testProjectJSONFile), malformedProject)
		listed := run(t, "project", "list")
		if strings.Contains(listed, "project9") {
			t.Fatalf("malformed project directory should not be listed, got:\n%s", listed)
		}
		out, err := runErr(t, "project", "show", "--project=project9")
		if err == nil {
			t.Fatalf("malformed project directory should not resolve by id, got stdout:\n%s", out)
		}

		next := run(t, "project", "create", "First valid project")
		if !strings.Contains(next, `"id": "project1"`) {
			t.Fatalf("malformed project directory should not affect next id, got:\n%s", next)
		}

		parentPath := filepath.Join(root, "project1")
		parentID := "project1"
		malformedTask := app.Task{ResourceMeta: app.ResourceMeta{SchemaVersion: 1, ID: "project1.task8", Type: "task", Title: "Malformed task", CreatedAt: now, UpdatedAt: now}, Parent: parentID, Description: "Malformed task"}
		writeTestResourceJSON(t, filepath.Join(parentPath, "task8--bad", "task.json"), malformedTask)
		children := run(t, "task", "list", "--project=project1", "--all")
		if strings.Contains(children, "task8\tMalformed task") {
			t.Fatalf("malformed task directory should not be listed, got:\n%s", children)
		}

		child := run(t, "task", "create", "--project=project1", "First valid task")
		if !strings.Contains(child, `"id": "project1.task1"`) {
			t.Fatalf("malformed task directory should not affect next id, got:\n%s", child)
		}
	})
}

func TestResourceLocatorRejectsDuplicateIDs(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Project")
		duplicate := filepath.Join(root, testArchiveDir, "project1-copy")
		if err := os.MkdirAll(duplicate, 0o755); err != nil {
			t.Fatal(err)
		}
		data := readFile(t, filepath.Join(root, "project1", testProjectJSONFile))
		if err := os.WriteFile(filepath.Join(duplicate, testProjectJSONFile), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := runErr(t, "project", "show", "--project=project1")
		if err == nil || !strings.Contains(err.Error(), "multiple resource directories") {
			t.Fatalf("expected duplicate resource error, got stdout %q and error %v", out, err)
		}
	})
}

func TestInitRejectsExistingWorkspaceChild(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		child := filepath.Join(root, "nested")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(child); err != nil {
			t.Fatal(err)
		}

		out, err := runErr(t, "init")
		if err == nil {
			t.Fatalf("expected init inside existing workspace to fail, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), "cannot initialize workspace inside existing workspace") {
			t.Fatalf("expected existing workspace init error, got: %v\nstdout:\n%s", err, out)
		}
	})
}

func TestTaskArchiveAllowsMergedRepoWorktree(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Archive after merge")
		run(t, "task", "create", "--project=project1", "Code task")
		repoPath := filepath.Join(root, testReposDir, "disksing", "pua")
		writeGitRepo(t, repoPath, "master")
		worktreePath := filepath.Join(root, "project1", "task1", "worktree", "pua")
		runGit(t, repoPath, "worktree", "add", "-b", "agent/project1.task1", worktreePath, "master")

		archived := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(archived, "project1/archive/task1") {
			t.Fatalf("expected archive path, got:\n%s", archived)
		}
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task1"))
		gitdir := readFile(t, filepath.Join(repoPath, ".git", "worktrees", "pua", "gitdir"))
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		wantGitdir := filepath.Join(resolvedRoot, "project1", testArchiveDir, "task1", "worktree", "pua", ".git")
		if strings.TrimSpace(gitdir) != wantGitdir {
			t.Fatalf("expected worktree gitdir to be repaired to %q, got %q", wantGitdir, gitdir)
		}
		runGit(t, filepath.Join(root, "project1", testArchiveDir, "task1", "worktree", "pua"), "status", "--porcelain")
	})
}

func TestTaskArchiveWarnsUnmergedRepoWorktree(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Archive before merge")
		run(t, "task", "create", "--project=project1", "Code task")
		repoPath := filepath.Join(root, testReposDir, "disksing", "pua")
		writeGitRepo(t, repoPath, "master")
		worktreePath := filepath.Join(root, "project1", "task1", "worktree", "pua")
		runGit(t, repoPath, "worktree", "add", "-b", "agent/project1.task1", worktreePath, "master")
		if err := os.WriteFile(filepath.Join(worktreePath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, worktreePath, "add", "feature.txt")
		runGit(t, worktreePath, "-c", "user.name=PUA Test", "-c", "user.email=pua@example.com", "commit", "-m", "feature work")

		out := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(out, `warning[unmerged_commits]`) || !strings.Contains(out, `repo "disksing/pua"`) || !strings.Contains(out, `not merged into target branch "master"`) || !strings.Contains(out, "feature work") {
			t.Fatalf("expected clear unmerged commits warning, got stdout:\n%s", out)
		}
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task1"))
		if pathExists(filepath.Join(root, "project1", "task1")) {
			t.Fatal("project1.task1 should have been archived despite the warning")
		}
	})
}

func TestTaskArchiveIgnoresNonGitWorktreeDir(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Archive without a checkout")
		run(t, "task", "create", "--project=project1", "Code task")
		if err := os.MkdirAll(filepath.Join(root, "project1", "task1", "worktree", "scratch"), 0o755); err != nil {
			t.Fatal(err)
		}

		archived := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(archived, "project1/archive/task1") {
			t.Fatalf("expected archive path, got:\n%s", archived)
		}
		if strings.Contains(archived, "warning[") {
			t.Fatalf("expected no worktree warnings for a non-Git directory, got:\n%s", archived)
		}
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task1"))
	})
}

func TestTaskArchiveWarnsDirtyWorktreeAndMissingTarget(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Archive with Git warnings")
		run(t, "task", "create", "--project=project1", "Code task")
		repoPath := filepath.Join(root, testReposDir, "disksing", "pua")
		writeGitRepo(t, repoPath, "master")
		worktreePath := filepath.Join(root, "project1", "task1", "worktree", "pua")
		runGit(t, repoPath, "worktree", "add", "-b", "agent/project1.task1", worktreePath, "master")
		if err := os.WriteFile(filepath.Join(worktreePath, "uncommitted.txt"), []byte("preserve me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, repoPath, "update-ref", "-d", "refs/heads/master")

		out := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(out, "warning[dirty_worktree]") || !strings.Contains(out, "warning[target_branch_missing]") {
			t.Fatalf("expected dirty and missing-target warnings, got stdout:\n%s", out)
		}
		archivedWorktree := filepath.Join(root, "project1", testArchiveDir, "task1", "worktree", "pua", "uncommitted.txt")
		if got := readFile(t, archivedWorktree); got != "preserve me\n" {
			t.Fatalf("archive did not preserve dirty worktree content: %q", got)
		}
	})
}

func TestTaskArchiveSubtaskMovesToParentArchive(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		run(t, "task", "create", "--project=project1", "Child task")

		archived := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(archived, "project1/archive/task1") {
			t.Fatalf("expected parent-local archive path, got:\n%s", archived)
		}
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task1"))
		if pathExists(filepath.Join(root, testArchiveDir, "project1.task1")) {
			t.Fatal("subtask should not have moved to the workspace archive")
		}
		if pathExists(filepath.Join(root, "project1", "task1")) {
			t.Fatal("subtask should have moved out of the parent task's open subtasks")
		}

		children := run(t, "task", "list", "--project=project1")
		if strings.Contains(children, "task1\tChild task") {
			t.Fatalf("archived subtask should not be listed as open, got:\n%s", children)
		}
		allChildren := run(t, "task", "list", "--project=project1", "--all")
		if !strings.Contains(allChildren, "task1\tChild task") {
			t.Fatalf("expected subtask list --all to include archived subtask, got:\n%s", allChildren)
		}

		next := run(t, "task", "create", "--project=project1", "Next child")
		if !strings.Contains(next, `"id": "project1.task2"`) {
			t.Fatalf("expected archived subtask ids not to be reused, got:\n%s", next)
		}
	})
}

func TestArchiveDispatchesByTypedCommand(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Project")
		run(t, "task", "create", "--project=project1", "Task")

		taskOut := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(taskOut, "project1/archive/task1") {
			t.Fatalf("unexpected task archive path: %s", taskOut)
		}
		projectOut := run(t, "project", "archive", "--project=project1")
		if !strings.Contains(projectOut, "archive/project1") {
			t.Fatalf("unexpected project archive path: %s", projectOut)
		}
		assertDir(t, filepath.Join(root, testArchiveDir, "project1"))
	})
}

func TestTaskArchiveRejectsLegacyPositionalID(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")

		out, err := runErr(t, "task", "archive", "task1.1")
		if err == nil {
			t.Fatalf("expected positional task id to be rejected, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), taskArchiveUsage) {
			t.Fatalf("expected task archive usage error, got: %v\nstdout:\n%s", err, out)
		}
	})
}

func TestProjectListOnlyIncludesProjects(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		run(t, "task", "create", "--project=project1", "First child")
		run(t, "task", "create", "--project=project1", "Second child")
		run(t, "project", "create", "Other project")

		listed := run(t, "project", "list")
		if strings.Contains(listed, "project1.task1\tFirst child") {
			t.Fatalf("default project list should not include tasks, got:\n%s", listed)
		}
		if !strings.Contains(listed, "project1\tParent project") || !strings.Contains(listed, "project2\tOther project") {
			t.Fatalf("expected project list to include open projects, got:\n%s", listed)
		}

		children := run(t, "task", "list", "--project=project1")
		if !strings.Contains(children, "task1\tFirst child") || !strings.Contains(children, "task2\tSecond child") {
			t.Fatalf("expected task list to include project tasks, got:\n%s", children)
		}

		out, err := runErr(t, "project", "list", "--tree")
		if err == nil {
			t.Fatalf("expected --tree to be rejected, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), "usage: pua project list [--all]") {
			t.Fatalf("expected project list usage error, got: %v\nstdout:\n%s", err, out)
		}
	})
}

func TestTaskCreateRejectsNestedTasks(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		run(t, "task", "create", "--project=project1", "Child task")

		out, err := runErr(t, "task", "create", "--project=project1.task1", "Nested task")
		if err == nil {
			t.Fatalf("expected nested task creation to fail, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), "invalid project") {
			t.Fatalf("expected invalid project error, got: %v\nstdout:\n%s", err, out)
		}
	})
}

func TestSubtaskCommandRemoved(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")

		out, err := runErr(t, "subtask", "list", "project1")
		if err == nil {
			t.Fatalf("expected subtask command to fail, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), `unknown command "subtask"`) {
			t.Fatalf("expected unknown command error, got: %v\nstdout:\n%s", err, out)
		}
	})
}

func TestMigrateRejectsProjectTasksArgument(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")

		out, err := runErr(t, "migrate", "project-tasks")
		if err == nil {
			t.Fatalf("expected migrate project-tasks to fail, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), "usage: pua migrate") {
			t.Fatalf("expected migrate usage error, got: %v\nstdout:\n%s", err, out)
		}
	})
}

func TestProjectListAllIncludesArchivedProjectsOnly(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		run(t, "task", "create", "--project=project1", "Archived child")
		run(t, "task", "create", "--project=project1", "Open child")
		run(t, "task", "archive", "--project=project1", "--task=task1")

		openProjects := run(t, "project", "list")
		if strings.Contains(openProjects, "project1.task1") {
			t.Fatalf("project list should not include tasks, got:\n%s", openProjects)
		}

		allTasks := run(t, "task", "list", "--project=project1", "--all")
		if !strings.Contains(allTasks, "task1\tArchived child") || !strings.Contains(allTasks, "task2\tOpen child") {
			t.Fatalf("task list --all should include archived and open tasks, got:\n%s", allTasks)
		}

		out := run(t, "project", "archive", "--project=project1")
		if !strings.Contains(out, "warning[open_child_task]") || !strings.Contains(out, "project1.task2") {
			t.Fatalf("expected open child task warning, got stdout:\n%s", out)
		}
		assertDir(t, filepath.Join(root, testArchiveDir, "project1"))
		if pathExists(filepath.Join(root, "project1")) {
			t.Fatal("project1 should have moved out of the open workspace")
		}
		openProjects = run(t, "project", "list")
		if strings.Contains(openProjects, "project1\tParent project") {
			t.Fatalf("archived project should not be listed by default, got:\n%s", openProjects)
		}
		allProjects := run(t, "project", "list", "--all")
		if !strings.Contains(allProjects, "project1\tParent project") || strings.Contains(allProjects, "project1.task") {
			t.Fatalf("project list --all should include archived projects but not tasks, got:\n%s", allProjects)
		}
	})
}

func TestProjectAndTaskFlagSelection(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Flag project")
		run(t, "task", "create", "--project=1", "First task")
		run(t, "task", "create", "--project=project1", "Second task")

		projectByNumber := run(t, "project", "show", "--project=1")
		if !strings.Contains(projectByNumber, `"id": "project1"`) {
			t.Fatalf("expected numeric project selector to show project1, got:\n%s", projectByNumber)
		}

		taskByNumber := run(t, "task", "show", "--project=1", "--task=2")
		if !strings.Contains(taskByNumber, `"id": "project1.task2"`) {
			t.Fatalf("expected numeric task selector to show project1.task2, got:\n%s", taskByNumber)
		}
		taskByShortID := run(t, "task", "show", "--project=project1", "--task=task1")
		if !strings.Contains(taskByShortID, `"id": "project1.task1"`) {
			t.Fatalf("expected short task selector to show project1.task1, got:\n%s", taskByShortID)
		}
		out, err := runErr(t, "task", "show", "--task=project1.task1")
		if err == nil {
			t.Fatalf("expected full task id to be rejected as --task value, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), "invalid task") {
			t.Fatalf("expected invalid task error, got: %v\nstdout:\n%s", err, out)
		}

		if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
			t.Fatal(err)
		}
		projectFromCwd := run(t, "project", "show")
		if !strings.Contains(projectFromCwd, `"id": "project1"`) {
			t.Fatalf("expected project show to infer project from cwd, got:\n%s", projectFromCwd)
		}
		taskFromCwd := run(t, "task", "show")
		if !strings.Contains(taskFromCwd, `"id": "project1.task1"`) {
			t.Fatalf("expected task show to infer task from cwd, got:\n%s", taskFromCwd)
		}
		listFromCwd := run(t, "task", "list")
		if !strings.Contains(listFromCwd, "task1\tFirst task") || !strings.Contains(listFromCwd, "task2\tSecond task") {
			t.Fatalf("expected task list to infer project from cwd, got:\n%s", listFromCwd)
		}
		createdFromCwd := run(t, "task", "create", "Third task")
		if !strings.Contains(createdFromCwd, `"id": "project1.task3"`) {
			t.Fatalf("expected task create to infer project from cwd, got:\n%s", createdFromCwd)
		}
		if err := os.Chdir(root); err != nil {
			t.Fatal(err)
		}

		archived := run(t, "task", "archive", "--project=1", "--task=2")
		if !strings.Contains(archived, "project1/archive/task2") {
			t.Fatalf("expected task archive to accept numeric project/task selectors, got:\n%s", archived)
		}
		run(t, "task", "archive", "--project=1", "--task=1")
		run(t, "task", "archive", "--project=1", "--task=3")
		projectArchive := run(t, "project", "archive", "--project=1")
		if !strings.Contains(projectArchive, "archive/project1") {
			t.Fatalf("expected project archive to accept numeric project selector, got:\n%s", projectArchive)
		}
	})
}

func TestSubtaskCreateSkipsArchivedAndOpenSubtaskIDs(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		for _, description := range []string{
			"Archived child one",
			"Archived child two",
			"Archived child three",
			"Open child four",
			"Open child five",
		} {
			run(t, "task", "create", "--project=project1", description)
		}
		for _, id := range []string{"1", "2", "3"} {
			run(t, "task", "archive", "--project=project1", "--task="+id)
		}
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task1"))
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task2"))
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task3"))
		assertDir(t, filepath.Join(root, "project1", "task4"))
		assertDir(t, filepath.Join(root, "project1", "task5"))

		next := run(t, "task", "create", "--project=project1", "Next child")
		if !strings.Contains(next, `"id": "project1.task6"`) {
			t.Fatalf("expected archived and open subtask ids not to be reused, got:\n%s", next)
		}
		assertDir(t, filepath.Join(root, "project1", "task6"))
	})
}

func TestTaskArchiveWarnsUnmergedSubtaskRepoWorktree(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		run(t, "task", "create", "--project=project1", "Child task")
		repoPath := filepath.Join(root, testReposDir, "disksing", "pua")
		writeGitRepo(t, repoPath, "master")
		worktreePath := filepath.Join(root, "project1", "task1", "worktree", "pua")
		runGit(t, repoPath, "worktree", "add", "-b", "agent/project1.task1", worktreePath, "master")
		if err := os.WriteFile(filepath.Join(worktreePath, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, worktreePath, "add", "feature.txt")
		runGit(t, worktreePath, "-c", "user.name=PUA Test", "-c", "user.email=pua@example.com", "commit", "-m", "child feature work")

		out := run(t, "task", "archive", "--project=project1", "--task=task1")
		if !strings.Contains(out, `warning[unmerged_commits]`) || !strings.Contains(out, `repo "disksing/pua"`) || !strings.Contains(out, `not merged into target branch "master"`) || !strings.Contains(out, "child feature work") {
			t.Fatalf("expected clear unmerged commits warning, got stdout:\n%s", out)
		}
		assertDir(t, filepath.Join(root, "project1", testArchiveDir, "task1"))
		if pathExists(filepath.Join(root, "project1", "task1")) {
			t.Fatal("unmerged subtask should have been archived despite the warning")
		}
	})
}

func TestRepoAddClonesNormalCheckoutByDefaultAndBareWithFlag(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		source := filepath.Join(root, "source")
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "init", "-b", "main")
		if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# source\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, source, "add", "README.md")
		runGit(t, source, "-c", "user.name=PUA Test", "-c", "user.email=pua@example.com", "commit", "-m", "initial")

		added := run(t, "repo", "add", "disksing/pua", source)
		if !strings.Contains(added, "repos/disksing/pua") {
			t.Fatalf("expected normal repo path, got:\n%s", added)
		}
		assertDir(t, filepath.Join(root, testReposDir, "disksing", "pua", ".git"))
		assertFile(t, filepath.Join(root, testReposDir, "disksing", "pua", "README.md"))
		if pathExists(filepath.Join(root, testReposDir, "disksing", "pua.git")) {
			t.Fatal("default repo add should not create a bare .git repository")
		}

		bare := run(t, "repo", "add", "--bare", "disksing/pua-bare", source)
		if !strings.Contains(bare, "repos/disksing/pua-bare.git") {
			t.Fatalf("expected bare repo path, got:\n%s", bare)
		}
		assertFile(t, filepath.Join(root, testReposDir, "disksing", "pua-bare.git", "HEAD"))
	})
}

func TestRepoListFindsRepositories(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		writeFakeRepo(t, filepath.Join(root, testReposDir, "disksing", "pua"))
		writeFakeBareRepo(t, filepath.Join(root, testReposDir, "disksing", "legacy.git"), "master")

		listed := run(t, "repo", "list")
		if !strings.Contains(listed, "disksing/pua\trepos/disksing/pua") {
			t.Fatalf("expected repo list to include fake normal repo, got:\n%s", listed)
		}
		if !strings.Contains(listed, "disksing/legacy\trepos/disksing/legacy.git") {
			t.Fatalf("expected repo list to include fake bare repo, got:\n%s", listed)
		}
	})
}

func TestTaskRepoDiscoveryFromWorktree(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Derive repo metadata from worktree")
		run(t, "task", "create", "--project=project1", "Code task")
		repoPath := filepath.Join(root, testReposDir, "disksing", "pua")
		writeGitRepo(t, repoPath, "master")
		worktreePath := filepath.Join(root, "project1", "task1", "worktree", "pua")
		runGit(t, repoPath, "worktree", "add", "-b", "agent/project1.task1", worktreePath, "master")

		out, err := runErr(t, "project", "repo", "add", "project1", "disksing/pua")
		if err == nil {
			t.Fatalf("expected project repo command to fail, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), "projects do not manage repositories or worktrees") {
			t.Fatalf("expected project repo rejection, got: %v\nstdout:\n%s", err, out)
		}

		out, err = runErr(t, "task", "repo", "list", "--project=project1", "--task=task1")
		if err == nil {
			t.Fatalf("expected removed task repo command to fail, got stdout:\n%s", out)
		}
		if !strings.Contains(err.Error(), `unknown task subcommand "repo"`) {
			t.Fatalf("expected unknown subcommand error, got: %v\nstdout:\n%s", err, out)
		}

		detail := run(t, "workspace", "resource", "--id=project1.task1", "--json")
		for _, want := range []string{
			`"name": "disksing/pua"`,
			`"repoPath": "repos/disksing/pua"`,
			`"worktreePath": "project1/task1/worktree/pua"`,
			`"branch": "agent/project1.task1"`,
			`"targetBranch": "master"`,
		} {
			if !strings.Contains(detail, want) {
				t.Fatalf("expected resource detail to contain %s, got:\n%s", want, detail)
			}
		}

		var taskMetadata map[string]any
		if err := readJSON(filepath.Join(root, "project1", "task1", "task.json"), &taskMetadata); err != nil {
			t.Fatal(err)
		}
		if _, ok := taskMetadata["repos"]; ok {
			t.Fatalf("task.json should not persist repo metadata, got:\n%v", taskMetadata)
		}
	})
}

func TestTaskRepoDiscoverySupportsLegacyBareRepos(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Derive legacy bare repo metadata from worktree")
		run(t, "task", "create", "--project=project1", "Code task")
		barePath := filepath.Join(root, testReposDir, "disksing", "pua.git")
		sourcePath := filepath.Join(root, "source")
		writeGitRepo(t, sourcePath, "master")
		runGit(t, root, "clone", "--bare", sourcePath, barePath)
		worktreePath := filepath.Join(root, "project1", "task1", "worktree", "pua")
		runGit(t, barePath, "worktree", "add", "-b", "agent/project1.task1", worktreePath, "master")

		detail := run(t, "workspace", "resource", "--id=project1.task1", "--json")
		if !strings.Contains(detail, `"barePath": "repos/disksing/pua.git"`) {
			t.Fatalf("expected derived legacy bare path, got:\n%s", detail)
		}
		if strings.Contains(detail, `"repoPath"`) {
			t.Fatalf("legacy bare repo should not also set repoPath, got:\n%s", detail)
		}
		if !strings.Contains(detail, `"name": "disksing/pua"`) || !strings.Contains(detail, `"targetBranch": "master"`) {
			t.Fatalf("expected derived name and target branch, got:\n%s", detail)
		}
	})
}

func TestMigrateUpdatesOnlyManagedAgentsBlock(t *testing.T) {
	withTempCwd(t, func(root string) {
		agentsPath := filepath.Join(root, "AGENTS.md")
		original := "# Human Notes\n\nKeep this line.\n"
		if err := os.WriteFile(agentsPath, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}

		run(t, "init")
		first := readFile(t, agentsPath)
		if !strings.Contains(first, original) {
			t.Fatalf("expected human content to be preserved, got:\n%s", first)
		}
		for _, want := range []string{
			"# AgentWorkspace Agent Instructions",
			"read a small recent page of resource History",
			"project.json and task.json contain structured information understood by PUA",
			"Read wiki/index.md first",
			"rather than keeping another permanent progress file",
		} {
			if !strings.Contains(first, want) {
				t.Fatalf("Workspace AGENTS.md is missing %q:\n%s", want, first)
			}
		}
		if strings.Count(first, puaPromptStart) != 1 || strings.Count(first, puaPromptEnd) != 1 {
			t.Fatalf("expected one PUA managed block, got:\n%s", first)
		}

		replaced := strings.Replace(first, "# AgentWorkspace Agent Instructions", "old prompt text", 1)
		if err := os.WriteFile(agentsPath, []byte(replaced), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "migrate")
		second := readFile(t, agentsPath)
		if strings.Contains(second, "old prompt text") || !strings.Contains(second, "# AgentWorkspace Agent Instructions") {
			t.Fatalf("expected managed block to be replaced, got:\n%s", second)
		}
		if !strings.Contains(second, "Keep this line.") {
			t.Fatalf("expected human content to survive replacement, got:\n%s", second)
		}
		if strings.Count(second, puaPromptStart) != 1 || strings.Count(second, puaPromptEnd) != 1 {
			t.Fatalf("expected migrate to avoid duplicate managed blocks, got:\n%s", second)
		}
	})
}
func TestWorkspaceWikiInitMigrateAndSnapshot(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		indexPath := filepath.Join(root, testWikiDir, "index.md")
		if got := readFile(t, indexPath); got != defaultWikiIndex {
			t.Fatalf("unexpected default Wiki index:\n%s", got)
		}

		customIndex := "# Team Wiki\n\n- [Architecture](architecture.md)\n"
		if err := os.WriteFile(indexPath, []byte(customIndex), 0o644); err != nil {
			t.Fatal(err)
		}
		guideDir := filepath.Join(root, testWikiDir, "guides", "operations")
		if err := os.MkdirAll(guideDir, 0o755); err != nil {
			t.Fatal(err)
		}
		guidePath := filepath.Join(guideDir, "deploy.txt")
		if err := os.WriteFile(guidePath, []byte("deploy safely\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "migrate")
		if got := readFile(t, indexPath); got != customIndex {
			t.Fatalf("migrate rewrote the custom Wiki index:\n%s", got)
		}
		if got := readFile(t, guidePath); got != "deploy safely\n" {
			t.Fatalf("migrate rewrote a custom Wiki page: %q", got)
		}

		tree, err := buildWorkspaceTree()
		if err != nil {
			t.Fatal(err)
		}
		if !tree.Wiki.Exists || tree.Wiki.Error != "" || len(tree.Wiki.Entries) != 2 {
			t.Fatalf("unexpected Wiki snapshot: %+v", tree.Wiki)
		}
		if tree.Wiki.Entries[0].Name != "guides" || tree.Wiki.Entries[0].Path != "wiki/guides" || tree.Wiki.Entries[0].Type != "directory" {
			t.Fatalf("unexpected nested Wiki root entry: %+v", tree.Wiki.Entries[0])
		}
		operations := tree.Wiki.Entries[0].Children[0]
		if operations.Path != "wiki/guides/operations" || len(operations.Children) != 1 || operations.Children[0].Path != "wiki/guides/operations/deploy.txt" || operations.Children[0].Modified == "" {
			t.Fatalf("unexpected nested Wiki tree: %+v", tree.Wiki.Entries[0])
		}
		originalSize := operations.Children[0].Size
		if err := os.WriteFile(guidePath, []byte("deploy safely with a reviewed checklist\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refreshedTree, err := buildWorkspaceTree()
		if err != nil {
			t.Fatal(err)
		}
		refreshedPage := refreshedTree.Wiki.Entries[0].Children[0].Children[0]
		if refreshedPage.Size == originalSize {
			t.Fatalf("Wiki snapshot did not reflect a modified file: before=%d after=%d", originalSize, refreshedPage.Size)
		}

		if err := os.RemoveAll(filepath.Join(root, testWikiDir)); err != nil {
			t.Fatal(err)
		}
		tree, err = buildWorkspaceTree()
		if err != nil {
			t.Fatal(err)
		}
		if tree.Wiki.Exists || tree.Wiki.Entries == nil || len(tree.Wiki.Entries) != 0 {
			t.Fatalf("missing Wiki should have an explicit empty snapshot: %+v", tree.Wiki)
		}
		run(t, "migrate")
		if got := readFile(t, indexPath); got != defaultWikiIndex {
			t.Fatalf("migrate did not restore the default Wiki index:\n%s", got)
		}

		if err := os.Remove(indexPath); err != nil {
			t.Fatal(err)
		}
		tree, err = buildWorkspaceTree()
		if err != nil {
			t.Fatal(err)
		}
		if !tree.Wiki.Exists || tree.Wiki.Entries == nil || len(tree.Wiki.Entries) != 0 {
			t.Fatalf("empty Wiki should remain distinguishable from a missing Wiki: %+v", tree.Wiki)
		}

		if err := os.Remove(filepath.Join(root, testWikiDir)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, testWikiDir), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		tree, err = buildWorkspaceTree()
		if err != nil {
			t.Fatal(err)
		}
		if !tree.Wiki.Exists || !strings.Contains(tree.Wiki.Error, "not a directory") {
			t.Fatalf("invalid Wiki path should report a clear snapshot error: %+v", tree.Wiki)
		}
		if _, err := runErr(t, "migrate"); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("migrate should reject an invalid Wiki path, got %v", err)
		}
	})
}

func TestMigrateRefreshesOpenTaskAgentsAndPreservesManualContent(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Parent project")
		run(t, "task", "create", "--project=project1", "Open child")
		run(t, "task", "create", "--project=project1", "Archived child")
		run(t, "task", "archive", "--project=project1", "--task=task2")
		legacyProjectWork := filepath.Join(root, "project1", "work.md")
		if err := os.WriteFile(legacyProjectWork, []byte("# Legacy project work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "project1", "task1", "work.md"), "# Open child checkpoint\n\nKeep this manual task history.\n")
		writeFile(t, filepath.Join(root, "project1", testArchiveDir, "task2", "work.md"), "# Archived child checkpoint\n\nKeep archived history.\n")

		rootAgents := filepath.Join(root, "AGENTS.md")
		taskAgents := filepath.Join(root, "project1", "AGENTS.md")
		subtaskAgents := filepath.Join(root, "project1", "task1", "AGENTS.md")
		archivedAgents := filepath.Join(root, "project1", testArchiveDir, "task2", "AGENTS.md")

		writeStaleManagedBlock(t, rootAgents, "# AgentWorkspace Agent Instructions", "old workspace prompt")
		appendFile(t, taskAgents, "\n# Task Notes\n\nKeep task note.\n")
		writeStaleManagedBlock(t, taskAgents, "You are working in an AgentWorkspace Project directory.", "old project prompt")
		appendFile(t, subtaskAgents, "\n# Child Notes\n\nKeep child note.\n")
		writeStaleManagedBlock(t, subtaskAgents, "Resource ID: project1.task1", "old child prompt")
		archivedBefore := readFile(t, archivedAgents)

		if err := os.Chdir(filepath.Join(root, "project1", "task1")); err != nil {
			t.Fatal(err)
		}
		run(t, "migrate")
		assertFile(t, legacyProjectWork)
		assertFile(t, filepath.Join(root, "project1", "task1", "work.md"))
		assertFile(t, filepath.Join(root, "project1", testArchiveDir, "task2", "work.md"))
		for _, path := range []string{filepath.Join(root, "project1", "task1", "task.md"), filepath.Join(root, "project1", testArchiveDir, "task2", "task.md")} {
			if got := readFile(t, path); strings.Contains(got, "source=work.md") {
				t.Fatalf("migrate should no longer fold work.md into %s, got:\n%s", path, got)
			}
		}

		if pathExists(filepath.Join(root, "project1", "task1", testConfigFile)) {
			t.Fatal("migrate from task should not create nested workspace.json")
		}
		if pathExists(filepath.Join(root, "project1", "task1", testReposDir)) {
			t.Fatal("migrate from task should not create nested repos directory")
		}
		if pathExists(filepath.Join(root, "project1", "task1", testArchiveDir)) {
			t.Fatal("migrate from task should not create nested archive directory")
		}

		rootAfter := readFile(t, rootAgents)
		if strings.Contains(rootAfter, "old workspace prompt") || !strings.Contains(rootAfter, "# AgentWorkspace Agent Instructions") {
			t.Fatalf("expected workspace managed block to refresh, got:\n%s", rootAfter)
		}

		taskAfter := readFile(t, taskAgents)
		if strings.Contains(taskAfter, "old project prompt") {
			t.Fatalf("expected task managed block to refresh, got:\n%s", taskAfter)
		}
		if !strings.Contains(taskAfter, "Keep task note.") {
			t.Fatalf("expected task manual content to survive refresh, got:\n%s", taskAfter)
		}
		if !strings.Contains(taskAfter, "Resource ID: project1") || !strings.Contains(taskAfter, "../AGENTS.md") {
			t.Fatalf("expected Project launch card to be restored, got:\n%s", taskAfter)
		}
		if strings.Count(taskAfter, puaPromptStart) != 1 || strings.Count(taskAfter, puaPromptEnd) != 1 {
			t.Fatalf("expected task refresh to keep one managed block, got:\n%s", taskAfter)
		}

		subtaskAfter := readFile(t, subtaskAgents)
		if strings.Contains(subtaskAfter, "old child prompt") {
			t.Fatalf("expected subtask managed block to refresh, got:\n%s", subtaskAfter)
		}
		if !strings.Contains(subtaskAfter, "Keep child note.") {
			t.Fatalf("expected subtask manual content to survive refresh, got:\n%s", subtaskAfter)
		}
		if !strings.Contains(subtaskAfter, "Resource ID: project1.task1") || !strings.Contains(subtaskAfter, "../AGENTS.md") || !strings.Contains(subtaskAfter, "../../AGENTS.md") {
			t.Fatalf("expected Task launch card to be restored, got:\n%s", subtaskAfter)
		}
		if strings.Count(subtaskAfter, puaPromptStart) != 1 || strings.Count(subtaskAfter, puaPromptEnd) != 1 {
			t.Fatalf("expected subtask refresh to keep one managed block, got:\n%s", subtaskAfter)
		}

		archivedAfter := readFile(t, archivedAgents)
		if archivedAfter != archivedBefore {
			t.Fatalf("expected archived subtask AGENTS.md not to change\nbefore:\n%s\nafter:\n%s", archivedBefore, archivedAfter)
		}
	})
}

func TestStructuredTemplateCommandsAndTaskCreate(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Template project")
		template := `---
schema-version: 2
title: Request
task-title: "{{ summary }}"
fields:
  - name: summary
    type: text
    label: Summary
    required: true
  - name: body
    type: textarea
    label: Body
    required: true
  - name: enabled
    type: boolean
    label: Enabled
    default: false
---
# {{ summary }}

{{ body }}

Enabled: {{ enabled }}
`
		templatePath := filepath.Join(root, "project1", "templates", "request.md")
		if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
			t.Fatal(err)
		}
		listed := run(t, "template", "list", "--project=project1")
		if !strings.Contains(listed, "request\tRequest\tv2\t3 fields\tvalid") {
			t.Fatalf("unexpected template list:\n%s", listed)
		}
		shown := run(t, "template", "show", "--project=project1", "--schema", "request")
		if !strings.Contains(shown, `"name": "summary"`) || !strings.Contains(shown, `"digest": "sha256:`) {
			t.Fatalf("unexpected template schema:\n%s", shown)
		}
		rendered := run(t, "template", "render", "--project=project1", "--field", "summary=CLI task", "--field", "body=Created from CLI", "--field", "enabled=true", "request")
		if !strings.Contains(rendered, "# CLI task") || !strings.Contains(rendered, "Enabled: true") {
			t.Fatalf("unexpected rendered template:\n%s", rendered)
		}
		preview := run(t, "task", "create", "--project=project1", "--template=request", "--field", "summary=CLI task", "--field", "body=Created from CLI", "--dry-run")
		if !strings.Contains(preview, `"title": "CLI task"`) || strings.Contains(preview, `"selfDriving"`) {
			t.Fatalf("unexpected dry-run preview:\n%s", preview)
		}
		if matches, _ := filepath.Glob(filepath.Join(root, "project1", "task*")); len(matches) != 0 {
			t.Fatalf("dry-run created task directories: %#v", matches)
		}
		created := run(t, "task", "create", "--project=project1", "--template=request", "--field", "summary=CLI task", "--field", "body=Created from CLI")
		if !strings.Contains(created, `"template"`) || strings.Contains(created, `"selfDriving"`) {
			t.Fatalf("content template exposed removed execution metadata: %s", created)
		}
		var createdTask app.Task
		if err := json.Unmarshal([]byte(created), &createdTask); err != nil {
			t.Fatal(err)
		}
		var detail app.ResourceDetailView
		if err := json.Unmarshal([]byte(run(t, "workspace", "resource", "--id=project1.task1", "--json")), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Template == nil || createdTask.Template == nil || detail.Template.Digest != createdTask.Template.Digest {
			t.Fatalf("workspace resource omitted template source: %#v", detail)
		}
		if _, err := runErr(t, "task", "create", "Bad", "--project=project1", "--template=request", "--detail=conflict"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected mutually exclusive template inputs, got %v", err)
		}
	})
}

func TestTemplateValidateCLI(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Migration project")
		dir := filepath.Join(root, "project1", "templates")
		legacy := "---\ntitle: Legacy\n---\n# Legacy\n"
		if err := os.WriteFile(filepath.Join(dir, "legacy.md"), []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runErr(t, "template", "validate", "--project=project1", "--all"); err == nil {
			t.Fatal("template without schema-version should be invalid")
		}
		if _, err := runErr(t, "template", "migrate", "--project=project1", "legacy"); err == nil || !strings.Contains(err.Error(), "unknown template subcommand") {
			t.Fatalf("expected template migrate to be removed, got %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\nschema-version: 2\ntitle: Broken\nautorun: true\nfields: []\n---\nBody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		output, err := runErr(t, "template", "validate", "--project=project1", "--all", "--json")
		if err == nil || !strings.Contains(output, "unknown_property") {
			t.Fatalf("expected invalid template and non-zero exit: err=%v output=%s", err, output)
		}
	})
}

func TestTemplateShowIncludesBodyAndPreservesOutputModes(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Template project")
		body := "# {{ summary }}\n\nExecution rule: inspect every configured input.\n" + strings.Repeat("Long rule text must remain visible.\n", 128) + "\nAcceptance: preserve every line.\n"
		source := "---\nschema-version: 2\ntitle: Request\ndescription: Capture a concrete change.\ntask-title: \"{{ summary }}\"\nfields:\n  - name: summary\n    type: text\n    label: Summary\n    description: The short task summary.\n    placeholder: e.g. fix template output\n    required: true\n  - name: priority\n    type: select\n    label: Priority\n    default: medium\n    options: [low, medium, high]\n---\n" + body
		path := filepath.Join(root, "project1", "templates", "request.md")
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}

		shown := run(t, "template", "show", "--project=project1", "request")
		for _, marker := range []string{
			"Name: request",
			"Description: Capture a concrete change.",
			"Fields:\n",
			"  - summary (text, required): Summary",
			"    Description: The short task summary.",
			"    Placeholder: e.g. fix template output",
			"  - priority (select, optional): Priority",
			"    Default: medium",
			"    Options: low, medium, high",
			"Markdown body:\n" + body,
		} {
			if !strings.Contains(shown, marker) {
				t.Fatalf("default template show output is missing %q:\n%s", marker, shown)
			}
		}

		if raw := run(t, "template", "show", "--project=project1", "--raw", "request"); raw != source {
			t.Fatalf("--raw changed the original template source:\nwant:\n%s\ngot:\n%s", source, raw)
		}
		var structured map[string]any
		if err := json.Unmarshal([]byte(run(t, "template", "show", "--project=project1", "--json", "request")), &structured); err != nil {
			t.Fatal(err)
		}
		if structured["content"] != source || structured["body"] != body || structured["valid"] != true {
			t.Fatalf("--json lost template content: %#v", structured)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(run(t, "template", "show", "--project=project1", "--schema", "request")), &schema); err != nil {
			t.Fatal(err)
		}
		if schema["name"] != "request" || schema["schemaVersion"] != float64(2) || schema["digest"] == "" || schema["fields"] == nil {
			t.Fatalf("--schema changed schema output contract: %#v", schema)
		}
		if _, err := runErr(t, "template", "show", "--project=project1", "--json", "--schema", "request"); err == nil || !strings.Contains(err.Error(), "usage: pua template show") {
			t.Fatalf("expected mutually exclusive template show modes to return usage, got %v", err)
		}

		brokenBody := "# Broken rules\n\nThis body must remain inspectable even when the schema is invalid.\n"
		broken := "---\nschema-version: 2\ntitle: Broken\nautorun: true\nfields: []\n---\n" + brokenBody
		if err := os.WriteFile(filepath.Join(root, "project1", "templates", "broken.md"), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		invalid := run(t, "template", "show", "--project=project1", "broken")
		for _, marker := range []string{"Status: invalid", "unknown_property", "Markdown body:\n" + brokenBody} {
			if !strings.Contains(invalid, marker) {
				t.Fatalf("invalid template show output is missing %q:\n%s", marker, invalid)
			}
		}
	})

	help := strings.Join(strings.Fields(run(t, "help", "template")), " ")
	for _, marker := range []string{
		"show defaults to metadata, field requirements, diagnostics, and the complete Markdown body",
		"--raw for the original file, --json for structured template data,",
		"or --schema for schema metadata and diagnostics",
	} {
		if !strings.Contains(help, marker) {
			t.Fatalf("template show help is missing %q:\n%s", marker, help)
		}
	}
}

func withTempCwd(t *testing.T, fn func(root string)) {
	t.Helper()
	root := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatal(err)
		}
	})
	fn(root)
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runErr(t, args...)
	if err != nil {
		t.Fatalf("Run(%q) failed: %v\nstdout:\n%s", args, err, out)
	}
	return out
}

func runErr(t *testing.T, args ...string) (string, error) {
	return captureRun(t, Run, args...)
}

// runWithStdin feeds stdin to the CLI and captures both stdout and stderr.
func runWithStdin(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = stdinReader
	if _, err := io.WriteString(stdinWriter, stdin); err != nil {
		t.Fatal(err)
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		os.Stdin = oldStdin
		_ = stdinReader.Close()
	}()

	var stdoutBuf, stderrBuf bytes.Buffer
	oldStdout, oldStderr := os.Stdout, os.Stderr
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = stdoutWriter, stderrWriter
	runErr := Run(args)
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = oldStdout, oldStderr
	if _, err := io.Copy(&stdoutBuf, stdoutReader); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&stderrBuf, stderrReader); err != nil {
		t.Fatal(err)
	}
	if err := stdoutReader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrReader.Close(); err != nil {
		t.Fatal(err)
	}
	return stdoutBuf.String(), stderrBuf.String(), runErr
}

func captureRun(t *testing.T, fn func([]string) error, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	stdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	err = fn(args)
	if closeErr := writer.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	os.Stdout = stdout
	if _, copyErr := io.Copy(&buf, reader); copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return buf.String(), err
}

func assertDir(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("expected directory: %s", path)
	}
}

func assertFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatalf("expected file: %s", path)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected path to be absent: %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func buildWorkspaceTree() (app.WorkspaceTree, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return app.WorkspaceTree{}, err
	}
	workspace, err := app.OpenWorkspaceFrom(cwd)
	if err != nil {
		return app.WorkspaceTree{}, err
	}
	return workspace.Tree()
}

func writeTestResourceJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func assertNoHan(t *testing.T, path string) {
	t.Helper()
	content := readFile(t, path)
	for _, r := range content {
		if unicode.Is(unicode.Han, r) {
			t.Fatalf("expected %s to contain no Chinese characters, got:\n%s", path, content)
		}
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func writeStaleManagedBlock(t *testing.T, path, old, replacement string) {
	t.Helper()
	content := readFile(t, path)
	stale := strings.Replace(content, old, replacement, 1)
	if stale == content {
		t.Fatalf("could not make %s stale; missing %q in:\n%s", path, old, content)
	}
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFakeRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFakeBareRepo(t *testing.T, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGitRepo(t *testing.T, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "init", "-b", branch)
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("# test repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", "README.md")
	runGit(t, path, "-c", "user.name=PUA Test", "-c", "user.email=pua@example.com", "commit", "-m", "initial")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}
