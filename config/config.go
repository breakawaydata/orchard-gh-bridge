package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultWorkerStaleAfter is the heartbeat age past which a worker stops
	// counting as capacity when workerStaleAfter is unset. Defined here rather
	// than in the bridge package so Validate can compare workerPruneAfter
	// against the effective value without an import cycle; bridge re-exports it.
	DefaultWorkerStaleAfter = 2 * time.Minute
	// DefaultWorkerPruneAfter is the heartbeat age past which the cleanup loop
	// deletes an offline worker's record when workerPruneAfter is unset.
	DefaultWorkerPruneAfter = time.Hour
	// DefaultMaxPendingAge is how long a managed VM may sit in "pending" before
	// the cleanup loop reaps it, when maxPendingAge is unset.
	DefaultMaxPendingAge = 10 * time.Minute
)

type Config struct {
	LogLevel string `yaml:"logLevel"`
	MaxVMs   int    `yaml:"maxVMs"`
	// MaxVMAge overrides the safety timeout after which the cleanup loop reaps
	// a managed VM regardless of job state (a Go duration string, e.g. "4h").
	// Empty keeps the built-in default (bridge.DefaultMaxVMAge, 2h). Set it
	// above the longest consuming repo's GitHub job `timeout-minutes` so the
	// job-level timeout governs and this stays a runaway backstop.
	MaxVMAge string `yaml:"maxVMAge"`
	// WorkerStaleAfter overrides how long an Orchard worker may go without a
	// heartbeat before the bridge stops counting it as capacity (a Go duration
	// string, e.g. "2m"). Empty keeps the built-in default
	// (bridge.DefaultWorkerStaleAfter). Raise it only if your workers ping
	// infrequently; lowering it below the worker ping interval will flap.
	WorkerStaleAfter string `yaml:"workerStaleAfter"`
	// WorkerPruneAfter is how long an Orchard worker may go without a heartbeat
	// before the cleanup loop deletes its record from the controller (a Go
	// duration string, e.g. "1h"). Empty keeps DefaultWorkerPruneAfter; "0"
	// disables pruning. Must be longer than workerStaleAfter. A worker that is
	// still running re-registers on its next connection, so pruning discards
	// only the record left behind when a machine is renamed or retired.
	WorkerPruneAfter string `yaml:"workerPruneAfter"`
	// MaxPendingAge is how long a managed VM may sit in "pending" before the
	// cleanup loop reaps it as stuck (a Go duration string, e.g. "30m"). Empty
	// keeps DefaultMaxPendingAge. Raise it when a worker's first pull of a large
	// image legitimately takes longer than the default.
	MaxPendingAge string           `yaml:"maxPendingAge"`
	Orchard       OrchardConfig    `yaml:"orchard"`
	GitHub        GitHubConfig     `yaml:"github"`
	ScaleSets     []ScaleSetConfig `yaml:"scaleSets"`
	Health        HealthConfig     `yaml:"health"`
	Metrics       MetricsConfig    `yaml:"metrics"`
}

type OrchardConfig struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Insecure bool   `yaml:"insecure"`
}

type GitHubConfig struct {
	AppID          int64  `yaml:"appID"`
	InstallationID int64  `yaml:"installationID"`
	PrivateKeyPath string `yaml:"privateKeyPath"`
	PrivateKey     string `yaml:"privateKey"`
	Token          string `yaml:"token"`
}

type ScaleSetConfig struct {
	Name            string   `yaml:"name"`
	GitHubConfigURL string   `yaml:"githubConfigURL"`
	Labels          []string `yaml:"labels"`
	RunnerGroup     string   `yaml:"runnerGroup"`
	MaxRunners      int      `yaml:"maxRunners"`
	VM              VMConfig `yaml:"vm"`
}

type VMConfig struct {
	Image      string            `yaml:"image"`
	CPU        uint64            `yaml:"cpu"`
	Memory     uint64            `yaml:"memory"`
	DockerPort int               `yaml:"dockerPort"`
	Labels     map[string]string `yaml:"labels"`
	AutoSize   AutoSizeConfig    `yaml:"autoSize,omitempty"`
}

// AutoSizeConfig opts a scale set into per-host VM sizing.
//
// When Enabled, vm.cpu and vm.memory are ignored. Instead, each VM is sized
// from its target worker's advertised `org.cirruslabs.logical-cores` and
// `org.cirruslabs.memory-mib` resources, minus the configured reserves.
// The defaults (4 cores + 4096 MiB held back) leave room for macOS, the
// orchard-worker daemon, and a default Colima Docker VM on the host. If your
// Colima profile is bigger, bump ReserveCPU / ReserveMemoryMiB.
//
// The bridge selects a free worker per VM and pins placement by copying the
// worker's orchard-gh-bridge/worker-name label value onto the VM. Each
// AutoSize-eligible worker must carry that label with a value no other live
// worker shares; by convention it is the worker's Name, but it need not stay
// equal to it (see README's "Worker setup for AutoSize"). One managed VM per
// worker is enforced by the bridge regardless of the worker's tart-vms slot
// count.
type AutoSizeConfig struct {
	Enabled          bool   `yaml:"enabled"`
	ReserveCPU       uint64 `yaml:"reserveCPU"`
	ReserveMemoryMiB uint64 `yaml:"reserveMemoryMiB"`
}

type HealthConfig struct {
	Port int `yaml:"port"`
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyDefaults(cfg)
	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	// MaxVMs 0 means auto-detect from connected workers
	if cfg.Orchard.Username == "" {
		cfg.Orchard.Username = "bootstrap-admin"
	}
	if cfg.Health.Port == 0 {
		cfg.Health.Port = 8080
	}
	if cfg.Metrics.Port == 0 {
		cfg.Metrics.Port = 9090
	}
	for i := range cfg.ScaleSets {
		if cfg.ScaleSets[i].RunnerGroup == "" {
			cfg.ScaleSets[i].RunnerGroup = "default"
		}
		if cfg.ScaleSets[i].VM.CPU == 0 {
			cfg.ScaleSets[i].VM.CPU = 4
		}
		if cfg.ScaleSets[i].VM.Memory == 0 {
			cfg.ScaleSets[i].VM.Memory = 8192
		}
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("ORCHARD_GH_BRIDGE_GITHUB_TOKEN"); v != "" {
		cfg.GitHub.Token = v
	}
	if v := os.Getenv("ORCHARD_GH_BRIDGE_GITHUB_PRIVATE_KEY"); v != "" {
		cfg.GitHub.PrivateKey = v
	}
	if v := os.Getenv("ORCHARD_GH_BRIDGE_GITHUB_APP_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.GitHub.AppID = id
		}
	}
	if v := os.Getenv("ORCHARD_GH_BRIDGE_GITHUB_INSTALLATION_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.GitHub.InstallationID = id
		}
	}
	if v := os.Getenv("ORCHARD_GH_BRIDGE_ORCHARD_PASSWORD"); v != "" {
		cfg.Orchard.Password = v
	}
}

func (c *Config) Validate() error {
	var errs []string

	if c.Orchard.Address == "" {
		errs = append(errs, "orchard.address is required")
	}

	if c.MaxVMAge != "" {
		if d, err := time.ParseDuration(c.MaxVMAge); err != nil {
			errs = append(errs, fmt.Sprintf("maxVMAge %q is not a valid duration: %v", c.MaxVMAge, err))
		} else if d <= 0 {
			errs = append(errs, "maxVMAge must be a positive duration")
		}
	}

	if c.WorkerStaleAfter != "" {
		if d, err := time.ParseDuration(c.WorkerStaleAfter); err != nil {
			errs = append(errs, fmt.Sprintf("workerStaleAfter %q is not a valid duration: %v", c.WorkerStaleAfter, err))
		} else if d <= 0 {
			errs = append(errs, "workerStaleAfter must be a positive duration")
		}
	}

	if c.MaxPendingAge != "" {
		if d, err := time.ParseDuration(c.MaxPendingAge); err != nil {
			errs = append(errs, fmt.Sprintf("maxPendingAge %q is not a valid duration: %v", c.MaxPendingAge, err))
		} else if d <= 0 {
			errs = append(errs, "maxPendingAge must be a positive duration")
		}
	}

	if c.WorkerPruneAfter != "" {
		if d, err := time.ParseDuration(c.WorkerPruneAfter); err != nil {
			errs = append(errs, fmt.Sprintf("workerPruneAfter %q is not a valid duration: %v", c.WorkerPruneAfter, err))
		} else if d < 0 {
			errs = append(errs, "workerPruneAfter must not be negative (use \"0\" to disable pruning)")
		}
	}
	// Pruning a worker the bridge still counts as live would delete the record
	// of a machine that is merely slow to heartbeat, so an explicit prune
	// threshold must sit strictly beyond the staleness one. An unset one never
	// fails validation: WorkerPruneAfterDuration disables the default instead,
	// so a config with a long workerStaleAfter that was valid before
	// workerPruneAfter existed still starts.
	if prune, stale := c.WorkerPruneAfterDuration(), c.effectiveWorkerStaleAfter(); c.WorkerPruneAfter != "" && prune > 0 && stale > 0 && prune <= stale {
		errs = append(errs, fmt.Sprintf("workerPruneAfter (%s) must be greater than workerStaleAfter (%s)", prune, stale))
	}

	hasApp := c.GitHub.AppID != 0 && c.GitHub.InstallationID != 0 &&
		(c.GitHub.PrivateKey != "" || c.GitHub.PrivateKeyPath != "")
	hasPAT := c.GitHub.Token != ""

	if !hasApp && !hasPAT {
		errs = append(errs, "github auth required: set appID+installationID+privateKey, or token")
	}

	if len(c.ScaleSets) == 0 {
		errs = append(errs, "at least one scaleSet is required")
	}

	for i, ss := range c.ScaleSets {
		if ss.Name == "" {
			errs = append(errs, fmt.Sprintf("scaleSets[%d].name is required", i))
		}
		if ss.GitHubConfigURL == "" {
			errs = append(errs, fmt.Sprintf("scaleSets[%d].githubConfigURL is required", i))
		}
		if ss.VM.Image == "" {
			errs = append(errs, fmt.Sprintf("scaleSets[%d].vm.image is required", i))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// MaxVMAgeDuration returns the configured VM reaping age, or 0 when unset
// (callers keep their default). Validate guarantees a non-empty value parses,
// so the parse error is intentionally ignored here.
func (c *Config) MaxVMAgeDuration() time.Duration {
	if c.MaxVMAge == "" {
		return 0
	}
	d, _ := time.ParseDuration(c.MaxVMAge)
	return d
}

// WorkerStaleAfterDuration returns the configured worker heartbeat staleness
// threshold, or 0 when unset (callers keep their default). Validate guarantees
// a non-empty value parses, so the parse error is intentionally ignored here.
func (c *Config) WorkerStaleAfterDuration() time.Duration {
	if c.WorkerStaleAfter == "" {
		return 0
	}
	d, _ := time.ParseDuration(c.WorkerStaleAfter)
	return d
}

// effectiveWorkerStaleAfter is WorkerStaleAfterDuration with the default
// applied, or 0 when the configured value does not parse (Validate reports
// that separately).
func (c *Config) effectiveWorkerStaleAfter() time.Duration {
	if c.WorkerStaleAfter == "" {
		return DefaultWorkerStaleAfter
	}
	d, err := time.ParseDuration(c.WorkerStaleAfter)
	if err != nil {
		return 0
	}
	return d
}

// WorkerPruneAfterDuration returns the heartbeat age past which offline worker
// records are deleted: DefaultWorkerPruneAfter when unset, 0 when explicitly
// disabled ("0") or unparseable (Validate rejects the latter). When unset and
// workerStaleAfter is at or beyond the default, pruning is off (see
// WorkerPruneDefaultSuppressed): the default must never prune a worker the
// bridge still counts as live.
func (c *Config) WorkerPruneAfterDuration() time.Duration {
	if c.WorkerPruneAfter == "" {
		if c.WorkerPruneDefaultSuppressed() {
			return 0
		}
		return DefaultWorkerPruneAfter
	}
	d, err := time.ParseDuration(c.WorkerPruneAfter)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// WorkerPruneDefaultSuppressed reports whether workerPruneAfter is unset and
// the default is switched off because workerStaleAfter is not below it.
func (c *Config) WorkerPruneDefaultSuppressed() bool {
	return c.WorkerPruneAfter == "" && c.effectiveWorkerStaleAfter() >= DefaultWorkerPruneAfter
}

// MaxPendingAgeDuration returns the configured stuck-pending reaping age, or 0
// when unset (callers keep their default). Validate guarantees a non-empty
// value parses, so the parse error is intentionally ignored here.
func (c *Config) MaxPendingAgeDuration() time.Duration {
	if c.MaxPendingAge == "" {
		return 0
	}
	d, _ := time.ParseDuration(c.MaxPendingAge)
	return d
}

func (c *Config) GitHubPrivateKeyPEM() (string, error) {
	if c.GitHub.PrivateKey != "" {
		return c.GitHub.PrivateKey, nil
	}
	if c.GitHub.PrivateKeyPath != "" {
		data, err := os.ReadFile(c.GitHub.PrivateKeyPath)
		if err != nil {
			return "", fmt.Errorf("reading private key file: %w", err)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("no private key configured")
}
