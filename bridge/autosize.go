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

	// MinAutoSizeSlotMemoryMiB is the least memory a VM may get on a worker that
	// advertises more than one tart-vms slot. It is Tart's default macOS VM
	// memory: a guest below that cannot usefully run a CI job, so a host
	// advertising more slots than it can feed is a misconfiguration to surface
	// (the worker is excluded as below_reserves), not to silently starve. The
	// floor does not apply to single-slot workers, which keep taking whatever
	// the host has left after the reserves.
	MinAutoSizeSlotMemoryMiB = uint64(4096)
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

// AutoSizeSlots returns how many VMs an AutoSize worker hosts at once: its
// org.cirruslabs.tart-vms resource, or 1 when that is missing or 0. Single-slot
// workers are the baseline AutoSize was built for, so a worker that does not
// advertise the resource keeps behaving as one.
func AutoSizeSlots(w orchard.Worker) uint64 {
	if n := w.Resources[resourceTartVMs]; n > 0 {
		return n
	}
	return 1
}

// autoSizeShare returns the CPU and memory each VM slot on w gets: the host's
// cores and memory minus the reserves, which are held back once for the host
// and then split evenly across the worker's slots (integer floor). It is the
// one place that sizing and eligibility are decided, so AutoSizedVM and
// AutoSizeExclusionReason cannot disagree about a worker.
//
// It errors when the reserves leave nothing. A worker with more than one slot
// also errors when its share is under 1 CPU or MinAutoSizeSlotMemoryMiB: a host
// advertising more slots than it can feed is a misconfiguration to surface. The
// floors do not apply to a single-slot worker, so it behaves as it always did.
func autoSizeShare(w orchard.Worker, reserveCPU, reserveMemMiB uint64) (cpu, memMiB uint64, err error) {
	cores := w.Resources[resourceLogicalCores]
	mem := w.Resources[resourceMemoryMiB]
	if cores <= reserveCPU {
		return 0, 0, fmt.Errorf("worker %s has %d cores, reserve %d would leave VM with 0", w.Name, cores, reserveCPU)
	}
	if mem <= reserveMemMiB {
		return 0, 0, fmt.Errorf("worker %s has %d MiB, reserve %d MiB would leave VM with 0", w.Name, mem, reserveMemMiB)
	}
	slots := AutoSizeSlots(w)
	cpu = (cores - reserveCPU) / slots
	memMiB = (mem - reserveMemMiB) / slots
	if slots > 1 && (cpu < 1 || memMiB < MinAutoSizeSlotMemoryMiB) {
		return 0, 0, fmt.Errorf(
			"worker %s advertises %d tart-vms slots but %d cores and %d MiB minus reserves %d / %d MiB leave %d CPU and %d MiB per VM (minimum 1 CPU and %d MiB)",
			w.Name, slots, cores, mem, reserveCPU, reserveMemMiB, cpu, memMiB, MinAutoSizeSlotMemoryMiB)
	}
	return cpu, memMiB, nil
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
	_, okCores := w.Resources[resourceLogicalCores]
	_, okMem := w.Resources[resourceMemoryMiB]
	if !okCores || !okMem {
		return ExclusionMissingResources
	}
	// A worker whose share would be rejected by AutoSizedVM is excluded here,
	// which keeps WorkerCountForLabels, SlotCountForLabels and
	// freeAutoSizeSlots accurate.
	if _, _, err := autoSizeShare(w, reserveCPU, reserveMemMiB); err != nil {
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
// workers that can satisfy the given reserves. It counts workers, not VM slots:
// it backs the eligible-workers gauge. The per-scale-set capacity ceiling is
// SlotCountForLabels. workers must be the live set (see PinCounts).
func WorkerCountForLabels(workers []orchard.Worker, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) int {
	return len(autoSizeCandidates(workers, vmLabels, reserveCPU, reserveMemMiB))
}

// SlotCountForLabels returns the number of VMs the label-matching,
// AutoSize-eligible workers can host at once: the sum of AutoSizeSlots over
// them. Used as the per-scale-set capacity ceiling when AutoSize is enabled.
// workers must be the live set (see PinCounts).
func SlotCountForLabels(workers []orchard.Worker, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) int {
	total := 0
	for _, w := range autoSizeCandidates(workers, vmLabels, reserveCPU, reserveMemMiB) {
		total += int(AutoSizeSlots(w))
	}
	return total
}

// freeAutoSizeSlots returns one entry per free VM slot on the AutoSize-eligible
// workers that match vmLabels and satisfy the given reserves. A worker's free
// slots are its AutoSizeSlots minus the non-terminal managed VMs assigned (or
// pending-pinned) to its pin identity; a count at or above the slot total, such
// as after the slot count was lowered, just means the worker is full.
//
// The result is round-major so placement spreads across the fleet: every
// worker's first free slot in input order, then every worker's second, and so
// on. For a fleet of single-slot workers that is the free workers in input
// order, deterministic for tests.
func freeAutoSizeSlots(workers []orchard.Worker, vms []orchard.VM, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) []orchard.Worker {
	inUse := managedPinCounts(vms, workers)
	candidates := autoSizeCandidates(workers, vmLabels, reserveCPU, reserveMemMiB)
	free := make([]int, len(candidates))
	rounds := 0
	total := 0
	for i, w := range candidates {
		n := int(AutoSizeSlots(w)) - inUse[PinIdentity(w)]
		if n < 0 {
			n = 0
		}
		free[i] = n
		total += n
		if n > rounds {
			rounds = n
		}
	}
	out := make([]orchard.Worker, 0, total)
	for round := 0; round < rounds; round++ {
		for i, w := range candidates {
			if free[i] > round {
				out = append(out, w)
			}
		}
	}
	return out
}

// managedPinCounts returns, per pin identity, how many non-terminal managed VMs
// currently occupy a slot on that worker or are about to.
//
// A scheduled VM reports the Orchard Name of the worker it landed on, so it is
// mapped back through workers to that worker's pin identity; comparing the
// Name against pin labels directly would miss every worker whose label and
// Name differ.
//
// A VM whose worker is absent from workers (a record that stopped
// heartbeating) falls back to its own pin label. The usual cause is a rename:
// the machine now heartbeats under a new Name with the same label, and the VM
// may still be running on it, so handing that slot out again could put more VMs
// on one host than it has slots. The cost is that a VM truly stranded on a
// dead machine holds its slot until its job completes or cleanup reaps it.
//
// A pending VM has no worker yet, so it is matched by the pin label the bridge
// gave it; that stops concurrent scale-ups from both picking the same slot
// before its freshly-created VM has been scheduled.
//
// Unmanaged VMs and VMs that are Stopped or Failed are not counted.
func managedPinCounts(vms []orchard.VM, workers []orchard.Worker) map[string]int {
	pinByName := make(map[string]string, len(workers))
	for _, w := range workers {
		pinByName[w.Name] = PinIdentity(w)
	}
	out := make(map[string]int, len(vms))
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
					out[pin]++
				}
				continue
			}
		}
		if target := vm.Labels[PinLabelKey]; target != "" {
			out[target]++
		}
	}
	return out
}

// AutoSizedVM returns the CPU and memory one VM should be created with on the
// given worker: the host's cores and memory minus the reserves, split evenly
// across the worker's tart-vms slots (see AutoSizeSlots). Errors if the worker
// is missing its resources, if the reserves leave nothing, or if a multi-slot
// worker's per-VM share is under 1 CPU or MinAutoSizeSlotMemoryMiB.
func AutoSizedVM(w orchard.Worker, reserveCPU, reserveMemMiB uint64) (cpu uint64, memMiB uint64, err error) {
	cores := w.Resources[resourceLogicalCores]
	mem := w.Resources[resourceMemoryMiB]
	if cores == 0 || mem == 0 {
		return 0, 0, fmt.Errorf("worker %s missing logical-cores or memory-mib resource", w.Name)
	}
	return autoSizeShare(w, reserveCPU, reserveMemMiB)
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
