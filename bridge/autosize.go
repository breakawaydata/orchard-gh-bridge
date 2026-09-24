package bridge

import (
	"fmt"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

const (
	// resourceLogicalCores is the Orchard resource key under which workers
	// advertise their host's logical core count.
	resourceLogicalCores = "org.cirruslabs.logical-cores"
	// resourceMemoryMiB is the Orchard resource key under which workers
	// advertise their host's total memory in MiB.
	resourceMemoryMiB = "org.cirruslabs.memory-mib"

	// PinLabelKey is the label key both VMs and workers use to pin AutoSize
	// VMs to the worker their CPU/memory was computed from. Each AutoSize-
	// eligible worker registers with `--labels orchard-gh-bridge/worker-name=<id>`,
	// and <id> — not the worker's Orchard Name — is its pin identity. The two
	// usually match, but a worker's Name defaults to its hostname and silently
	// changes with it (a macOS upgrade is enough), while the label lives in the
	// host's launchd plist and does not. See PinIdentity.
	PinLabelKey = "orchard-gh-bridge/worker-name"

	// DefaultAutoSizeReserveCPU is held back from the VM for host overhead.
	// Sized for the typical macOS + orchard-worker + Colima VM combination:
	// roughly 1c for macOS background tasks, 1c for orchard-worker / Tart
	// overhead, and 2c for a default Colima allocation. Override per scale
	// set via vm.autoSize.reserveCPU if your Colima profile is bigger.
	DefaultAutoSizeReserveCPU = uint64(4)
	// DefaultAutoSizeReserveMemoryMiB is held back from the VM for host overhead.
	// Sized for macOS (~2 GiB working set), orchard-worker (~50 MiB), and a
	// default 2 GiB Colima VM. Override via vm.autoSize.reserveMemoryMiB.
	DefaultAutoSizeReserveMemoryMiB = uint64(4096)
)

// Reasons a label-matching live worker is left out of AutoSize placement.
// They double as the `reason` label on the exclusion metric, so they are
// stable, lower-case identifiers.
const (
	ExclusionSchedulingPaused  = "scheduling_paused"
	ExclusionMissingPinLabel   = "missing_pin_label"
	ExclusionDuplicatePinLabel = "duplicate_pin_label"
	ExclusionMissingResources  = "missing_resources"
	ExclusionBelowReserves     = "below_reserves"
)

// PinIdentity returns the identity AutoSize pins VMs by: the worker's
// PinLabelKey label value, or "" if it has none.
//
// It is deliberately not the Orchard Name. Orchard schedules a pinned VM onto
// whichever worker carries the matching label, so the label is the thing that
// actually decides placement; requiring it to also equal the Name meant a
// hostname change dropped the machine from every AutoSize pool without a word.
func PinIdentity(w orchard.Worker) string {
	return w.Labels[PinLabelKey]
}

// PinCounts returns how many of the given workers hold each pin identity.
// Callers pass live workers, so a stale record left behind by a rename does
// not make its successor's identity look shared.
func PinCounts(workers []orchard.Worker) map[string]int {
	out := make(map[string]int, len(workers))
	for _, w := range workers {
		if pin := PinIdentity(w); pin != "" {
			out[pin]++
		}
	}
	return out
}

// AutoSizeExclusionReason returns why w cannot take an AutoSize VM with the
// given reserves, or "" if it can. pinCounts must come from PinCounts over the
// same live worker set: a pin identity held by more than one live worker is
// unusable, because Orchard could schedule the pinned VM onto either holder
// and the bridge sized it for only one of them.
func AutoSizeExclusionReason(w orchard.Worker, pinCounts map[string]int, reserveCPU, reserveMemMiB uint64) string {
	if w.SchedulingPaused {
		return ExclusionSchedulingPaused
	}
	pin := PinIdentity(w)
	if pin == "" {
		return ExclusionMissingPinLabel
	}
	if pinCounts[pin] > 1 {
		return ExclusionDuplicatePinLabel
	}
	cores, okCores := w.Resources[resourceLogicalCores]
	mem, okMem := w.Resources[resourceMemoryMiB]
	if !okCores || !okMem {
		return ExclusionMissingResources
	}
	// Workers with cores <= reserveCPU or memory <= reserveMemMiB would be
	// rejected by AutoSizedVM anyway; excluding them here keeps
	// WorkerCountForLabels and freeAutoSizeWorkers accurate.
	if cores <= reserveCPU || mem <= reserveMemMiB {
		return ExclusionBelowReserves
	}
	return ""
}

// autoSizeCandidates returns the workers that match vmLabels and can take an
// AutoSize VM with the given reserves, in input order.
func autoSizeCandidates(workers []orchard.Worker, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) []orchard.Worker {
	counts := PinCounts(workers)
	out := make([]orchard.Worker, 0, len(workers))
	for _, w := range workers {
		if !workerMatchesLabels(w, vmLabels) {
			continue
		}
		if AutoSizeExclusionReason(w, counts, reserveCPU, reserveMemMiB) != "" {
			continue
		}
		out = append(out, w)
	}
	return out
}

// WorkerCountForLabels returns the number of label-matching, AutoSize-eligible
// workers that can satisfy the given reserves. Used as the per-scale-set
// capacity ceiling when AutoSize is enabled: each worker hosts at most one
// managed VM, so the ceiling is the schedulable worker count, not the sum of
// tart-vms slots. workers must be the live set (see PinCounts).
func WorkerCountForLabels(workers []orchard.Worker, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) int {
	return len(autoSizeCandidates(workers, vmLabels, reserveCPU, reserveMemMiB))
}

// freeAutoSizeWorkers returns AutoSize-eligible workers that match vmLabels,
// have enough resources to satisfy the given reserves, and have no managed VM
// currently assigned (or pending-pinned) to their pin identity.
// Returned in input order so placement is deterministic for tests.
func freeAutoSizeWorkers(workers []orchard.Worker, vms []orchard.VM, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) []orchard.Worker {
	inUse := managedPinIdentities(vms, workers)
	candidates := autoSizeCandidates(workers, vmLabels, reserveCPU, reserveMemMiB)
	out := candidates[:0]
	for _, w := range candidates {
		if inUse[PinIdentity(w)] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// managedPinIdentities returns the pin identities of workers that currently
// host, or are about to host, a non-terminal managed VM.
//
// A scheduled VM reports the Orchard Name of the worker it landed on, so it is
// mapped back through workers to that worker's pin identity; comparing the
// Name against pin labels directly would miss every worker whose label and
// Name differ.
//
// A VM whose worker is absent from workers (a record that stopped
// heartbeating) falls back to its own pin label. The usual cause is a rename:
// the machine now heartbeats under a new Name with the same label, and the VM
// may still be running on it, so handing that identity out again would put a
// second full-size VM on one host. The cost is that a VM truly stranded on a
// dead machine holds its pin identity until its job completes or cleanup
// reaps it.
//
// A pending VM has no worker yet, so it is matched by the pin label the bridge
// gave it; that stops concurrent scale-ups from both picking the same worker
// before its freshly-created VM has been scheduled.
func managedPinIdentities(vms []orchard.VM, workers []orchard.Worker) map[string]bool {
	pinByName := make(map[string]string, len(workers))
	for _, w := range workers {
		pinByName[w.Name] = PinIdentity(w)
	}
	out := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if !IsManagedVM(vm.Name) {
			continue
		}
		if vm.Status == orchard.VMStatusStopped || vm.Status == orchard.VMStatusFailed {
			continue
		}
		if vm.Worker != "" {
			if pin, known := pinByName[vm.Worker]; known {
				if pin != "" {
					out[pin] = true
				}
				continue
			}
		}
		if target := vm.Labels[PinLabelKey]; target != "" {
			out[target] = true
		}
	}
	return out
}

// AutoSizedVM returns the CPU and memory a VM should be created with on the
// given worker, after subtracting reserves. Errors if the worker is too small
// to satisfy the reserves.
func AutoSizedVM(w orchard.Worker, reserveCPU, reserveMemMiB uint64) (cpu uint64, memMiB uint64, err error) {
	cores := w.Resources[resourceLogicalCores]
	mem := w.Resources[resourceMemoryMiB]
	if cores == 0 || mem == 0 {
		return 0, 0, fmt.Errorf("worker %s missing logical-cores or memory-mib resource", w.Name)
	}
	if cores <= reserveCPU {
		return 0, 0, fmt.Errorf("worker %s has %d cores, reserve %d would leave VM with 0", w.Name, cores, reserveCPU)
	}
	if mem <= reserveMemMiB {
		return 0, 0, fmt.Errorf("worker %s has %d MiB, reserve %d MiB would leave VM with 0", w.Name, mem, reserveMemMiB)
	}
	return cores - reserveCPU, mem - reserveMemMiB, nil
}

// AutoSizeReserves returns the effective reserves for an AutoSizeConfig,
// substituting defaults for zero values.
func AutoSizeReserves(cpu, memMiB uint64) (uint64, uint64) {
	if cpu == 0 {
		cpu = DefaultAutoSizeReserveCPU
	}
	if memMiB == 0 {
		memMiB = DefaultAutoSizeReserveMemoryMiB
	}
	return cpu, memMiB
}
