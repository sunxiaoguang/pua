package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func newLegacyConfiguredServer(t *testing.T, workspaces ...serveWorkspace) *server {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "serve.json")
	server := &server{config: configPath, locks: newWorkspaceLockManager("127.0.0.1:4936", configPath)}
	server.agents = newAgentManager(server)
	if err := server.saveConfig(config{
		Version: agentHubConfigVersion, Workspaces: workspaces,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.agents.waitBackground()
		server.locks.closeAll()
	})
	return server
}

func initializedLegacyWorkspace(t *testing.T, path string) (serveWorkspace, string) {
	t.Helper()
	workspace, err := app.Initialize(path, "en")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	return serveWorkspace{ID: workspaceID(path), Name: workspaceName(path), Path: path}, runtime.InstanceID
}

func acquireAndBackfillLegacyConfig(t *testing.T, server *server) {
	t.Helper()
	if err := server.acquireConfiguredWorkspaceLocks(); err != nil {
		t.Fatal(err)
	}
	if err := server.backfillConfiguredWorkspaceInstanceIDs(); err != nil {
		t.Fatal(err)
	}
	if err := server.ensureConfiguredResourceRuntimes(); err != nil {
		t.Fatal(err)
	}
}

func holdStaleWorkspaceRemovalController(t *testing.T, manager *agentManager, path string) (*resourceController, func()) {
	t.Helper()
	controller, err := manager.controllerForStaleWorkspacePath(path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	controller.enqueueWithStart(context.Background(), func() error {
		close(started)
		<-release
		return nil
	}, manager.runBackground)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stale Workspace removal controller blocker did not start")
	}
	closeBlocker := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeBlocker)
	return controller, closeBlocker
}

func TestLegacyWorkspaceInstanceBackfillSurvivesRestartAndStaleRemoval(t *testing.T) {
	for _, test := range []struct {
		name       string
		breakState func(*testing.T, serveWorkspace)
	}{
		{
			name: "missing",
			breakState: func(t *testing.T, workspace serveWorkspace) {
				if err := os.RemoveAll(workspace.Path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt",
			breakState: func(t *testing.T, workspace serveWorkspace) {
				if err := os.WriteFile(filepath.Join(workspace.Path, "workspace.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "moved",
			breakState: func(t *testing.T, workspace serveWorkspace) {
				moved := filepath.Join(filepath.Dir(workspace.Path), "moved")
				if err := os.Rename(workspace.Path, moved); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "workspace")
			workspace, instanceID := initializedLegacyWorkspace(t, root)
			first := newLegacyConfiguredServer(t, workspace)
			acquireAndBackfillLegacyConfig(t, first)
			persisted, err := first.loadConfig()
			if err != nil || len(persisted.Workspaces) != 1 || persisted.Workspaces[0].InstanceID != instanceID {
				t.Fatalf("persisted backfill = %#v, %v", persisted.Workspaces, err)
			}

			first.locks.closeAll()
			test.breakState(t, workspace)
			restarted := &server{config: first.config, locks: newWorkspaceLockManager("127.0.0.1:4999", first.config)}
			restarted.agents = newAgentManager(restarted)
			t.Cleanup(func() {
				restarted.agents.waitBackground()
				restarted.locks.closeAll()
			})
			if err := restarted.acquireConfiguredWorkspaceLocks(); err != nil {
				t.Fatal(err)
			}
			if err := restarted.removeWorkspace(workspace.ID); err != nil {
				t.Fatalf("remove stale Workspace after restart: %v", err)
			}
			requireWorkspaceRemoved(t, restarted, serveWorkspace{
				ID: workspace.ID, InstanceID: instanceID, Path: workspace.Path,
			})
		})
	}
}

func TestLegacyWorkspaceAlreadyStaleAtUpgradeCanBeRemoved(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "missing"},
		{
			name: "corrupt",
			prepare: func(t *testing.T, path string) {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "workspace.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "moved",
			prepare: func(t *testing.T, path string) {
				workspace, _ := initializedLegacyWorkspace(t, path)
				if err := os.Rename(workspace.Path, filepath.Join(filepath.Dir(path), "moved")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspace")
			if test.prepare != nil {
				test.prepare(t, path)
			}
			workspace := serveWorkspace{ID: workspaceID(path), Name: "Legacy", Path: path}
			server := newLegacyConfiguredServer(t, workspace)
			acquireAndBackfillLegacyConfig(t, server)
			cfg, err := server.loadConfig()
			if err != nil || len(cfg.Workspaces) != 1 || cfg.Workspaces[0].InstanceID != "" {
				t.Fatalf("stale config was assigned an identity: %#v, %v", cfg.Workspaces, err)
			}
			pathController, err := server.agents.controllerForStaleWorkspacePath(path, app.SchedulerResourceID)
			if err != nil {
				t.Fatal(err)
			}
			instanceController, err := server.agents.controllerForResourceInstanceID(path, app.SchedulerResourceID)
			if err != nil {
				t.Fatal(err)
			}
			if pathController == instanceController {
				t.Fatal("stale path controller aliased an instance controller")
			}
			if err := server.removeWorkspace(workspace.ID); err != nil {
				t.Fatalf("remove stale legacy Workspace: %v", err)
			}
			requireWorkspaceRemoved(t, server, workspace)
			probe := newWorkspaceLockManager("", "")
			defer probe.closeAll()
			if _, err := probe.acquire(path); err != nil {
				t.Fatalf("released stale path cannot be acquired: %v", err)
			}
		})
	}
}

func TestLegacyWorkspaceBackfillIsAtomicAndPartial(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "first")
	secondPath := filepath.Join(t.TempDir(), "second")
	stalePath := filepath.Join(t.TempDir(), "stale")
	first, firstID := initializedLegacyWorkspace(t, firstPath)
	second, secondID := initializedLegacyWorkspace(t, secondPath)
	stale := serveWorkspace{ID: workspaceID(stalePath), Name: "Stale", Path: stalePath}
	server := newLegacyConfiguredServer(t, first, stale, second)
	if err := server.acquireConfiguredWorkspaceLocks(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(server.config)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	var candidate config
	wantErr := errors.New("injected backfill save failure")
	err = server.backfillConfiguredWorkspaceInstanceIDsWithSave(func(cfg config) error {
		writes++
		candidate = cfg
		return wantErr
	})
	if !errors.Is(err, wantErr) || writes != 1 {
		t.Fatalf("backfill failure = %v, writes = %d", err, writes)
	}
	if len(candidate.Workspaces) != 3 || candidate.Workspaces[0].InstanceID != firstID ||
		candidate.Workspaces[1].InstanceID != "" || candidate.Workspaces[2].InstanceID != secondID {
		t.Fatalf("atomic backfill candidate = %#v", candidate.Workspaces)
	}
	after, err := os.ReadFile(server.config)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed backfill partially changed serve configuration")
	}
	if err := server.backfillConfiguredWorkspaceInstanceIDs(); err != nil {
		t.Fatal(err)
	}
	persisted, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Workspaces) != 3 || persisted.Workspaces[0].InstanceID != firstID ||
		persisted.Workspaces[1].InstanceID != "" || persisted.Workspaces[2].InstanceID != secondID {
		t.Fatalf("partial stale backfill = %#v", persisted.Workspaces)
	}
}

func TestStaleLegacyRemovalRevalidatesRestoredWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace")
	workspace := serveWorkspace{ID: workspaceID(path), Name: "Legacy", Path: path}
	server := newLegacyConfiguredServer(t, workspace)
	if err := server.acquireConfiguredWorkspaceLocks(); err != nil {
		t.Fatal(err)
	}
	controller, release := holdStaleWorkspaceRemovalController(t, server.agents, path)
	removeDone := make(chan error, 1)
	go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)
	if _, err := app.Initialize(path, "en"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-removeDone; err == nil || !strings.Contains(err.Error(), "became available") {
		t.Fatalf("stale removal did not revalidate restored Workspace: %v", err)
	}
	cfg, err := server.loadConfig()
	if err != nil || len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != workspace.ID {
		t.Fatalf("failed revalidation changed config: %#v, %v", cfg.Workspaces, err)
	}
	if !server.locks.owns(path) {
		t.Fatal("failed revalidation released Workspace lock")
	}
}

func TestBackfilledLegacyRemovalRejectsReplacementAtSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace")
	workspace, instanceID := initializedLegacyWorkspace(t, path)
	server := newLegacyConfiguredServer(t, workspace)
	acquireAndBackfillLegacyConfig(t, server)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	_, replacementID := initializedLegacyWorkspace(t, path)
	if replacementID == instanceID {
		t.Fatal("replacement Workspace reused the old instance id")
	}
	if err := server.removeWorkspace(workspace.ID); err == nil || !strings.Contains(err.Error(), "live instance changed") {
		t.Fatalf("removal did not reject replacement Workspace: %v", err)
	}
	cfg, err := server.loadConfig()
	if err != nil || len(cfg.Workspaces) != 1 || cfg.Workspaces[0].InstanceID != instanceID {
		t.Fatalf("replacement rejection changed config: %#v, %v", cfg.Workspaces, err)
	}
	if server.locks.owns(path) {
		t.Fatal("replacement rejection treated the unlinked old lock inode as current ownership")
	}
}

func TestBackfilledLegacyRemovalDrainsSchedulerBeforeHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace")
	workspace, instanceID := initializedLegacyWorkspace(t, path)
	server := newLegacyConfiguredServer(t, workspace)
	acquireAndBackfillLegacyConfig(t, server)
	controller, release := holdSchedulerController(t, server.agents, serveWorkspace{
		ID: workspace.ID, InstanceID: instanceID, Path: path,
	})
	removeDone := make(chan error, 1)
	go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, controller, 1)

	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- server.agents.withResourceControllerInstanceID(context.Background(), instanceID, app.SchedulerResourceID, func() error {
			return server.requireWorkspaceOwnership(path)
		})
	}()
	waitForSchedulerControllerQueue(t, controller, 2)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-removeDone; err != nil {
		t.Fatalf("remove backfilled Workspace: %v", err)
	}
	if err := <-queuedDone; err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("queued Scheduler work crossed handoff: %v", err)
	}
	requireWorkspaceRemoved(t, server, workspace)

	second := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "second.json"))
	defer second.closeAll()
	if _, err := second.acquire(path); err != nil {
		t.Fatalf("new owner did not acquire after drained removal: %v", err)
	}
}
