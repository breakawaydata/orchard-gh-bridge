package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
logLevel: debug
maxVMs: 2
orchard:
  address: http://localhost:6120
  insecure: true
github:
  token: ghp_test123
scaleSets:
  - name: macos-runner
    githubConfigURL: https://github.com/testorg
    labels: [self-hosted, macOS]
    vm:
      image: ghcr.io/cirruslabs/macos-sequoia-xcode:latest
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.MaxVMs != 2 {
		t.Errorf("MaxVMs = %d, want 2", cfg.MaxVMs)
	}
	if cfg.Orchard.Address != "http://localhost:6120" {
		t.Errorf("Orchard.Address = %q, want http://localhost:6120", cfg.Orchard.Address)
	}
	if len(cfg.ScaleSets) != 1 {
		t.Fatalf("len(ScaleSets) = %d, want 1", len(cfg.ScaleSets))
	}
	ss := cfg.ScaleSets[0]
	if ss.RunnerGroup != "default" {
		t.Errorf("RunnerGroup = %q, want default", ss.RunnerGroup)
	}
	if ss.VM.CPU != 4 {
		t.Errorf("VM.CPU = %d, want 4 (default)", ss.VM.CPU)
	}
	if ss.VM.Memory != 8192 {
		t.Errorf("VM.Memory = %d, want 8192 (default)", ss.VM.Memory)
	}
}

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
orchard:
  address: http://localhost:6120
github:
  token: ghp_test
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.MaxVMs != 0 {
		t.Errorf("MaxVMs = %d, want 0 (auto-detect)", cfg.MaxVMs)
	}
	if cfg.Health.Port != 8080 {
		t.Errorf("Health.Port = %d, want 8080", cfg.Health.Port)
	}
	if cfg.Metrics.Port != 9090 {
		t.Errorf("Metrics.Port = %d, want 9090", cfg.Metrics.Port)
	}
	if cfg.Orchard.Username != "bootstrap-admin" {
		t.Errorf("Orchard.Username = %q, want bootstrap-admin", cfg.Orchard.Username)
	}
}

func TestLoad_MaxVMAge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
maxVMAge: 4h
orchard:
  address: http://localhost:6120
github:
  token: ghp_test
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.MaxVMAgeDuration(); got != 4*time.Hour {
		t.Errorf("MaxVMAgeDuration = %v, want 4h", got)
	}
}

func TestLoad_MaxVMAgeUnsetIsZero(t *testing.T) {
	cfg := &Config{}
	if got := cfg.MaxVMAgeDuration(); got != 0 {
		t.Errorf("MaxVMAgeDuration = %v, want 0 when unset", got)
	}
}

func TestLoad_MaxVMAgeInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
maxVMAge: not-a-duration
orchard:
  address: http://localhost:6120
github:
  token: ghp_test
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid maxVMAge duration")
	}
}

func TestLoad_MissingOrchardAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
github:
  token: ghp_test
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing orchard address")
	}
}

func TestLoad_MissingGitHubAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
orchard:
  address: http://localhost:6120
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing github auth")
	}
}

func TestLoad_NoScaleSets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
orchard:
  address: http://localhost:6120
github:
  token: ghp_test
scaleSets: []
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty scaleSets")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `
orchard:
  address: http://localhost:6120
github:
  token: original-token
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORCHARD_GH_BRIDGE_GITHUB_TOKEN", "env-token")
	t.Setenv("ORCHARD_GH_BRIDGE_ORCHARD_PASSWORD", "env-password")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.GitHub.Token != "env-token" {
		t.Errorf("GitHub.Token = %q, want env-token", cfg.GitHub.Token)
	}
	if cfg.Orchard.Password != "env-password" {
		t.Errorf("Orchard.Password = %q, want env-password", cfg.Orchard.Password)
	}
}

func TestGitHubPrivateKeyPEM_Inline(t *testing.T) {
	cfg := &Config{GitHub: GitHubConfig{PrivateKey: "inline-pem"}}
	pem, err := cfg.GitHubPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if pem != "inline-pem" {
		t.Errorf("got %q, want inline-pem", pem)
	}
}

func TestGitHubPrivateKeyPEM_File(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte("file-pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{GitHub: GitHubConfig{PrivateKeyPath: keyPath}}
	pem, err := cfg.GitHubPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if pem != "file-pem" {
		t.Errorf("got %q, want file-pem", pem)
	}
}

// loadWith writes a minimal valid config plus extra top-level keys and loads it.
func loadWith(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := extra + `
orchard:
  address: http://localhost:6120
github:
  token: ghp_test
scaleSets:
  - name: test
    githubConfigURL: https://github.com/org
    vm:
      image: test-image
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad_WorkerPruneAfter(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    time.Duration
		wantErr string
	}{
		{name: "unset defaults to 1h", yaml: "", want: time.Hour},
		{name: "explicit", yaml: "workerPruneAfter: 3h", want: 3 * time.Hour},
		{name: "string zero disables", yaml: `workerPruneAfter: "0"`, want: 0},
		{name: "bare zero disables", yaml: "workerPruneAfter: 0", want: 0},
		{name: "0s disables", yaml: "workerPruneAfter: 0s", want: 0},
		{name: "zero disables even with long staleAfter", yaml: "workerStaleAfter: 2h\nworkerPruneAfter: 0", want: 0},
		{name: "invalid", yaml: "workerPruneAfter: soon", wantErr: "workerPruneAfter"},
		{name: "negative", yaml: "workerPruneAfter: -1h", wantErr: "must not be negative"},
		{name: "equal to default staleAfter", yaml: "workerPruneAfter: 2m", wantErr: "must be greater than workerStaleAfter"},
		{name: "below explicit staleAfter", yaml: "workerStaleAfter: 30m\nworkerPruneAfter: 10m", wantErr: "must be greater than workerStaleAfter"},
		{name: "default prune vs longer explicit staleAfter", yaml: "workerStaleAfter: 2h", wantErr: "must be greater than workerStaleAfter"},
		{name: "above explicit staleAfter", yaml: "workerStaleAfter: 30m\nworkerPruneAfter: 31m", want: 31 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, tc.yaml)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.WorkerPruneAfterDuration(); got != tc.want {
				t.Errorf("WorkerPruneAfterDuration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoad_MaxPendingAge(t *testing.T) {
	cfg, err := loadWith(t, "maxPendingAge: 45m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.MaxPendingAgeDuration(); got != 45*time.Minute {
		t.Errorf("MaxPendingAgeDuration = %v, want 45m", got)
	}

	cfg, err = loadWith(t, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.MaxPendingAgeDuration(); got != 0 {
		t.Errorf("MaxPendingAgeDuration = %v, want 0 when unset (caller keeps the 10m default)", got)
	}

	for _, bad := range []string{"maxPendingAge: later", "maxPendingAge: 0s", "maxPendingAge: -5m"} {
		if _, err := loadWith(t, bad); err == nil || !strings.Contains(err.Error(), "maxPendingAge") {
			t.Errorf("%s: err = %v, want a maxPendingAge validation error", bad, err)
		}
	}
}
