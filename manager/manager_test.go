package manager

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	brdg "github.com/breakawaydata/orchard-gh-bridge/bridge"
	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func autoSizeWorker(name, pin string, extra map[string]string) orchard.Worker {
	labels := map[string]string{brdg.PinLabelKey: pin}
	for k, v := range extra {
		labels[k] = v
	}
	return orchard.Worker{
		Name:   name,
		Labels: labels,
		Resources: map[string]uint64{
			"org.cirruslabs.logical-cores": 10,
			"org.cirruslabs.memory-mib":    32768,
			"org.cirruslabs.tart-vms":      1,
		},
		LastSeen: time.Now(),
	}
}

// TestVMConsumesLabeledWorker_RenamedWorker covers both branches for a worker
// whose Orchard Name differs from its pin label (BAE-8010).
func TestVMConsumesLabeledWorker_RenamedWorker(t *testing.T) {
	large := map[string]string{"vm-class-large": "true"}
	workers := []orchard.Worker{
		autoSizeWorker("BreakAway-SF-Mac-Mini-1.local", "BreakAwySFMini1.localdomain", large),
		autoSizeWorker("small-mini", "small-mini", nil),
	}

	scheduled := orchard.VM{
		Name:   "gha-orchard-large-aaaaaaaa",
		Worker: "BreakAway-SF-Mac-Mini-1.local",
		Status: orchard.VMStatusRunning,
		Labels: map[string]string{"vm-class-large": "true", brdg.PinLabelKey: "BreakAwySFMini1.localdomain"},
	}
	if !vmConsumesLabeledWorker(scheduled, workers, large) {
		t.Error("scheduled VM on the renamed worker not counted against the large pool")
	}

	pending := scheduled
	pending.Worker = ""
	pending.Status = orchard.VMStatusCreating
	if !vmConsumesLabeledWorker(pending, workers, large) {
		t.Error("pending VM pinned by label to the renamed worker not counted against the large pool")
	}

	if vmConsumesLabeledWorker(pending, workers, map[string]string{"vm-class-small": "true"}) {
		t.Error("pending large VM counted against an unrelated pool")
	}
}

func TestObserveAutoSizeEligibility_OnlyAutoSizeScaleSets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m := &Manager{
		cfg: &config.Config{ScaleSets: []config.ScaleSetConfig{
			{Name: "fixed", VM: config.VMConfig{}},
			{Name: "large", VM: config.VMConfig{AutoSize: config.AutoSizeConfig{Enabled: true}}},
		}},
		eligibility: brdg.NewEligibilityMonitor(logger, nil),
	}

	unlabeled := autoSizeWorker("unlabeled", "", nil)
	delete(unlabeled.Labels, brdg.PinLabelKey)
	m.observeAutoSizeEligibility([]orchard.Worker{unlabeled})

	out := buf.String()
	if !strings.Contains(out, "scaleSet=large") || !strings.Contains(out, "reason=missing_pin_label") {
		t.Errorf("missing WARN for the AutoSize scale set:\n%s", out)
	}
	if strings.Contains(out, "scaleSet=fixed") {
		t.Errorf("non-AutoSize scale set was evaluated:\n%s", out)
	}
}
