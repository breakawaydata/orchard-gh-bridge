package bridge

import (
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
