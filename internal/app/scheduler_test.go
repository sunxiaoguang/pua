package app_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestInitializeCreatesSchedulerResource(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != 2 || config.AgentBinding.Kind != "profile" || config.AgentBinding.Name != "default" || len(config.Schedules) != 0 {
		t.Fatalf("default Scheduler configuration = %#v", config)
	}
	tree, err := workspace.Tree()
	if err != nil || tree.Scheduler.ID != app.SchedulerResourceID || tree.Scheduler.Type != "scheduler" || tree.Scheduler.Path != "scheduler" || tree.Scheduler.AgentBinding != config.AgentBinding {
		t.Fatalf("Scheduler tree resource = %#v, %v", tree.Scheduler, err)
	}
	detail, err := workspace.Resource(app.SchedulerResourceID)
	if err != nil || detail.Scheduler == nil || detail.Type != "scheduler" || detail.Path != "scheduler" || len(detail.Files) != 2 {
		t.Fatalf("Scheduler detail = %#v, %v", detail, err)
	}
	for _, name := range []string{"scheduler.json", "scheduler.md", "AGENTS.md"} {
		info, statErr := os.Stat(filepath.Join(workspace.Root(), "scheduler", name))
		if statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("Scheduler file %s = %#v, %v", name, info, statErr)
		}
	}
	agents, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "../AGENTS.md") || !strings.Contains(string(agents), "needs_compilation") || !strings.Contains(string(agents), "不得直接覆写") || !strings.Contains(string(agents), "不能作为执行 target") {
		t.Fatalf("Scheduler guidance is incomplete:\n%s", agents)
	}
	schedulerMarkdown, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.md"))
	if err != nil || !strings.Contains(string(schedulerMarkdown), "完成调度编译或澄清所需的最小上下文") {
		t.Fatalf("Scheduler context guidance is incomplete: %v\n%s", err, schedulerMarkdown)
	}
	inside, err := workspace.IsSchedulerPath(filepath.Join(workspace.Root(), "scheduler", "nested"))
	if err == nil || inside {
		t.Fatal("a nonexistent nested path unexpectedly matched")
	}
	if err := os.Mkdir(filepath.Join(workspace.Root(), "scheduler", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside, err = workspace.IsSchedulerPath(filepath.Join(workspace.Root(), "scheduler", "nested"))
	if err != nil || !inside {
		t.Fatalf("Scheduler path match = %v, %v", inside, err)
	}
}

func TestResourceGuidanceIsLocalizedAndInherited(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Guidance project", "guidance")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Guidance task", Slug: "guidance"})
	if err != nil {
		t.Fatal(err)
	}
	projectDetail, err := workspace.Resource(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskDetail, err := workspace.Resource(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(workspace.Root(), "scheduler", "AGENTS.md"),
		filepath.Join(root, projectDetail.Path, "AGENTS.md"),
		filepath.Join(root, taskDetail.Path, "AGENTS.md"),
	}
	rootAgents, err := os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(rootAgents), "## 6. Agent collaboration") || !strings.Contains(string(rootAgents), "[[project1.task2]]") {
		t.Fatalf("English Workspace guidance is incomplete: %v\n%s", err, rootAgents)
	}
	if err := workspace.Migrate("zh-CN"); err != nil {
		t.Fatal(err)
	}
	rootAgents, err = os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(rootAgents), "## 6. Agent 协作") || !strings.Contains(string(rootAgents), "[[project1.task2]]") {
		t.Fatalf("Chinese Workspace guidance is incomplete: %v\n%s", err, rootAgents)
	}
	projectAgents, err := os.ReadFile(paths[2])
	if err != nil {
		t.Fatal(err)
	}
	taskAgents, err := os.ReadFile(paths[3])
	if err != nil {
		t.Fatal(err)
	}
	schedulerAgents, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projectAgents), project.ID) || !strings.Contains(string(projectAgents), "../AGENTS.md") ||
		!strings.Contains(string(taskAgents), task.ID) || !strings.Contains(string(taskAgents), "../../AGENTS.md") ||
		!strings.Contains(string(schedulerAgents), "../AGENTS.md") {
		t.Fatal("resource prompts no longer inherit the Workspace root guidance")
	}
}

func TestScheduleLifecycleValidatesTargets(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Scheduled project", "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Target", Slug: "target"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "  Remind the target  ",
		Condition:   "  tomorrow morning  ",
		Target:      task.ID,
		Trigger:     &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Description != "Remind the target" || created.Condition != "tomorrow morning" || created.Target != task.ID || created.CreatedAt == "" || created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created schedule = %#v", created)
	}
	condition := "when the build is green"
	target := app.SchedulerResourceID
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Condition: &condition, Target: &target}); !errors.Is(err, app.ErrScheduleTargetScheduler) || err.Error() != "update schedule: "+app.ErrScheduleTargetScheduler.Error() {
		t.Fatalf("Scheduler self-target update error = %v", err)
	}
	config, err := workspace.Scheduler()
	if err != nil || len(config.Schedules) != 1 || config.Schedules[0].Revision != created.Revision || config.Schedules[0].Condition != created.Condition || config.Schedules[0].Target != created.Target {
		t.Fatalf("rejected self-target changed Scheduler configuration: %#v, %v", config, err)
	}
	target = "workspace"
	updated, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Condition: &condition, Target: &target})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Condition != condition || updated.Target != target || updated.CreatedAt != created.CreatedAt {
		t.Fatalf("updated schedule = %#v", updated)
	}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Self target", Condition: "later", Target: app.SchedulerResourceID,
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	}); !errors.Is(err, app.ErrScheduleTargetScheduler) || err.Error() != "add schedule: "+app.ErrScheduleTargetScheduler.Error() {
		t.Fatalf("Scheduler self-target create error = %v", err)
	}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Bad", Condition: "now", Target: "project999.task999",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	}); err == nil {
		t.Fatal("missing cross-resource target unexpectedly accepted")
	}
	if _, err := workspace.ArchiveResource(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Archived", Condition: "now", Target: task.ID,
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	}); err == nil {
		t.Fatal("archived target unexpectedly accepted")
	}
	removed, err := workspace.RemoveSchedule(created.ID)
	if err != nil || removed.ID != created.ID {
		t.Fatalf("removed schedule = %#v, %v", removed, err)
	}
	config, err = workspace.Scheduler()
	if err != nil || len(config.Schedules) != 0 {
		t.Fatalf("Scheduler after removal = %#v, %v", config, err)
	}
}

func TestSchedulerResourceBindingAndConcurrentScheduleWrites(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	binding := app.AgentBinding{Kind: "profile", Name: "fast"}
	if got, err := workspace.SetResourceAgentBinding(app.SchedulerResourceID, binding); err != nil || got != binding {
		t.Fatalf("set Scheduler resource binding = %#v, %v", got, err)
	}
	config, err := workspace.Scheduler()
	if err != nil || config.AgentBinding != binding {
		t.Fatalf("Scheduler resource binding = %#v, %v", config, err)
	}
	const count = 16
	var wait sync.WaitGroup
	errors := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, addErr := workspace.AddSchedule(app.CreateScheduleInput{
				Description: fmt.Sprintf("Concurrent schedule %d", index),
				Condition:   "when appropriate",
				Target:      "workspace",
				Trigger:     &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
			})
			if addErr != nil {
				errors <- addErr
			}
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	config, err = workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != count {
		t.Fatalf("got %d schedules, want %d", len(config.Schedules), count)
	}
	seen := make(map[string]bool, count)
	for _, schedule := range config.Schedules {
		if seen[schedule.ID] {
			t.Fatalf("duplicate schedule id %q", schedule.ID)
		}
		seen[schedule.ID] = true
	}
	data, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("scheduler.json is not formatted valid JSON:\n%s", data)
	}
}

func TestMigratePreservesSchedulerContentAndRejectsUnsafeConflicts(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Keep me", Condition: "next week", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	markdownPath := filepath.Join(workspace.Root(), "scheduler", "scheduler.md")
	if err := os.WriteFile(markdownPath, []byte("# Durable scheduler notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(workspace.Root(), "scheduler", "AGENTS.md")
	agents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsPath, append([]byte("Manual preface\n\n"), agents...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate("zh-CN"); err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil || len(config.Schedules) != 1 || config.Schedules[0].ID != created.ID {
		t.Fatalf("migrated schedules = %#v, %v", config.Schedules, err)
	}
	markdown, _ := os.ReadFile(markdownPath)
	agents, _ = os.ReadFile(agentsPath)
	if string(markdown) != "# Durable scheduler notes\n" || !strings.HasPrefix(string(agents), "Manual preface") || !strings.Contains(string(agents), "Scheduler Agent 指引") {
		t.Fatalf("migration did not preserve unmanaged content:\nmarkdown=%s\nagents=%s", markdown, agents)
	}

	unsafeRoot := t.TempDir()
	unsafeWorkspace, err := app.Initialize(unsafeRoot, "en")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(unsafeRoot, "scheduler", "scheduler.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(unsafeRoot, "workspace.json"), filepath.Join(unsafeRoot, "scheduler", "scheduler.json")); err != nil {
		t.Fatal(err)
	}
	if err := unsafeWorkspace.Migrate(""); err == nil || !app.IsKind(err, "scheduler") {
		t.Fatalf("unsafe Scheduler conflict = %v", err)
	}
}
