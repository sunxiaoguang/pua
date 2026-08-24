package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func newWorkspaceRemovalFixture(t *testing.T) (*server, serveWorkspace) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	server := &server{config: configPath, locks: newWorkspaceLockManager("127.0.0.1:4936", configPath)}
	server.agents = newAgentManager(server)
	workspace, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(workspace.InstanceID) == "" {
		t.Fatal("added Workspace did not persist its runtime instance id")
	}
	t.Cleanup(func() {
		server.agents.waitBackground()
		server.locks.closeAll()
	})
	return server, workspace
}

func requireWorkspaceRemoved(t *testing.T, server *server, workspace serveWorkspace) {
	t.Helper()
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range cfg.Workspaces {
		if configured.ID == workspace.ID {
			t.Fatalf("Workspace %s remains configured: %#v", workspace.ID, cfg.Workspaces)
		}
	}
	if server.locks.owns(workspace.Path) {
		t.Fatalf("Workspace lock remains held for %s", workspace.Path)
	}
}

func TestRemoveWorkspaceUsesPersistedInstanceWhenRuntimeUnavailable(t *testing.T) {
	for _, test := range []struct {
		name       string
		breakState func(*testing.T, serveWorkspace)
	}{
		{
			name: "missing directory",
			breakState: func(t *testing.T, workspace serveWorkspace) {
				if err := os.RemoveAll(workspace.Path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt runtime config",
			breakState: func(t *testing.T, workspace serveWorkspace) {
				if err := os.WriteFile(filepath.Join(workspace.Path, "workspace.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "moved directory",
			breakState: func(t *testing.T, workspace serveWorkspace) {
				moved := filepath.Join(filepath.Dir(workspace.Path), "workspace-moved")
				if err := os.Rename(workspace.Path, moved); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(moved, "workspace.json")); err != nil {
					t.Fatalf("moved Workspace is not intact: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, workspace := newWorkspaceRemovalFixture(t)
			test.breakState(t, workspace)
			if err := server.removeWorkspace(workspace.ID); err != nil {
				t.Fatalf("remove stale Workspace: %v", err)
			}
			requireWorkspaceRemoved(t, server, workspace)
		})
	}
}

func TestRemoveUnavailableWorkspaceAfterOrdinaryOwnershipFails(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	if err := os.RemoveAll(workspace.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := server.requireWorkspaceInstanceOwnership(workspace); err == nil {
		t.Fatal("ordinary ownership unexpectedly accepted an unavailable Workspace")
	} else {
		requireWorkspaceOwnershipError(t, err)
	}
	if err := server.removeWorkspace(workspace.ID); err != nil {
		t.Fatalf("remove unavailable Workspace after rejected ordinary access: %v", err)
	}
	requireWorkspaceRemoved(t, server, workspace)
}

func TestRemoveLegacyWorkspaceUsesVerifiedLiveInstanceFallback(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	controller, err := server.agents.controllerForResource(workspace, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Workspaces {
		if cfg.Workspaces[index].ID == workspace.ID {
			cfg.Workspaces[index].InstanceID = ""
		}
	}
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if err := server.removeWorkspace(workspace.ID); err != nil {
		t.Fatalf("remove legacy Workspace: %v", err)
	}
	requireWorkspaceRemoved(t, server, workspace)
	direct, err := server.agents.controllerForResourceInstanceID(workspace.InstanceID, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if direct != controller {
		t.Fatal("legacy fallback created a second Scheduler controller")
	}
}

func TestRemoveLegacyWorkspaceRechecksLiveInstanceInsideController(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Workspaces {
		if cfg.Workspaces[index].ID == workspace.ID {
			cfg.Workspaces[index].InstanceID = ""
		}
	}
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	controller, release := holdSchedulerController(t, server.agents, workspace)
	removeDone := make(chan error, 1)
	go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)
	if err := os.WriteFile(filepath.Join(workspace.Path, "workspace.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-removeDone; err == nil || !strings.Contains(err.Error(), "verify legacy Workspace") {
		t.Fatalf("legacy removal did not recheck its live instance: %v", err)
	}
	cfg, err = server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != workspace.ID {
		t.Fatalf("failed legacy removal changed configuration: %#v", cfg.Workspaces)
	}
	if !server.locks.owns(workspace.Path) {
		t.Fatal("failed legacy removal released Workspace ownership")
	}
}

func TestRemoveWorkspaceDrainsQueuedSchedulerJobsBeforeLockHandoff(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	controller, release := holdSchedulerController(t, server.agents, workspace)
	removeDone := make(chan error, 1)
	go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)

	description := "must not cross removal"
	condition := "once in the future"
	target := "workspace"
	trigger := app.ScheduleTrigger{
		Type: app.ScheduleTriggerAt,
		At:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}
	staleDone := make(chan error, 1)
	go func() {
		staleDone <- server.agents.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error {
			_, err := newNativeScheduler(server.agents, workspace).Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeCreate, Description: &description, Condition: &condition,
				Target: &target, Trigger: &trigger,
			})
			return err
		})
	}()
	waitForSchedulerControllerQueue(t, controller, 2)
	if err := os.RemoveAll(workspace.Path); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-removeDone; err != nil {
		t.Fatalf("remove missing Workspace: %v", err)
	}
	if err := <-staleDone; err == nil || !strings.Contains(err.Error(), "not owned by this pua serve instance") {
		t.Fatalf("queued Scheduler job crossed ownership handoff: %v", err)
	}
	requireWorkspaceRemoved(t, server, workspace)
}

func TestHealthyWorkspaceRemovalUsesExistingSchedulerController(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	controller, release := holdSchedulerController(t, server.agents, workspace)
	removeDone := make(chan error, 1)
	go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)

	server.agents.resourceControllersMu.Lock()
	controllerCount := len(server.agents.resourceControllers)
	server.agents.resourceControllersMu.Unlock()
	if controllerCount != 1 {
		t.Fatalf("healthy removal addressed %d resource controllers, want 1", controllerCount)
	}
	release()
	if err := <-removeDone; err != nil {
		t.Fatalf("remove healthy Workspace: %v", err)
	}
	requireWorkspaceRemoved(t, server, workspace)
}
