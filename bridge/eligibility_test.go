package bridge

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/metrics"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func TestAutoSizeExclusions_DuplicateNamesTheOtherHolders(t *testing.T) {
	now := time.Now()
	workers := []orchard.Worker{
		mkRenamedWorker("mini-a", "shared", 10, 32768, now, nil),
		mkRenamedWorker("mini-b", "shared", 10, 32768, now, nil),
		mkWorker("ok", 10, 32768, 1, nil),
		// Does not match the scale set's labels: not this pool's concern.
		{Name: "other-pool", Labels: map[string]string{"class": "thin"}},
	}
	got := AutoSizeExclusions(workers, nil, 4, 4096)
	// other-pool matches nil labels too, and lacks a pin label.
	if len(got) != 3 {
		t.Fatalf("exclusions = %+v, want 3", got)
	}
	if got[0].Worker != "mini-a" || got[0].Reason != ExclusionDuplicatePinLabel || strings.Join(got[0].SharedWith, ",") != "mini-b" {
		t.Errorf("exclusion[0] = %+v", got[0])
	}
	if got[2].Worker != "other-pool" || got[2].Reason != ExclusionMissingPinLabel {
		t.Errorf("exclusion[2] = %+v", got[2])
	}

	scoped := AutoSizeExclusions(workers, map[string]string{"class": "thin"}, 4, 4096)
	if len(scoped) != 1 || scoped[0].Worker != "other-pool" {
		t.Errorf("label-scoped exclusions = %+v, want only other-pool", scoped)
	}
}

func TestEligibilityMonitor_WarnsOncePerStateChange(t *testing.T) {
	now := time.Now()
	var buf bytes.Buffer
	reg := metrics.NewRegistry()
	m := NewEligibilityMonitor(captureLogger(&buf), reg)

	dup := []orchard.Worker{
		mkRenamedWorker("mini-a", "shared", 10, 32768, now, nil),
		mkRenamedWorker("mini-b", "shared", 10, 32768, now, nil),
		mkWorker("ok", 10, 32768, 1, nil),
	}
	for range 3 {
		m.Observe("large", dup, nil, 4, 4096)
	}
	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Fatalf("WARN lines = %d over 3 identical ticks, want 2 (one per excluded worker):\n%s", n, out)
	}
	for _, want := range []string{"worker=mini-a", "worker=mini-b", "pinLabel=shared", "reason=duplicate_pin_label", "sharedWith=mini-b", "scaleSet=large"} {
		if !strings.Contains(out, want) {
			t.Errorf("warn output missing %q:\n%s", want, out)
		}
	}
	if v, ok := m.excluded.Value("large", "mini-a", "shared", ExclusionDuplicatePinLabel); !ok || v != 1 {
		t.Errorf("excluded gauge for mini-a = (%v, %v), want (1, true)", v, ok)
	}
	if v, _ := m.eligible.Value("large"); v != 1 {
		t.Errorf("eligible gauge = %v, want 1", v)
	}

	// mini-b goes offline (drops out of the live list): mini-a's label is now
	// unique, so it recovers.
	buf.Reset()
	m.Observe("large", []orchard.Worker{dup[0], dup[2]}, nil, 4, 4096)
	out = buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("unexpected WARN on recovery:\n%s", out)
	}
	if !strings.Contains(out, "worker is AutoSize-eligible again") || !strings.Contains(out, "worker=mini-a") {
		t.Errorf("missing recovery log for mini-a:\n%s", out)
	}
	if _, ok := m.excluded.Value("large", "mini-a", "shared", ExclusionDuplicatePinLabel); ok {
		t.Error("excluded gauge for mini-a still present after recovery")
	}
	if _, ok := m.excluded.Value("large", "mini-b", "shared", ExclusionDuplicatePinLabel); ok {
		t.Error("excluded gauge for departed mini-b still present")
	}
	if v, _ := m.eligible.Value("large"); v != 2 {
		t.Errorf("eligible gauge = %v, want 2", v)
	}
}

func TestEligibilityMonitor_ReasonChangeWarnsAgain(t *testing.T) {
	var buf bytes.Buffer
	reg := metrics.NewRegistry()
	m := NewEligibilityMonitor(captureLogger(&buf), reg)

	w := mkWorker("w1", 10, 32768, 1, nil)
	w.SchedulingPaused = true
	m.Observe("large", []orchard.Worker{w}, nil, 4, 4096)

	delete(w.Labels, PinLabelKey)
	w.SchedulingPaused = false
	m.Observe("large", []orchard.Worker{w}, nil, 4, 4096)

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Fatalf("WARN lines = %d, want 2 (paused, then missing label):\n%s", n, out)
	}
	if !strings.Contains(out, "reason=scheduling_paused") || !strings.Contains(out, "reason=missing_pin_label") {
		t.Errorf("reasons missing from output:\n%s", out)
	}
	if _, ok := m.excluded.Value("large", "w1", "w1", ExclusionSchedulingPaused); ok {
		t.Error("old paused series not cleared on reason change")
	}
	if _, ok := m.excluded.Value("large", "w1", "", ExclusionMissingPinLabel); !ok {
		t.Error("new missing-label series not set")
	}
}

func TestEligibilityMonitor_BelowReservesIsReported(t *testing.T) {
	var buf bytes.Buffer
	m := NewEligibilityMonitor(captureLogger(&buf), nil)
	m.Observe("large", []orchard.Worker{mkWorker("small", 4, 32768, 1, nil)}, nil, 4, 4096)
	if !strings.Contains(buf.String(), "reason=below_reserves") {
		t.Errorf("below-reserves exclusion not warned:\n%s", buf.String())
	}
}

func TestEligibilityMonitor_ScaleSetsTrackedIndependently(t *testing.T) {
	var buf bytes.Buffer
	m := NewEligibilityMonitor(captureLogger(&buf), nil)
	w := mkWorker("w1", 10, 32768, 1, nil)
	w.SchedulingPaused = true
	m.Observe("a", []orchard.Worker{w}, nil, 4, 4096)
	m.Observe("b", []orchard.Worker{w}, nil, 4, 4096)
	m.Observe("a", []orchard.Worker{w}, nil, 4, 4096)
	if n := strings.Count(buf.String(), "level=WARN"); n != 2 {
		t.Errorf("WARN lines = %d, want 2 (one per scale set):\n%s", n, buf.String())
	}
}
