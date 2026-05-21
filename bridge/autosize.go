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
	// eligible worker must register with `--labels orchard-gh-bridge/worker-name=<itsOrchardName>`.
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

// AutoSizeEligible reports whether a worker is a candidate for AutoSize
// placement: not paused, advertises both core and memory resources, and
// self-labels under PinLabelKey with its own Orchard Name.
func AutoSizeEligible(w orchard.Worker) bool {
	if w.SchedulingPaused {
		return false
	}
	if w.Labels[PinLabelKey] != w.Name {
		return false
	}
	if _, ok := w.Resources[resourceLogicalCores]; !ok {
		return false
	}
	if _, ok := w.Resources[resourceMemoryMiB]; !ok {
		return false
	}
	return true
}

// WorkerCountForLabels returns the number of label-matching, AutoSize-eligible
// workers. Used as the per-scale-set capacity ceiling when AutoSize is enabled:
// each worker hosts at most one managed VM, so the ceiling is the worker count,
// not the sum of tart-vms slots.
func WorkerCountForLabels(workers []orchard.Worker, vmLabels map[string]string) int {
	var n int
	for _, w := range workers {
		if !AutoSizeEligible(w) {
			continue
		}
		if !workerMatchesLabels(w, vmLabels) {
			continue
		}
		n++
	}
	return n
}

// freeAutoSizeWorkers returns AutoSize-eligible workers that match vmLabels
// and have no managed VM currently assigned (or pending-pinned) to them.
// Returned in input order so placement is deterministic for tests.
func freeAutoSizeWorkers(workers []orchard.Worker, vms []orchard.VM, vmLabels map[string]string) []orchard.Worker {
	inUse := managedWorkersByName(vms)
	out := make([]orchard.Worker, 0, len(workers))
	for _, w := range workers {
		if !AutoSizeEligible(w) {
			continue
		}
		if !workerMatchesLabels(w, vmLabels) {
			continue
		}
		if inUse[w.Name] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// managedWorkersByName returns workers that currently host a non-terminal
// managed VM. A pending VM is matched to its target worker via PinLabelKey so
// concurrent scale-up requests don't both pick the same worker before its
// freshly-created VM has been scheduled.
func managedWorkersByName(vms []orchard.VM) map[string]bool {
	out := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if !IsManagedVM(vm.Name) {
			continue
		}
		if vm.Status == orchard.VMStatusStopped || vm.Status == orchard.VMStatusFailed {
			continue
		}
		if vm.Worker != "" {
			out[vm.Worker] = true
			continue
		}
		if target, ok := vm.Labels[PinLabelKey]; ok && target != "" {
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
