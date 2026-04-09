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
)

// Cleanup periodically reaps stale VMs from Orchard that are managed by the bridge.
type Cleanup struct {
	orchardClient orchard.Client
	capacity      *Capacity
	logger        *slog.Logger
	interval      time.Duration
	maxAge        time.Duration
}

func NewCleanup(orchardClient orchard.Client, capacity *Capacity, logger *slog.Logger) *Cleanup {
	return &Cleanup{
		orchardClient: orchardClient,
		capacity:      capacity,
		logger:        logger.With("component", "cleanup"),
		interval:      DefaultCleanupInterval,
		maxAge:        DefaultMaxVMAge,
	}
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
	vms, err := c.orchardClient.ListVMs(ctx)
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

		shouldDelete := false
		reason := ""

		switch vm.Status {
		case orchard.VMStatusStopped:
			shouldDelete = true
			reason = "stopped"
		case orchard.VMStatusFailed:
			shouldDelete = true
			reason = "failed"
		default:
			if !vm.CreatedAt.IsZero() && now.Sub(vm.CreatedAt) > c.maxAge {
				shouldDelete = true
				reason = "max age exceeded"
			}
		}

		if shouldDelete {
			c.logger.Info("cleaning up VM", "vm", vm.Name, "reason", reason, "status", vm.Status)
			if err := c.orchardClient.DeleteVM(ctx, vm.Name); err != nil {
				c.logger.Error("failed to delete VM during cleanup", "vm", vm.Name, "error", err)
				continue
			}
			deleted++
			managedCount--
		}
	}

	// Reconcile capacity with actual managed VM count
	c.capacity.Reconcile(managedCount)

	// Refresh max capacity from current workers
	c.refreshMaxCapacity(ctx)

	if deleted > 0 {
		c.logger.Info("cleanup sweep complete", "deleted", deleted, "remaining", managedCount)
	}
}

const resourceTartVMs = "org.cirruslabs.tart-vms"

func (c *Cleanup) refreshMaxCapacity(ctx context.Context) {
	workers, err := c.orchardClient.ListWorkers(ctx)
	if err != nil {
		c.logger.Error("failed to list workers for capacity refresh", "error", err)
		return
	}
	var total int
	for _, w := range workers {
		if n, ok := w.Resources[resourceTartVMs]; ok {
			total += int(n)
		}
	}
	if total > 0 {
		c.capacity.SetMax(total)
	}
}
