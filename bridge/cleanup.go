package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

const (
	DefaultCleanupInterval = 60 * time.Second
	DefaultMaxVMAge        = 2 * time.Hour
	// DefaultMaxPendingAge is how long a managed VM may sit in "creating"
	// (Orchard's wire-level "pending") before cleanup reaps it. Comfortably
	// above image pull + runner download + register (~1–3 min typical). Stuck
	// pending VMs otherwise occupy a capacity slot until DefaultMaxVMAge,
	// starving the queue.
	DefaultMaxPendingAge = 10 * time.Minute
)

// RunnerRemover deregisters GitHub Actions runner registrations.
type RunnerRemover interface {
	RemoveRunnerByName(ctx context.Context, name string) error
}

// Cleanup periodically reaps stale VMs from Orchard that are managed by the bridge.
type Cleanup struct {
	orchardClient orchard.Client
	capacity      *Capacity
	state         *StateView
	runnerRemover RunnerRemover
	onVMCleaned   func(vmName string)
	logger        *slog.Logger
	interval      time.Duration
	maxAge        time.Duration
	maxPendingAge time.Duration
}

func NewCleanup(orchardClient orchard.Client, capacity *Capacity, runnerRemover RunnerRemover, logger *slog.Logger) *Cleanup {
	return &Cleanup{
		orchardClient: orchardClient,
		capacity:      capacity,
		runnerRemover: runnerRemover,
		logger:        logger.With("component", "cleanup"),
		interval:      DefaultCleanupInterval,
		maxAge:        DefaultMaxVMAge,
		maxPendingAge: DefaultMaxPendingAge,
	}
}

// SetStateView wires in a shared StateView so sweep reads from the same
// snapshot that scaling decisions use, and invalidates it after deletes.
func (c *Cleanup) SetStateView(s *StateView) { c.state = s }

// SetOnVMCleaned registers a callback invoked after a VM is reaped.
// Used by the manager to notify bridges so they can purge stale activeVM entries.
func (c *Cleanup) SetOnVMCleaned(fn func(vmName string)) {
	c.onVMCleaned = fn
}

// Run starts the cleanup loop. Blocks until context is cancelled.
func (c *Cleanup) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

func (c *Cleanup) sweep(ctx context.Context) {
	vms, workers, err := c.listVMsAndWorkers(ctx)
	if err != nil {
		c.logger.Error("failed to list VMs for cleanup", "error", err)
		return
	}

	now := time.Now()
	managedCount := 0
	var deleted int

	for _, vm := range vms {
		if !IsManagedVM(vm.Name) {
			continue
		}
		managedCount++

		if c.processVM(ctx, vm, now) {
			deleted++
			managedCount--
		}
	}

	if deleted > 0 && c.state != nil {
		c.state.Invalidate()
	}

	// Reconcile capacity with actual managed VM count. Does NOT clamp at max:
	// if actual > max we want Available() to go negative until the backlog
	// drains, so over-provisioning stays visible instead of silently hiding.
	c.capacity.Reconcile(managedCount)

	// Refresh max capacity from current workers, always — including the
	// total=0 case. If all workers disappear, GitHub needs to see
	// maxRunners=0 or it will keep dispatching jobs into a void.
	c.pushMaxCapacity(workers)

	if deleted > 0 {
		c.logger.Info("cleanup sweep complete", "deleted", deleted, "remaining", managedCount)
	}
}

func (c *Cleanup) listVMsAndWorkers(ctx context.Context) ([]orchard.VM, []orchard.Worker, error) {
	if c.state != nil {
		snap, err := c.state.Get(ctx)
		if snap != nil {
			return snap.VMs, snap.Workers, nil
		}
		return nil, nil, err
	}
	vms, err := c.orchardClient.ListVMs(ctx)
	if err != nil {
		return nil, nil, err
	}
	workers, err := c.orchardClient.ListWorkers(ctx)
	if err != nil {
		return nil, nil, err
	}
	return vms, workers, nil
}

// processVM evaluates one VM and, if needed, deletes it and deregisters its runner.
// Recovers from panics so a single bad record cannot kill the cleanup goroutine
// and cascade into a full pod restart (which resets bridge state and causes
// over-provisioning on the next boot).
func (c *Cleanup) processVM(ctx context.Context, vm orchard.VM, now time.Time) (deleted bool) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("panic during VM cleanup, skipping", "vm", vm.Name, "panic", r)
			deleted = false
		}
	}()

	shouldDelete := false
	reason := ""

	switch vm.Status {
	case orchard.VMStatusStopped:
		shouldDelete = true
		reason = "stopped"
	case orchard.VMStatusFailed:
		shouldDelete = true
		reason = "failed"
	case orchard.VMStatusCreating:
		// Orchard's v1 API exposes "pending" and the client maps it to
		// VMStatusCreating — so this case covers pending VMs that never get
		// scheduled onto a worker. Reap them aggressively so the queue drains.
		if !vm.CreatedAt.IsZero() && now.Sub(vm.CreatedAt) > c.maxPendingAge {
			shouldDelete = true
			reason = "stuck pending"
		}
	default:
		if !vm.CreatedAt.IsZero() && now.Sub(vm.CreatedAt) > c.maxAge {
			shouldDelete = true
			reason = "max age exceeded"
		}
	}

	if !shouldDelete {
		return false
	}

	c.logger.Info("cleaning up VM", "vm", vm.Name, "reason", reason, "status", vm.Status)
	if err := c.orchardClient.DeleteVM(ctx, vm.Name); err != nil {
		c.logger.Error("failed to delete VM during cleanup", "vm", vm.Name, "error", err)
		return false
	}
	c.removeRunner(ctx, vm.Name)
	if c.onVMCleaned != nil {
		c.onVMCleaned(vm.Name)
	}
	return true
}

func (c *Cleanup) removeRunner(ctx context.Context, name string) {
	if c.runnerRemover == nil {
		return
	}
	if err := c.runnerRemover.RemoveRunnerByName(ctx, name); err != nil {
		c.logger.Debug("failed to deregister runner from GitHub", "runner", name, "error", err)
	}
}

const resourceTartVMs = "org.cirruslabs.tart-vms"

func (c *Cleanup) pushMaxCapacity(workers []orchard.Worker) {
	var total int
	for _, w := range workers {
		if n, ok := w.Resources[resourceTartVMs]; ok {
			total += int(n)
		}
	}
	c.capacity.SetMax(total, workers)
}

// CapacityForLabels computes the total tart-vms capacity across workers
// whose labels are a superset of the given VM labels.
func CapacityForLabels(workers []orchard.Worker, vmLabels map[string]string) int {
	var total int
	for _, w := range workers {
		if !workerMatchesLabels(w, vmLabels) {
			continue
		}
		if n, ok := w.Resources[resourceTartVMs]; ok {
			total += int(n)
		}
	}
	return total
}

func workerMatchesLabels(w orchard.Worker, vmLabels map[string]string) bool {
	for k, v := range vmLabels {
		if w.Labels[k] != v {
			return false
		}
	}
	return true
}
