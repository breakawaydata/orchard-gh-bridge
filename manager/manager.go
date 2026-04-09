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

// Manager orchestrates multiple scale set bridges sharing a global capacity pool.
type Manager struct {
	cfg           *config.Config
	orchardClient orchard.Client
	logger        *slog.Logger
	capacity      *brdg.Capacity

	// newGHClient creates a scaleset client for the given config URL.
	// Extracted for testing.
	newGHClient func(configURL string) (*scaleset.Client, error)

	bridgesMu sync.Mutex
	bridges   []*brdg.Bridge
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
	cleanup.SetOnVMCleaned(func(vmName string) {
		m.bridgesMu.Lock()
		defer m.bridgesMu.Unlock()
		for _, b := range m.bridges {
			b.PurgeActiveVM(vmName)
		}
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

	// Determine max runners: static override or label-matched capacity
	var maxRunners int
	if ssCfg.MaxRunners > 0 {
		maxRunners = ssCfg.MaxRunners
	} else {
		workers, err := m.orchardClient.ListWorkers(ctx)
		if err != nil {
			logger.Warn("failed to list workers for initial capacity", "error", err)
		} else {
			maxRunners = brdg.CapacityForLabels(workers, ssCfg.VM.Labels)
		}
		logger.Info("initial maxRunners from matching workers", "maxRunners", maxRunners)
	}

	l, err := listener.New(sessionClient, listener.Config{
		ScaleSetID: ss.ID,
		MaxRunners: maxRunners,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("creating listener for %s: %w", ssCfg.Name, err)
	}

	// Update GitHub when worker capacity changes, using only workers
	// whose labels match this scale set's VM labels
	if ssCfg.MaxRunners == 0 {
		vmLabels := ssCfg.VM.Labels
		m.capacity.OnWorkersChanged(func(workers []orchard.Worker) {
			matched := brdg.CapacityForLabels(workers, vmLabels)
			logger.Info("updating maxRunners from matching workers", "maxRunners", matched)
			l.SetMaxRunners(matched)
		})
	}

	// Create bridge (Scaler implementation)
	b := brdg.New(brdg.Config{
		ScaleSetName:  ssCfg.Name,
		ScaleSetID:    ss.ID,
		VMConfig:      ssCfg.VM,
		OrchardClient: m.orchardClient,
		GHClient:      ghClient,
		Capacity:      m.capacity,
		Logger:        m.logger,
	})

	m.bridgesMu.Lock()
	m.bridges = append(m.bridges, b)
	m.bridgesMu.Unlock()

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
