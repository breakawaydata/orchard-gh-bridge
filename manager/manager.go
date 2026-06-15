package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"golang.org/x/sync/errgroup"

	brdg "github.com/breakawaydata/orchard-gh-bridge/bridge"
	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// scaleSetHandle ties a bridge to its listener so the manager can push
// updated maxRunners values as the shared worker pool shifts.
type scaleSetHandle struct {
	bridge           *brdg.Bridge
	listener         *listener.Listener
	staticMaxRunners int // > 0 means the config pinned maxRunners; don't recompute
}

// Manager orchestrates multiple scale set bridges sharing a global capacity pool.
type Manager struct {
	cfg           *config.Config
	orchardClient orchard.Client
	logger        *slog.Logger
	capacity      *brdg.Capacity
	state         *brdg.StateView

	// newGHClient creates a scaleset client for the given config URL.
	// Extracted for testing.
	newGHClient func(configURL string) (*scaleset.Client, error)

	bridgesMu sync.Mutex
	handles   []*scaleSetHandle
}

func New(cfg *config.Config, orchardClient orchard.Client, logger *slog.Logger) (*Manager, error) {
	mgrLogger := logger.With("component", "manager")

	maxVMs := cfg.MaxVMs
	if maxVMs == 0 {
		total, err := discoverCapacity(context.Background(), orchardClient)
		if err != nil {
			mgrLogger.Warn("failed to discover worker capacity, starting with 0", "error", err)
		} else if total == 0 {
			mgrLogger.Warn("no workers with tart-vms capacity found yet, will detect when workers connect")
		} else {
			mgrLogger.Info("auto-detected capacity from workers", "maxVMs", total)
		}
		maxVMs = total
	}

	m := &Manager{
		cfg:           cfg,
		orchardClient: orchardClient,
		logger:        mgrLogger,
		capacity:      brdg.NewCapacity(maxVMs),
		state:         brdg.NewStateView(orchardClient, brdg.DefaultStateViewTTL),
	}

	m.newGHClient = m.defaultNewGHClient
	return m, nil
}

const resourceTartVMs = "org.cirruslabs.tart-vms"

func discoverCapacity(ctx context.Context, client orchard.Client) (int, error) {
	workers, err := client.ListWorkers(ctx)
	if err != nil {
		return 0, err
	}
	var total int
	for _, w := range workers {
		if n, ok := w.Resources[resourceTartVMs]; ok {
			total += int(n)
		}
	}
	return total, nil
}

func (m *Manager) defaultNewGHClient(configURL string) (*scaleset.Client, error) {
	gh := m.cfg.GitHub

	if gh.Token != "" {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     configURL,
			PersonalAccessToken: gh.Token,
			SystemInfo: scaleset.SystemInfo{
				System:  "orchard-gh-bridge",
				Version: "0.1.0",
			},
		})
	}

	pem, err := m.cfg.GitHubPrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("reading private key: %w", err)
	}

	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: configURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       fmt.Sprintf("%d", gh.AppID),
			InstallationID: gh.InstallationID,
			PrivateKey:     pem,
		},
		SystemInfo: scaleset.SystemInfo{
			System:  "orchard-gh-bridge",
			Version: "0.1.0",
		},
	})
}

// Run starts all scale set listeners and the cleanup goroutine.
// Blocks until context is cancelled or a fatal error occurs.
func (m *Manager) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	// Whenever the shared worker pool changes (workers come/go, resources
	// change), recompute each scale set's share of the pool and push it to
	// GitHub. Registered once at manager level, not per-scale-set.
	m.capacity.OnWorkersChanged(func(_ []orchard.Worker) {
		m.recomputeScaleSetShares(ctx)
	})

	// Create a GitHub client for runner cleanup
	var runnerRemover brdg.RunnerRemover
	ghClient, err := m.newGHClient(m.cfg.ScaleSets[0].GitHubConfigURL)
	if err != nil {
		m.logger.Warn("failed to create GitHub client for runner cleanup", "error", err)
	} else {
		runnerRemover = brdg.NewScaleSetRunnerRemover(ghClient)
	}

	// Start cleanup goroutine
	cleanup := brdg.NewCleanup(m.orchardClient, m.capacity, runnerRemover, m.logger)
	cleanup.SetStateView(m.state)
	if maxAge := m.cfg.MaxVMAgeDuration(); maxAge > 0 {
		cleanup.SetMaxAge(maxAge)
		m.logger.Info("overriding VM reaping age from config", "maxVMAge", maxAge)
	}
	cleanup.SetOnVMCleaned(func(vmName string) {
		m.bridgesMu.Lock()
		handles := make([]*scaleSetHandle, len(m.handles))
		copy(handles, m.handles)
		m.bridgesMu.Unlock()
		for _, h := range handles {
			h.bridge.PurgeActiveVM(vmName)
		}
		// Cleanup deleted something → shared pool shifted, refresh each
		// scale set's reported maxRunners.
		m.recomputeScaleSetShares(context.Background())
	})
	g.Go(func() error {
		cleanup.Run(ctx)
		return nil
	})

	// Start a listener for each scale set
	for _, ssCfg := range m.cfg.ScaleSets {
		ssCfg := ssCfg
		g.Go(func() error {
			return m.runScaleSet(ctx, ssCfg)
		})
	}

	return g.Wait()
}

func (m *Manager) runScaleSet(ctx context.Context, ssCfg config.ScaleSetConfig) error {
	logger := m.logger.With("scaleSet", ssCfg.Name)
	logger.Info("starting scale set listener", "configURL", ssCfg.GitHubConfigURL)

	ghClient, err := m.newGHClient(ssCfg.GitHubConfigURL)
	if err != nil {
		return fmt.Errorf("creating GitHub client for %s: %w", ssCfg.Name, err)
	}

	// Register or find existing scale set
	ss, err := m.registerScaleSet(ctx, ghClient, ssCfg, logger)
	if err != nil {
		return fmt.Errorf("registering scale set %s: %w", ssCfg.Name, err)
	}
	logger.Info("scale set registered", "id", ss.ID)

	// Determine max runners: static override or label-matched capacity.
	// Wait for matching workers before creating the session so GitHub
	// never sees maxCapacity=0 (which causes it to skip routing jobs
	// to the scale set even after capacity increases).
	capacityFn := workerCapacityFn(ssCfg.VM.AutoSize.Enabled, ssCfg.VM.AutoSize.ReserveCPU, ssCfg.VM.AutoSize.ReserveMemoryMiB)
	var maxRunners int
	if ssCfg.MaxRunners > 0 {
		maxRunners = ssCfg.MaxRunners
	} else {
		workers, err := m.orchardClient.ListWorkers(ctx)
		if err != nil {
			logger.Warn("failed to list workers for initial capacity", "error", err)
		} else {
			maxRunners = capacityFn(workers, ssCfg.VM.Labels)
		}

		if maxRunners == 0 {
			logger.Info("no matching workers yet, waiting for workers to connect")
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Second):
				}
				workers, err := m.orchardClient.ListWorkers(ctx)
				if err != nil {
					logger.Warn("failed to list workers while waiting", "error", err)
					continue
				}
				maxRunners = capacityFn(workers, ssCfg.VM.Labels)
				if maxRunners > 0 {
					break
				}
			}
		}
		logger.Info("initial maxRunners from matching workers", "maxRunners", maxRunners, "autoSize", ssCfg.VM.AutoSize.Enabled)
	}

	// Create message session with retry (GitHub sessions have a TTL and may
	// conflict briefly during redeployments)
	var sessionClient *scaleset.MessageSessionClient
	for attempt := 1; ; attempt++ {
		sessionClient, err = ghClient.MessageSessionClient(ctx, ss.ID, "orchard-gh-bridge")
		if err == nil {
			break
		}
		if attempt >= 10 {
			return fmt.Errorf("creating message session for %s: %w", ssCfg.Name, err)
		}
		logger.Warn("session conflict, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*3) * time.Second):
		}
	}
	defer sessionClient.Close(ctx) //nolint:errcheck

	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: ss.ID,
		MaxRunners: maxRunners,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("creating listener for %s: %w", ssCfg.Name, err)
	}

	// Create bridge (Scaler implementation)
	b := brdg.New(brdg.Config{
		ScaleSetName:  ssCfg.Name,
		ScaleSetID:    ss.ID,
		VMConfig:      ssCfg.VM,
		OrchardClient: m.orchardClient,
		GHClient:      ghClient,
		Capacity:      m.capacity,
		State:         m.state,
		Logger:        m.logger,
	})

	handle := &scaleSetHandle{
		bridge:           b,
		listener:         l,
		staticMaxRunners: ssCfg.MaxRunners,
	}

	// Recompute shares on every VM create/complete so GitHub's view of
	// per-scale-set capacity stays in sync with the shared worker pool.
	b.SetOnVMChange(func() {
		m.recomputeScaleSetShares(ctx)
	})

	m.bridgesMu.Lock()
	m.handles = append(m.handles, handle)
	m.bridgesMu.Unlock()

	// Hydrate activeVMs from any existing Orchard VMs for this scale set,
	// and reserve the corresponding capacity slots. Must happen before
	// listener.Run, which is the only caller of HandleDesiredRunnerCount.
	// HydrateFromOrchard may return adopted>0 alongside a non-nil err when
	// StateView.Get returns a stale snapshot after a failed refresh — in that
	// case the VMs are already in activeVMs, so we must still reserve their
	// slots or the global semaphore will let other bridges over-provision.
	adopted, herr := b.HydrateFromOrchard(ctx)
	if herr != nil {
		logger.Warn("hydrate from orchard returned error", "adopted", adopted, "error", herr)
	}
	if adopted > 0 {
		logger.Info("adopted existing VMs from orchard", "count", adopted)
		m.capacity.AdoptExisting(adopted)
	}

	// After hydration, run one share recompute so initial maxRunners
	// reflects existing cross-scale-set load.
	m.recomputeScaleSetShares(ctx)

	// Run the listener (blocks until context cancelled)
	logger.Info("listening for jobs", "maxRunners", maxRunners)
	if err := l.Run(ctx, b); err != nil {
		if ctx.Err() != nil {
			logger.Info("listener stopped (context cancelled)")
			return nil
		}
		return fmt.Errorf("listener for %s: %w", ssCfg.Name, err)
	}
	return nil
}

func (m *Manager) registerScaleSet(ctx context.Context, ghClient *scaleset.Client, ssCfg config.ScaleSetConfig, logger *slog.Logger) (*scaleset.RunnerScaleSet, error) {
	// Try to find existing scale set first
	runnerGroupID := 1 // default group
	if ssCfg.RunnerGroup != "" && ssCfg.RunnerGroup != "default" {
		rg, err := ghClient.GetRunnerGroupByName(ctx, ssCfg.RunnerGroup)
		if err != nil {
			return nil, fmt.Errorf("getting runner group %q: %w", ssCfg.RunnerGroup, err)
		}
		runnerGroupID = rg.ID
	}

	labels := make([]scaleset.Label, len(ssCfg.Labels))
	for i, l := range ssCfg.Labels {
		labels[i] = scaleset.Label{Name: l}
	}

	desired := &scaleset.RunnerScaleSet{
		Name:          ssCfg.Name,
		RunnerGroupID: runnerGroupID,
		Labels:        labels,
		RunnerSetting: scaleset.RunnerSetting{
			DisableUpdate: true,
		},
	}

	// If the scale set already exists, update it to sync labels
	existing, err := ghClient.GetRunnerScaleSet(ctx, runnerGroupID, ssCfg.Name)
	if err == nil && existing != nil {
		updated, err := ghClient.UpdateRunnerScaleSet(ctx, existing.ID, desired)
		if err != nil {
			return nil, fmt.Errorf("updating scale set %s: %w", ssCfg.Name, err)
		}
		logger.Info("updated existing scale set", "id", updated.ID)
		return updated, nil
	}

	// Create new scale set
	ss, err := ghClient.CreateRunnerScaleSet(ctx, desired)
	if err != nil {
		return nil, fmt.Errorf("creating scale set: %w", err)
	}
	logger.Info("created new scale set", "id", ss.ID)
	return ss, nil
}

// Capacity returns the shared capacity tracker, exposed for health checks.
func (m *Manager) Capacity() *brdg.Capacity {
	return m.capacity
}

// recomputeScaleSetShares recalculates each scale set's share of the shared
// worker pool and pushes the new value to its listener as maxRunners. The
// share is: (label-matched worker capacity) − (active managed VMs of OTHER
// scale sets on those same workers). Self-active VMs aren't subtracted because
// GitHub's listener already accounts for runners it has dispatched to us.
//
// Scale sets with a statically-configured MaxRunners are left alone.
func (m *Manager) recomputeScaleSetShares(ctx context.Context) {
	snap, err := m.state.Get(ctx)
	if snap == nil {
		if err != nil {
			m.logger.Debug("skip share recompute: no snapshot", "error", err)
		}
		return
	}

	m.bridgesMu.Lock()
	handles := make([]*scaleSetHandle, len(m.handles))
	copy(handles, m.handles)
	m.bridgesMu.Unlock()

	for _, h := range handles {
		if h.staticMaxRunners > 0 {
			continue
		}
		labels := h.bridge.VMLabels()
		reserveCPU, reserveMemMiB := h.bridge.AutoSizeReserveConfig()
		capacityFn := workerCapacityFn(h.bridge.AutoSizeEnabled(), reserveCPU, reserveMemMiB)
		workerCap := capacityFn(snap.Workers, labels)
		othersActive := 0
		for _, other := range handles {
			if other == h {
				continue
			}
			for _, vm := range snap.ManagedVMsForScaleSet(other.bridge.ScaleSetName()) {
				if vm.Status == orchard.VMStatusStopped || vm.Status == orchard.VMStatusFailed {
					continue
				}
				if vmConsumesLabeledWorker(vm, snap.Workers, labels) {
					othersActive++
				}
			}
		}
		share := workerCap - othersActive
		if share < 0 {
			share = 0
		}
		m.logger.Info("updating maxRunners",
			"scaleSet", h.bridge.ScaleSetName(),
			"share", share,
			"workerCap", workerCap,
			"othersActive", othersActive,
			"autoSize", h.bridge.AutoSizeEnabled(),
		)
		h.listener.SetMaxRunners(share)
	}
}

// workerCapacityFn returns the capacity-counting function for a scale set:
// per-worker count when AutoSize is on (one VM per worker, filtered by
// reserves so undersized workers are not reported as schedulable), or sum of
// tart-vms slots otherwise.
func workerCapacityFn(autoSize bool, reserveCPU, reserveMemMiB uint64) func([]orchard.Worker, map[string]string) int {
	if autoSize {
		cpu, mem := brdg.AutoSizeReserves(reserveCPU, reserveMemMiB)
		return func(workers []orchard.Worker, labels map[string]string) int {
			return brdg.WorkerCountForLabels(workers, labels, cpu, mem)
		}
	}
	return brdg.CapacityForLabels
}

// vmConsumesLabeledWorker returns true if vm occupies a slot on a worker
// whose labels match vmLabels. Pending VMs (no worker assignment yet) are
// counted if their own Labels would match a label-matching worker.
func vmConsumesLabeledWorker(vm orchard.VM, workers []orchard.Worker, vmLabels map[string]string) bool {
	if vm.Worker != "" {
		for _, w := range workers {
			if w.Name != vm.Worker {
				continue
			}
			return workerMatchesAllLabels(w, vmLabels)
		}
		return false
	}
	// Pending VM: if its own labels would select any of our label-matching
	// workers, count it (belt-and-suspenders — otherwise an in-flight VM
	// for another scale set wouldn't be visible until Orchard places it).
	for _, w := range workers {
		if workerMatchesAllLabels(w, vm.Labels) && workerMatchesAllLabels(w, vmLabels) {
			return true
		}
	}
	return false
}

func workerMatchesAllLabels(w orchard.Worker, want map[string]string) bool {
	for k, v := range want {
		if w.Labels[k] != v {
			return false
		}
	}
	return true
}
