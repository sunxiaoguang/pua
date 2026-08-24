package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestWorkspaceLockConflictAcrossInstances(t *testing.T) {
	workspace := t.TempDir()
	first := newWorkspaceLockManager("127.0.0.1:4936", filepath.Join(t.TempDir(), "first.json"))
	second := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "second.json"))

	canonical, err := first.acquire(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(canonical, ".pua", workspaceServeLockName)); err != nil {
		t.Fatalf("expected serve lock file: %v", err)
	}

	_, err = second.acquire(workspace)
	if err == nil {
		t.Fatal("expected second instance to fail acquiring the workspace lock")
	}
	var conflict *workspaceLockConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected workspaceLockConflictError, got %v", err)
	}
	if !strings.Contains(err.Error(), canonical) {
		t.Fatalf("expected conflict error to name the canonical workspace %s, got %v", canonical, err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:4936") {
		t.Fatalf("expected conflict error to include owner diagnostics, got %v", err)
	}
	if !first.owns(workspace) {
		t.Fatal("first instance must own the workspace")
	}
	if second.owns(workspace) {
		t.Fatal("second instance must not own the workspace")
	}
}

func TestWorkspaceLockCanonicalPathAliases(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "ws-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	first := newWorkspaceLockManager("", "")
	second := newWorkspaceLockManager("", "")
	if _, err := first.acquire(link); err != nil {
		t.Fatal(err)
	}
	// A ".." spelling of the same directory must hit the same lock.
	if _, err := second.acquire(filepath.Join(base, "ws-link", "..", "ws")); err == nil {
		t.Fatal("expected aliased path to conflict with the held lock")
	}
	// Re-acquiring through another alias is a no-op for the owner and must
	// not drop or stack locks.
	if _, err := first.acquire(filepath.Join(workspace, ".")); err != nil {
		t.Fatal(err)
	}
	if !first.owns(workspace) || !first.owns(link) {
		t.Fatal("owner must recognize every canonical alias")
	}
	first.closeAll()
	if _, err := second.acquire(workspace); err != nil {
		t.Fatalf("expected takeover after release, got %v", err)
	}
}

func TestWorkspaceLockReleaseAllowsTakeover(t *testing.T) {
	workspace := t.TempDir()
	first := newWorkspaceLockManager("", "")
	if _, err := first.acquire(workspace); err != nil {
		t.Fatal(err)
	}
	first.closeAll()
	if first.owns(workspace) {
		t.Fatal("closeAll must drop ownership")
	}
	second := newWorkspaceLockManager("", "")
	if _, err := second.acquire(workspace); err != nil {
		t.Fatalf("expected second instance to acquire after release, got %v", err)
	}
	second.release(workspace)
	if second.owns(workspace) {
		t.Fatal("release must drop ownership")
	}
}

func TestWorkspaceLockRejectsReplacedNamedInode(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	first := newWorkspaceLockManager("127.0.0.1:4936", filepath.Join(t.TempDir(), "first.json"))
	second := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "second.json"))
	defer first.closeAll()
	defer second.closeAll()
	if _, err := first.acquire(workspace); err != nil {
		t.Fatal(err)
	}
	if !first.owns(workspace) {
		t.Fatal("unchanged named lock inode was not recognized")
	}

	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// The first descriptor still owns its unlinked inode, but it must not
	// prevent a new Server from locking the replacement pathname.
	if _, err := second.acquire(workspace); err != nil {
		t.Fatalf("new owner could not lock replacement Workspace: %v", err)
	}
	if first.owns(workspace) {
		t.Fatal("old owner accepted a descriptor for an unlinked lock inode")
	}
	if !first.holds(workspace) {
		t.Fatal("failed ordinary ownership check discarded the stale-removal descriptor claim")
	}
	if !second.owns(workspace) {
		t.Fatal("new owner did not retain the replacement lock")
	}
	if _, err := first.acquire(workspace); err == nil {
		t.Fatal("old manager reacquired a replacement lock owned by another Server")
	}
}

func TestAcquireConfiguredWorkspaceLocksAllOrNothing(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	// An external instance already owns wsB.
	external := newWorkspaceLockManager("", "")
	defer external.closeAll()
	if _, err := external.acquire(wsB); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("", configPath)}
	if err := s.saveConfig(config{
		Version: agentHubConfigVersion,
		Workspaces: []serveWorkspace{
			{ID: "a", Path: wsA},
			{ID: "b", Path: wsB},
		},
	}); err != nil {
		t.Fatal(err)
	}

	err := s.acquireConfiguredWorkspaceLocks()
	if err == nil {
		t.Fatal("expected startup lock acquisition to fail")
	}
	var conflict *workspaceLockConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict error, got %v", err)
	}
	// All or nothing: the lock taken for wsA during this round is released.
	probe := newWorkspaceLockManager("", "")
	defer probe.closeAll()
	if _, err := probe.acquire(wsA); err != nil {
		t.Fatalf("expected wsA lock to be released after abort, got %v", err)
	}
	if s.locks.owns(wsA) || s.locks.owns(wsB) {
		t.Fatal("failed startup must not retain any workspace lock")
	}
}

func TestAddWorkspaceAcquiresLockAndDeduplicates(t *testing.T) {
	workspace := t.TempDir()
	if _, err := app.Initialize(workspace, "en"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("127.0.0.1:4936", configPath)}

	added, err := s.addWorkspace(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !s.locks.owns(workspace) {
		t.Fatal("addWorkspace must take workspace ownership")
	}
	canonical, err := canonicalWorkspacePath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if added.Path != canonical {
		t.Fatalf("expected canonical workspace path %s, got %s", canonical, added.Path)
	}

	// Adding the same Workspace again must keep the existing lock and config.
	again, err := s.addWorkspace(context.Background(), filepath.Join(workspace, "."))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != added.ID {
		t.Fatalf("duplicate add changed workspace id: %s vs %s", again.ID, added.ID)
	}
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 1 {
		t.Fatalf("expected one workspace after duplicate add, got %d", len(cfg.Workspaces))
	}
	if !s.locks.owns(workspace) {
		t.Fatal("duplicate add must not drop the existing lock")
	}
}

func TestAddWorkspaceConflictLeavesConfigUntouched(t *testing.T) {
	workspace := t.TempDir()
	owner := newWorkspaceLockManager("127.0.0.1:4936", filepath.Join(t.TempDir(), "owner.json"))
	if _, err := owner.acquire(workspace); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("127.0.0.1:4999", configPath)}
	if _, err := s.addWorkspace(context.Background(), workspace); err == nil {
		t.Fatal("expected addWorkspace to fail on lock conflict")
	}
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 0 {
		t.Fatalf("conflict must not persist the workspace, got %d entries", len(cfg.Workspaces))
	}

	body, err := json.Marshal(map[string]string{"path": workspace})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleWorkspaces(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAddWorkspaceRollbackOnConfigSaveFailure(t *testing.T) {
	workspace := t.TempDir()
	// Point the config at an existing directory so the atomic rename fails.
	configPath := t.TempDir()
	s := &server{config: configPath, locks: newWorkspaceLockManager("", configPath)}

	if _, err := s.addWorkspace(context.Background(), workspace); err == nil {
		t.Fatal("expected addWorkspace to fail when the config cannot be saved")
	}
	if s.locks.owns(workspace) {
		t.Fatal("failed add must roll back the workspace lock")
	}
	probe := newWorkspaceLockManager("", "")
	if _, err := probe.acquire(workspace); err != nil {
		t.Fatalf("expected another instance to acquire after rollback, got %v", err)
	}
}

func TestRemoveWorkspaceReleasesLock(t *testing.T) {
	workspace := t.TempDir()
	if _, err := app.Initialize(workspace, "en"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("", configPath)}
	added, err := s.addWorkspace(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.removeWorkspace(added.ID); err != nil {
		t.Fatal(err)
	}
	if s.locks.owns(workspace) {
		t.Fatal("removeWorkspace must release the workspace lock")
	}
	probe := newWorkspaceLockManager("", "")
	if _, err := probe.acquire(workspace); err != nil {
		t.Fatalf("expected another instance to take over after removal, got %v", err)
	}
}

func TestUnownedWorkspaceRejectsManagementAndWrites(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("", configPath)}
	if err := s.saveConfig(config{
		Version:    agentHubConfigVersion,
		Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.workspace("workspace-one"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected ownership error, got %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/tasks", bytes.NewBufferString(`{"project":"project1","title":"Task"}`))
	rec := httptest.NewRecorder()
	s.createTask(rec, req, "workspace-one")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not owned") {
		t.Fatalf("expected write to be rejected, got %d: %s", rec.Code, rec.Body.String())
	}
}
