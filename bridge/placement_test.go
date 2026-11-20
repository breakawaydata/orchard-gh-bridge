package bridge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// TestAutoSizePlacement_SpreadsAcrossFreeWorkers reproduces the starvation seen
// in dev on 2026-09-14: over 8 consecutive scale-ups the fleet placed 4 VMs on
// one mini, 2 and 2 on the next two, and zero on the last worker in Orchard's
// list order — which sat idle for 40 minutes while jobs queued. The machine was
// healthy; a VM pinned to it by hand booted in 13 seconds.
//
// GitHub normally offers one job per message, so every scale-up asks for one VM
// and took candidates[0]. Whichever worker sorts last is never reached until it
// is the only free one.
func TestAutoSizePlacement_SpreadsAcrossFreeWorkers(t *testing.T) {
	const workerCount = 4

	mock := newMockOrchard()
	now := time.Now()
	for _, name := range []string{"w-a", "w-b", "w-c", "w-d"} {
		w := mkWorker(name, 12, 36864, 1, map[string]string{"vm-class-large": "true"})
		w.LastSeen = now
		mock.workers = append(mock.workers, w)
	}

	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			Labels:   map[string]string{"vm-class-large": "true"},
			AutoSize: config.AutoSizeConfig{Enabled: true},
		},
		OrchardClient: mock,
		Capacity:      NewCapacity(workerCount),
		State:         NewStateView(mock, time.Millisecond, DefaultWorkerStaleAfter),
		Logger:        testLogger(),
	})

	// Record which worker each placement targets, and free it again so the next
	// scale-up sees the same full set of candidates — the drip-feed case.
	placements := map[string]int{}
	b.testCreateOneVM = func(_ context.Context, _, _ uint64, labels map[string]string, _ []any) error {
		placements[labels[PinLabelKey]]++
		return nil
	}

	for i := 0; i < workerCount*3; i++ {
		if _, err := b.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
			t.Fatalf("scale-up %d: %v", i, err)
		}
		b.capacity.Reconcile(0)
	}

	if len(placements) != workerCount {
		t.Errorf("only %d of %d workers ever received a VM: %v", len(placements), workerCount, placements)
	}
	for _, name := range []string{"w-a", "w-b", "w-c", "w-d"} {
		if placements[name] == 0 {
			t.Errorf("worker %q never received a VM: %v", name, placements)
		}
	}
}

// TestAutoSizePlacement_TwoSlotWorkerTakesTwoVMs: a worker advertising two
// tart-vms slots hosts two VMs, each sized as half the host after the reserves
// (18 cores, 64 GiB, reserves 4 / 4096 -> 7 CPU / 30720 MiB), and a further
// scale-up with both present creates nothing.
func TestAutoSizePlacement_TwoSlotWorkerTakesTwoVMs(t *testing.T) {
	mock := newMockOrchard()
	w := slotWorker("sfmini", 18, 65536, 2)
	w.LastSeen = time.Now()
	mock.workers = []orchard.Worker{w}

	b := New(Config{
		ScaleSetName:  "test",
		VMConfig:      config.VMConfig{AutoSize: config.AutoSizeConfig{Enabled: true}},
		OrchardClient: mock,
		Capacity:      NewCapacity(10),
		State:         NewStateView(mock, time.Millisecond, DefaultWorkerStaleAfter),
		Logger:        testLogger(),
	})

	type placement struct {
		cpu, mem uint64
		pin      string
	}
	var placed []placement
	b.testCreateOneVM = func(_ context.Context, cpu, mem uint64, labels map[string]string, _ []any) error {
		placed = append(placed, placement{cpu, mem, labels[PinLabelKey]})
		// Record the VM as Orchard would, pending and pinned, so the next
		// scale-up sees the slot as taken.
		name := fmt.Sprintf("gha-orchard-test-%08d", len(placed))
		mock.mu.Lock()
		mock.vms[name] = &orchard.VM{Name: name, Status: orchard.VMStatusCreating, Labels: labels}
		mock.mu.Unlock()
		return nil
	}

	if _, err := b.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("first scale-up: %v", err)
	}
	want := []placement{{7, 30720, "sfmini"}, {7, 30720, "sfmini"}}
	if len(placed) != 2 || placed[0] != want[0] || placed[1] != want[1] {
		t.Fatalf("placements = %+v, want %+v", placed, want)
	}

	if _, err := b.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("second scale-up: %v", err)
	}
	if len(placed) != 2 {
		t.Errorf("created %d VMs in total, want 2 (both slots already taken)", len(placed))
	}
}

// TestAutoSizePlacement_SecondSlotWaitsForOtherWorkersAcrossBatches: with an
// idle two-slot worker a and an idle one-slot worker b, every two-VM batch must
// use b before it doubles up on a. The slot list is [a, b, a]; rotating that
// list by the placement cursor after it was expanded made the second batch pick
// [a, a] and left b idle, so the rotation has to happen over workers.
func TestAutoSizePlacement_SecondSlotWaitsForOtherWorkersAcrossBatches(t *testing.T) {
	mock := newMockOrchard()
	now := time.Now()
	a := slotWorker("a", 18, 65536, 2)
	a.LastSeen = now
	bw := slotWorker("b", 10, 16384, 1)
	bw.LastSeen = now
	mock.workers = []orchard.Worker{a, bw}

	b := New(Config{
		ScaleSetName:  "test",
		VMConfig:      config.VMConfig{AutoSize: config.AutoSizeConfig{Enabled: true}},
		OrchardClient: mock,
		Capacity:      NewCapacity(10),
		State:         NewStateView(mock, time.Millisecond, DefaultWorkerStaleAfter),
		Logger:        testLogger(),
	})

	var placed []string
	b.testCreateOneVM = func(_ context.Context, _, _ uint64, labels map[string]string, _ []any) error {
		placed = append(placed, labels[PinLabelKey])
		return nil // not recorded in Orchard: each batch sees both workers idle
	}

	for batch := 0; batch < 2; batch++ {
		placed = nil
		if _, err := b.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
			t.Fatalf("batch %d: %v", batch, err)
		}
		b.capacity.Reconcile(0)
		if len(placed) != 2 || placed[0] == placed[1] {
			t.Errorf("batch %d placed %v, want one VM on each of a and b", batch, placed)
		}
	}
}
