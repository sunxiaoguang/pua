package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func configureMailboxHandoffAgentHub(t *testing.T, server *server, endpoint string) {
	t.Helper()
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentHubEndpoint = endpoint
	cfg.AgentHubInstanceID = "pua-mailbox-handoff-test"
	cfg.AgentProfiles = []agentProfileRoute{{Key: "default", AgentName: "fake-agent"}}
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func requireWorkspaceOwnershipError(t *testing.T, err error) {
	t.Helper()
	var apiErr *resourceAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "workspace_not_owned" {
		t.Fatalf("mailbox reconcile ownership error = %v", err)
	}
}

func waitForWorkspaceHandoffWriter(t *testing.T, barrier *workspaceHandoffBarrier) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		barrier.mu.Lock()
		waiting := barrier.writersWaiting > 0 || barrier.writer
		barrier.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Workspace handoff writer did not reach the barrier")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResourceMailboxReconcileAllowsIsolatedManagerWithoutServer(t *testing.T) {
	workspacePath := t.TempDir()
	if _, err := app.Initialize(workspacePath, "en"); err != nil {
		t.Fatal(err)
	}
	manager := newAgentManager(nil)
	workspace := serveWorkspace{ID: "isolated", Path: workspacePath}
	if err := manager.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace"); err != nil {
		t.Fatalf("isolated storage-only mailbox reconcile: %v", err)
	}
}

func TestWorkspaceHandoffEpochRejectsOldJobsAfterRevive(t *testing.T) {
	barrier := newWorkspaceHandoffBarrier()
	oldTicket := barrier.ticket()
	releaseExclusive, err := barrier.acquireExclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	barrier.retireExclusive()
	releaseExclusive()
	barrier.revive()
	if release, err := barrier.acquireShared(context.Background(), oldTicket); !errors.Is(err, errWorkspaceHandoffComplete) {
		if release != nil {
			release()
		}
		t.Fatalf("old handoff ticket = %v, want completed handoff", err)
	}
	currentTicket := barrier.ticket()
	release, err := barrier.acquireShared(context.Background(), currentTicket)
	if err != nil {
		t.Fatalf("revived handoff ticket: %v", err)
	}
	release()
}

func TestOrdinaryResourceJobRejectsReplacementWorkspaceAndStaleLock(t *testing.T) {
	first, second, workspace, _ := newSchedulerOwnershipHandoffFixture(t)
	var healthyRan atomic.Bool
	if err := first.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
		healthyRan.Store(true)
		return nil
	}); err != nil {
		t.Fatalf("healthy ordinary resource job: %v", err)
	}
	if !healthyRan.Load() {
		t.Fatal("healthy ordinary resource job did not run")
	}

	controller, err := first.agents.controllerForResource(workspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	controller.enqueueWithStart(context.Background(), func() error {
		close(blockerStarted)
		<-releaseBlocker
		return nil
	}, first.agents.runBackground)
	select {
	case <-blockerStarted:
	case <-time.After(time.Second):
		t.Fatal("ordinary controller blocker did not start")
	}

	var staleCallbackRan atomic.Bool
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- first.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
			staleCallbackRan.Store(true)
			if err := os.WriteFile(filepath.Join(workspace.Path, "stale-owner-write"), []byte("stale"), 0o600); err != nil {
				return err
			}
			if _, err := acceptMailboxMessage(workspace.Path, "workspace", resourceMessageRequest{
				Text: "stale owner mailbox write", Mode: resourceMessageModeEnqueue, Role: "user",
			}); err != nil {
				return err
			}
			description := "stale owner schedule write"
			condition := "one hour from now"
			target := "workspace"
			trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
			_, err := newNativeScheduler(nil, workspace).Change(context.Background(), NativeSchedulerChange{
				Operation: app.ScheduleChangeCreate, Description: &description, Condition: &condition,
				Target: &target, Trigger: &trigger,
			})
			return err
		})
	}()
	waitForResourceControllerQueue(t, controller, 1)

	if err := os.RemoveAll(workspace.Path); err != nil {
		t.Fatal(err)
	}
	replacementApp, err := app.Initialize(workspace.Path, "en")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := second.addWorkspace(context.Background(), workspace.Path)
	if err != nil {
		t.Fatalf("new Server add replacement Workspace: %v", err)
	}
	if replacement.InstanceID == workspace.InstanceID {
		t.Fatal("replacement Workspace reused the removed runtime identity")
	}
	mailboxBefore, err := loadResourceMailboxStoreForRead(workspace.Path, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	schedulerBefore, err := replacementApp.Scheduler()
	if err != nil {
		t.Fatal(err)
	}

	close(releaseBlocker)
	select {
	case err := <-queuedDone:
		requireWorkspaceOwnershipError(t, err)
	case <-time.After(time.Second):
		t.Fatal("queued old-owner job did not fail after Workspace replacement")
	}
	if staleCallbackRan.Load() {
		t.Fatal("queued old-owner callback ran against the replacement Workspace")
	}
	if first.ownsWorkspace(workspace.Path) {
		t.Fatal("old Server retained a lock descriptor for the removed inode")
	}
	if !second.ownsWorkspace(replacement.Path) {
		t.Fatal("new Server does not own the replacement Workspace")
	}

	var lateCallbackRan atomic.Bool
	err = first.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
		lateCallbackRan.Store(true)
		return nil
	})
	requireWorkspaceOwnershipError(t, err)
	if lateCallbackRan.Load() {
		t.Fatal("old Server allocated a fresh controller for the replacement Workspace")
	}
	if err := second.agents.withResourceController(context.Background(), replacement, "workspace", func() error { return nil }); err != nil {
		t.Fatalf("new owner ordinary resource job: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace.Path, "stale-owner-write")); !os.IsNotExist(err) {
		t.Fatalf("old owner created a filesystem marker: %v", err)
	}
	mailboxAfter, err := loadResourceMailboxStoreForRead(workspace.Path, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mailboxAfter, mailboxBefore) {
		t.Fatalf("old owner changed replacement mailbox: before=%#v after=%#v", mailboxBefore, mailboxAfter)
	}
	schedulerAfter, err := replacementApp.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schedulerAfter, schedulerBefore) {
		t.Fatalf("old owner changed replacement Scheduler: before=%#v after=%#v", schedulerBefore, schedulerAfter)
	}
}

func TestQueuedOrdinaryResourceJobRechecksConfiguredRuntimeIdentity(t *testing.T) {
	server, workspace := newWorkspaceRemovalFixture(t)
	controller, err := server.agents.controllerForResource(workspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	controller.enqueueWithStart(context.Background(), func() error {
		close(blockerStarted)
		<-releaseBlocker
		return nil
	}, server.agents.runBackground)
	select {
	case <-blockerStarted:
	case <-time.After(time.Second):
		t.Fatal("ordinary controller blocker did not start")
	}

	var callbackRan atomic.Bool
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- server.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
			callbackRan.Store(true)
			return nil
		})
	}()
	waitForResourceControllerQueue(t, controller, 1)

	configPath := filepath.Join(workspace.Path, "workspace.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var runtimeConfig app.Config
	if err := json.Unmarshal(data, &runtimeConfig); err != nil {
		t.Fatal(err)
	}
	runtimeConfig.InstanceID = "replacement-runtime-instance"
	data, err = json.MarshalIndent(runtimeConfig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !server.locks.owns(workspace.Path) {
		t.Fatal("runtime identity edit unexpectedly invalidated the named lock inode")
	}

	close(releaseBlocker)
	select {
	case err := <-queuedDone:
		requireWorkspaceOwnershipError(t, err)
	case <-time.After(time.Second):
		t.Fatal("queued job did not fail after runtime identity replacement")
	}
	if callbackRan.Load() {
		t.Fatal("queued job ran after its configured runtime identity was replaced")
	}
	if !server.locks.owns(workspace.Path) {
		t.Fatal("identity mismatch should not discard the still-current advisory lock")
	}
}

func TestWorkspaceRemovalDrainsAgentHubDeliveryBeforeOwnershipHandoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hubEntered := make(chan struct{})
	releaseHub := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sessions" {
			select {
			case <-hubEntered:
			default:
				close(hubEntered)
			}
			<-releaseHub
		}
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	first, second, workspace, _ := newSchedulerOwnershipHandoffFixture(t)
	configureMailboxHandoffAgentHub(t, first, hub.URL)
	configureMailboxHandoffAgentHub(t, second, hub.URL)

	message, err := first.agents.acceptResourceMessageDurable(context.Background(), workspace, "workspace", resourceMessageRequest{
		Text: "delivery already inside AgentHub", Mode: resourceMessageModeEnqueue, Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciledWhileOwned := make(chan error, 1)
	allowResourceJobReturn := make(chan struct{})
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- first.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
			reconcileErr := first.agents.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace")
			reconciledWhileOwned <- reconcileErr
			<-allowResourceJobReturn
			return reconcileErr
		})
	}()
	select {
	case <-hubEntered:
	case <-time.After(time.Second):
		t.Fatal("ordinary mailbox reconcile did not enter AgentHub")
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- first.removeWorkspace(workspace.ID) }()
	barrier, err := first.agents.workspaceBarrier(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkspaceHandoffWriter(t, barrier)
	select {
	case err := <-removeDone:
		t.Fatalf("Workspace removal crossed an active AgentHub delivery: %v", err)
	default:
	}

	var queuedJobRan atomic.Bool
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- first.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
			queuedJobRan.Store(true)
			return nil
		})
	}()
	cancelledContext, cancelQueued := context.WithCancel(context.Background())
	cancelledDone := make(chan error, 1)
	go func() {
		cancelledDone <- first.agents.withResourceController(cancelledContext, workspace, "workspace", func() error {
			return errors.New("cancelled handoff job unexpectedly ran")
		})
	}()
	cancelQueued()
	if err := <-cancelledDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handoff job = %v", err)
	}

	close(releaseHub)
	if err := <-reconciledWhileOwned; err != nil {
		t.Fatalf("started mailbox delivery: %v", err)
	}
	delivered, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || delivered.Status != resourceMessageDelivered {
		t.Fatalf("delivery before handoff = found %v, message %#v, error %v", found, delivered, err)
	}
	if !first.ownsWorkspace(workspace.Path) {
		t.Fatal("old Server released ownership before the started job returned")
	}
	if _, err := first.workspace(workspace.ID); err != nil {
		t.Fatalf("old Server removed config before the started job returned: %v", err)
	}
	select {
	case err := <-removeDone:
		t.Fatalf("Workspace removal completed before the resource lease drained: %v", err)
	default:
	}

	close(allowResourceJobReturn)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("join started mailbox reconcile: %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("remove Workspace after delivery: %v", err)
	}
	requireWorkspaceRemoved(t, first, workspace)
	if err := <-queuedDone; err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("job queued during handoff = %v", err)
	}
	if queuedJobRan.Load() {
		t.Fatal("job queued during handoff ran after ownership release")
	}

	shutdownDone := make(chan struct{})
	go func() {
		first.agents.waitBackground()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown wait did not drain handoff jobs")
	}

	workspace = addWorkspaceAfterSchedulerHandoff(t, second, workspace)
	if err := second.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
		return second.agents.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace")
	}); err != nil {
		t.Fatalf("new owner reconcile existing delivery: %v", err)
	}
	fake.mu.Lock()
	sessionCount := len(fake.sessions)
	if sessionCount != 1 {
		fake.mu.Unlock()
		t.Fatalf("new owner duplicated the delivered generation: sessions=%d", sessionCount)
	}
	for id, session := range fake.sessions {
		session.State = "ready"
		session.CurrentTurnID = ""
		fake.sessions[id] = session
	}
	fake.mu.Unlock()

	next, err := second.agents.acceptResourceMessageDurable(context.Background(), workspace, "workspace", resourceMessageRequest{
		Text: "new owner delivery", Mode: resourceMessageModeEnqueue, Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
		return second.agents.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace")
	}); err != nil {
		t.Fatalf("new owner mailbox delivery: %v", err)
	}
	next, found, err = mailboxMessageByID(workspace.Path, next.ID)
	if err != nil || !found || next.Status != resourceMessageDelivered {
		t.Fatalf("new owner delivery = found %v, message %#v, error %v", found, next, err)
	}
	fake.mu.Lock()
	sessions := len(fake.sessions)
	canonicalInputs := 0
	for _, events := range fake.events {
		for _, event := range events {
			if event.Type == "message.input" {
				canonicalInputs++
			}
		}
	}
	fake.mu.Unlock()
	if sessions != 1 || canonicalInputs != 2 {
		t.Fatalf("handoff AgentHub state = sessions %d, canonical inputs %d; want 1 and 2", sessions, canonicalInputs)
	}
}

func TestWorkspaceBarrierAllowsSchedulerFollowUpAndTaskControllerNesting(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Barrier project", "barrier-project")
	if err != nil {
		t.Fatal(err)
	}
	task, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Barrier task", Slug: "barrier-task"})
	if err != nil {
		t.Fatal(err)
	}
	followUp := make(chan struct{})
	if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error {
		if err := manager.withResourceControllers(context.Background(), workspace, []string{task.ID, project.ID}, func() error {
			return nil
		}); err != nil {
			return err
		}
		return manager.enqueueResourceController(workspace, app.SchedulerResourceID, func() error {
			close(followUp)
			return nil
		})
	}); err != nil {
		t.Fatalf("Scheduler nested delivery: %v", err)
	}
	select {
	case <-followUp:
	case <-time.After(time.Second):
		t.Fatal("Scheduler natural follow-up deadlocked behind its outer Workspace lease")
	}
	manager.waitBackground()
}

func TestResourceMailboxReconcileCannotCrossWorkspaceOwnershipHandoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	var hubRequests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubRequests.Add(1)
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	first, second, workspace, _ := newSchedulerOwnershipHandoffFixture(t)
	configureMailboxHandoffAgentHub(t, first, hub.URL)
	configureMailboxHandoffAgentHub(t, second, hub.URL)

	schedulerController, releaseScheduler := holdSchedulerController(t, first.agents, workspace)
	targetController, releaseTarget := holdResourceController(t, first.agents, workspace, "workspace")

	schedulerMessage, err := first.agents.acceptResourceMessageDurable(context.Background(), workspace, app.SchedulerResourceID, resourceMessageRequest{
		Text: "compile a natural-language schedule", Mode: resourceMessageModeEnqueue, Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	targetMessage, err := first.agents.acceptResourceMessageDurable(context.Background(), workspace, "workspace", resourceMessageRequest{
		Text: "ordinary target follow-up", Mode: resourceMessageModeEnqueue, Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerBefore, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore, err := loadResourceMailboxStoreForRead(workspace.Path, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	generationsBefore, err := loadGenerationRecords(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- first.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, schedulerController, 1)
	first.enqueueSchedulerMailboxReconcile(workspace, schedulerMessage)
	waitForSchedulerControllerQueue(t, schedulerController, 2)

	cancelledContext, cancelQueued := context.WithCancel(context.Background())
	cancelledDone := make(chan error, 1)
	go func() {
		cancelledDone <- first.agents.withResourceController(cancelledContext, workspace, "workspace", func() error {
			return errors.New("cancelled mailbox reconcile unexpectedly ran")
		})
	}()
	waitForResourceControllerQueue(t, targetController, 1)
	cancelQueued()
	if err := <-cancelledDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queued mailbox reconcile = %v", err)
	}
	targetReconcileDone := make(chan error, 1)
	go func() {
		targetReconcileDone <- first.agents.withResourceController(context.Background(), workspace, "workspace", func() error {
			reconcileErr := first.agents.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace")
			if reconcileErr != nil {
				recordMailboxFailure(workspace.Path, targetMessage.ID, reconcileErr)
			}
			return reconcileErr
		})
	}()
	waitForResourceControllerQueue(t, targetController, 2)

	releaseScheduler()
	// Removal now drains every started Workspace resource job, not only the
	// Scheduler FIFO. Let the target blocker return so the exclusive handoff can
	// proceed; the queued target job remains behind the writer-preferred barrier.
	barrier, err := first.agents.workspaceBarrier(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkspaceHandoffWriter(t, barrier)
	releaseTarget()
	if err := <-removeDone; err != nil {
		t.Fatalf("remove Workspace: %v", err)
	}
	if err := first.agents.withResourceControllerInstanceID(context.Background(), workspace.InstanceID, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if first.ownsWorkspace(workspace.Path) {
		t.Fatal("removed Server still owns the Workspace")
	}

	workspace = addWorkspaceAfterSchedulerHandoff(t, second, workspace)
	select {
	case err := <-targetReconcileDone:
		requireWorkspaceOwnershipError(t, err)
	case <-time.After(time.Second):
		t.Fatal("stale ordinary mailbox follow-up did not finish")
	}
	if err := first.agents.withResourceControllerInstanceID(context.Background(), workspace.InstanceID, "workspace", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	schedulerAfter, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	targetAfter, err := loadResourceMailboxStoreForRead(workspace.Path, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	generationsAfter, err := loadGenerationRecords(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schedulerAfter, schedulerBefore) {
		t.Fatalf("stale Scheduler follow-up changed mailbox: before=%#v after=%#v", schedulerBefore, schedulerAfter)
	}
	if !reflect.DeepEqual(targetAfter, targetBefore) {
		t.Fatalf("stale target follow-up changed mailbox: before=%#v after=%#v", targetBefore, targetAfter)
	}
	if !reflect.DeepEqual(generationsAfter, generationsBefore) {
		t.Fatalf("stale mailbox follow-up changed generations: before=%#v after=%#v", generationsBefore, generationsAfter)
	}
	fake.mu.Lock()
	staleSessions, staleInputs := len(fake.sessions), len(fake.messageIDs)
	fake.mu.Unlock()
	if staleSessions != 0 || staleInputs != 0 {
		t.Fatalf("stale mailbox follow-up contacted AgentHub: sessions=%d inputs=%d", staleSessions, staleInputs)
	}
	if staleRequests := hubRequests.Load(); staleRequests != 0 {
		t.Fatalf("stale mailbox follow-up made %d AgentHub requests", staleRequests)
	}

	closed := make(chan struct{})
	go func() {
		first.agents.waitBackground()
		first.locks.closeAll()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stale Server close did not drain cancelled and guarded mailbox work")
	}

	for _, resourceID := range []string{app.SchedulerResourceID, "workspace"} {
		if err := second.agents.withResourceController(context.Background(), workspace, resourceID, func() error {
			return second.agents.reconcileResourceMailboxLocked(context.Background(), workspace, resourceID)
		}); err != nil {
			t.Fatalf("new owner reconcile %s: %v", resourceID, err)
		}
	}
	for _, messageID := range []string{schedulerMessage.ID, targetMessage.ID} {
		message, found, err := mailboxMessageByID(workspace.Path, messageID)
		if err != nil || !found || message.Status != resourceMessageDelivered {
			t.Fatalf("new owner delivery %s = found %v, message %#v, error %v", messageID, found, message, err)
		}
	}
	fake.mu.Lock()
	newOwnerSessions := len(fake.sessions)
	fake.mu.Unlock()
	if newOwnerSessions != 2 {
		t.Fatalf("new owner AgentHub sessions = %d, want 2", newOwnerSessions)
	}
	if newOwnerRequests := hubRequests.Load(); newOwnerRequests == 0 {
		t.Fatal("new owner delivery did not contact AgentHub")
	}
}
