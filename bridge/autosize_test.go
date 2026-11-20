package bridge

import (
	"context"
	"errors"
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

// mkRenamedWorker is a worker whose Orchard Name no longer matches its pin
// label: the SF Mini 1 incident, where a macOS upgrade changed the hostname
// (and so the default worker Name) but the launchd plist kept the old label.
func mkRenamedWorker(name, pin string, cores, memMiB uint64, lastSeen time.Time, extraLabels map[string]string) orchard.Worker {
	w := mkWorker(name, cores, memMiB, 1, extraLabels)
	w.Labels[PinLabelKey] = pin
	w.LastSeen = lastSeen
	return w
}

func TestAutoSizeExclusionReason(t *testing.T) {
	res := map[string]uint64{resourceLogicalCores: 10, resourceMemoryMiB: 16384}
	tests := []struct {
		name   string
		worker orchard.Worker
		counts map[string]int
		want   string
	}{
		{"complete", mkWorker("w1", 10, 16384, 1, nil), map[string]int{"w1": 1}, ""},
		{"label differs from name is still eligible",
			orchard.Worker{Name: "BreakAway-SF-Mac-Mini-1.local", Labels: map[string]string{PinLabelKey: "BreakAwySFMini1.localdomain"}, Resources: res},
			map[string]int{"BreakAwySFMini1.localdomain": 1}, ""},
		{"paused",
			orchard.Worker{Name: "w1", SchedulingPaused: true, Labels: map[string]string{PinLabelKey: "w1"}, Resources: res},
			map[string]int{"w1": 1}, ExclusionSchedulingPaused},
		{"missing pin label",
			orchard.Worker{Name: "w1", Resources: res},
			nil, ExclusionMissingPinLabel},
		{"empty pin label",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: ""}, Resources: res},
			nil, ExclusionMissingPinLabel},
		{"duplicate pin label",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: "shared"}, Resources: res},
			map[string]int{"shared": 2}, ExclusionDuplicatePinLabel},
		{"no cores resource",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: "w1"}, Resources: map[string]uint64{resourceMemoryMiB: 16384}},
			map[string]int{"w1": 1}, ExclusionMissingResources},
		{"no memory resource",
			orchard.Worker{Name: "w1", Labels: map[string]string{PinLabelKey: "w1"}, Resources: map[string]uint64{resourceLogicalCores: 10}},
			map[string]int{"w1": 1}, ExclusionMissingResources},
		{"cores exactly equal to reserve",
			mkWorker("w1", 4, 16384, 1, nil), map[string]int{"w1": 1}, ExclusionBelowReserves},
		{"memory exactly equal to reserve",
			mkWorker("w1", 10, 4096, 1, nil), map[string]int{"w1": 1}, ExclusionBelowReserves},
		{"memory below reserve",
			mkWorker("w1", 10, 2048, 1, nil), map[string]int{"w1": 1}, ExclusionBelowReserves},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutoSizeExclusionReason(tc.worker, tc.counts, 4, 4096); got != tc.want {
				t.Errorf("AutoSizeExclusionReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkerCountForLabels_RenamedWorkerIsCounted reproduces BAE-8010: a live
// worker whose Name changed but whose pin label did not must still count as
// AutoSize capacity.
func TestWorkerCountForLabels_RenamedWorkerIsCounted(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, now, nil),
		mkWorker("BreakAwySFMini2.localdomain", 10, 32768, 1, nil),
		mkWorker("BA-Zoom-Mac-Mini.local", 10, 32768, 1, nil),
	}
	if got := WorkerCountForLabels(workers, nil, 4, 4096); got != 3 {
		t.Errorf("WorkerCountForLabels = %d, want 3 (renamed worker must still count)", got)
	}
}

// TestWorkerCountForLabels_DuplicateLiveLabelsExcludeAllHolders: when two live
// workers claim the same pin identity, a VM pinned to it could land on either,
// so neither is usable.
func TestWorkerCountForLabels_DuplicateLiveLabelsExcludeAllHolders(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		mkRenamedWorker("mini-a", "shared", 10, 32768, now, nil),
		mkRenamedWorker("mini-b", "shared", 10, 32768, now, nil),
		mkWorker("c", 10, 32768, 1, nil),
	}
	if got := WorkerCountForLabels(workers, nil, 4, 4096); got != 1 {
		t.Errorf("WorkerCountForLabels = %d, want 1 (both holders of the shared label excluded)", got)
	}
}

// TestWorkerCountForLabels_StaleDuplicateDoesNotBlockLiveWorker is the exact
// incident shape: the old registration still sits in Orchard with the same pin
// label, but it has stopped heartbeating, so once the snapshot filters it out
// the live worker's label is unique.
func TestWorkerCountForLabels_StaleDuplicateDoesNotBlockLiveWorker(t *testing.T) {
	now := time.Now()
	all := []orchard.Worker{
		mkRenamedWorker("BreakAwySFMini1.localdomain", "BreakAwySFMini1.localdomain", 10, 32768, now.Add(-3*time.Hour), nil),
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, now.Add(-5*time.Second), nil),
	}
	live := LiveWorkers(all, DefaultWorkerStaleAfter, now)
	if got := WorkerCountForLabels(live, nil, 4, 4096); got != 1 {
		t.Fatalf("WorkerCountForLabels(live) = %d, want 1", got)
	}
	free := freeAutoSizeSlots(live, nil, nil, 4, 4096)
	if len(free) != 1 || free[0].Name != "BreakAway-SF-Mac-Mini-1.local" {
		t.Errorf("freeAutoSizeSlots = %+v, want the live renamed worker", free)
	}
}

func TestFreeAutoSizeSlots_ScheduledVMMapsBackToPinIdentity(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, now, nil),
		mkWorker("b", 10, 32768, 1, nil),
	}
	// Orchard reports the Name of the worker the VM landed on, not the label.
	vms := []orchard.VM{{
		Name:   "gha-orchard-x-aaaaaaaa",
		Worker: "BreakAway-SF-Mac-Mini-1.local",
		Status: orchard.VMStatusRunning,
		Labels: map[string]string{PinLabelKey: "BreakAwySFMini1.localdomain"},
	}}
	free := freeAutoSizeSlots(workers, vms, nil, 4, 4096)
	if len(free) != 1 || free[0].Name != "b" {
		t.Errorf("freeAutoSizeSlots = %+v, want only [b]", free)
	}
}

func TestFreeAutoSizeSlots_PendingVMMatchedByPinLabel(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, now, nil),
		mkWorker("b", 10, 32768, 1, nil),
	}
	// Pending: no worker yet. Its pin label is the worker's label, not its
	// Name, and must still reserve that worker.
	vms := []orchard.VM{{
		Name:   "gha-orchard-x-bbbbbbbb",
		Status: orchard.VMStatusCreating,
		Labels: map[string]string{PinLabelKey: "BreakAwySFMini1.localdomain"},
	}}
	free := freeAutoSizeSlots(workers, vms, nil, 4, 4096)
	if len(free) != 1 || free[0].Name != "b" {
		t.Errorf("freeAutoSizeSlots = %+v, want only [b]", free)
	}
}

// TestFreeAutoSizeSlots_VMOnRenamedWorkersOldNameStillBlocks: a worker was
// renamed while a VM was running on it. Orchard still attributes the VM to the
// old, now-offline Name, but the machine (live under its new Name, same label)
// may still be running it, so its pin identity must not be handed out again.
func TestFreeAutoSizeSlots_VMOnRenamedWorkersOldNameStillBlocks(t *testing.T) {
	now := time.Now()
	// Only the live renamed worker is in the (live) list; the VM sits on the
	// stale record's Name, which is no longer there.
	workers := []orchard.Worker{
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, now, nil),
	}
	vms := []orchard.VM{{
		Name:   "gha-orchard-x-cccccccc",
		Worker: "BreakAwySFMini1.localdomain",
		Status: orchard.VMStatusRunning,
		Labels: map[string]string{PinLabelKey: "BreakAwySFMini1.localdomain"},
	}}
	if free := freeAutoSizeSlots(workers, vms, nil, 4, 4096); len(free) != 0 {
		t.Errorf("freeAutoSizeSlots = %+v, want none (VM under the old Name still holds the pin identity)", free)
	}

	// Once that VM is gone the renamed worker is free again.
	vms[0].Status = orchard.VMStatusStopped
	if free := freeAutoSizeSlots(workers, vms, nil, 4, 4096); len(free) != 1 {
		t.Errorf("freeAutoSizeSlots = %+v, want the renamed worker once the VM stopped", free)
	}
}

// TestHandleDesired_AutoSize_PinsByLabelNotName: the VM the bridge creates for
// a renamed worker must carry the worker's label value, because that is what
// Orchard matches when it schedules; the Name would match nothing.
func TestHandleDesired_AutoSize_PinsByLabelNotName(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		mkRenamedWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", 10, 32768, time.Now(), nil),
	}
	b := New(Config{
		ScaleSetName:  "test",
		VMConfig:      config.VMConfig{AutoSize: config.AutoSizeConfig{Enabled: true}},
		OrchardClient: mock,
		Capacity:      NewCapacity(10),
		State:         NewStateView(mock, time.Minute, DefaultWorkerStaleAfter),
		Logger:        testLogger(),
	})
	var gotLabels map[string]string
	var gotCPU uint64
	b.testCreateOneVM = func(_ context.Context, cpu, _ uint64, labels map[string]string, _ []any) error {
		gotCPU, gotLabels = cpu, labels
		return nil
	}

	if _, err := b.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if gotLabels == nil {
		t.Fatal("no VM created for the renamed worker")
	}
	if got := gotLabels[PinLabelKey]; got != "BreakAwySFMini1.localdomain" {
		t.Errorf("VM pin label = %q, want the worker's label value", got)
	}
	if gotCPU != 6 {
		t.Errorf("VM cpu = %d, want 6 (sized from the renamed worker)", gotCPU)
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
	got := WorkerCountForLabels(workers, map[string]string{"class": "fat"}, DefaultAutoSizeReserveCPU, DefaultAutoSizeReserveMemoryMiB)
	if got != 2 {
		t.Errorf("WorkerCountForLabels = %d, want 2 (a + b; d lacks pin label)", got)
	}
}

func TestWorkerCountForLabels_ExcludesUndersizedWorkers(t *testing.T) {
	// Workers a and b are label-matching and base-eligible; a is too small to
	// satisfy the configured reserves, so only b should count.
	workers := []orchard.Worker{
		// 4 cores == reserveCPU(4): AutoSizedVM would error; must not be counted.
		mkWorker("a", 4, 32768, 1, map[string]string{"class": "fat"}),
		// 10 cores > reserveCPU(4): schedulable.
		mkWorker("b", 10, 32768, 1, map[string]string{"class": "fat"}),
	}
	got := WorkerCountForLabels(workers, map[string]string{"class": "fat"}, 4, DefaultAutoSizeReserveMemoryMiB)
	if got != 1 {
		t.Errorf("WorkerCountForLabels = %d, want 1 (only b; a has cores == reserve)", got)
	}
}

func TestFreeAutoSizeSlots_ExcludesAssigned(t *testing.T) {
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

	free := freeAutoSizeSlots(workers, vms, nil, DefaultAutoSizeReserveCPU, DefaultAutoSizeReserveMemoryMiB)
	gotNames := make([]string, len(free))
	for i, w := range free {
		gotNames[i] = w.Name
	}
	if len(free) != 2 || gotNames[0] != "b" || gotNames[1] != "c" {
		t.Errorf("freeAutoSizeSlots names = %v, want [b c]", gotNames)
	}
}

func TestFreeAutoSizeSlots_ExcludesPendingPinned(t *testing.T) {
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
	free := freeAutoSizeSlots(workers, vms, nil, DefaultAutoSizeReserveCPU, DefaultAutoSizeReserveMemoryMiB)
	if len(free) != 1 || free[0].Name != "b" {
		t.Errorf("freeAutoSizeSlots = %+v, want only [b]", free)
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
	sv := NewStateView(mock, time.Minute, DefaultWorkerStaleAfter)

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
	sv := NewStateView(mock, time.Minute, DefaultWorkerStaleAfter)

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

func TestHandleDesired_AutoSize_BlocksWhenWorkersTooSmallForReserves(t *testing.T) {
	mock := newMockOrchard()
	// Worker has exactly DefaultAutoSizeReserveCPU cores — AutoSizedVM would
	// error (cores <= reserveCPU). The reserve-aware filter must exclude it from
	// the candidate list so GitHub capacity is reported as 0, not 1.
	mock.workers = []orchard.Worker{
		mkWorker("small", DefaultAutoSizeReserveCPU, 65536, 1, nil),
	}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute, DefaultWorkerStaleAfter)

	b := New(Config{
		ScaleSetName: "test",
		VMConfig: config.VMConfig{
			AutoSize: config.AutoSizeConfig{
				Enabled: true,
				// Zero values → defaults applied: reserveCPU = DefaultAutoSizeReserveCPU.
			},
		},
		OrchardClient: mock,
		Capacity:      cap,
		State:         sv,
		Logger:        testLogger(),
	})

	got, err := b.HandleDesiredRunnerCount(context.Background(), 1)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 {
		t.Errorf("active count = %d, want 0 (worker too small for reserves)", got)
	}
	if cap.InUse() != 0 {
		t.Errorf("capacity in use = %d, want 0 (no slots should have been acquired)", cap.InUse())
	}
	if mock.vmCount() != 0 {
		t.Errorf("VM count = %d, want 0", mock.vmCount())
	}
}

// TestHandleDesired_AutoSize_CapacityReleasedCorrectlyOnCreateFailure verifies
// that when createOneVM fails mid-batch, only the truly unaccounted slots are
// released — not slots that were already individually released for skipped
// workers. With acquired=2, created=1 succeeding then 1 failing, the error
// path must Release(acquired - skipped - created) = Release(1), leaving
// cap.InUse() = 1 (the successfully-created VM still holds its slot).
//
// The skip path (AutoSizedVM returning an error for a freeAutoSizeSlots
// candidate) is a TOCTOU defensive branch; freeAutoSizeSlots now pre-filters
// undersized workers, so skipped=0 in normal operation. This test exercises
// the createOneVM-failure accounting with skipped=0 to confirm the fix.
func TestHandleDesired_AutoSize_CapacityReleasedCorrectlyOnCreateFailure(t *testing.T) {
	mock := newMockOrchard()
	// Two large workers: both pass AutoSizeExclusionReason → both candidates.
	mock.workers = []orchard.Worker{
		mkWorker("w1", 10, 32768, 1, nil),
		mkWorker("w2", 10, 32768, 1, nil),
	}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute, DefaultWorkerStaleAfter)

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

	// First createOneVM call succeeds (created=1); second fails.
	callCount := 0
	b.testCreateOneVM = func(_ context.Context, _, _ uint64, _ map[string]string, _ []any) error {
		callCount++
		if callCount == 1 {
			return nil
		}
		return errors.New("injected create failure")
	}

	_, err := b.HandleDesiredRunnerCount(context.Background(), 2)
	if err == nil {
		t.Fatal("expected error from HandleDesiredRunnerCount, got nil")
	}

	// acquired=2, skipped=0, created=1 → Release(2-0-1)=Release(1)
	// cap.InUse() must be 1 (the first VM still holds its slot).
	if got := cap.InUse(); got != 1 {
		t.Errorf("cap.InUse() = %d, want 1 (one VM created, one slot released on error)", got)
	}
}

// slotWorker is mkWorker with a custom tart-vms resource: a missing resource
// (slots < 0) is dropped entirely rather than set to 0.
func slotWorker(name string, cores, memMiB uint64, slots int64) orchard.Worker {
	w := mkWorker(name, cores, memMiB, 0, nil)
	if slots < 0 {
		delete(w.Resources, resourceTartVMs)
	} else {
		w.Resources[resourceTartVMs] = uint64(slots)
	}
	return w
}

// managedVM is a managed VM as Orchard reports it: worker is the Orchard Name
// it landed on ("" while pending) and pin is the pin label the bridge gave it.
func managedVM(suffix, worker, status, pin string) orchard.VM {
	vm := orchard.VM{Name: "gha-orchard-x-" + suffix, Worker: worker, Status: status}
	if pin != "" {
		vm.Labels = map[string]string{PinLabelKey: pin}
	}
	return vm
}

func workerNames(ws []orchard.Worker) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAutoSizeSlots(t *testing.T) {
	tests := []struct {
		name   string
		worker orchard.Worker
		want   uint64
	}{
		{"one slot", slotWorker("w", 10, 16384, 1), 1},
		{"two slots", slotWorker("w", 10, 16384, 2), 2},
		{"missing resource counts as one", slotWorker("w", 10, 16384, -1), 1},
		{"zero counts as one", slotWorker("w", 10, 16384, 0), 1},
		{"no resources at all", orchard.Worker{Name: "w"}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AutoSizeSlots(tc.worker); got != tc.want {
				t.Errorf("AutoSizeSlots = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestAutoSizedVM_Slots: the reserve is held back once per host and the rest is
// split across the slots, each share floored.
func TestAutoSizedVM_Slots(t *testing.T) {
	tests := []struct {
		name       string
		worker     orchard.Worker
		reserveCPU uint64
		reserveMem uint64
		wantCPU    uint64
		wantMem    uint64
		wantErr    bool
	}{
		{"one slot is whole host minus reserves", slotWorker("w", 10, 16384, 1), 4, 4096, 6, 12288, false},
		{"missing slot count is one slot", slotWorker("w", 10, 16384, -1), 4, 4096, 6, 12288, false},
		{"zero slot count is one slot", slotWorker("w", 10, 16384, 0), 4, 4096, 6, 12288, false},
		{"two slots on the 18 core 64 GiB host", slotWorker("sfmini", 18, 65536, 2), 4, 4096, 7, 30720, false},
		{"odd cpu remainder floors", slotWorker("w", 19, 65536, 2), 4, 4096, 7, 30720, false},
		{"odd memory remainder floors", slotWorker("w", 18, 65537, 2), 4, 4096, 7, 30720, false},
		{"three slots", slotWorker("w", 19, 40960, 3), 4, 4096, 5, 12288, false},
		{"multi-slot share exactly at both floors", slotWorker("w", 6, 4096*3, 2), 4, 4096, 1, 4096, false},
		{"multi-slot cpu share under 1", slotWorker("w", 5, 65536, 2), 4, 4096, 0, 0, true},
		{"multi-slot memory share under the floor", slotWorker("w", 18, 4096+8191, 2), 4, 4096, 0, 0, true},
		{"multi-slot reserve eats all cpu", slotWorker("w", 4, 65536, 2), 4, 4096, 0, 0, true},
		{"multi-slot reserve eats all memory", slotWorker("w", 18, 4096, 2), 4, 4096, 0, 0, true},
		{"one slot memory just above reserve is unchanged", slotWorker("w", 10, 4097, 1), 4, 4096, 6, 1, false},
		{"one slot one cpu left is unchanged", slotWorker("w", 5, 16384, 1), 4, 4096, 1, 12288, false},
		{"one slot reserve eats all cpu", slotWorker("w", 4, 16384, 1), 4, 4096, 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem, err := AutoSizedVM(tc.worker, tc.reserveCPU, tc.reserveMem)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AutoSizedVM error = %v, wantErr %v", err, tc.wantErr)
			}
			if cpu != tc.wantCPU || mem != tc.wantMem {
				t.Errorf("AutoSizedVM = (%d, %d), want (%d, %d)", cpu, mem, tc.wantCPU, tc.wantMem)
			}
		})
	}
}

// TestAutoSizeExclusionReason_Slots: eligibility and AutoSizedVM share one
// computation, so a worker is excluded exactly when AutoSizedVM would fail.
func TestAutoSizeExclusionReason_Slots(t *testing.T) {
	tests := []struct {
		name   string
		worker orchard.Worker
		want   string
	}{
		{"two slots on the 18 core 64 GiB host", slotWorker("w", 18, 65536, 2), ""},
		{"multi-slot share exactly at both floors", slotWorker("w", 6, 4096*3, 2), ""},
		{"multi-slot cpu share under 1", slotWorker("w", 5, 65536, 2), ExclusionBelowReserves},
		{"multi-slot memory share one MiB under the floor", slotWorker("w", 18, 4096+8191, 2), ExclusionBelowReserves},
		{"multi-slot reserve eats all cpu", slotWorker("w", 4, 65536, 2), ExclusionBelowReserves},
		{"multi-slot reserve eats all memory", slotWorker("w", 18, 4096, 2), ExclusionBelowReserves},
		{"one slot memory just above reserve is still eligible", slotWorker("w", 10, 4097, 1), ""},
		{"missing slot count with memory just above reserve is still eligible", slotWorker("w", 10, 4097, -1), ""},
		{"zero slot count with memory just above reserve is still eligible", slotWorker("w", 10, 4097, 0), ""},
		{"one slot cores equal to reserve", slotWorker("w", 4, 16384, 1), ExclusionBelowReserves},
		{"multi-slot missing resources keeps its reason", orchard.Worker{
			Name: "w", Labels: map[string]string{PinLabelKey: "w"},
			Resources: map[string]uint64{resourceTartVMs: 2},
		}, ExclusionMissingResources},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoSizeExclusionReason(tc.worker, map[string]int{"w": 1}, 4, 4096)
			if got != tc.want {
				t.Errorf("AutoSizeExclusionReason = %q, want %q", got, tc.want)
			}
			_, _, err := AutoSizedVM(tc.worker, 4, 4096)
			if tc.want == "" && err != nil {
				t.Errorf("eligible worker but AutoSizedVM errored: %v", err)
			}
			if tc.want == ExclusionBelowReserves && err == nil {
				t.Errorf("below_reserves worker but AutoSizedVM succeeded")
			}
		})
	}
}

func TestFreeAutoSizeSlots(t *testing.T) {
	renamed2 := mkRenamedWorker("host-new.local", "pin-a", 18, 65536, time.Now(), nil)
	renamed2.Resources[resourceTartVMs] = 2

	tests := []struct {
		name    string
		workers []orchard.Worker
		vms     []orchard.VM
		want    []string
	}{
		{
			name: "one-slot workers, including missing and zero tart-vms, are free once each",
			workers: []orchard.Worker{
				slotWorker("a", 10, 16384, 1),
				slotWorker("b", 10, 16384, -1),
				slotWorker("c", 10, 16384, 0),
			},
			want: []string{"a", "b", "c"},
		},
		{
			name:    "one-slot worker with a VM is full",
			workers: []orchard.Worker{slotWorker("a", 10, 16384, 1), slotWorker("b", 10, 16384, -1)},
			vms:     []orchard.VM{managedVM("aaaaaaaa", "a", orchard.VMStatusRunning, "a"), managedVM("bbbbbbbb", "b", orchard.VMStatusRunning, "b")},
			want:    []string{},
		},
		{
			name:    "empty two-slot worker is free twice",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			want:    []string{"w", "w"},
		},
		{
			name:    "two-slot worker with one scheduled VM is free once",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			vms:     []orchard.VM{managedVM("aaaaaaaa", "w", orchard.VMStatusRunning, "w")},
			want:    []string{"w"},
		},
		{
			name:    "two-slot worker with two VMs is full",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			vms: []orchard.VM{
				managedVM("aaaaaaaa", "w", orchard.VMStatusRunning, "w"),
				managedVM("bbbbbbbb", "w", orchard.VMStatusCreating, "w"),
			},
			want: []string{},
		},
		{
			name:    "more VMs than slots after the slot count was lowered is just full",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2), slotWorker("x", 18, 65536, 1)},
			vms: []orchard.VM{
				managedVM("aaaaaaaa", "w", orchard.VMStatusRunning, "w"),
				managedVM("bbbbbbbb", "w", orchard.VMStatusRunning, "w"),
				managedVM("cccccccc", "w", orchard.VMStatusRunning, "w"),
			},
			want: []string{"x"},
		},
		{
			name:    "pin label differs from the Orchard Name: scheduled VM counts against the pin",
			workers: []orchard.Worker{renamed2},
			vms:     []orchard.VM{managedVM("aaaaaaaa", "host-new.local", orchard.VMStatusRunning, "pin-a")},
			want:    []string{"host-new.local"},
		},
		{
			name:    "pin label differs from the Orchard Name: two VMs fill it",
			workers: []orchard.Worker{renamed2},
			vms: []orchard.VM{
				managedVM("aaaaaaaa", "host-new.local", orchard.VMStatusRunning, "pin-a"),
				managedVM("bbbbbbbb", "host-new.local", orchard.VMStatusRunning, "pin-a"),
			},
			want: []string{},
		},
		{
			name:    "one pending VM by pin label leaves one slot",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			vms:     []orchard.VM{managedVM("aaaaaaaa", "", orchard.VMStatusCreating, "w")},
			want:    []string{"w"},
		},
		{
			name:    "one pending plus one scheduled fills the worker",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			vms: []orchard.VM{
				managedVM("aaaaaaaa", "", orchard.VMStatusCreating, "w"),
				managedVM("bbbbbbbb", "w", orchard.VMStatusRunning, "w"),
			},
			want: []string{},
		},
		{
			name:    "VM on a dead worker counts through its own pin label",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			vms:     []orchard.VM{managedVM("aaaaaaaa", "w-old-name", orchard.VMStatusRunning, "w")},
			want:    []string{"w"},
		},
		{
			name:    "Stopped, Failed and unmanaged VMs do not count",
			workers: []orchard.Worker{slotWorker("w", 18, 65536, 2)},
			vms: []orchard.VM{
				managedVM("aaaaaaaa", "w", orchard.VMStatusStopped, "w"),
				managedVM("bbbbbbbb", "w", orchard.VMStatusFailed, "w"),
				{Name: "manual", Worker: "w", Status: orchard.VMStatusRunning, Labels: map[string]string{PinLabelKey: "w"}},
			},
			want: []string{"w", "w"},
		},
		{
			name:    "round-major order spreads across workers",
			workers: []orchard.Worker{slotWorker("w1", 10, 16384, 1), slotWorker("w2", 18, 65536, 2), slotWorker("w3", 10, 16384, 1)},
			want:    []string{"w1", "w2", "w3", "w2"},
		},
		{
			name:    "round-major order after one VM on w2",
			workers: []orchard.Worker{slotWorker("w1", 10, 16384, 1), slotWorker("w2", 18, 65536, 2), slotWorker("w3", 10, 16384, 1)},
			vms:     []orchard.VM{managedVM("aaaaaaaa", "w2", orchard.VMStatusRunning, "w2")},
			want:    []string{"w1", "w2", "w3"},
		},
		{
			name:    "multi-slot worker below the floors offers no slots",
			workers: []orchard.Worker{slotWorker("w", 5, 65536, 2), slotWorker("x", 10, 16384, 1)},
			want:    []string{"x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := workerNames(freeAutoSizeSlots(tc.workers, tc.vms, nil, 4, 4096))
			if !equalStrings(got, tc.want) {
				t.Errorf("freeAutoSizeSlots = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagedPinCounts(t *testing.T) {
	workers := []orchard.Worker{
		mkRenamedWorker("host-new.local", "pin-a", 18, 65536, time.Now(), nil),
		mkWorker("b", 10, 16384, 1, nil),
	}
	vms := []orchard.VM{
		managedVM("aaaaaaaa", "host-new.local", orchard.VMStatusRunning, "pin-a"), // scheduled, via Name
		managedVM("bbbbbbbb", "host-new.local", orchard.VMStatusRunning, ""),      // scheduled, no label of its own
		managedVM("cccccccc", "", orchard.VMStatusCreating, "pin-a"),              // pending, via label
		managedVM("dddddddd", "gone", orchard.VMStatusRunning, "b"),               // dead worker, via label
		managedVM("eeeeeeee", "b", orchard.VMStatusStopped, "b"),                  // terminal
		managedVM("ffffffff", "b", orchard.VMStatusFailed, "b"),                   // terminal
		{Name: "manual", Worker: "b", Status: orchard.VMStatusRunning},            // unmanaged
		managedVM("99999999", "", orchard.VMStatusCreating, ""),                   // pending, no pin
	}
	got := managedPinCounts(vms, workers)
	if len(got) != 2 || got["pin-a"] != 3 || got["b"] != 1 {
		t.Errorf("managedPinCounts = %v, want map[b:1 pin-a:3]", got)
	}
}

func TestSlotCountForLabels(t *testing.T) {
	fat := map[string]string{"class": "fat"}
	paused := mkWorker("paused", 18, 65536, 4, fat)
	paused.SchedulingPaused = true
	workers := []orchard.Worker{
		mkWorker("one", 10, 16384, 1, fat),
		mkWorker("two", 18, 65536, 2, fat),
		slotWorker("none", 10, 16384, -1),
		mkWorker("thin", 18, 65536, 2, map[string]string{"class": "thin"}),
		mkWorker("tiny", 5, 65536, 3, fat), // 1 CPU left over 3 slots
		paused,
		{Name: "unlabeled", Labels: fat, Resources: map[string]uint64{resourceLogicalCores: 18, resourceMemoryMiB: 65536, resourceTartVMs: 2}},
		mkRenamedWorker("dup-a", "shared", 18, 65536, time.Now(), fat),
		mkRenamedWorker("dup-b", "shared", 18, 65536, time.Now(), fat),
	}
	workers[2].Labels["class"] = "fat"

	if got := SlotCountForLabels(workers, fat, 4, 4096); got != 4 {
		t.Errorf("SlotCountForLabels = %d, want 4 (one 1 + two 2 + none 1; thin, tiny, paused, unlabeled and both duplicate-pin workers left out)", got)
	}
	if got := WorkerCountForLabels(workers, fat, 4, 4096); got != 3 {
		t.Errorf("WorkerCountForLabels = %d, want 3 workers, not slots", got)
	}
}
