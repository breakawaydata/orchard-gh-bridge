package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func TestWorkerLive(t *testing.T) {
	now := time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		lastSeen   time.Time
		staleAfter time.Duration
		want       bool
	}{
		{"just pinged", now.Add(-2 * time.Second), 2 * time.Minute, true},
		{"within threshold", now.Add(-90 * time.Second), 2 * time.Minute, true},
		{"exactly at threshold", now.Add(-2 * time.Minute), 2 * time.Minute, true},
		{"just past threshold", now.Add(-2*time.Minute - time.Second), 2 * time.Minute, false},
		{"gone for a month", now.Add(-30 * 24 * time.Hour), 2 * time.Minute, false},
		{"zero LastSeen fails open", time.Time{}, 2 * time.Minute, true},
		{"disabled by non-positive threshold", now.Add(-30 * 24 * time.Hour), 0, true},
		// A clock skew between controller and bridge must not evict a worker.
		{"future LastSeen", now.Add(time.Minute), 2 * time.Minute, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := orchard.Worker{Name: "w1", LastSeen: tc.lastSeen}
			if got := WorkerLive(w, tc.staleAfter, now); got != tc.want {
				t.Errorf("WorkerLive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLiveWorkers_PreservesOrderAndKeepsPaused(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		{Name: "live-1", LastSeen: now.Add(-3 * time.Second)},
		{Name: "dead", LastSeen: now.Add(-30 * 24 * time.Hour)},
		// Paused but present: staleness and pausing are orthogonal, and the
		// capacity predicates already handle SchedulingPaused themselves.
		{Name: "paused", LastSeen: now.Add(-3 * time.Second), SchedulingPaused: true},
		{Name: "live-2", LastSeen: now.Add(-1 * time.Second)},
	}

	got := LiveWorkers(workers, DefaultWorkerStaleAfter, now)

	want := []string{"live-1", "paused", "live-2"}
	if len(got) != len(want) {
		t.Fatalf("got %d workers, want %d: %+v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("position %d = %q, want %q", i, got[i].Name, name)
		}
	}
}

// TestWorkerCountForLabels_ExcludesOfflineWorker reproduces the September 2026
// dev incident: a Mac was wiped and re-registered under a new name, leaving the
// old registration behind. The old one was never paused and still satisfied the
// AutoSize pin invariant (label == name), so it kept counting as a slot a month
// after the machine stopped heartbeating. The bridge acquired a real GitHub job
// for that phantom slot on every scale-up, so the job could never start.
func TestWorkerCountForLabels_ExcludesOfflineWorker(t *testing.T) {
	now := time.Now()
	labels := map[string]string{"vm-class-large": "true"}

	live := mkWorker("BreakAwySFMini1.localdomain", 10, 16384, 1, labels)
	live.LastSeen = now.Add(-5 * time.Second)

	// Same physical machine as the live one, under its pre-wipe hostname.
	abandoned := mkWorker("Nikkis-MBP.localdomain", 11, 36864, 1, labels)
	abandoned.LastSeen = now.Add(-30 * 24 * time.Hour)

	all := []orchard.Worker{live, abandoned}

	// Before filtering, the abandoned registration inflates capacity.
	if got := WorkerCountForLabels(all, labels, 4, 4096); got != 2 {
		t.Fatalf("unfiltered capacity = %d, want 2 (precondition)", got)
	}

	got := WorkerCountForLabels(LiveWorkers(all, DefaultWorkerStaleAfter, now), labels, 4, 4096)
	if got != 1 {
		t.Errorf("capacity with offline worker excluded = %d, want 1", got)
	}
}

// TestNewSnapshot_DropsStaleWorkers pins the invariant that every consumer of a
// Snapshot relies on: Workers holds only machines that still exist, while
// AllWorkers keeps the raw list for diagnostics.
func TestNewSnapshot_DropsStaleWorkers(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		{Name: "live", LastSeen: now.Add(-time.Second)},
		{Name: "gone", LastSeen: now.Add(-time.Hour)},
	}

	snap := newSnapshot(nil, workers, now, DefaultWorkerStaleAfter)

	if len(snap.Workers) != 1 || snap.Workers[0].Name != "live" {
		t.Errorf("Workers = %+v, want only [live]", snap.Workers)
	}
	if len(snap.AllWorkers) != 2 {
		t.Errorf("AllWorkers = %d entries, want 2 (raw list preserved)", len(snap.AllWorkers))
	}
	// The by-name index must agree with Workers, or placement lookups would
	// resurrect a worker the capacity math already wrote off.
	if _, ok := snap.workersByName["gone"]; ok {
		t.Error("workersByName still indexes the stale worker")
	}
}

// TestCleanup_StrandedVMDoesNotBlockHealthyWorker guards the accounting
// symmetry that filtering stale workers depends on.
//
// Dropping a stale worker removes its slots from max. If the VM still pinned to
// that worker kept counting toward current, Available() would fall by one for
// every stranded VM — so with two one-slot workers and one stranded VM,
// max=1/current=1 leaves zero available and the healthy worker cannot take a
// replacement until maxAge (2h by default) reaps the VM.
func TestCleanup_StrandedVMDoesNotBlockHealthyWorker(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		{
			Name:      "live-worker",
			LastSeen:  now.Add(-2 * time.Second),
			Resources: map[string]uint64{resourceTartVMs: 1},
		},
		{
			Name:      "gone-worker",
			LastSeen:  now.Add(-time.Hour),
			Resources: map[string]uint64{resourceTartVMs: 1},
		},
	}
	// Still "running" to Orchard: nothing stopped it, its host just left.
	mock.vms["gha-orchard-test-stranded"] = &orchard.VM{
		Name:      "gha-orchard-test-stranded",
		Status:    orchard.VMStatusRunning,
		Worker:    "gone-worker",
		CreatedAt: now,
	}

	cap := NewCapacity(0)
	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.SetStateView(NewStateView(mock, time.Minute, DefaultWorkerStaleAfter))
	cleanup.sweep(context.Background())

	if got := cap.Max(); got != 1 {
		t.Fatalf("Max = %d, want 1 (only the live worker's slot)", got)
	}
	if got := cap.InUse(); got != 0 {
		t.Errorf("InUse = %d, want 0 (the stranded VM's worker is excluded from Max too)", got)
	}
	if got := cap.Available(); got != 1 {
		t.Errorf("Available = %d, want 1; the healthy worker must still be able to take a job", got)
	}

	// The VM itself is left alone — maxAge is the backstop, and it must come
	// back into the count if its worker returns.
	if _, err := mock.GetVM(context.Background(), "gha-orchard-test-stranded"); err != nil {
		t.Errorf("stranded VM should not be reaped by this path: %v", err)
	}
}

// The mirror case: a VM on a live worker still occupies a slot.
func TestCleanup_VMOnLiveWorkerStillCounts(t *testing.T) {
	now := time.Now()
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		{
			Name:      "live-worker",
			LastSeen:  now.Add(-2 * time.Second),
			Resources: map[string]uint64{resourceTartVMs: 1},
		},
	}
	mock.vms["gha-orchard-test-busy"] = &orchard.VM{
		Name:      "gha-orchard-test-busy",
		Status:    orchard.VMStatusRunning,
		Worker:    "live-worker",
		CreatedAt: now,
	}

	cap := NewCapacity(0)
	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.SetStateView(NewStateView(mock, time.Minute, DefaultWorkerStaleAfter))
	cleanup.sweep(context.Background())

	if got := cap.InUse(); got != 1 {
		t.Errorf("InUse = %d, want 1", got)
	}
	if got := cap.Available(); got != 0 {
		t.Errorf("Available = %d, want 0 (the only slot is busy)", got)
	}
}
