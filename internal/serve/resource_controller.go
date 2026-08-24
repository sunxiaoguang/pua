package serve

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// resourceController serializes operations for one stable resource address.
// Jobs are FIFO within a key, while different keys have independent workers.
// A job may perform a bounded or slow AgentHub operation; that only delays
// later jobs for the same resource.
type resourceController struct {
	mu      sync.Mutex
	jobs    []resourceControllerJob
	running bool
}

type resourceControllerJob struct {
	ctx  context.Context
	fn   func() error
	done chan error
}

// workspaceHandoffBarrier prevents a Server from releasing Workspace
// ownership while one of its resource jobs can still mutate durable state or
// contact AgentHub. New readers stop at the first waiting writer so removal
// has a bounded handoff point even when unrelated resource controllers remain
// busy.
type workspaceHandoffBarrier struct {
	mu             sync.Mutex
	changed        chan struct{}
	readers        int
	writer         bool
	writersWaiting int
	epoch          uint64
	retired        bool
}

var errWorkspaceHandoffComplete = errors.New("Workspace ownership handoff completed")

func newWorkspaceHandoffBarrier() *workspaceHandoffBarrier {
	return &workspaceHandoffBarrier{changed: make(chan struct{})}
}

func (b *workspaceHandoffBarrier) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *workspaceHandoffBarrier) ticket() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch
}

func (b *workspaceHandoffBarrier) acquireShared(ctx context.Context, ticket uint64) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	for b.writer || b.writersWaiting > 0 {
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		b.mu.Lock()
	}
	if b.retired || ticket != b.epoch {
		b.mu.Unlock()
		return nil, errWorkspaceHandoffComplete
	}
	if err := ctx.Err(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	b.readers++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.readers--
		b.signalLocked()
		b.mu.Unlock()
	}, nil
}

func (b *workspaceHandoffBarrier) retireExclusive() {
	b.mu.Lock()
	b.epoch++
	b.retired = true
	b.signalLocked()
	b.mu.Unlock()
}

func (b *workspaceHandoffBarrier) revive() {
	b.mu.Lock()
	if b.retired {
		b.epoch++
		b.retired = false
		b.signalLocked()
	}
	b.mu.Unlock()
}

func (b *workspaceHandoffBarrier) acquireExclusive(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	b.writersWaiting++
	b.signalLocked()
	for b.writer || b.readers > 0 {
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			b.mu.Lock()
			b.writersWaiting--
			b.signalLocked()
			b.mu.Unlock()
			return nil, ctx.Err()
		}
		b.mu.Lock()
	}
	if err := ctx.Err(); err != nil {
		b.writersWaiting--
		b.signalLocked()
		b.mu.Unlock()
		return nil, err
	}
	b.writersWaiting--
	b.writer = true
	b.signalLocked()
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.writer = false
		b.signalLocked()
		b.mu.Unlock()
	}, nil
}

func (m *agentManager) workspaceBarrier(workspacePath string) (*workspaceHandoffBarrier, error) {
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		return nil, err
	}
	m.workspaceBarriersMu.Lock()
	defer m.workspaceBarriersMu.Unlock()
	barrier := m.workspaceBarriers[canonical]
	if barrier == nil {
		barrier = newWorkspaceHandoffBarrier()
		m.workspaceBarriers[canonical] = barrier
	}
	return barrier, nil
}

func (m *agentManager) workspaceBarrierTicket(workspacePath string) (*workspaceHandoffBarrier, uint64, error) {
	barrier, err := m.workspaceBarrier(workspacePath)
	if err != nil {
		return nil, 0, err
	}
	return barrier, barrier.ticket(), nil
}

func (m *agentManager) withWorkspaceResourceJob(ctx context.Context, workspace serveWorkspace, barrier *workspaceHandoffBarrier, ticket uint64, fn func() error) error {
	release, err := barrier.acquireShared(ctx, ticket)
	if err != nil {
		if errors.Is(err, errWorkspaceHandoffComplete) {
			message := fmt.Sprintf("workspace %s is not owned by this pua serve instance; ownership handoff completed before the resource job started", workspace.Path)
			return &resourceAPIError{Code: "workspace_not_owned", Message: message}
		}
		return err
	}
	defer release()
	// A queued job can outlive a remove/recreate cycle at the same pathname.
	// Recheck both the named advisory-lock inode and the configured runtime
	// identity after acquiring the shared lease, immediately before user code
	// can touch the replacement Workspace.
	if _, err := m.workspaceControllerInstanceID(workspace); err != nil {
		return err
	}
	return fn()
}

func (m *agentManager) withWorkspaceHandoff(ctx context.Context, workspacePath string, fn func() error) error {
	barrier, err := m.workspaceBarrier(workspacePath)
	if err != nil {
		return err
	}
	release, err := barrier.acquireExclusive(ctx)
	if err != nil {
		return err
	}
	defer release()
	err = fn()
	if err == nil {
		barrier.retireExclusive()
	}
	return err
}

func (m *agentManager) reviveWorkspaceBarrier(workspacePath string) error {
	barrier, err := m.workspaceBarrier(workspacePath)
	if err != nil {
		return err
	}
	barrier.revive()
	return nil
}

func (c *resourceController) enqueue(ctx context.Context, fn func() error) <-chan error {
	return c.enqueueWithStart(ctx, fn, func(run func()) { go run() })
}

func (c *resourceController) enqueueWithStart(ctx context.Context, fn func() error, startWorker func(func())) <-chan error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	c.mu.Lock()
	c.jobs = append(c.jobs, resourceControllerJob{ctx: ctx, fn: fn, done: done})
	start := !c.running
	if start {
		c.running = true
	}
	c.mu.Unlock()
	if start {
		startWorker(c.run)
	}
	return done
}

func (c *resourceController) run() {
	for {
		c.mu.Lock()
		if len(c.jobs) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		job := c.jobs[0]
		c.jobs[0] = resourceControllerJob{}
		c.jobs = c.jobs[1:]
		c.mu.Unlock()

		var err error
		if job.ctx != nil && job.ctx.Err() != nil {
			err = job.ctx.Err()
		} else if job.fn != nil {
			err = job.fn()
		}
		job.done <- err
	}
}

func (c *resourceController) do(ctx context.Context, fn func() error) error {
	return c.doWithStart(ctx, fn, func(run func()) { go run() })
}

func (c *resourceController) doWithStart(ctx context.Context, fn func() error, startWorker func(func())) error {
	done := c.enqueueWithStart(ctx, fn, startWorker)
	if ctx == nil {
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type resourceControllerKey struct {
	workspaceInstanceID string
	staleWorkspacePath  string
	resourceID          string
}

func resourceControllerKeyForInstanceID(instanceID, resourceID string) (resourceControllerKey, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return resourceControllerKey{}, errors.New("Workspace runtime instance id is empty")
	}
	return resourceControllerKey{workspaceInstanceID: instanceID, resourceID: normalizedResourceID(resourceID)}, nil
}

// resourceControllerKeyForStaleWorkspacePath addresses removal of a legacy
// serve-config entry whose Workspace identity can no longer be read. The
// distinct key field is a separate namespace, not a fabricated instance ID,
// so it cannot alias a healthy Workspace controller.
func resourceControllerKeyForStaleWorkspacePath(workspacePath, resourceID string) (resourceControllerKey, error) {
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		return resourceControllerKey{}, err
	}
	return resourceControllerKey{staleWorkspacePath: canonical, resourceID: normalizedResourceID(resourceID)}, nil
}

func (m *agentManager) resourceControllerKey(workspace serveWorkspace, resourceID string) (resourceControllerKey, error) {
	instanceID, err := m.workspaceControllerInstanceID(workspace)
	if err != nil {
		return resourceControllerKey{}, err
	}
	return resourceControllerKeyForInstanceID(instanceID, resourceID)
}

// workspaceControllerInstanceID validates the complete ordinary-job ownership
// claim before a controller is selected. Removal-only controller primitives
// intentionally bypass it because they address a persisted instance directly
// or use the collision-free stale-path namespace.
func (m *agentManager) workspaceControllerInstanceID(workspace serveWorkspace) (string, error) {
	if m != nil && m.server != nil {
		return m.server.requireWorkspaceInstanceOwnership(workspace)
	}
	return workspaceInstanceID(workspace.Path)
}

func (m *agentManager) controllerForResourceKey(key resourceControllerKey) *resourceController {
	m.resourceControllersMu.Lock()
	defer m.resourceControllersMu.Unlock()
	controller := m.resourceControllers[key]
	if controller == nil {
		controller = &resourceController{}
		m.resourceControllers[key] = controller
	}
	return controller
}

func (m *agentManager) controllerForStaleWorkspacePath(workspacePath, resourceID string) (*resourceController, error) {
	key, err := resourceControllerKeyForStaleWorkspacePath(workspacePath, resourceID)
	if err != nil {
		return nil, err
	}
	return m.controllerForResourceKey(key), nil
}

func (m *agentManager) controllerForResourceInstanceID(instanceID, resourceID string) (*resourceController, error) {
	key, err := resourceControllerKeyForInstanceID(instanceID, resourceID)
	if err != nil {
		return nil, err
	}
	return m.controllerForResourceKey(key), nil
}

func (m *agentManager) controllerForResource(workspace serveWorkspace, resourceID string) (*resourceController, error) {
	key, err := m.resourceControllerKey(workspace, resourceID)
	if err != nil {
		return nil, err
	}
	return m.controllerForResourceKey(key), nil
}

// withResourceControllerInstanceID and withStaleWorkspacePathController are
// path/identity-only primitives for the outer removal controller. Their
// production callback must acquire the exclusive Workspace handoff barrier;
// ordinary production jobs enter through withResourceController instead.
func (m *agentManager) withResourceControllerInstanceID(ctx context.Context, instanceID, resourceID string, fn func() error) error {
	controller, err := m.controllerForResourceInstanceID(instanceID, resourceID)
	if err != nil {
		return err
	}
	return controller.doWithStart(ctx, fn, m.runBackground)
}

func (m *agentManager) withStaleWorkspacePathController(ctx context.Context, workspacePath, resourceID string, fn func() error) error {
	controller, err := m.controllerForStaleWorkspacePath(workspacePath, resourceID)
	if err != nil {
		return err
	}
	return controller.doWithStart(ctx, fn, m.runBackground)
}

func (m *agentManager) withResourceController(ctx context.Context, workspace serveWorkspace, resourceID string, fn func() error) error {
	barrier, ticket, err := m.workspaceBarrierTicket(workspace.Path)
	if err != nil {
		return err
	}
	controller, err := m.controllerForResource(workspace, resourceID)
	if err != nil {
		return err
	}
	// Track even synchronously requested workers. They can enqueue asynchronous
	// follow-up jobs before the caller returns; keeping the worker registered
	// until its FIFO is empty prevents shutdown from racing those follow-ups.
	return controller.doWithStart(ctx, func() error {
		return m.withWorkspaceResourceJob(ctx, workspace, barrier, ticket, fn)
	}, m.runBackground)
}

// withResourceControllers serializes one operation with every listed stable
// resource address. Controllers are always acquired by normalized resource ID
// so overlapping multi-resource operations cannot invert their lock order.
// Callers already running under a resource controller must not include it and
// must ensure no path holding an inner controller can acquire their outer one.
// This helper is intentionally for a job that already owns the Workspace
// shared barrier (native Scheduler delivery is the production caller), so the
// nested controllers must not try to reacquire it while removal is waiting.
func (m *agentManager) withResourceControllers(ctx context.Context, workspace serveWorkspace, resourceIDs []string, fn func() error) error {
	ids := make([]string, 0, len(resourceIDs))
	seen := make(map[string]bool, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		resourceID = normalizedResourceID(resourceID)
		if resourceID == "" || seen[resourceID] {
			continue
		}
		seen[resourceID] = true
		ids = append(ids, resourceID)
	}
	sort.Strings(ids)
	var acquire func(int) error
	acquire = func(index int) error {
		if index == len(ids) {
			if _, err := m.workspaceControllerInstanceID(workspace); err != nil {
				return err
			}
			return fn()
		}
		controller, err := m.controllerForResource(workspace, ids[index])
		if err != nil {
			return err
		}
		return controller.doWithStart(ctx, func() error {
			return acquire(index + 1)
		}, m.runBackground)
	}
	return acquire(0)
}

func (m *agentManager) enqueueResourceController(workspace serveWorkspace, resourceID string, fn func() error) error {
	barrier, ticket, err := m.workspaceBarrierTicket(workspace.Path)
	if err != nil {
		return err
	}
	controller, err := m.controllerForResource(workspace, resourceID)
	if err != nil {
		return err
	}
	// Register a newly started controller worker before it can run. Besides
	// making fire-and-forget work observable during shutdown, this ordering
	// lets a tracked worker enqueue follow-up work without racing the final
	// background wait.
	controller.enqueueWithStart(context.Background(), func() error {
		return m.withWorkspaceResourceJob(context.Background(), workspace, barrier, ticket, fn)
	}, m.runBackground)
	return nil
}

func (m *agentManager) enqueueRuntimeOperation(rt *agentRuntime, fn func()) error {
	if rt == nil {
		return errors.New("resource runtime is nil")
	}
	record := rt.snapshotGeneration()
	return m.enqueueResourceController(rt.workspace, record.ResourceID, func() error {
		fn()
		return nil
	})
}
