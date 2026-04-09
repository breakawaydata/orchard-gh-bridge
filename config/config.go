package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LogLevel  string           `yaml:"logLevel"`
	MaxVMs    int              `yaml:"maxVMs"`
	Orchard   OrchardConfig    `yaml:"orchard"`
	GitHub    GitHubConfig     `yaml:"github"`
	ScaleSets []ScaleSetConfig `yaml:"scaleSets"`
	Health    HealthConfig     `yaml:"health"`
	Metrics   MetricsConfig    `yaml:"metrics"`
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
	Nested     bool              `yaml:"nested"`
	DockerPort int               `yaml:"dockerPort"`
	Labels     map[string]string `yaml:"labels"`
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
