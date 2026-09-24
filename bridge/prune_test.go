package bridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/metrics"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// captureLogger returns a logger that records INFO and above into buf.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func pruneWorker(name string, lastSeen time.Time) orchard.Worker {
	return orchard.Worker{
		Name:      name,
		Labels:    map[string]string{PinLabelKey: name},
		Resources: map[string]uint64{resourceTartVMs: 1},
		LastSeen:  lastSeen,
	}
}

func newPruneCleanup(mock *mockOrchardClient, pruneAfter time.Duration, logger *slog.Logger) *Cleanup {
	c := NewCleanup(mock, NewCapacity(4), nil, logger)
	c.SetStateView(NewStateView(mock, time.Minute, DefaultWorkerStaleAfter))
	c.SetWorkerPruneAfter(pruneAfter)
	return c
}

func TestCleanup_PrunesOnlyOfflineUnpausedWorkersWithoutVMs(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	paused := pruneWorker("offline-paused", now.Add(-3*time.Hour))
	paused.SchedulingPaused = true
	mock.workers = []orchard.Worker{
		pruneWorker("live", now.Add(-5*time.Second)),
		// Stale (not counted as capacity) but not yet past the prune threshold.
		pruneWorker("stale-recent", now.Add(-30*time.Minute)),
		pruneWorker("offline-old", now.Add(-2*time.Hour)),
		pruneWorker("offline-with-vm", now.Add(-2*time.Hour)),
		paused,
		// Zero LastSeen means the controller did not report it: fail safe.
		pruneWorker("no-last-seen", time.Time{}),
	}
	// Any VM counts, managed or not, whatever its status.
	mock.vms["manual-vm"] = &orchard.VM{Name: "manual-vm", Worker: "offline-with-vm", Status: orchard.VMStatusStopped}

	reg := metrics.NewRegistry()
	c := newPruneCleanup(mock, time.Hour, testLogger())
	c.SetMetrics(reg)
	c.sweep(context.Background())

	if got := mock.deletedWorkerNames(); !slices.Equal(got, []string{"offline-old"}) {
		t.Errorf("deleted workers = %v, want [offline-old]", got)
	}
	if got := c.prunedTotal.Value(); got != 1 {
		t.Errorf("orchard_gh_bridge_workers_pruned_total = %v, want 1", got)
	}
}

// TestCleanup_PruneNeverTouchesLiveWorkers guards the invariant even against a
// misconfiguration where pruneAfter sits below the staleness threshold, which
// config.Validate rejects: a worker the bridge counts as live is never pruned.
func TestCleanup_PruneNeverTouchesLiveWorkers(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{pruneWorker("live", now.Add(-90*time.Second))}

	c := newPruneCleanup(mock, time.Second, testLogger())
	c.sweep(context.Background())

	if got := mock.deletedWorkerNames(); len(got) != 0 {
		t.Errorf("deleted live workers %v", got)
	}
}

// TestCleanup_PrunesStaleRenamedRecord is the BAE-8010 shape: the old record
// of a renamed machine shares the live worker's pin label and is offline.
func TestCleanup_PrunesStaleRenamedRecord(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		mkRenamedWorker("BreakAwySFMini1.localdomain", "BreakAwySFMini1.localdomain", 10, 32768, now.Add(-4*time.Hour), nil),
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, now, nil),
	}

	c := newPruneCleanup(mock, time.Hour, testLogger())
	c.sweep(context.Background())

	if got := mock.deletedWorkerNames(); !slices.Equal(got, []string{"BreakAwySFMini1.localdomain"}) {
		t.Errorf("deleted workers = %v, want only the stale record", got)
	}
}

func TestCleanup_PruneDisabled(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{pruneWorker("offline-old", time.Now().Add(-48*time.Hour))}

	c := newPruneCleanup(mock, 0, testLogger())
	c.sweep(context.Background())

	if got := mock.deletedWorkerNames(); len(got) != 0 {
		t.Errorf("pruning disabled but deleted %v", got)
	}
}

func TestCleanup_PruneDefaultsOffWithoutConfig(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{pruneWorker("offline-old", time.Now().Add(-48*time.Hour))}

	c := NewCleanup(mock, NewCapacity(4), nil, testLogger())
	c.SetStateView(NewStateView(mock, time.Minute, DefaultWorkerStaleAfter))
	c.sweep(context.Background())

	if got := mock.deletedWorkerNames(); len(got) != 0 {
		t.Errorf("Cleanup without SetWorkerPruneAfter deleted %v", got)
	}
}

// TestCleanup_PruneSkipsStaleSnapshot: when the StateView cannot refresh it
// returns its last snapshot, which may be hours old. Judging that against the
// clock would make every worker look offline, so no prune happens.
func TestCleanup_PruneSkipsStaleSnapshot(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{pruneWorker("offline-old", now.Add(-2*time.Hour))}

	c := NewCleanup(mock, NewCapacity(4), nil, testLogger())
	sv := NewStateView(mock, time.Nanosecond, DefaultWorkerStaleAfter)
	c.SetStateView(sv)
	c.SetWorkerPruneAfter(time.Hour)
	if _, err := sv.Get(context.Background()); err != nil {
		t.Fatal(err)
	}

	mock.mu.Lock()
	mock.listWorkersErr = errors.New("connection refused")
	mock.mu.Unlock()
	time.Sleep(time.Millisecond)
	c.sweep(context.Background())

	if got := mock.deletedWorkerNames(); len(got) != 0 {
		t.Errorf("pruned %v from a snapshot that failed to refresh", got)
	}
}

func TestCleanup_PruneSkipLoggedOncePerState(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	paused := pruneWorker("paused", now.Add(-2*time.Hour))
	paused.SchedulingPaused = true
	mock.workers = []orchard.Worker{paused}

	var buf bytes.Buffer
	c := newPruneCleanup(mock, time.Hour, captureLogger(&buf))
	for range 3 {
		c.state.Invalidate()
		c.sweep(context.Background())
	}

	if n := strings.Count(buf.String(), "not pruning offline worker"); n != 1 {
		t.Errorf("skip logged %d times over 3 sweeps, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "scheduling paused") {
		t.Errorf("skip log does not name the reason:\n%s", buf.String())
	}
	if got := mock.deletedWorkerNames(); len(got) != 0 {
		t.Errorf("deleted paused worker: %v", got)
	}
}

func TestCleanup_PruneFailureIsRetried(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{pruneWorker("offline-old", time.Now().Add(-2*time.Hour))}
	mock.deleteWorkerErr = errors.New("connection refused")

	c := newPruneCleanup(mock, time.Hour, testLogger())
	c.sweep(context.Background())
	if got := mock.deletedWorkerNames(); len(got) != 0 {
		t.Fatalf("deleted despite error: %v", got)
	}

	mock.mu.Lock()
	mock.deleteWorkerErr = nil
	mock.mu.Unlock()
	c.state.Invalidate()
	c.sweep(context.Background())
	if got := mock.deletedWorkerNames(); !slices.Equal(got, []string{"offline-old"}) {
		t.Errorf("deleted workers after recovery = %v, want [offline-old]", got)
	}
}

func TestCleanup_PrunedWorkerLogsAtInfo(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{pruneWorker("offline-old", time.Now().Add(-2*time.Hour))}

	var buf bytes.Buffer
	c := newPruneCleanup(mock, time.Hour, captureLogger(&buf))
	c.sweep(context.Background())

	if !strings.Contains(buf.String(), "level=INFO msg=\"pruned offline worker\" component=cleanup worker=offline-old") {
		t.Errorf("missing INFO prune log:\n%s", buf.String())
	}
}

func TestCleanup_SetMaxPendingAge(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["gha-orchard-test-pulling"] = &orchard.VM{
		Name:      "gha-orchard-test-pulling",
		Status:    orchard.VMStatusCreating,
		CreatedAt: time.Now().Add(-25 * time.Minute),
	}

	c := NewCleanup(mock, NewCapacity(4), nil, testLogger())
	c.SetMaxPendingAge(45 * time.Minute)
	c.SetMaxPendingAge(0) // ignored: keeps 45m
	c.sweep(context.Background())

	if mock.vmCount() != 1 {
		t.Errorf("VM pending for 25m was reaped with maxPendingAge=45m")
	}
}

func TestCleanup_OnSweepReceivesLiveWorkers(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		pruneWorker("live", now),
		pruneWorker("stale", now.Add(-10*time.Minute)),
	}
	c := newPruneCleanup(mock, 0, testLogger())
	var got []string
	c.SetOnSweep(func(ws []orchard.Worker) {
		for _, w := range ws {
			got = append(got, w.Name)
		}
	})
	c.sweep(context.Background())
	if !slices.Equal(got, []string{"live"}) {
		t.Errorf("onSweep workers = %v, want [live]", got)
	}
}
