package bridge

import (
	"context"
	"sync"
	"testing"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

type mockOrchardClient struct {
	mu  sync.Mutex
	vms map[string]*orchard.VM

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
