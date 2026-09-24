package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/metrics"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

const (
	DefaultCleanupInterval = 60 * time.Second
	DefaultMaxVMAge        = 2 * time.Hour
	// DefaultMaxPendingAge is how long a managed VM may sit in "creating"
	// (Orchard's wire-level "pending") before cleanup reaps it. Comfortably
	// above image pull + runner download + register (~1–3 min typical) on a
	// worker that already has the image cached. Stuck pending VMs otherwise
	// occupy a capacity slot until DefaultMaxVMAge, starving the queue. A
	// worker's first pull of a large (~50 GB) Tart image can take longer; raise
	// it with config maxPendingAge.
	DefaultMaxPendingAge = config.DefaultMaxPendingAge
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

	// pruneAfter is the heartbeat age past which an offline worker's record is
	// deleted from Orchard. Zero disables pruning.
	pruneAfter time.Duration
	// pruneSkipLogged remembers workers whose prune was skipped and why, so a
	// worker that stays skipped is logged once, not every sweep.
	pruneSkipLogged map[string]string
	prunedTotal     *metrics.CounterVec

	// onSweep is invoked with the live worker list after every successful
	// sweep. The manager uses it to report AutoSize exclusions.
	onSweep func(workers []orchard.Worker)

	// pendingMu guards pendingDeletes, which holds VMs whose delete already
	// failed once (typically because the controller was briefly unreachable).
	// The sweep retries these regardless of status, because a VM whose job has
	// finished looks "running" to Orchard and would otherwise sit on a worker's
	// only slot until maxAge.
	pendingMu      sync.Mutex
	pendingDeletes map[string]struct{}
}

func NewCleanup(orchardClient orchard.Client, capacity *Capacity, runnerRemover RunnerRemover, logger *slog.Logger) *Cleanup {
	return &Cleanup{
		orchardClient:  orchardClient,
		capacity:       capacity,
		runnerRemover:  runnerRemover,
		logger:         logger.With("component", "cleanup"),
		interval:       DefaultCleanupInterval,
		maxAge:         DefaultMaxVMAge,
		maxPendingAge:  DefaultMaxPendingAge,
		pendingDeletes: make(map[string]struct{}),
		// Pruning is opt-in at this level; the manager enables it from config,
		// whose default (config.DefaultWorkerPruneAfter) is on.
		pruneSkipLogged: make(map[string]string),
	}
}

// MarkForDeletion queues a VM for deletion on the next sweep, and on every
// sweep after that until it is gone. Callers use it when their own delete
// failed and dropping the VM on the floor would leak a worker slot.
func (c *Cleanup) MarkForDeletion(vmName string) {
	if vmName == "" {
		return
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.pendingDeletes == nil {
		c.pendingDeletes = make(map[string]struct{})
	}
	c.pendingDeletes[vmName] = struct{}{}
}

func (c *Cleanup) isPendingDelete(vmName string) bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	_, ok := c.pendingDeletes[vmName]
	return ok
}

// retainPendingDeletes drops queued names that Orchard no longer reports, so a
// VM that did get deleted (or one deleted by another path) cannot keep the
// entry alive forever.
func (c *Cleanup) retainPendingDeletes(seen map[string]struct{}) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for name := range c.pendingDeletes {
		if _, ok := seen[name]; !ok {
			delete(c.pendingDeletes, name)
		}
	}
}

func (c *Cleanup) clearPendingDelete(vmName string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	delete(c.pendingDeletes, vmName)
}

// SetStateView wires in a shared StateView so sweep reads from the same
// snapshot that scaling decisions use, and invalidates it after deletes.
func (c *Cleanup) SetStateView(s *StateView) { c.state = s }

// SetMaxAge overrides the age at which managed VMs are reaped. Non-positive
// values are ignored, preserving the DefaultMaxVMAge safety timeout. Set this
// above the longest expected job runtime so a long-but-legitimate job (e.g. a
// 2h+ nightly E2E suite) is governed by GitHub's per-job timeout instead of
// being killed mid-run by this backstop.
func (c *Cleanup) SetMaxAge(maxAge time.Duration) {
	if maxAge > 0 {
		c.maxAge = maxAge
	}
}

// SetMaxPendingAge overrides how long a managed VM may stay pending before it
// is reaped as stuck. Non-positive values are ignored, preserving
// DefaultMaxPendingAge.
func (c *Cleanup) SetMaxPendingAge(d time.Duration) {
	if d > 0 {
		c.maxPendingAge = d
	}
}

// SetWorkerPruneAfter enables deleting worker records that have not
// heartbeated for d. Zero or negative disables pruning. Callers must keep d
// above the worker staleness threshold (config.Validate enforces this), so a
// worker the bridge still counts as live is never pruned.
func (c *Cleanup) SetWorkerPruneAfter(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.pruneAfter = d
}

// SetMetrics registers the cleanup loop's metrics on reg.
func (c *Cleanup) SetMetrics(reg *metrics.Registry) {
	c.prunedTotal = reg.NewCounterVec(
		"orchard_gh_bridge_workers_pruned_total",
		"Offline Orchard worker records deleted by the cleanup loop.",
	)
}

// SetOnSweep registers a callback invoked with the live workers after each
// successful sweep.
func (c *Cleanup) SetOnSweep(fn func(workers []orchard.Worker)) {
	c.onSweep = fn
}

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
	st, err := c.listVMsAndWorkers(ctx)
	if err != nil {
		c.logger.Error("failed to list VMs for cleanup", "error", err)
		return
	}
	vms, workers := st.vms, st.live

	now := time.Now()
	managedCount := 0
	var deleted int
	var orphaned int
	seen := make(map[string]struct{}, len(vms))

	// workers is already the live set, so a VM pinned to a name that is not in
	// it is stranded on a machine that has gone away.
	liveWorkers := make(map[string]struct{}, len(workers))
	for _, w := range workers {
		liveWorkers[w.Name] = struct{}{}
	}

	for _, vm := range vms {
		if !IsManagedVM(vm.Name) {
			continue
		}
		seen[vm.Name] = struct{}{}

		if c.processVM(ctx, vm, now) {
			deleted++
			continue
		}

		// A VM stranded on a worker that is no longer live must not be counted
		// as in use. Its worker's slots are already excluded from max by
		// pushMaxCapacity, so counting the VM would shrink max and leave
		// current unchanged — Available() would drop by one for every stranded
		// VM, blocking healthy workers from taking replacements until maxAge
		// reaped it. Both sides of the accounting drop the same machine.
		if vm.Worker != "" {
			if _, ok := liveWorkers[vm.Worker]; !ok {
				orphaned++
				continue
			}
		}

		managedCount++
	}

	c.retainPendingDeletes(seen)

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

	if orphaned > 0 {
		// Not an error: the VM is reaped by the maxAge backstop, or resumes
		// counting if its worker comes back. Logged because a persistent count
		// here means a machine left the fleet without its VMs being cleaned up.
		c.logger.Info("managed VMs stranded on workers that are no longer live",
			"count", orphaned)
	}

	if deleted > 0 {
		c.logger.Info("cleanup sweep complete", "deleted", deleted, "remaining", managedCount)
	}

	// Only prune from a fresh read. A snapshot the StateView kept after a
	// failed refresh can be arbitrarily old, and judged against the clock it
	// would make every worker look offline.
	if st.fresh && c.pruneOfflineWorkers(ctx, vms, st.all, liveWorkers, st.fetchedAt) > 0 && c.state != nil {
		c.state.Invalidate()
	}

	if c.onSweep != nil {
		c.onSweep(workers)
	}
}

// pruneOfflineWorkers deletes the Orchard records of workers that have not
// heartbeated for pruneAfter as of now, the time the worker list was read.
// Returns how many were deleted.
//
// A renamed or re-imaged Mac registers as a new worker and leaves its old
// record behind, still carrying the same labels. Left alone, those records
// accumulate and confuse anyone reading `orchard list workers`; in the
// BAE-8010 incident the stale record also shared the live worker's pin label.
// A worker that is merely down re-registers itself on its next connection, so
// deleting the record loses nothing.
//
// Never deleted: a worker the bridge still counts as live, a worker with a
// zero LastSeen (the controller did not report it; fail safe, as WorkerLive
// does), a worker that still has any VM assigned to it, and a worker an
// operator has paused on purpose.
func (c *Cleanup) pruneOfflineWorkers(ctx context.Context, vms []orchard.VM, all []orchard.Worker, live map[string]struct{}, now time.Time) int {
	if c.pruneAfter <= 0 {
		return 0
	}
	hasVMs := make(map[string]int, len(vms))
	for _, vm := range vms {
		if vm.Worker != "" {
			hasVMs[vm.Worker]++
		}
	}

	pruned := 0
	stillSkipped := make(map[string]struct{})
	for _, w := range all {
		if _, ok := live[w.Name]; ok {
			continue
		}
		if w.LastSeen.IsZero() || now.Sub(w.LastSeen) <= c.pruneAfter {
			continue
		}
		offlineFor := now.Sub(w.LastSeen).Round(time.Second)

		skip := ""
		switch {
		case w.SchedulingPaused:
			skip = "scheduling paused by an operator"
		case hasVMs[w.Name] > 0:
			skip = "worker still has VMs assigned"
		}
		if skip != "" {
			stillSkipped[w.Name] = struct{}{}
			if c.pruneSkipLogged[w.Name] != skip {
				c.pruneSkipLogged[w.Name] = skip
				c.logger.Info("not pruning offline worker",
					"worker", w.Name,
					"pinLabel", PinIdentity(w),
					"offlineFor", offlineFor.String(),
					"vms", hasVMs[w.Name],
					"reason", skip)
			}
			continue
		}

		if err := c.orchardClient.DeleteWorker(ctx, w.Name); err != nil {
			c.logger.Error("failed to prune offline worker", "worker", w.Name, "error", err)
			continue
		}
		pruned++
		c.prunedTotal.Inc()
		c.logger.Info("pruned offline worker",
			"worker", w.Name,
			"pinLabel", PinIdentity(w),
			"lastSeen", w.LastSeen,
			"offlineFor", offlineFor.String(),
			"pruneAfter", c.pruneAfter.String())
	}
	for name := range c.pruneSkipLogged {
		if _, ok := stillSkipped[name]; !ok {
			delete(c.pruneSkipLogged, name)
		}
	}
	return pruned
}

// sweepState is what one sweep reads from Orchard.
type sweepState struct {
	vms []orchard.VM
	// live holds workers still heartbeating; all holds every record,
	// including offline ones.
	live, all []orchard.Worker
	// fetchedAt is when the lists were read. fresh is false when the
	// StateView could not refresh and handed back its last snapshot instead.
	fetchedAt time.Time
	fresh     bool
}

// listVMsAndWorkers reads the state for one sweep. Without a StateView there
// is no staleness threshold to apply, so every worker counts as live.
func (c *Cleanup) listVMsAndWorkers(ctx context.Context) (*sweepState, error) {
	if c.state != nil {
		snap, err := c.state.Get(ctx)
		if snap == nil {
			return nil, err
		}
		return &sweepState{
			vms:       snap.VMs,
			live:      snap.Workers,
			all:       snap.AllWorkers,
			fetchedAt: snap.FetchedAt,
			fresh:     err == nil,
		}, nil
	}
	vms, err := c.orchardClient.ListVMs(ctx)
	if err != nil {
		return nil, err
	}
	workers, err := c.orchardClient.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	return &sweepState{vms: vms, live: workers, all: workers, fetchedAt: time.Now(), fresh: true}, nil
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

	// A VM queued by MarkForDeletion is deleted whatever its status says: its
	// job is already over, so "running" here means a delete that did not land,
	// not work in progress.
	shouldDelete, reason := true, "retrying failed delete"
	if !c.isPendingDelete(vm.Name) {
		shouldDelete, reason = c.shouldDeleteByStatus(vm, now)
	}

	if !shouldDelete {
		return false
	}

	c.logger.Info("cleaning up VM", "vm", vm.Name, "reason", reason, "status", vm.Status)
	if err := c.orchardClient.DeleteVM(ctx, vm.Name); err != nil {
		c.logger.Error("failed to delete VM during cleanup", "vm", vm.Name, "error", err)
		return false
	}
	c.clearPendingDelete(vm.Name)
	c.removeRunner(ctx, vm.Name)
	if c.onVMCleaned != nil {
		c.onVMCleaned(vm.Name)
	}
	return true
}

// shouldDeleteByStatus applies the ordinary status-and-age reaping rules.
func (c *Cleanup) shouldDeleteByStatus(vm orchard.VM, now time.Time) (bool, string) {
	switch vm.Status {
	case orchard.VMStatusStopped:
		return true, "stopped"
	case orchard.VMStatusFailed:
		return true, "failed"
	case orchard.VMStatusCreating:
		// Orchard's v1 API exposes "pending" and the client maps it to
		// VMStatusCreating — so this case covers pending VMs that never get
		// scheduled onto a worker. Reap them aggressively so the queue drains.
		if !vm.CreatedAt.IsZero() && now.Sub(vm.CreatedAt) > c.maxPendingAge {
			return true, "stuck pending"
		}
	default:
		if !vm.CreatedAt.IsZero() && now.Sub(vm.CreatedAt) > c.maxAge {
			return true, "max age exceeded"
		}
	}
	return false, ""
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
		if w.SchedulingPaused {
			continue
		}
		if n, ok := w.Resources[resourceTartVMs]; ok {
			total += int(n)
		}
	}
	c.capacity.SetMax(total, workers)
}

// CapacityForLabels computes the total tart-vms capacity across workers
// whose labels are a superset of the given VM labels. Workers with
// SchedulingPaused set are excluded — Orchard will refuse to place VMs
// on them, so counting their slots would over-report capacity.
func CapacityForLabels(workers []orchard.Worker, vmLabels map[string]string) int {
	var total int
	for _, w := range workers {
		if w.SchedulingPaused {
			continue
		}
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
