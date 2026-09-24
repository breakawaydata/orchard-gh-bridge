package bridge

import (
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/breakawaydata/orchard-gh-bridge/metrics"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// AutoSizeExclusion describes a live, label-matching worker that an AutoSize
// scale set cannot place VMs on, and why.
type AutoSizeExclusion struct {
	Worker   string
	PinLabel string
	Reason   string
	// SharedWith lists the other live workers holding the same pin label when
	// Reason is ExclusionDuplicatePinLabel.
	SharedWith []string
}

// AutoSizeExclusions returns every worker in workers that matches vmLabels but
// is not AutoSize-eligible with the given reserves, in input order. workers
// must be the live set, as for WorkerCountForLabels.
func AutoSizeExclusions(workers []orchard.Worker, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) []AutoSizeExclusion {
	counts := PinCounts(workers)
	var out []AutoSizeExclusion
	for _, w := range workers {
		if !workerMatchesLabels(w, vmLabels) {
			continue
		}
		reason := AutoSizeExclusionReason(w, counts, reserveCPU, reserveMemMiB)
		if reason == "" {
			continue
		}
		ex := AutoSizeExclusion{Worker: w.Name, PinLabel: PinIdentity(w), Reason: reason}
		if reason == ExclusionDuplicatePinLabel {
			for _, other := range workers {
				if other.Name != w.Name && PinIdentity(other) == ex.PinLabel {
					ex.SharedWith = append(ex.SharedWith, other.Name)
				}
			}
		}
		out = append(out, ex)
	}
	return out
}

// EligibilityMonitor reports AutoSize exclusions so a worker never drops out
// of a pool silently. It logs a WARN when a worker becomes excluded or its
// reason changes, an INFO when it recovers, and nothing while the state holds,
// so a reconcile loop can call Observe every tick without flooding the log.
// The current state is also exported as gauges.
type EligibilityMonitor struct {
	logger   *slog.Logger
	excluded *metrics.GaugeVec
	eligible *metrics.GaugeVec

	mu   sync.Mutex
	last map[string]map[string]AutoSizeExclusion // scale set → worker → exclusion
}

// NewEligibilityMonitor builds a monitor. reg may be nil, which disables the
// gauges but keeps the logging.
func NewEligibilityMonitor(logger *slog.Logger, reg *metrics.Registry) *EligibilityMonitor {
	return &EligibilityMonitor{
		logger: logger.With("component", "autosize-eligibility"),
		excluded: reg.NewGaugeVec(
			"orchard_gh_bridge_autosize_worker_excluded",
			"1 for each live worker that matches an AutoSize scale set's labels but cannot take its VMs, labelled with the reason.",
			"scale_set", "worker", "pin_label", "reason",
		),
		eligible: reg.NewGaugeVec(
			"orchard_gh_bridge_autosize_eligible_workers",
			"Number of live workers an AutoSize scale set can place VMs on.",
			"scale_set",
		),
		last: make(map[string]map[string]AutoSizeExclusion),
	}
}

// Observe evaluates one AutoSize scale set against the current live workers.
// reserveCPU and reserveMemMiB are the effective reserves (after
// AutoSizeReserves).
func (m *EligibilityMonitor) Observe(scaleSet string, workers []orchard.Worker, vmLabels map[string]string, reserveCPU, reserveMemMiB uint64) {
	if m == nil {
		return
	}
	current := make(map[string]AutoSizeExclusion)
	for _, ex := range AutoSizeExclusions(workers, vmLabels, reserveCPU, reserveMemMiB) {
		current[ex.Worker] = ex
	}
	present := make(map[string]bool, len(workers))
	for _, w := range workers {
		present[w.Name] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.last[scaleSet]

	for _, name := range sortedKeys(current) {
		ex := current[name]
		old, had := prev[name]
		if had && old.Reason == ex.Reason && old.PinLabel == ex.PinLabel {
			continue
		}
		if had {
			m.excluded.Delete(scaleSet, old.Worker, old.PinLabel, old.Reason)
		}
		m.excluded.Set(1, scaleSet, ex.Worker, ex.PinLabel, ex.Reason)
		args := []any{
			"scaleSet", scaleSet,
			"worker", ex.Worker,
			"pinLabel", ex.PinLabel,
			"reason", ex.Reason,
		}
		if len(ex.SharedWith) > 0 {
			args = append(args, "sharedWith", strings.Join(ex.SharedWith, ","))
		}
		m.logger.Warn("worker excluded from AutoSize scale set", args...)
	}

	for _, name := range sortedKeys(prev) {
		if _, still := current[name]; still {
			continue
		}
		old := prev[name]
		m.excluded.Delete(scaleSet, old.Worker, old.PinLabel, old.Reason)
		if present[name] {
			m.logger.Info("worker is AutoSize-eligible again",
				"scaleSet", scaleSet, "worker", name, "previousReason", old.Reason)
		} else {
			// Gone from the live set: offline workers are tracked by the
			// staleness and prune paths, not reported as exclusions.
			m.logger.Info("excluded worker is no longer live",
				"scaleSet", scaleSet, "worker", name, "previousReason", old.Reason)
		}
	}

	m.last[scaleSet] = current
	m.eligible.Set(float64(WorkerCountForLabels(workers, vmLabels, reserveCPU, reserveMemMiB)), scaleSet)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
