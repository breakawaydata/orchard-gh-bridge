//go:build integration

package orchard

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
)

func integrationClient(t *testing.T) Client {
	t.Helper()

	addr := os.Getenv("ORCHARD_ADDRESS")
	if addr == "" {
		t.Skip("ORCHARD_ADDRESS not set, skipping integration test")
	}

	password := os.Getenv("ORCHARD_PASSWORD")

	return NewClient(config.OrchardConfig{
		Address:  addr,
		Username: "bootstrap-admin",
		Password: password,
		Insecure: true,
	}, slog.Default())
}

func TestIntegration_VMLifecycle(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vmName := "integration-test-vm"

	// Cleanup from any previous failed run
	_ = c.DeleteVM(ctx, vmName)

	// Create VM
	created, err := c.CreateVM(ctx, &VM{
		Name:   vmName,
		Image:  "ghcr.io/cirruslabs/ubuntu:latest",
		CPU:    2,
		Memory: 4096,
		Labels: map[string]string{
			"managed-by": "integration-test",
		},
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if created.Name != vmName {
		t.Errorf("created.Name = %q, want %q", created.Name, vmName)
	}

	// Get VM
	got, err := c.GetVM(ctx, vmName)
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.Name != vmName {
		t.Errorf("GetVM name = %q, want %q", got.Name, vmName)
	}

	// List VMs — should include ours
	vms, err := c.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	found := false
	for _, vm := range vms {
		if vm.Name == vmName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListVMs did not include %q", vmName)
	}

	// Delete VM
	if err := c.DeleteVM(ctx, vmName); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}

	// Verify gone
	_, err = c.GetVM(ctx, vmName)
	if err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestIntegration_ListWorkers(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workers, err := c.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	// In CI with just a controller (no workers), this may be empty — that's fine
	t.Logf("found %d workers", len(workers))
}

func TestIntegration_Ping(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestIntegration_DeleteNonexistent(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Should not error — our client treats 404 on delete as success
	if err := c.DeleteVM(ctx, "nonexistent-vm-12345"); err != nil {
		t.Fatalf("DeleteVM nonexistent: %v", err)
	}
}
