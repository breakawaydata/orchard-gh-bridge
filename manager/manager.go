package manager

import (
	"context"
	"fmt"
	"log/slog"

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
}

func New(cfg *config.Config, orchardClient orchard.Client, logger *slog.Logger) (*Manager, error) {
	m := &Manager{
		cfg:           cfg,
		orchardClient: orchardClient,
		logger:        logger.With("component", "manager"),
		capacity:      brdg.NewCapacity(cfg.MaxVMs),
	}

	m.newGHClient = m.defaultNewGHClient
	return m, nil
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

	// Start cleanup goroutine
	cleanup := brdg.NewCleanup(m.orchardClient, m.capacity, m.logger)
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

	// Create message session
	sessionClient, err := ghClient.MessageSessionClient(ctx, ss.ID, "orchard-gh-bridge")
	if err != nil {
		return fmt.Errorf("creating message session for %s: %w", ssCfg.Name, err)
	}
	defer sessionClient.Close(ctx) //nolint:errcheck

	// Determine max runners for this scale set
	maxRunners := m.cfg.MaxVMs
	if ssCfg.MaxRunners > 0 {
		maxRunners = ssCfg.MaxRunners
	}

	// Create listener
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
		Logger:        m.logger,
	})

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

	existing, err := ghClient.GetRunnerScaleSet(ctx, runnerGroupID, ssCfg.Name)
	if err == nil && existing != nil {
		logger.Info("found existing scale set", "id", existing.ID)
		return existing, nil
	}

	// Create new scale set
	labels := make([]scaleset.Label, len(ssCfg.Labels))
	for i, l := range ssCfg.Labels {
		labels[i] = scaleset.Label{Name: l}
	}

	ss, err := ghClient.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          ssCfg.Name,
		RunnerGroupID: runnerGroupID,
		Labels:        labels,
		RunnerSetting: scaleset.RunnerSetting{
			DisableUpdate: true,
		},
	})
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
