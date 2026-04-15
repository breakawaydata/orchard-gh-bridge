package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

type mockOrchardClient struct {
	mu      sync.Mutex
	vms     map[string]*orchard.VM
	workers []orchard.Worker

	createErr error
	deleteErr error
}

func newMockOrchard() *mockOrchardClient {
	return &mockOrchardClient{vms: make(map[string]*orchard.VM)}
}

func (m *mockOrchardClient) CreateVM(_ context.Context, vm *orchard.VM) (*orchard.VM, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	created := *vm
	created.Status = orchard.VMStatusCreating
	m.vms[vm.Name] = &created
	return &created, nil
}

func (m *mockOrchardClient) GetVM(_ context.Context, name string) (*orchard.VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm, ok := m.vms[name]
	if !ok {
		return nil, orchard.ErrNotFound
	}
	return vm, nil
}

func (m *mockOrchardClient) ListVMs(_ context.Context) ([]orchard.VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []orchard.VM
	for _, vm := range m.vms {
		result = append(result, *vm)
	}
	return result, nil
}

func (m *mockOrchardClient) DeleteVM(_ context.Context, name string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vms, name)
	return nil
}

func (m *mockOrchardClient) ListWorkers(_ context.Context) ([]orchard.Worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers != nil {
		out := make([]orchard.Worker, len(m.workers))
		copy(out, m.workers)
		return out, nil
	}
	return []orchard.Worker{{Name: "worker-1"}}, nil
}

func (m *mockOrchardClient) Ping(_ context.Context) error {
	return nil
}

func (m *mockOrchardClient) vmCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.vms)
}

func TestCapacity_Basic(t *testing.T) {
	cap := NewCapacity(4)
	if got := cap.Available(); got != 4 {
		t.Errorf("Available = %d, want 4", got)
	}
	if got := cap.TryAcquire(2); got != 2 {
		t.Errorf("TryAcquire(2) = %d, want 2", got)
	}
	if got := cap.Available(); got != 2 {
		t.Errorf("Available = %d, want 2", got)
	}
}

func TestCleanup_ReapsStopped(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["gha-orchard-test-1"] = &orchard.VM{
		Name:   "gha-orchard-test-1",
		Status: orchard.VMStatusStopped,
	}
	mock.vms["gha-orchard-test-2"] = &orchard.VM{
		Name:   "gha-orchard-test-2",
		Status: orchard.VMStatusRunning,
	}

	cap := NewCapacity(4)
	cap.TryAcquire(2)

	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.sweep(context.Background())

	if mock.vmCount() != 1 {
		t.Errorf("VM count = %d, want 1 (stopped should be deleted)", mock.vmCount())
	}
	if cap.InUse() != 1 {
		t.Errorf("capacity in use = %d, want 1 (reconciled)", cap.InUse())
	}
}

func TestCleanup_NotifiesBridgeOnReap(t *testing.T) {
	mock := newMockOrchard()
	vmName := "gha-orchard-test-stale"
	mock.vms[vmName] = &orchard.VM{
		Name:   vmName,
		Status: orchard.VMStatusFailed,
	}

	cap := NewCapacity(4)
	cap.TryAcquire(1)

	// Simulate a bridge tracking this VM as active
	b := New(Config{
		ScaleSetName:  "test",
		OrchardClient: mock,
		Capacity:      cap,
		Logger:        testLogger(),
	})
	b.mu.Lock()
	b.activeVMs[vmName] = vmName
	b.mu.Unlock()

	if b.ActiveVMCount() != 1 {
		t.Fatalf("activeVMs = %d, want 1", b.ActiveVMCount())
	}

	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.SetOnVMCleaned(func(name string) {
		b.PurgeActiveVM(name)
	})
	cleanup.sweep(context.Background())

	if b.ActiveVMCount() != 0 {
		t.Errorf("activeVMs = %d, want 0 (stale VM should be purged)", b.ActiveVMCount())
	}
}

func TestHandleDesired_BlocksWhenNoFreeSlots(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		{Name: "w1", Resources: map[string]uint64{resourceTartVMs: 2}, Labels: map[string]string{"arch": "arm"}},
	}
	// Two matching VMs already exist → 0 free slots on the arm worker.
	mock.vms["gha-orchard-test-aaaaaaaa"] = &orchard.VM{
		Name: "gha-orchard-test-aaaaaaaa", Worker: "w1", Status: orchard.VMStatusCreating,
	}
	mock.vms["gha-orchard-test-bbbbbbbb"] = &orchard.VM{
		Name: "gha-orchard-test-bbbbbbbb", Worker: "w1", Status: orchard.VMStatusRunning,
	}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute)

	// GHClient intentionally nil: if the gate fails to short-circuit,
	// HandleDesiredRunnerCount will panic when it tries to generate a JIT
	// config — which is exactly the test signal we want.
	b := New(Config{
		ScaleSetName:  "test",
		VMConfig:      config.VMConfig{Labels: map[string]string{"arch": "arm"}},
		OrchardClient: mock,
		Capacity:      cap,
		State:         sv,
		Logger:        testLogger(),
	})

	got, err := b.HandleDesiredRunnerCount(context.Background(), 5)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 {
		t.Errorf("active count = %d, want 0 (gate should have blocked)", got)
	}
	if cap.InUse() != 0 {
		t.Errorf("Capacity.InUse = %d, want 0 (no slot reserved when blocked)", cap.InUse())
	}
	if mock.vmCount() != 2 {
		t.Errorf("VM count = %d, want 2 (no VM should have been created)", mock.vmCount())
	}
}

func TestHandleDesired_RespectsLabelsForCapacity(t *testing.T) {
	mock := newMockOrchard()
	mock.workers = []orchard.Worker{
		{Name: "arm", Resources: map[string]uint64{resourceTartVMs: 1}, Labels: map[string]string{"arch": "arm"}},
		{Name: "amd", Resources: map[string]uint64{resourceTartVMs: 4}, Labels: map[string]string{"arch": "amd"}},
	}
	// Fill the arm worker.
	mock.vms["gha-orchard-test-cccccccc"] = &orchard.VM{
		Name: "gha-orchard-test-cccccccc", Worker: "arm", Status: orchard.VMStatusRunning,
	}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute)
	b := New(Config{
		ScaleSetName:  "test",
		VMConfig:      config.VMConfig{Labels: map[string]string{"arch": "arm"}},
		OrchardClient: mock,
		Capacity:      cap,
		State:         sv,
		Logger:        testLogger(),
	})

	got, err := b.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 {
		t.Errorf("active count = %d, want 0 (all arm capacity used; amd doesn't count)", got)
	}
}

func TestHydrateFromOrchard_AdoptsExisting(t *testing.T) {
	mock := newMockOrchard()
	// Three VMs belonging to the test scale set, one from a different one.
	mock.vms["gha-orchard-test-11111111"] = &orchard.VM{Name: "gha-orchard-test-11111111", Status: orchard.VMStatusCreating}
	mock.vms["gha-orchard-test-22222222"] = &orchard.VM{Name: "gha-orchard-test-22222222", Status: orchard.VMStatusRunning}
	mock.vms["gha-orchard-test-33333333"] = &orchard.VM{Name: "gha-orchard-test-33333333", Status: orchard.VMStatusCreating}
	mock.vms["gha-orchard-other-44444444"] = &orchard.VM{Name: "gha-orchard-other-44444444", Status: orchard.VMStatusRunning}
	// Terminal-state VMs should not be adopted — cleanup will reap them.
	mock.vms["gha-orchard-test-55555555"] = &orchard.VM{Name: "gha-orchard-test-55555555", Status: orchard.VMStatusStopped}

	cap := NewCapacity(10)
	sv := NewStateView(mock, time.Minute)
	b := New(Config{
		ScaleSetName:  "test",
		OrchardClient: mock,
		Capacity:      cap,
		State:         sv,
		Logger:        testLogger(),
	})

	adopted, err := b.HydrateFromOrchard(context.Background())
	if err != nil {
		t.Fatalf("HydrateFromOrchard: %v", err)
	}
	if adopted != 3 {
		t.Errorf("adopted = %d, want 3", adopted)
	}
	if b.ActiveVMCount() != 3 {
		t.Errorf("ActiveVMCount = %d, want 3", b.ActiveVMCount())
	}
}

func TestCleanup_IgnoresUnmanaged(t *testing.T) {
	mock := newMockOrchard()
	mock.vms["unmanaged"] = &orchard.VM{
		Name:   "unmanaged",
		Status: orchard.VMStatusStopped,
	}

	cap := NewCapacity(4)
	cleanup := NewCleanup(mock, cap, nil, testLogger())
	cleanup.sweep(context.Background())

	if mock.vmCount() != 1 {
		t.Errorf("unmanaged VM should not be deleted")
	}
}
