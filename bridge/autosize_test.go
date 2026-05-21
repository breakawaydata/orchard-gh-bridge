package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func mkWorker(name string, cores, memMiB, tartVMs uint64, extraLabels map[string]string) orchard.Worker {
	labels := map[string]string{PinLabelKey: name}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return orchard.Worker{
		Name:   name,
		Labels: labels,
		Resources: map[string]uint64{
			resourceLogicalCores: cores,
			resourceMemoryMiB:    memMiB,
			resourceTartVMs:      tartVMs,
		},
	}
}

func TestAutoSizeEligible(t *testing.T) {
	tests := []struct {
		name   string
		worker orchard.Worker
		want   bool
	}{
		{"complete", mkWorker("w1", 10, 16384, 1, nil), true},
		{"paused",
			orchard.Worker{Name: "w1", SchedulingPaused: true, Labels: map[string]string{PinLabelKey: "w1"}, Resources: map[string]uint64{resourceLogicalCores: 10, resourceMemoryMiB: 16384}},
			false},
		{"missing pin label",
			orchard.Worker{Name: "w1", Resources: map[string]uint64{resourceLogicalCores: 10, resourceMemoryMiB: 16384}},
			false},
		{"wrong pin value",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: "different"}, Resources: map[string]uint64{resourceLogicalCores: 10, resourceMemoryMiB: 16384}},
			false},
		{"no cores resource",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: "w1"}, Resources: map[string]uint64{resourceMemoryMiB: 16384}},
			false},
		{"no memory resource",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: "w1"}, Resources: map[string]uint64{resourceLogicalCores: 10}},
			false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutoSizeEligible(tc.worker); got != tc.want {
				t.Errorf("AutoSizeEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkerCountForLabels(t *testing.T) {
	workers := []orchard.Worker{
		mkWorker("a", 10, 16384, 1, map[string]string{"class": "fat"}),
		mkWorker("b", 12, 32768, 2, map[string]string{"class": "fat"}),
		mkWorker("c", 8, 8192, 1, map[string]string{"class": "thin"}),
		// missing pin label — ineligible
		{Name: "d", Labels: map[string]string{"class": "fat"}, Resources: map[string]uint64{resourceLogicalCores: 10, resourceMemoryMiB: 16384}},
	}
	got := WorkerCountForLabels(workers, map[string]string{"class": "fat"})
	if got != 2 {
		t.Errorf("WorkerCountForLabels = %d, want 2 (a + b; d lacks pin label)", got)
	}
}

func TestFreeAutoSizeWorkers_ExcludesAssigned(t *testing.T) {
	workers := []orchard.Worker{
		mkWorker("a", 10, 16384, 1, nil),
		mkWorker("b", 12, 32768, 1, nil),
		mkWorker("c", 12, 32768, 1, nil),
	}
	vms := []orchard.VM{
		{Name: "gha-orchard-x-aaaaaaaa", Worker: "a", Status: orchard.VMStatusRunning},
		// terminal VMs do not count
		{Name: "gha-orchard-x-bbbbbbbb", Worker: "b", Status: orchard.VMStatusStopped},
		// unmanaged VM also doesn't count
		{Name: "manual", Worker: "c", Status: orchard.VMStatusRunning},
	}

	free := freeAutoSizeWorkers(workers, vms, nil)
	gotNames := make([]string, len(free))
	for i, w := range free {
		gotNames[i] = w.Name
	}
	if len(free) != 2 || gotNames[0] != "b" || gotNames[1] != "c" {
		t.Errorf("freeAutoSizeWorkers names = %v, want [b c]", gotNames)
	}
}

func TestFreeAutoSizeWorkers_ExcludesPendingPinned(t *testing.T) {
	workers := []orchard.Worker{
		mkWorker("a", 10, 16384, 1, nil),
		mkWorker("b", 12, 32768, 1, nil),
	}
	vms := []orchard.VM{
		// pending VM with pin label but no Worker yet — must still block its target
		{
			Name:   "gha-orchard-x-cccccccc",
			Status: orchard.VMStatusCreating,
			Labels: map[string]string{PinLabelKey: "a"},
		},
	}
	free := freeAutoSizeWorkers(workers, vms, nil)
	if len(free) != 1 || free[0].Name != "b" {
		t.Errorf("freeAutoSizeWorkers = %+v, want only [b]", free)
	}
}

func TestAutoSizedVM(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		w := mkWorker("w", 10, 16384, 1, nil)
		cpu, mem, err := AutoSizedVM(w, 4, 4096)
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if cpu != 6 || mem != 12288 {
			t.Errorf("AutoSizedVM = (%d, %d), want (6, 12288)", cpu, mem)
		}
	})

	t.Run("reserve_eats_all_cpu", func(t *testing.T) {
		w := mkWorker("w", 4, 16384, 1, nil)
		_, _, err := AutoSizedVM(w, 4, 4096)
		if err == nil {
			t.Errorf("expected error when reserve >= cores")
		}
	})

	t.Run("reserve_eats_all_mem", func(t *testing.T) {
		w := mkWorker("w", 10, 4096, 1, nil)
		_, _, err := AutoSizedVM(w, 4, 4096)
		if err == nil {
			t.Errorf("expected error when reserve >= memory")
		}
	})

	t.Run("missing_resources", func(t *testing.T) {
		w := orchard.Worker{Name: "w"}
		_, _, err := AutoSizedVM(w, 4, 4096)
		if err == nil {
			t.Errorf("expected error when resources unset")
		}
	})
}

func TestAutoSizeReserves_Defaults(t *testing.T) {
	cpu, mem := AutoSizeReserves(0, 0)
	if cpu != DefaultAutoSizeReserveCPU || mem != DefaultAutoSizeReserveMemoryMiB {
		t.Errorf("AutoSizeReserves(0,0) = (%d, %d), want (%d, %d)", cpu, mem, DefaultAutoSizeReserveCPU, DefaultAutoSizeReserveMemoryMiB)
	}
}

func TestAutoSizeReserves_Overrides(t *testing.T) {
	cpu, mem := AutoSizeReserves(6, 8192)
	if cpu != 6 || mem != 8192 {
		t.Errorf("AutoSizeReserves(6,8192) = (%d, %d), want (6, 8192)", cpu, mem)
	}
}

func TestHandleDesired_AutoSize_BlocksWhenAllWorkersAssigned(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		mkWorker("only-worker", 10, 16384, 1, nil),
	}
	// All workers already have a managed VM → no free autoSize candidates.
	mock.vms["gha-orchard-test-aaaaaaaa"] = &orchard.VM{
		Name:   "gha-orchard-test-aaaaaaaa",
		Worker: "only-worker",
		Status: orchard.VMStatusRunning,
	}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute)

	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			AutoSize: config.AutoSizeConfig{Enabled: true},
		},
		OrchardClient: mock,
		Capacity:      cap,
		State:         sv,
		Logger:        testLogger(),
	})

	// GHClient nil — if the autoSize gate fails to short-circuit,
	// createOneVM will panic when generating the JIT config.
	got, err := b.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 {
		t.Errorf("active count = %d, want 0 (no free autoSize worker)", got)
	}
	if cap.InUse() != 0 {
		t.Errorf("capacity in use = %d, want 0", cap.InUse())
	}
	if mock.vmCount() != 1 {
		t.Errorf("VM count = %d, want 1 (existing only; no new VMs)", mock.vmCount())
	}
}

func TestHandleDesired_AutoSize_BlocksWhenWorkerIneligible(t *testing.T) {
	mock := newMockOrchard()
	// Worker is missing the pin label (not self-labeled) → ineligible for AutoSize.
	mock.workers = []orchard.Worker{
		{
			Name: "unlabeled",
			Resources: map[string]uint64{
				resourceLogicalCores: 10,
				resourceMemoryMiB:    16384,
				resourceTartVMs:      1,
			},
		},
	}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute)

	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			AutoSize: config.AutoSizeConfig{Enabled: true},
		},
		OrchardClient: mock,
		Capacity:      cap,
		State:         sv,
		Logger:        testLogger(),
	})

	got, err := b.HandleDesiredRunnerCount(context.Background(), 2)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 {
		t.Errorf("active count = %d, want 0 (worker ineligible)", got)
	}
	if mock.vmCount() != 0 {
		t.Errorf("VM count = %d, want 0", mock.vmCount())
	}
}
