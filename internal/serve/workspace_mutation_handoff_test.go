package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestWorkspaceMutationHandoffDrainsStartedWorkspaceWrite(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	mutationDone := make(chan error, 1)
	policy := app.GenerationPolicy{Enabled: false, MaxTurns: 17, MaxAccumulatedTurnMinutes: 91}
	go func() {
		mutationDone <- server.withWorkspaceMutation(context.Background(), workspace, "workspace", func(current serveWorkspace) error {
			close(started)
			<-release
			opened, err := app.OpenWorkspace(current.Path)
			if err != nil {
				return err
			}
			_, err = opened.SetGenerationPolicy(policy)
			return err
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Workspace mutation did not acquire its shared handoff lease")
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
	barrier, err := server.agents.workspaceBarrier(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkspaceHandoffWriter(t, barrier)
	select {
	case err := <-removeDone:
		t.Fatalf("Workspace removal crossed a started mutation: %v", err)
	default:
	}

	close(release)
	if err := <-mutationDone; err != nil {
		t.Fatalf("started Workspace mutation: %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("remove Workspace after mutation drained: %v", err)
	}
	requireWorkspaceRemoved(t, server, workspace)
	opened, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := opened.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.GenerationPolicy != policy {
		t.Fatalf("drained Workspace policy = %#v, want %#v", runtimeConfig.GenerationPolicy, policy)
	}
}

func TestWorkspaceMutationHandoffRejectsQueuedHTTPWrites(t *testing.T) {
	t.Run("Workspace name", func(t *testing.T) {
		server, workspace := newWorkspaceRemovalFixture(t)
		controller, release := holdResourceController(t, server.agents, workspace, "workspace")
		updateDone := make(chan error, 1)
		go func() {
			_, err := server.updateWorkspaceName(workspace.ID, "Must not cross handoff")
			updateDone <- err
		}()
		waitForResourceControllerQueue(t, controller, 1)

		removeDone := make(chan error, 1)
		go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
		barrier, err := server.agents.workspaceBarrier(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		waitForWorkspaceHandoffWriter(t, barrier)
		release()
		if err := <-removeDone; err != nil {
			t.Fatalf("remove Workspace: %v", err)
		}
		if err := <-updateDone; err == nil || !strings.Contains(err.Error(), "handoff") {
			t.Fatalf("queued Workspace name mutation = %v", err)
		}
		requireWorkspaceRemoved(t, server, workspace)
		if got := app.WorkspaceName(workspace.Path); got == "Must not cross handoff" {
			t.Fatalf("queued Workspace name wrote after ownership handoff: %q", got)
		}
	})

	t.Run("resource title", func(t *testing.T) {
		server, workspace := newWorkspaceRemovalFixture(t)
		opened, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		project, err := opened.CreateProject("Original project", "handoff-project")
		if err != nil {
			t.Fatal(err)
		}
		controller, release := holdResourceController(t, server.agents, workspace, project.ID)
		requestDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPut,
				"/api/workspaces/"+workspace.ID+"/resources/"+project.ID+"/title",
				strings.NewReader(`{"title":"Must not cross handoff"}`))
			server.handleWorkspace(recorder, request)
			requestDone <- recorder
		}()
		waitForResourceControllerQueue(t, controller, 1)

		removeDone := make(chan error, 1)
		go func() { removeDone <- server.removeWorkspace(workspace.ID) }()
		barrier, err := server.agents.workspaceBarrier(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		waitForWorkspaceHandoffWriter(t, barrier)
		release()
		if err := <-removeDone; err != nil {
			t.Fatalf("remove Workspace: %v", err)
		}
		response := <-requestDone
		if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "handoff") {
			t.Fatalf("queued resource title mutation = %d %s", response.Code, response.Body.String())
		}
		requireWorkspaceRemoved(t, server, workspace)
		reopened, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		value, err := reopened.ResourceValue(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if value.Project == nil || value.Project.Title != "Original project" {
			t.Fatalf("queued resource title wrote after ownership handoff: %#v", value.Project)
		}
	})
}
