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

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0)
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

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, false, 0, 0)
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

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0)
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

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0)
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

		share, workerCap, othersActive := scaleSetShare(workers, vms, largeLabels, false, 0, 0)
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

		share, _, othersActive := scaleSetShare(workers, vms, largeLabels, true, 0, 0)
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

	share, workerCap, othersActive := scaleSetShare(workers, []orchard.VM{pendingOnIdle, pendingOnPaused}, largeLabels, true, 0, 0)
	if share != 0 || workerCap != 2 || othersActive != 2 {
		t.Errorf("share=%d workerCap=%d othersActive=%d, want 0/2/2", share, workerCap, othersActive)
	}
}
