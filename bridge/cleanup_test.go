package bridge

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCleanup_ReapsStuckPending(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["gha-orchard-test-old"] = &orchard.VM{
		Name:      "gha-orchard-test-old",
		Status:    orchard.VMStatusCreating,
		CreatedAt: time.Now().Add(-15 * time.Minute),
	}
	mock.vms["gha-orchard-test-fresh"] = &orchard.VM{
		Name:      "gha-orchard-test-fresh",
		Status:    orchard.VMStatusCreating,
		CreatedAt: time.Now().Add(-1 * time.Minute),
	}

	cap := NewCapacity(4)
	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.sweep(context.Background())

	if _, err := mock.GetVM(context.Background(), "gha-orchard-test-old"); err == nil {
		t.Errorf("stuck-pending VM should have been reaped")
	}
	if _, err := mock.GetVM(context.Background(), "gha-orchard-test-fresh"); err != nil {
		t.Errorf("fresh pending VM should remain: %v", err)
	}
}

func TestCleanup_SetMaxWhenAllWorkersGone(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{} // empty

	cap := NewCapacity(7) // previously-set max
	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.sweep(context.Background())

	if got := cap.Max(); got != 0 {
		t.Errorf("Max = %d, want 0 (must propagate zero capacity to listener)", got)
	}
}

func TestCleanup_SetMaxExcludesPausedWorkers(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		{Name: "live", Resources: map[string]uint64{resourceTartVMs: 3}},
		{Name: "paused", Resources: map[string]uint64{resourceTartVMs: 5}, SchedulingPaused: true},
	}

	cap := NewCapacity(20)
	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.sweep(context.Background())

	if got := cap.Max(); got != 3 {
		t.Errorf("Max = %d, want 3 (paused worker's 5 slots must not count)", got)
	}
}

func TestCapacityForLabels_ExcludesPausedWorkers(t *testing.T) {
	workers := []orchard.Worker{
		{Name: "live", Resources: map[string]uint64{resourceTartVMs: 2}, Labels: map[string]string{"arch": "arm"}},
		{Name: "paused", Resources: map[string]uint64{resourceTartVMs: 4}, Labels: map[string]string{"arch": "arm"}, SchedulingPaused: true},
		{Name: "other-arch", Resources: map[string]uint64{resourceTartVMs: 8}, Labels: map[string]string{"arch": "amd"}},
	}

	got := CapacityForLabels(workers, map[string]string{"arch": "arm"})
	if got != 2 {
		t.Errorf("CapacityForLabels = %d, want 2 (only the live arm worker counts)", got)
	}
}

func TestCleanup_SetMaxAgeExtendsReapWindow(t *testing.T) {
	// A running VM older than the 2h default but within a raised 4h window must
	// NOT be reaped once SetMaxAge widens the safety timeout. This is the fix
	// for nightly E2E suites that legitimately run >2h (the macOS VM was being
	// killed mid-run at the 2h DefaultMaxVMAge backstop).
	newVM := func() *orchard.VM {
		return &orchard.VM{
			Name:      "gha-orchard-test-long",
			Status:    orchard.VMStatusRunning,
			CreatedAt: time.Now().Add(-3 * time.Hour),
		}
	}

	// Default 2h max age reaps the 3h-old running VM.
	mockDefault := newMockOrchard()
	mockDefault.vms["gha-orchard-test-long"] = newVM()
	def := NewCleanup(mockDefault, NewCapacity(4), nil, testLogger())
	def.sweep(context.Background())
	if _, err := mockDefault.GetVM(context.Background(), "gha-orchard-test-long"); err == nil {
		t.Errorf("with default 2h maxAge, a 3h-old VM should have been reaped")
	}

	// Raised 4h max age keeps it.
	mockRaised := newMockOrchard()
	mockRaised.vms["gha-orchard-test-long"] = newVM()
	raised := NewCleanup(mockRaised, NewCapacity(4), nil, testLogger())
	raised.SetMaxAge(4 * time.Hour)
	raised.sweep(context.Background())
	if _, err := mockRaised.GetVM(context.Background(), "gha-orchard-test-long"); err != nil {
		t.Errorf("with 4h maxAge, a 3h-old VM should remain: %v", err)
	}

	// A non-positive override is ignored, preserving the default backstop.
	mockZero := newMockOrchard()
	mockZero.vms["gha-orchard-test-long"] = newVM()
	zero := NewCleanup(mockZero, NewCapacity(4), nil, testLogger())
	zero.SetMaxAge(0)
	zero.sweep(context.Background())
	if _, err := mockZero.GetVM(context.Background(), "gha-orchard-test-long"); err == nil {
		t.Errorf("SetMaxAge(0) must be ignored, so the 3h-old VM is still reaped at the 2h default")
	}
}
