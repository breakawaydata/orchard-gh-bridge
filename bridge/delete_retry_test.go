package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// TestHandleJobCompleted_SurvivesDeleteFailure reproduces the September 2026 dev
// outage. Karpenter evicted the single-replica Orchard controller; the delete of
// a finished job's VM got "connection refused"; HandleJobCompleted returned that
// error, it travelled up through the scale set listener, and the manager exited
// with status 1. A few seconds of controller downtime restarted the whole bridge.
func TestHandleJobCompleted_SurvivesDeleteFailure(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["gha-orchard-test-aaaa"] = &orchard.VM{
		Name:   "gha-orchard-test-aaaa",
		Status: orchard.VMStatusRunning,
	}
	mock.deleteErr = errors.New(`dial tcp 172.20.190.76:6120: connect: connection refused`)

	b := New(Config{
		ScaleSetName:  "test",
		OrchardClient: mock,
		Capacity:      NewCapacity(4),
		Logger:        testLogger(),
	})
	b.mu.Lock()
	b.activeVMs["runner-1"] = "gha-orchard-test-aaaa"
	b.mu.Unlock()

	var queued []string
	b.SetOnDeleteFailed(func(vmName string) { queued = append(queued, vmName) })

	err := b.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{
		RunnerName: "runner-1",
		Result:     "succeeded",
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted returned %v; a transient delete failure must not kill the manager", err)
	}

	if len(queued) != 1 || queued[0] != "gha-orchard-test-aaaa" {
		t.Errorf("onDeleteFailed got %v, want [gha-orchard-test-aaaa]; otherwise the VM leaks its worker slot", queued)
	}
}

// TestCleanup_RetriesFailedDelete covers the other half: the VM whose delete
// failed reports "running" to Orchard (its job is over, but nothing stopped it),
// so none of the status rules match and it would sit on a worker's only
// tart-vms slot until maxVMAge — 4h in dev — starving the queue.
func TestCleanup_RetriesFailedDelete(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["gha-orchard-test-leaked"] = &orchard.VM{
		Name:      "gha-orchard-test-leaked",
		Status:    orchard.VMStatusRunning,
		CreatedAt: time.Now(), // nowhere near maxAge
	}

	cleanup := NewCleanup(mock, NewCapacity(4), nil, testLogger())

	// Without the marker the sweep leaves a running VM alone.
	cleanup.sweep(context.Background())
	if _, err := mock.GetVM(context.Background(), "gha-orchard-test-leaked"); err != nil {
		t.Fatalf("precondition: running VM should survive an ordinary sweep, got %v", err)
	}

	cleanup.MarkForDeletion("gha-orchard-test-leaked")
	cleanup.sweep(context.Background())

	if _, err := mock.GetVM(context.Background(), "gha-orchard-test-leaked"); err == nil {
		t.Error("marked VM should have been deleted on the next sweep")
	}
	if cleanup.isPendingDelete("gha-orchard-test-leaked") {
		t.Error("successful delete should clear the retry entry")
	}
}

// A retry that keeps failing must stay queued, or one unlucky sweep during a
// longer outage would drop the VM on the floor permanently.
func TestCleanup_KeepsRetryingWhileDeleteFails(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["gha-orchard-test-stuck"] = &orchard.VM{
		Name:      "gha-orchard-test-stuck",
		Status:    orchard.VMStatusRunning,
		CreatedAt: time.Now(),
	}
	mock.deleteErr = errors.New("connection refused")

	cleanup := NewCleanup(mock, NewCapacity(4), nil, testLogger())
	cleanup.MarkForDeletion("gha-orchard-test-stuck")

	cleanup.sweep(context.Background())
	if !cleanup.isPendingDelete("gha-orchard-test-stuck") {
		t.Fatal("entry dropped after a failed retry")
	}

	// Controller comes back.
	mock.deleteErr = nil
	cleanup.sweep(context.Background())

	if _, err := mock.GetVM(context.Background(), "gha-orchard-test-stuck"); err == nil {
		t.Error("VM should be deleted once the controller recovers")
	}
}

// The retry set must not grow without bound: a name Orchard no longer reports
// is forgotten, whether it was deleted here or by some other path.
func TestCleanup_PrunesRetryEntriesForVanishedVMs(t *testing.T) {
	mock := newMockOrchard()
	cleanup := NewCleanup(mock, NewCapacity(4), nil, testLogger())

	cleanup.MarkForDeletion("gha-orchard-test-gone")
	cleanup.sweep(context.Background())

	if cleanup.isPendingDelete("gha-orchard-test-gone") {
		t.Error("retry entry for a VM Orchard no longer reports should be pruned")
	}
}
