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
	logger        *slog.Logger

	mu        sync.Mutex
	activeVMs map[string]string // runnerName → vmName
}

type Config struct {
	ScaleSetName  string
	ScaleSetID    int
	VMConfig      config.VMConfig
	OrchardClient orchard.Client
	GHClient      *scaleset.Client
	Capacity      *Capacity
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
		logger:        cfg.Logger.With("scaleSet", cfg.ScaleSetName),
		activeVMs:     make(map[string]string),
	}
}

// HandleDesiredRunnerCount is called by the listener when GitHub reports desired runner count.
// We provision new VMs for the delta, respecting global capacity.
func (b *Bridge) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	b.mu.Lock()
	currentActive := len(b.activeVMs)
	b.mu.Unlock()

	needed := count - currentActive
	if needed <= 0 {
		return currentActive, nil
	}

	acquired := b.capacity.TryAcquire(needed)
	if acquired == 0 {
		b.logger.Warn("no capacity available", "needed", needed, "currentActive", currentActive)
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
			return currentActive + created, fmt.Errorf("generating JIT config: %w", err)
		}

		script := StartupScript(jitConfig.EncodedJITConfig, b.vmConfig.Nested)
		vm := &orchard.VM{
			Name:   vmName,
			Image:  b.vmConfig.Image,
			CPU:    b.vmConfig.CPU,
			Memory: b.vmConfig.Memory,
			Nested: b.vmConfig.Nested,
			Labels: b.vmConfig.Labels,
			StartupScript: &orchard.VMScript{
				ScriptContent: script,
			},
		}

		if _, err := b.orchardClient.CreateVM(ctx, vm); err != nil {
			b.logger.Error("failed to create VM", "vm", vmName, "error", err)
			b.capacity.Release(acquired - created)
			return currentActive + created, fmt.Errorf("creating VM %s: %w", vmName, err)
		}

		b.mu.Lock()
		b.activeVMs[vmName] = vmName
		b.mu.Unlock()

		created++
		b.logger.Info("created VM", "vm", vmName, "image", b.vmConfig.Image)
	}

	return currentActive + created, nil
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
	return nil
}

// ActiveVMCount returns the number of active VMs tracked by this bridge.
func (b *Bridge) ActiveVMCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.activeVMs)
}
