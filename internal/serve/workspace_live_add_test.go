package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func initializeWorkspaceWithoutInstanceID(t *testing.T, root string) *app.Workspace {
	t.Helper()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "workspace.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "instanceId")
	data, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RuntimeConfig(); err == nil {
		t.Fatal("legacy Workspace unexpectedly retained a runtime identity")
	}
	return workspace
}

func newLiveAddTestServer(t *testing.T, configPath string) *server {
	t.Helper()
	server := &server{
		config: configPath,
		locks:  newWorkspaceLockManager("127.0.0.1:4936", configPath),
	}
	server.agents = newAgentManager(server)
	t.Cleanup(func() {
		server.agents.waitBackground()
		server.locks.closeAll()
	})
	return server
}

func requirePersistedLiveAddIdentity(t *testing.T, server *server, added serveWorkspace, want string) {
	t.Helper()
	if strings.TrimSpace(added.InstanceID) == "" {
		t.Fatal("added Workspace has no instance id")
	}
	if want != "" && added.InstanceID != want {
		t.Fatalf("added Workspace instance id = %q, want %q", added.InstanceID, want)
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != added.ID || cfg.Workspaces[0].InstanceID != added.InstanceID {
		t.Fatalf("persisted live add = %#v, want instance %q", cfg.Workspaces, added.InstanceID)
	}
	runtimeID, err := workspaceInstanceID(added.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeID != added.InstanceID {
		t.Fatalf("runtime instance id = %q, persisted %q", runtimeID, added.InstanceID)
	}
}

func addLiveScheduleForTest(t *testing.T, root string, at time.Time) (*app.Workspace, app.Schedule) {
	t.Helper()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run live-added work",
		Condition:   "at the configured instant",
		Target:      "workspace",
		Trigger: &app.ScheduleTrigger{
			Type: app.ScheduleTriggerAt,
			At:   at.Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspace, schedule
}

func clearLiveAddReconcileState(manager *agentManager) {
	_ = manager.takeReconcileRequests()
	select {
	case <-manager.reconcileWake:
	default:
	}
}

func requireLiveAddSchedulerWake(t *testing.T, manager *agentManager) {
	t.Helper()
	request := manager.takeReconcileRequests()
	if request != reconcileScheduler {
		t.Fatalf("live add reconcile request = %08b, want Scheduler only", request)
	}
	select {
	case <-manager.reconcileWake:
	default:
		t.Fatal("live add did not wake the reconcile loop")
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("live add emitted duplicate reconcile wake tokens")
	default:
	}
}

func requireNoLiveAddSchedulerWake(t *testing.T, manager *agentManager) {
	t.Helper()
	if request := manager.takeReconcileRequests(); request != 0 {
		t.Fatalf("failed live add reconcile request = %08b, want none", request)
	}
	select {
	case <-manager.reconcileWake:
		t.Fatal("failed live add woke the reconcile loop")
	default:
	}
}

func configureLiveAddAgentHub(t *testing.T, server *server, endpoint string) {
	t.Helper()
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentHubEndpoint = endpoint
	cfg.AgentHubInstanceID = "pua-live-add-test"
	cfg.AgentProfiles = []agentProfileRoute{{
		Key: "default", Description: systemAgentProfileDefinitions[0].Description, AgentName: "fake-agent",
	}}
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestLiveAddRequestsSchedulerReconcileAndInstallsExactDeadline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	at := time.Now().UTC().Add(time.Hour)
	_, schedule := addLiveScheduleForTest(t, root, at)
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	now := at.Add(-30 * time.Minute)
	server.agents.now = func() time.Time { return now }

	if deadline := server.agents.nextSchedulerReconcileDeadline(now); !deadline.IsZero() {
		t.Fatalf("unconfigured Workspace contributed deadline %s", deadline)
	}
	clearLiveAddReconcileState(server.agents)
	added, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if added.ID == "" || added.InstanceID == "" {
		t.Fatalf("live-added Workspace identity = %#v", added)
	}
	requireLiveAddSchedulerWake(t, server.agents)

	if err := server.agents.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deadline := server.agents.nextSchedulerReconcileDeadline(now); !deadline.Equal(at) {
		t.Fatalf("live-added Scheduler deadline = %s, want %s", deadline, at)
	}
	snapshot, err := newNativeScheduler(server.agents, added).Snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].ID != schedule.ID || snapshot.Schedules[0].NextRunAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("live-added Scheduler snapshot = %#v", snapshot)
	}
	if messages := scheduleOccurrenceMessages(t, root, "workspace"); len(messages) != 0 {
		t.Fatalf("future live-added schedule delivered early: %#v", messages)
	}
}

func TestLiveAddReconcilesAlreadyDueScheduleWithoutDuplicateDelivery(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()

	root := filepath.Join(t.TempDir(), "workspace")
	now := time.Now().UTC().Add(2 * time.Hour)
	due := now.Add(-time.Minute)
	workspace, schedule := addLiveScheduleForTest(t, root, now.Add(time.Hour))
	rewriteSchedulerTestOneTimeDeadline(t, workspace, schedule.ID, due)
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	server.agents.now = func() time.Time { return now }
	configureLiveAddAgentHub(t, server, hub.URL)

	clearLiveAddReconcileState(server.agents)
	added, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	requireLiveAddSchedulerWake(t, server.agents)
	if err := server.agents.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := scheduleOccurrenceMessages(t, root, "workspace")
	if len(first) != 1 || first[0].Causation == nil || first[0].Causation.ScheduleID != schedule.ID || first[0].Causation.ScheduledFor != due.Format(time.RFC3339Nano) {
		t.Fatalf("due live-added occurrence = %#v", first)
	}
	if deadline := server.agents.nextSchedulerReconcileDeadline(now); !deadline.IsZero() {
		t.Fatalf("completed live-added schedule retained deadline %s", deadline)
	}

	clearLiveAddReconcileState(server.agents)
	again, err := server.addWorkspace(context.Background(), filepath.Join(root, "."))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != added.ID || again.InstanceID != added.InstanceID {
		t.Fatalf("no-op live add changed Workspace identity: %#v, want %#v", again, added)
	}
	requireLiveAddSchedulerWake(t, server.agents)
	if err := server.agents.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := scheduleOccurrenceMessages(t, root, "workspace")
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("repeated live add duplicated due occurrence: before=%#v after=%#v", first, second)
	}
	if inputs := schedulerAgentHubInputCount(fake); inputs != 1 {
		t.Fatalf("repeated live add AgentHub input count = %d, want 1", inputs)
	}
}

func TestLiveAddSchedulerWakeSurvivesCancellationAfterSave(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	at := time.Now().UTC().Add(time.Hour)
	addLiveScheduleForTest(t, root, at)
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	now := at.Add(-30 * time.Minute)
	server.agents.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clearLiveAddReconcileState(server.agents)
	added, err := server.addWorkspaceWithOptionsAndSave(ctx, root, false, "", "", func(cfg config) error {
		if err := server.saveConfigLocked(cfg); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("post-save context state = %v, want cancelled", ctx.Err())
	}
	requireLiveAddSchedulerWake(t, server.agents)
	if err := server.agents.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deadline := server.agents.nextSchedulerReconcileDeadline(now); !deadline.Equal(at) {
		t.Fatalf("cancelled caller Scheduler deadline = %s, want %s for %#v", deadline, at, added)
	}
}

func TestLiveReAddAfterOwnershipHandoffRefreshesSchedulerDeadline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	at := time.Now().UTC().Add(time.Hour)
	addLiveScheduleForTest(t, root, at)
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	now := at.Add(-30 * time.Minute)
	server.agents.now = func() time.Time { return now }

	added, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	requireLiveAddSchedulerWake(t, server.agents)
	if err := server.agents.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.removeWorkspace(added.ID); err != nil {
		t.Fatal(err)
	}
	if server.locks.owns(root) {
		t.Fatal("ownership handoff retained the Workspace lock")
	}
	clearLiveAddReconcileState(server.agents)

	readded, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if readded.InstanceID != added.InstanceID {
		t.Fatalf("ownership handoff changed Workspace instance: %#v, want %#v", readded, added)
	}
	requireLiveAddSchedulerWake(t, server.agents)
	if err := server.agents.reconcileOwnedWorkspaceSchedulers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deadline := server.agents.nextSchedulerReconcileDeadline(now); !deadline.Equal(at) {
		t.Fatalf("re-added Workspace Scheduler deadline = %s, want %s", deadline, at)
	}
	if err := server.agents.withResourceController(context.Background(), readded, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatalf("revived Scheduler controller rejected new ownership: %v", err)
	}
}

func TestLiveAddFailuresDoNotWakeScheduler(t *testing.T) {
	t.Run("ownership lock", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		addLiveScheduleForTest(t, root, time.Now().UTC().Add(time.Hour))
		external := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "owner.json"))
		defer external.closeAll()
		if _, err := external.acquire(root); err != nil {
			t.Fatal(err)
		}
		server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
		clearLiveAddReconcileState(server.agents)
		if _, err := server.addWorkspace(context.Background(), root); err == nil {
			t.Fatal("live add unexpectedly crossed an ownership conflict")
		}
		requireNoLiveAddSchedulerWake(t, server.agents)
	})

	t.Run("resource runtime", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		addLiveScheduleForTest(t, root, time.Now().UTC().Add(time.Hour))
		if err := os.WriteFile(filepath.Join(root, ".pua", "runtime"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
		clearLiveAddReconcileState(server.agents)
		if _, err := server.addWorkspace(context.Background(), root); err == nil {
			t.Fatal("live add unexpectedly accepted a broken resource runtime")
		}
		requireNoLiveAddSchedulerWake(t, server.agents)
	})
}

func TestLiveAddPersistsAuthoritativeWorkspaceInstanceID(t *testing.T) {
	t.Run("legacy Workspace", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		initializeWorkspaceWithoutInstanceID(t, root)
		server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))

		added, err := server.addWorkspace(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		requirePersistedLiveAddIdentity(t, server, added, "")
	})

	t.Run("initialized Workspace", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		workspace, err := app.Initialize(root, "en")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := workspace.RuntimeConfig()
		if err != nil {
			t.Fatal(err)
		}
		server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))

		added, err := server.addWorkspace(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		requirePersistedLiveAddIdentity(t, server, added, runtime.InstanceID)
	})
}

func TestLiveAddedWorkspaceStaleRemovalDrainsPersistedController(t *testing.T) {
	for _, test := range []struct {
		name       string
		breakState func(*testing.T, string)
	}{
		{
			name: "missing after restart",
			breakState: func(t *testing.T, root string) {
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt after restart",
			breakState: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "workspace.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "workspace")
			initializeWorkspaceWithoutInstanceID(t, root)
			configPath := filepath.Join(t.TempDir(), "serve.json")
			first := newLiveAddTestServer(t, configPath)
			added, err := first.addWorkspace(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			requirePersistedLiveAddIdentity(t, first, added, "")
			first.locks.closeAll()

			restarted := newLiveAddTestServer(t, configPath)
			if err := restarted.acquireConfiguredWorkspaceLocks(); err != nil {
				t.Fatal(err)
			}
			if err := restarted.ensureConfiguredResourceRuntimes(); err != nil {
				t.Fatal(err)
			}
			persisted, err := restarted.loadConfig()
			if err != nil || len(persisted.Workspaces) != 1 || persisted.Workspaces[0].InstanceID != added.InstanceID {
				t.Fatalf("restarted live add = %#v, %v", persisted.Workspaces, err)
			}

			controller, release := holdSchedulerController(t, restarted.agents, added)
			test.breakState(t, root)
			removeDone := make(chan error, 1)
			go func() { removeDone <- restarted.removeWorkspace(added.ID) }()
			waitForSchedulerControllerQueue(t, controller, 1)

			queuedDone := make(chan error, 1)
			go func() {
				queuedDone <- restarted.agents.withResourceControllerInstanceID(context.Background(), added.InstanceID, app.SchedulerResourceID, func() error {
					return restarted.requireWorkspaceOwnership(root)
				})
			}()
			waitForSchedulerControllerQueue(t, controller, 2)
			release()
			if err := <-removeDone; err != nil {
				t.Fatalf("remove stale live-added Workspace: %v", err)
			}
			if err := <-queuedDone; err == nil || !strings.Contains(err.Error(), "not owned") {
				t.Fatalf("queued Scheduler work crossed ownership handoff: %v", err)
			}
			requireWorkspaceRemoved(t, restarted, added)

			newOwner := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "new-owner.json"))
			defer newOwner.closeAll()
			if _, err := newOwner.acquire(root); err != nil {
				t.Fatalf("new owner could not acquire after drained removal: %v", err)
			}
		})
	}
}

func TestLiveAddSaveFailureLeavesIdentityRecoverableAndUnmanaged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	workspace := initializeWorkspaceWithoutInstanceID(t, root)
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	wantErr := errors.New("injected live add save failure")
	var candidate config

	clearLiveAddReconcileState(server.agents)
	_, err := server.addWorkspaceWithOptionsAndSave(context.Background(), root, false, "", "", func(cfg config) error {
		candidate = cfg
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("live add save failure = %v, want %v", err, wantErr)
	}
	if len(candidate.Workspaces) != 1 || strings.TrimSpace(candidate.Workspaces[0].InstanceID) == "" {
		t.Fatalf("failed save candidate has no runtime identity: %#v", candidate.Workspaces)
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 0 {
		t.Fatalf("failed save created managed entry: %#v", cfg.Workspaces)
	}
	if server.locks.owns(root) {
		t.Fatal("failed live add retained the Workspace ownership lock")
	}
	requireNoLiveAddSchedulerWake(t, server.agents)
	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.InstanceID != candidate.Workspaces[0].InstanceID {
		t.Fatalf("recoverable runtime id = %q, candidate %q", runtime.InstanceID, candidate.Workspaces[0].InstanceID)
	}

	probe := newWorkspaceLockManager("", "")
	if _, err := probe.acquire(root); err != nil {
		t.Fatalf("failed live add lock was not recoverable: %v", err)
	}
	probe.release(root)
	probe.closeAll()

	added, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("retry live add: %v", err)
	}
	requireLiveAddSchedulerWake(t, server.agents)
	requirePersistedLiveAddIdentity(t, server, added, runtime.InstanceID)
}
