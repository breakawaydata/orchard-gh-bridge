package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
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
