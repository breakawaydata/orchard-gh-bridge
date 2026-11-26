package bridge

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// The dev fleet's shapes: two 16 GiB M4 minis, a 36 GiB M3 Pro and a 64 GiB
// two-slot M5 Pro. With the default reserves the minis give 6 CPU / 12 GiB,
// which is what autoSize.minMemoryMiB exists to keep a scale set away from.
func minMemFleet() []orchard.Worker {
	large := map[string]string{"vm-class-large": "true"}
	now := time.Now()
	ws := []orchard.Worker{
		mkWorker("mini1", 10, 16384, 1, large),
		mkWorker("m3pro", 12, 36864, 1, large),
		mkWorker("mini2", 10, 16384, 1, large),
		mkWorker("m5pro", 18, 65536, 2, large),
	}
	for i := range ws {
		ws[i].LastSeen = now
	}
	return ws
}

const fleetMinMemMiB = 32768

// minMemBridge builds an AutoSize bridge over mock with the given floor, and
// records every placement's pin label in placed. Each placement is recorded in
// the mock as a pending pinned VM and in the bridge's active set, as
// createOneVM would, so a later scale-up sees its slot as taken.
func minMemBridge(mock *mockOrchardClient, minMemMiB uint64, placed *[]string, logger *bytes.Buffer) *Bridge {
	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			Labels:   map[string]string{"vm-class-large": "true"},
			AutoSize: config.AutoSizeConfig{Enabled: true, MinMemoryMiB: minMemMiB},
		},
		OrchardClient: mock,
		Capacity:      NewCapacity(100),
		State:         NewStateView(mock, time.Millisecond, DefaultWorkerStaleAfter),
		Logger:        captureLogger(logger),
	})
	b.testCreateOneVM = func(_ context.Context, _, _ uint64, labels map[string]string, _ []any) error {
		*placed = append(*placed, labels[PinLabelKey])
		name := fmt.Sprintf("gha-orchard-test-%08d", len(*placed))
		mock.mu.Lock()
		mock.vms[name] = &orchard.VM{Name: name, Status: orchard.VMStatusCreating, Labels: labels}
		mock.mu.Unlock()
		// Track it as createOneVM does, so the next desired count sees it.
		b.mu.Lock()
		b.activeVMs[name] = name
		b.mu.Unlock()
		return nil
	}
	return b
}

func TestAutoSizeExclusionReason_MinMemory(t *testing.T) {
	counts := map[string]int{"w": 1}
	tests := []struct {
		name   string
		worker orchard.Worker
		minMem uint64
		want   string
	}{
		{"unset keeps a small worker", mkWorker("w", 10, 16384, 1, nil), 0, ""},
		{"below the floor", mkWorker("w", 10, 16384, 1, nil), fleetMinMemMiB, ExclusionBelowMinMemory},
		{"exactly the floor qualifies", mkWorker("w", 12, 32768, 1, nil), fleetMinMemMiB, ""},
		{"above the floor", mkWorker("w", 12, 36864, 1, nil), fleetMinMemMiB, ""},
		// The floor is on the host's advertised memory, not the per-slot share:
		// 64 GiB over two slots is 30 GiB per VM, and the host still qualifies.
		{"multi-slot host judged on its total", mkWorker("w", 18, 65536, 2, nil), fleetMinMemMiB, ""},
		// A real fault is reported ahead of the scale set's own floor.
		{"below reserves wins over the floor", mkWorker("w", 4, 16384, 1, nil), fleetMinMemMiB, ExclusionBelowReserves},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutoSizeExclusionReason(tc.worker, counts, 4, 4096, tc.minMem); got != tc.want {
				t.Errorf("AutoSizeExclusionReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// Unset: placement, slot count and worker count are exactly what they were
// before the option existed.
func TestMinMemory_UnsetIsUnchanged(t *testing.T) {
	ws := minMemFleet()
	labels := map[string]string{"vm-class-large": "true"}
	for cursor := uint64(0); cursor < 8; cursor++ {
		before := workerNames(freeAutoSizeSlots(ws, nil, labels, 4, 4096, 0, cursor))
		want := map[uint64][]string{
			0: {"mini1", "m3pro", "mini2", "m5pro", "m5pro"},
			1: {"m3pro", "mini2", "m5pro", "mini1", "m5pro"},
		}
		if w, ok := want[cursor]; ok && !equalStrings(before, w) {
			t.Errorf("cursor %d: freeAutoSizeSlots = %v, want %v", cursor, before, w)
		}
	}
	if got := SlotCountForLabels(ws, labels, 4, 4096, 0); got != 5 {
		t.Errorf("SlotCountForLabels = %d, want 5", got)
	}
	if got := WorkerCountForLabels(ws, labels, 4, 4096, 0); got != 4 {
		t.Errorf("WorkerCountForLabels = %d, want 4", got)
	}

	mock := newMockOrchard()
	mock.workers = ws
	var placed []string
	var logs bytes.Buffer
	b := minMemBridge(mock, 0, &placed, &logs)
	if _, err := b.HandleDesiredRunnerCount(context.Background(), 5); err != nil {
		t.Fatalf("scale-up: %v", err)
	}
	if !equalStrings(placed, []string{"mini1", "m3pro", "mini2", "m5pro", "m5pro"}) {
		t.Errorf("placed = %v, want every slot including both minis", placed)
	}
}

// Set: the small workers are never offered, and the scale set's capacity counts
// only the slots on the workers that qualify.
func TestMinMemory_SkipsSmallWorkers(t *testing.T) {
	ws := minMemFleet()
	labels := map[string]string{"vm-class-large": "true"}
	if got := workerNames(freeAutoSizeSlots(ws, nil, labels, 4, 4096, fleetMinMemMiB, 0)); !equalStrings(got, []string{"m3pro", "m5pro", "m5pro"}) {
		t.Errorf("freeAutoSizeSlots = %v, want [m3pro m5pro m5pro]", got)
	}
	if got := SlotCountForLabels(ws, labels, 4, 4096, fleetMinMemMiB); got != 3 {
		t.Errorf("SlotCountForLabels = %d, want 3", got)
	}
	if got := WorkerCountForLabels(ws, labels, 4, 4096, fleetMinMemMiB); got != 2 {
		t.Errorf("WorkerCountForLabels = %d, want 2", got)
	}

	mock := newMockOrchard()
	mock.workers = ws
	var placed []string
	var logs bytes.Buffer
	b := minMemBridge(mock, fleetMinMemMiB, &placed, &logs)
	// Ask for more than qualify: only the three big slots are used.
	if _, err := b.HandleDesiredRunnerCount(context.Background(), 5); err != nil {
		t.Fatalf("scale-up: %v", err)
	}
	if len(placed) != 3 {
		t.Fatalf("placed %d VMs (%v), want 3", len(placed), placed)
	}
	for _, pin := range placed {
		if pin == "mini1" || pin == "mini2" {
			t.Errorf("placed a VM on small worker %q: %v", pin, placed)
		}
	}
}

// None qualify: with only small workers free the job waits, exactly as when no
// worker is free. Nothing is created, no error is returned, nothing falls back
// to a small worker, and one line says why. A big worker freeing up later is
// used at once.
func TestMinMemory_NoneQualifyWaits(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = minMemFleet()
	var placed []string
	var logs bytes.Buffer
	b := minMemBridge(mock, fleetMinMemMiB, &placed, &logs)

	// Fill the three big slots.
	if _, err := b.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatalf("filling big slots: %v", err)
	}
	if len(placed) != 3 {
		t.Fatalf("placed %d VMs filling the big slots, want 3", len(placed))
	}
	logs.Reset()

	// Two more jobs; the two minis are idle but below the floor.
	got, err := b.HandleDesiredRunnerCount(context.Background(), 5)
	if err != nil {
		t.Fatalf("scale-up with none qualifying returned an error: %v", err)
	}
	if got != 3 {
		t.Errorf("HandleDesiredRunnerCount = %d, want 3 (unchanged)", got)
	}
	if len(placed) != 3 {
		t.Errorf("created VMs with none qualifying: %v", placed)
	}
	if n := b.capacity.InUse(); n != 3 {
		t.Errorf("capacity in use = %d, want 3 (none leaked by the waiting scale-up)", n)
	}
	out := logs.String()
	if strings.Count(out, "no free autoSize worker slots") != 1 {
		t.Errorf("want exactly one wait line, got:\n%s", out)
	}
	for _, want := range []string{"minMemoryMiB=32768", "freeSlotsBelowMinMemory=2", "reason="} {
		if !strings.Contains(out, want) {
			t.Errorf("wait line missing %q:\n%s", want, out)
		}
	}

	// The m3pro's job finishes: the next scale-up goes there, not to a mini.
	mock.mu.Lock()
	for name, vm := range mock.vms {
		if vm.Labels[PinLabelKey] == "m3pro" {
			delete(mock.vms, name)
			b.mu.Lock()
			delete(b.activeVMs, name)
			b.mu.Unlock()
		}
	}
	mock.mu.Unlock()
	b.capacity.Release(1)
	b.state.Invalidate() // as HandleJobCompleted does
	if _, err := b.HandleDesiredRunnerCount(context.Background(), 4); err != nil {
		t.Fatalf("scale-up after a big slot freed: %v", err)
	}
	if len(placed) != 4 || placed[3] != "m3pro" {
		t.Errorf("placed = %v, want the freed m3pro slot used next", placed)
	}
}

// The placement cursor rotates over the qualifying workers only: one-VM
// scale-ups that each free their slot walk m3pro and m5pro in turn, and never
// reach a mini however long the run.
func TestMinMemory_CursorRotatesAmongQualifying(t *testing.T) {
	ws := minMemFleet()
	labels := map[string]string{"vm-class-large": "true"}
	tests := []struct {
		cursor uint64
		want   []string
	}{
		{0, []string{"m3pro", "m5pro", "m5pro"}},
		{1, []string{"m5pro", "m3pro", "m5pro"}},
		{2, []string{"m3pro", "m5pro", "m5pro"}},
		{3, []string{"m5pro", "m3pro", "m5pro"}},
	}
	for _, tc := range tests {
		if got := workerNames(freeAutoSizeSlots(ws, nil, labels, 4, 4096, fleetMinMemMiB, tc.cursor)); !equalStrings(got, tc.want) {
			t.Errorf("cursor %d: freeAutoSizeSlots = %v, want %v", tc.cursor, got, tc.want)
		}
	}

	mock := newMockOrchard()
	mock.workers = ws
	var logs bytes.Buffer
	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			Labels:   labels,
			AutoSize: config.AutoSizeConfig{Enabled: true, MinMemoryMiB: fleetMinMemMiB},
		},
		OrchardClient: mock,
		Capacity:      NewCapacity(100),
		State:         NewStateView(mock, time.Millisecond, DefaultWorkerStaleAfter),
		Logger:        captureLogger(&logs),
	})
	placements := map[string]int{}
	var order []string
	b.testCreateOneVM = func(_ context.Context, _, _ uint64, labels map[string]string, _ []any) error {
		placements[labels[PinLabelKey]]++
		order = append(order, labels[PinLabelKey])
		return nil
	}
	for i := 0; i < 8; i++ {
		if _, err := b.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
			t.Fatalf("scale-up %d: %v", i, err)
		}
		// Drip-feed: the job finishes before the next one arrives.
		b.mu.Lock()
		b.activeVMs = map[string]string{}
		b.mu.Unlock()
		b.capacity.Reconcile(0)
	}
	if placements["mini1"] != 0 || placements["mini2"] != 0 {
		t.Errorf("a mini received a VM: %v", placements)
	}
	if placements["m3pro"] != 4 || placements["m5pro"] != 4 {
		t.Errorf("placements = %v (order %v), want 4 each on m3pro and m5pro", placements, order)
	}
}

// The eligibility monitor reports a worker under the floor at INFO, not WARN:
// it is the scale set's choice, not a broken worker. The eligible-workers gauge
// counts only the workers that qualify.
func TestEligibilityMonitor_BelowMinMemoryIsInfo(t *testing.T) {
	var buf bytes.Buffer
	m := NewEligibilityMonitor(captureLogger(&buf), nil)
	m.Observe("xlarge", minMemFleet(), map[string]string{"vm-class-large": "true"}, 4, 4096, fleetMinMemMiB)
	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("below_min_memory logged at WARN:\n%s", out)
	}
	if strings.Count(out, "reason="+ExclusionBelowMinMemory) != 2 {
		t.Errorf("want one below_min_memory line per mini:\n%s", out)
	}
}
