package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestConcurrentLiveAddsPreserveEveryWorkspace(t *testing.T) {
	roots := []string{
		filepath.Join(t.TempDir(), "workspace-one"),
		filepath.Join(t.TempDir(), "workspace-two"),
	}
	for _, root := range roots {
		if _, err := app.Initialize(root, "en"); err != nil {
			t.Fatal(err)
		}
	}
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	start := make(chan struct{})
	results := make(chan struct {
		workspace serveWorkspace
		err       error
	}, len(roots))
	for _, root := range roots {
		root := root
		go func() {
			<-start
			workspace, err := server.addWorkspace(context.Background(), root)
			results <- struct {
				workspace serveWorkspace
				err       error
			}{workspace: workspace, err: err}
		}()
	}
	close(start)
	want := make(map[string]bool, len(roots))
	for range roots {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		want[result.workspace.ID] = true
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != len(want) {
		t.Fatalf("concurrent adds persisted %#v, want %d Workspaces", cfg.Workspaces, len(want))
	}
	for _, workspace := range cfg.Workspaces {
		if !want[workspace.ID] {
			t.Fatalf("concurrent adds persisted unexpected Workspace %#v", workspace)
		}
		delete(want, workspace.ID)
	}
	if len(want) != 0 {
		t.Fatalf("concurrent adds lost Workspace IDs %#v", want)
	}
}

func TestConcurrentWorkspaceNameAndIconUpdatesMergeFields(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	start := make(chan struct{})
	errors := make(chan error, 2)
	go func() {
		<-start
		_, err := server.updateWorkspaceName(workspace.ID, "Concurrent name")
		errors <- err
	}()
	go func() {
		<-start
		_, err := server.updateWorkspaceIcon(workspace.ID, "research-lab")
		errors <- err
	}()
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].Name != "Concurrent name" || cfg.Workspaces[0].Icon != "research-lab" {
		t.Fatalf("concurrent field updates did not merge: %#v", cfg.Workspaces)
	}
}

func TestAgentHubSettingsCommitDoesNotResurrectRemovedWorkspace(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	agentsStarted := make(chan struct{})
	releaseAgents := make(chan struct{})
	var startedOnce sync.Once
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			writeFakeAgentHubJSON(t, w, map[string]any{
				"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "test",
			})
		case "/v1/agents":
			startedOnce.Do(func() { close(agentsStarted) })
			<-releaseAgents
			writeFakeAgentHubJSON(t, w, catalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	server, workspace := newWorkspaceRemovalFixture(t)
	settingsDone := make(chan error, 1)
	go func() {
		_, err := server.saveAgentHubSettings(context.Background(), updateAgentHubSettingsRequest{
			Endpoint: fake.URL,
			AgentProfiles: []agentHubProfileRoute{
				{Key: "default", AgentName: "gpt-5.6-sol"},
			},
		})
		settingsDone <- err
	}()
	<-agentsStarted
	if err := server.removeWorkspace(workspace.ID); err != nil {
		close(releaseAgents)
		t.Fatal(err)
	}
	close(releaseAgents)
	if err := <-settingsDone; err != nil {
		t.Fatal(err)
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 0 || cfg.ActiveID != "" {
		t.Fatalf("AgentHub settings resurrected removed Workspace: %#v", cfg)
	}
	if cfg.AgentHubEndpoint != fake.URL || configuredAgentProfileName(cfg.AgentProfiles, "default") != "gpt-5.6-sol" {
		t.Fatalf("AgentHub settings were not committed: %#v", cfg)
	}
}
