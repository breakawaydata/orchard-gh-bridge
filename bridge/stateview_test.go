package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// countingOrchard wraps mockOrchardClient and counts ListVMs calls so tests
// can assert singleflight and TTL behavior.
type countingOrchard struct {
	*mockOrchardClient
	listVMCalls     atomic.Int32
	listWorkerCalls atomic.Int32
	workers         []orchard.Worker
	listErr         error
	blockList       chan struct{} // when non-nil, ListVMs blocks until closed
}

func newCountingOrchard() *countingOrchard {
	return &countingOrchard{mockOrchardClient: newMockOrchard()}
}

func (c *countingOrchard) ListVMs(ctx context.Context) ([]orchard.VM, error) {
	c.listVMCalls.Add(1)
	if c.blockList != nil {
		<-c.blockList
	}
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.mockOrchardClient.ListVMs(ctx)
}

func (c *countingOrchard) ListWorkers(ctx context.Context) ([]orchard.Worker, error) {
	c.listWorkerCalls.Add(1)
	if c.listErr != nil {
		return nil, c.listErr
	}
	if c.workers != nil {
		return c.workers, nil
	}
	return c.mockOrchardClient.ListWorkers(ctx)
}

func TestStateView_CachesWithinTTL(t *testing.T) {
	c := newCountingOrchard()
	sv := NewStateView(c, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := sv.Get(context.Background()); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}

	if got := c.listVMCalls.Load(); got != 1 {
		t.Errorf("ListVMs calls = %d, want 1 (cached within TTL)", got)
	}
}

func TestStateView_RefreshesAfterTTL(t *testing.T) {
	c := newCountingOrchard()
	sv := NewStateView(c, 5*time.Millisecond)

	if _, err := sv.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := sv.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got := c.listVMCalls.Load(); got != 2 {
		t.Errorf("ListVMs calls = %d, want 2 (refresh after TTL)", got)
	}
}

func TestStateView_InvalidateForcesRefresh(t *testing.T) {
	c := newCountingOrchard()
	sv := NewStateView(c, time.Minute)

	if _, err := sv.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	sv.Invalidate()
	if _, err := sv.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got := c.listVMCalls.Load(); got != 2 {
		t.Errorf("ListVMs calls = %d, want 2 (refresh after Invalidate)", got)
	}
}

func TestStateView_CoalescesConcurrentRefreshes(t *testing.T) {
	c := newCountingOrchard()
	c.blockList = make(chan struct{})
	sv := NewStateView(c, time.Minute)

	var wg sync.WaitGroup
	const n = 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := sv.Get(context.Background()); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}

	// Give goroutines time to reach the refresh path.
	time.Sleep(20 * time.Millisecond)
	close(c.blockList)
	wg.Wait()

	if got := c.listVMCalls.Load(); got != 1 {
		t.Errorf("ListVMs calls = %d, want 1 (concurrent Get should coalesce)", got)
	}
}

func TestStateView_ReturnsLastSnapshotOnRefreshError(t *testing.T) {
	c := newCountingOrchard()
	c.vms["gha-orchard-test-1"] = &orchard.VM{Name: "gha-orchard-test-1", Status: orchard.VMStatusRunning}
	sv := NewStateView(c, 5*time.Millisecond)

	snap1, err := sv.Get(context.Background())
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if len(snap1.VMs) != 1 {
		t.Fatalf("snap1 VMs = %d, want 1", len(snap1.VMs))
	}

	time.Sleep(20 * time.Millisecond)
	c.listErr = errors.New("orchard down")

	snap2, err := sv.Get(context.Background())
	if err == nil {
		t.Fatalf("expected error on failed refresh")
	}
	if snap2 == nil {
		t.Fatalf("expected last snapshot returned with error")
	}
	if snap2 != snap1 {
		t.Errorf("expected same snapshot pointer as stale fallback")
	}
}

func TestSnapshot_ManagedVMsForScaleSet(t *testing.T) {
	snap := newSnapshot([]orchard.VM{
		{Name: "gha-orchard-macos-tahoe-xcode-26-4-aaaa"},
		{Name: "gha-orchard-macos-tahoe-xcode-26-4-large-bbbb"},
		{Name: "gha-orchard-other-cccc"},
		{Name: "some-other-vm"},
	}, nil, time.Now())

	got := snap.ManagedVMsForScaleSet("macos-tahoe-xcode-26.4")
	if len(got) != 1 {
		t.Fatalf("got %d VMs, want 1; names: %+v", len(got), got)
	}
	if got[0].Name != "gha-orchard-macos-tahoe-xcode-26-4-aaaa" {
		t.Errorf("unexpected VM: %s", got[0].Name)
	}

	// The -large variant has its own prefix and should not be matched.
	got = snap.ManagedVMsForScaleSet("macos-tahoe-xcode-26.4-large")
	if len(got) != 1 || got[0].Name != "gha-orchard-macos-tahoe-xcode-26-4-large-bbbb" {
		t.Errorf("expected -large match, got %+v", got)
	}
}

func TestSnapshot_ManagedVMsMatchingLabels_ViaWorker(t *testing.T) {
	snap := newSnapshot(
		[]orchard.VM{
			{Name: "gha-orchard-a", Worker: "w1"},
			{Name: "gha-orchard-b", Worker: "w2"},
			{Name: "unmanaged-c", Worker: "w1"},
		},
		[]orchard.Worker{
			{Name: "w1", Labels: map[string]string{"arch": "arm", "size": "large"}},
			{Name: "w2", Labels: map[string]string{"arch": "amd"}},
		},
		time.Now(),
	)

	got := snap.ManagedVMsMatchingLabels(map[string]string{"arch": "arm"})
	if len(got) != 1 || got[0].Name != "gha-orchard-a" {
		t.Errorf("expected only gha-orchard-a, got %+v", got)
	}
}

func TestSnapshot_ManagedVMsMatchingLabels_FallbackToVMLabels(t *testing.T) {
	snap := newSnapshot(
		[]orchard.VM{
			// Pending VM: no Worker assignment yet, but has Labels.
			{Name: "gha-orchard-pending", Labels: map[string]string{"arch": "arm"}},
		},
		nil,
		time.Now(),
	)

	got := snap.ManagedVMsMatchingLabels(map[string]string{"arch": "arm"})
	if len(got) != 1 {
		t.Errorf("expected pending VM matched by own Labels, got %d", len(got))
	}
}
