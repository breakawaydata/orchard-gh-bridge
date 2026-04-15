package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/actions/scaleset"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

const ManagedByLabel = "orchard-gh-bridge"

// Bridge implements the scaleset listener.Scaler interface for a single scale set.
// It provisions and deprovisions Orchard VMs in response to GitHub Actions job events.
type Bridge struct {
	scaleSetName string
	scaleSetID   int
	vmConfig     config.VMConfig

	orchardClient orchard.Client
	ghClient      *scaleset.Client
	capacity      *Capacity
	state         *StateView
	logger        *slog.Logger

	// onVMChange is invoked after a successful create or release so the manager
	// can recompute per-scale-set maxRunners reported to GitHub. May be nil.
	onVMChange func()

	mu        sync.Mutex
	activeVMs map[string]string // runnerName or vmName → vmName
}

type Config struct {
	ScaleSetName  string
	ScaleSetID    int
	VMConfig      config.VMConfig
	OrchardClient orchard.Client
	GHClient      *scaleset.Client
	Capacity      *Capacity
	State         *StateView
	Logger        *slog.Logger
}

func New(cfg Config) *Bridge {
	return &Bridge{
		scaleSetName:  cfg.ScaleSetName,
		scaleSetID:    cfg.ScaleSetID,
		vmConfig:      cfg.VMConfig,
		orchardClient: cfg.OrchardClient,
		ghClient:      cfg.GHClient,
		capacity:      cfg.Capacity,
		state:         cfg.State,
		logger:        cfg.Logger.With("scaleSet", cfg.ScaleSetName),
		activeVMs:     make(map[string]string),
	}
}

// SetOnVMChange registers a callback invoked after a VM is created or released.
func (b *Bridge) SetOnVMChange(fn func()) {
	b.onVMChange = fn
}

// ScaleSetName returns the configured scale set name.
func (b *Bridge) ScaleSetName() string { return b.scaleSetName }

// VMLabels returns the VM label selector used for worker matching.
func (b *Bridge) VMLabels() map[string]string { return b.vmConfig.Labels }

// HydrateFromOrchard populates activeVMs from existing managed VMs in Orchard
// that belong to this scale set. Returns the number of VMs adopted so the
// caller can reserve those slots in the shared Capacity. Called once before
// the listener starts — before HandleDesiredRunnerCount can fire.
func (b *Bridge) HydrateFromOrchard(ctx context.Context) (int, error) {
	if b.state == nil {
		return 0, nil
	}
	snap, err := b.state.Get(ctx)
	if snap == nil {
		return 0, err
	}
	vms := snap.ManagedVMsForScaleSet(b.scaleSetName)

	b.mu.Lock()
	defer b.mu.Unlock()
	adopted := 0
	for _, vm := range vms {
		if vm.Status == orchard.VMStatusStopped || vm.Status == orchard.VMStatusFailed {
			continue
		}
		// Key by VM name — the runner name is only known after GitHub registers
		// the runner. HandleJobCompleted looks up by runner name and already
		// tolerates misses; cleanup will reap the VM when the job ends.
		b.activeVMs[vm.Name] = vm.Name
		adopted++
	}
	return adopted, err
}

// HandleDesiredRunnerCount is called by the listener when GitHub reports the
// desired runner count. We provision VMs only up to the real free slots on
// label-matching Orchard workers. Anything beyond that stays in GitHub's
// queue and gets picked up once a slot frees.
func (b *Bridge) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	b.mu.Lock()
	currentActive := len(b.activeVMs)
	b.mu.Unlock()

	needed := count - currentActive
	if needed <= 0 {
		return currentActive, nil
	}

	// Authoritative check: real free slots on label-matching workers. Uses
	// the shared StateView so the two bridges see a consistent view of what's
	// already in-flight.
	if b.state != nil {
		snap, err := b.state.Get(ctx)
		if snap == nil {
			b.logger.Warn("no orchard snapshot, skipping scale-up", "error", err)
			return currentActive, nil
		}
		workerCap := CapacityForLabels(snap.Workers, b.vmConfig.Labels)
		managedMatching := countActiveVMs(snap.ManagedVMsMatchingLabels(b.vmConfig.Labels))
		free := workerCap - managedMatching
		if free <= 0 {
			b.logger.Info("no free worker slots for labels",
				"needed", needed, "workerCap", workerCap, "managedMatching", managedMatching)
			return currentActive, nil
		}
		if needed > free {
			needed = free
		}
	}

	// Global semaphore is retained as a second-line guardrail against runaway
	// bugs (e.g. misconfigured labels matching zero workers slipping past above).
	acquired := b.capacity.TryAcquire(needed)
	if acquired == 0 {
		b.logger.Warn("global capacity exhausted", "needed", needed, "currentActive", currentActive)
		return currentActive, nil
	}

	b.logger.Info("scaling up", "needed", needed, "acquired", acquired, "currentActive", currentActive)

	created := 0
	for range acquired {
		vmName := VMName(b.scaleSetName)

		jitConfig, err := b.ghClient.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
			Name: vmName,
		}, b.scaleSetID)
		if err != nil {
			b.logger.Error("failed to generate JIT config", "error", err)
			b.capacity.Release(acquired - created)
			b.notifyChange()
			return currentActive + created, fmt.Errorf("generating JIT config: %w", err)
		}

		script := StartupScript(jitConfig.EncodedJITConfig, b.vmConfig.DockerPort)
		vm := &orchard.VM{
			Name:   vmName,
			Image:  b.vmConfig.Image,
			CPU:    b.vmConfig.CPU,
			Memory: b.vmConfig.Memory,
			Labels: b.vmConfig.Labels,
			StartupScript: &orchard.VMScript{
				ScriptContent: script,
			},
		}

		if _, err := b.orchardClient.CreateVM(ctx, vm); err != nil {
			b.logger.Error("failed to create VM", "vm", vmName, "error", err)
			b.capacity.Release(acquired - created)
			b.notifyChange()
			return currentActive + created, fmt.Errorf("creating VM %s: %w", vmName, err)
		}

		b.mu.Lock()
		b.activeVMs[vmName] = vmName
		b.mu.Unlock()

		created++
		b.logger.Info("created VM", "vm", vmName, "image", b.vmConfig.Image)
	}

	if b.state != nil {
		b.state.Invalidate()
	}
	b.notifyChange()

	return currentActive + created, nil
}

func (b *Bridge) notifyChange() {
	if b.onVMChange != nil {
		b.onVMChange()
	}
}

// countActiveVMs returns VMs that are not in terminal states.
func countActiveVMs(vms []orchard.VM) int {
	n := 0
	for _, vm := range vms {
		if vm.Status == orchard.VMStatusStopped || vm.Status == orchard.VMStatusFailed {
			continue
		}
		n++
	}
	return n
}

// HandleJobStarted is called when a job starts on a runner. The VM is already running.
func (b *Bridge) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	b.logger.Info("job started",
		"runner", jobInfo.RunnerName,
		"job", jobInfo.JobDisplayName,
		"repo", jobInfo.RepositoryName,
	)
	return nil
}

// HandleJobCompleted is called when a job finishes. We delete the ephemeral VM.
func (b *Bridge) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	runnerName := jobInfo.RunnerName

	b.mu.Lock()
	vmName, ok := b.activeVMs[runnerName]
	if ok {
		delete(b.activeVMs, runnerName)
	}
	b.mu.Unlock()

	if !ok {
		b.logger.Warn("job completed for unknown runner", "runner", runnerName)
		return nil
	}

	b.logger.Info("job completed, deleting VM",
		"runner", runnerName,
		"vm", vmName,
		"result", jobInfo.Result,
	)

	if err := b.orchardClient.DeleteVM(ctx, vmName); err != nil {
		b.logger.Error("failed to delete VM", "vm", vmName, "error", err)
		return err
	}

	// Deregister the runner from GitHub (best-effort)
	if err := b.ghClient.RemoveRunner(ctx, int64(jobInfo.RunnerID)); err != nil {
		b.logger.Debug("failed to deregister runner", "runner", runnerName, "error", err)
	}

	b.capacity.Release(1)
	if b.state != nil {
		b.state.Invalidate()
	}
	b.notifyChange()
	return nil
}

// PurgeActiveVM removes a VM from the active tracking map.
// Called by Cleanup when a VM is reaped externally (e.g. failed/stopped).
func (b *Bridge) PurgeActiveVM(vmName string) {
	b.mu.Lock()
	_, had := b.activeVMs[vmName]
	delete(b.activeVMs, vmName)
	b.mu.Unlock()
	if had {
		b.logger.Info("purged stale VM from active tracking", "vm", vmName)
	}
}

// ActiveVMCount returns the number of active VMs tracked by this bridge.
func (b *Bridge) ActiveVMCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.activeVMs)
}
