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
