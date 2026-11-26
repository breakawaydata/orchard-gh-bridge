package manager

import (
	"testing"

	brdg "github.com/breakawaydata/orchard-gh-bridge/bridge"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func countAll(orchard.Worker) bool { return true }

var largeLabels = map[string]string{"vm-class-large": "true"}

func tartWorker(name string, slots uint64, paused bool) orchard.Worker {
	w := autoSizeWorker(name, name, largeLabels)
	w.Resources["org.cirruslabs.tart-vms"] = slots
	w.SchedulingPaused = paused
	return w
}

func otherVM(name, worker, status string) orchard.VM {
	return orchard.VM{
		Name:   name,
		Worker: worker,
		Status: status,
		Labels: map[string]string{"vm-class-large": "true"},
	}
}

// TestScaleSetShare_PausedWorkerDrainingVM is the BAE-13738 regression: a VM
// draining on a scheduling-paused worker must not be subtracted from a
// capacity figure that already leaves that worker out, or the idle worker
// next to it gets share 0.
func TestScaleSetShare_PausedWorkerDrainingVM(t *testing.T) {
	t.Run("autoSize", func(t *testing.T) {
		idle := autoSizeWorker("mini1", "mini1", largeLabels)
		paused := autoSizeWorker("mini2", "mini2", largeLabels)
		paused.SchedulingPaused = true
		workers := []orchard.Worker{idle, paused}
		vms := []orchard.VM{otherVM("gha-orchard-other-aaaaaaaa", "mini2", orchard.VMStatusRunning)}

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0, 0)
		if share != 1 || workerCap != 1 || othersActive != 0 {
			t.Errorf("share=%d workerCap=%d othersActive=%d, want 1/1/0", share, workerCap, othersActive)
		}
	})

	t.Run("tart-vms slots", func(t *testing.T) {
		// The shape seen live: share:0 workerCap:2 othersActive:2.
		workers := []orchard.Worker{tartWorker("mini1", 2, false), tartWorker("mini2", 2, true)}
		vms := []orchard.VM{
			otherVM("gha-orchard-other-aaaaaaaa", "mini2", orchard.VMStatusRunning),
			otherVM("gha-orchard-other-bbbbbbbb", "mini2", orchard.VMStatusRunning),
		}

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, false, 0, 0, 0)
		if share != 2 || workerCap != 2 || othersActive != 0 {
			t.Errorf("share=%d workerCap=%d othersActive=%d, want 2/2/0", share, workerCap, othersActive)
		}
	})

	t.Run("autoSize worker excluded for another reason", func(t *testing.T) {
		idle := autoSizeWorker("mini1", "mini1", largeLabels)
		small := autoSizeWorker("mini2", "mini2", largeLabels)
		small.Resources["org.cirruslabs.logical-cores"] = 2 // below the default 4-core reserve
		workers := []orchard.Worker{idle, small}
		vms := []orchard.VM{otherVM("gha-orchard-other-aaaaaaaa", "mini2", orchard.VMStatusRunning)}

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0, 0)
		if share != 1 || workerCap != 1 || othersActive != 0 {
			t.Errorf("share=%d workerCap=%d othersActive=%d, want 1/1/0", share, workerCap, othersActive)
		}
	})
}

// TestScaleSetShare_NothingPaused pins the existing behaviour: another scale
// set's VM on a counted worker is still subtracted.
func TestScaleSetShare_NothingPaused(t *testing.T) {
	t.Run("autoSize", func(t *testing.T) {
		workers := []orchard.Worker{
			autoSizeWorker("mini1", "mini1", largeLabels),
			autoSizeWorker("mini2", "mini2", largeLabels),
		}
		vms := []orchard.VM{
			otherVM("gha-orchard-other-aaaaaaaa", "mini2", orchard.VMStatusRunning),
			otherVM("gha-orchard-other-stopped", "mini1", orchard.VMStatusStopped),
		}

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0, 0)
		if share != 1 || workerCap != 2 || othersActive != 1 {
			t.Errorf("share=%d workerCap=%d othersActive=%d, want 1/2/1", share, workerCap, othersActive)
		}
	})

	t.Run("tart-vms slots", func(t *testing.T) {
		workers := []orchard.Worker{tartWorker("mini1", 2, false), tartWorker("mini2", 2, false)}
		vms := []orchard.VM{
			otherVM("gha-orchard-other-aaaaaaaa", "mini2", orchard.VMStatusRunning),
			otherVM("gha-orchard-other-bbbbbbbb", "mini2", orchard.VMStatusRunning),
		}

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, false, 0, 0, 0)
		if share != 2 || workerCap != 4 || othersActive != 2 {
			t.Errorf("share=%d workerCap=%d othersActive=%d, want 2/4/2", share, workerCap, othersActive)
		}
	})

	t.Run("floored at zero", func(t *testing.T) {
		workers := []orchard.Worker{autoSizeWorker("mini1", "mini1", largeLabels)}
		vms := []orchard.VM{
			otherVM("gha-orchard-other-aaaaaaaa", "mini1", orchard.VMStatusRunning),
			otherVM("gha-orchard-other-bbbbbbbb", "", orchard.VMStatusCreating),
		}

		share, _, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0, 0)
		if share != 0 || othersActive != 2 {
			t.Errorf("share=%d othersActive=%d, want 0/2", share, othersActive)
		}
	})
}

// TestScaleSetShare_PendingVMStillCounted: a pending VM has no worker yet, so
// it is matched by its labels as before — including one pinned to a worker
// that has since been paused, which Orchard may still place.
func TestScaleSetShare_PendingVMStillCounted(t *testing.T) {
	idle := autoSizeWorker("mini1", "mini1", largeLabels)
	paused := autoSizeWorker("mini2", "mini2", largeLabels)
	paused.SchedulingPaused = true
	workers := []orchard.Worker{
		idle,
		autoSizeWorker("mini3", "mini3", largeLabels),
		paused,
	}

	pendingOnIdle := otherVM("gha-orchard-other-aaaaaaaa", "", orchard.VMStatusCreating)
	pendingOnIdle.Labels[brdg.PinLabelKey] = "mini1"
	pendingOnPaused := otherVM("gha-orchard-other-bbbbbbbb", "", orchard.VMStatusCreating)
	pendingOnPaused.Labels[brdg.PinLabelKey] = "mini2"

	share, workerCap, othersActive := scaleSetShare(workers, []orchard.VM{pendingOnIdle, pendingOnPaused}, largeLabels, true, 0, 0, 0)
	if share != 0 || workerCap != 2 || othersActive != 2 {
		t.Errorf("share=%d workerCap=%d othersActive=%d, want 0/2/2", share, workerCap, othersActive)
	}
}

// TestWorkerCapacityFn_AutoSizeSumsSlots: with AutoSize on, the capacity ceiling
// is the tart-vms slot sum over eligible workers, not the worker count, and each
// VM of another scale set takes one unit of it.
func TestWorkerCapacityFn_AutoSizeSumsSlots(t *testing.T) {
	two := tartWorker("mini1", 2, false)
	one := tartWorker("mini2", 1, false)
	paused := tartWorker("mini3", 4, true)
	undersized := tartWorker("mini4", 4, false)
	undersized.Resources["org.cirruslabs.logical-cores"] = 3 // below the default 4-core reserve
	noSlots := autoSizeWorker("mini5", "mini5", largeLabels)
	delete(noSlots.Resources, "org.cirruslabs.tart-vms") // a missing slot count is one slot
	workers := []orchard.Worker{two, one, paused, undersized, noSlots}

	if got := workerCapacityFn(true, 0, 0, 0)(workers, largeLabels); got != 4 {
		t.Errorf("AutoSize capacity = %d, want 4 (2 + 1 + 1; paused and undersized workers left out)", got)
	}
	if got := workerCapacityFn(false, 0, 0, 0)(workers, largeLabels); got != 7 {
		t.Errorf("fixed-slot capacity = %d, want 7 (2 + 1 + 4; the paused worker and the one with no slot count add none)", got)
	}

	vms := []orchard.VM{
		otherVM("gha-orchard-other-aaaaaaaa", "mini1", orchard.VMStatusRunning),
		otherVM("gha-orchard-other-bbbbbbbb", "mini1", orchard.VMStatusRunning),
	}
	share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0, 0)
	if share != 2 || workerCap != 4 || othersActive != 2 {
		t.Errorf("share=%d workerCap=%d othersActive=%d, want 2/4/2", share, workerCap, othersActive)
	}
}

// With autoSize.minMemoryMiB set, a scale set's capacity counts only the slots
// on workers at or above the floor, and a VM of another scale set running on a
// worker below the floor is not subtracted from it. Unset, both minis count.
func TestScaleSetShare_MinMemory(t *testing.T) {
	small1 := autoSizeWorker("mini1", "mini1", largeLabels)
	small1.Resources["org.cirruslabs.memory-mib"] = 16384
	small2 := autoSizeWorker("mini2", "mini2", largeLabels)
	small2.Resources["org.cirruslabs.memory-mib"] = 16384
	big := autoSizeWorker("m3pro", "m3pro", largeLabels)
	big.Resources["org.cirruslabs.memory-mib"] = 36864
	workers := []orchard.Worker{small1, small2, big}
	vms := []orchard.VM{otherVM("gha-orchard-other-aaaaaaaa", "mini1", orchard.VMStatusRunning)}

	share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0, 0)
	if share != 2 || workerCap != 3 || othersActive != 1 {
		t.Errorf("unset: share=%d workerCap=%d othersActive=%d, want 2/3/1", share, workerCap, othersActive)
	}
	share, workerCap, othersActive = scaleSetShare(workers, vms, largeLabels, true, 0, 0, 32768)
	if share != 1 || workerCap != 1 || othersActive != 0 {
		t.Errorf("minMemoryMiB 32768: share=%d workerCap=%d othersActive=%d, want 1/1/0", share, workerCap, othersActive)
	}
}
