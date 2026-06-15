package config

import (
	"os"
	"path/filepath"
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
