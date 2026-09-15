package bridge

import (
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// DefaultWorkerStaleAfter is how long a worker may go without a heartbeat
// before the bridge stops counting it as capacity. Orchard workers ping the
// controller every few seconds, so this is many missed beats: long enough to
// ride out a brief network blip or a controller restart, short enough that a
// machine which has gone away stops holding a slot.
//
// This matters more than "one idle worker" suggests. The bridge acquires a real
// GitHub job for every VM it creates, so a worker that is counted but cannot
// actually run anything does not sit idle — it repeatedly acquires jobs, fails
// to start them, and hands them back, starving the queue for everyone else.
const DefaultWorkerStaleAfter = 2 * time.Minute

// WorkerLive reports whether w has heartbeated recently enough that Orchard can
// still place a VM on it.
//
// A zero LastSeen is treated as live: it means the controller did not report the
// field, and failing open keeps a controller that omits it from silently zeroing
// out the whole fleet's capacity.
func WorkerLive(w orchard.Worker, staleAfter time.Duration, now time.Time) bool {
	if staleAfter <= 0 {
		return true
	}
	if w.LastSeen.IsZero() {
		return true
	}
	return now.Sub(w.LastSeen) <= staleAfter
}

// LiveWorkers returns the subset of workers still heartbeating within
// staleAfter, preserving input order.
//
// Paused workers are deliberately left in: SchedulingPaused is already handled
// by the capacity predicates (AutoSizeEligible, CapacityForLabels), and callers
// such as vmConsumesLabeledWorker need to see a paused worker to attribute a VM
// that is still running on it. Staleness is the orthogonal question of whether
// the machine is there at all.
func LiveWorkers(workers []orchard.Worker, staleAfter time.Duration, now time.Time) []orchard.Worker {
	out := make([]orchard.Worker, 0, len(workers))
	for _, w := range workers {
		if !WorkerLive(w, staleAfter, now) {
			continue
		}
		out = append(out, w)
	}
	return out
}
