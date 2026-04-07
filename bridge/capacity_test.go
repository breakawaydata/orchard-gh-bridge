package bridge

import "testing"

func TestCapacity_TryAcquire(t *testing.T) {
	c := NewCapacity(4)

	if got := c.TryAcquire(2); got != 2 {
		t.Errorf("TryAcquire(2) = %d, want 2", got)
	}
	if got := c.InUse(); got != 2 {
		t.Errorf("InUse = %d, want 2", got)
	}
	if got := c.Available(); got != 2 {
		t.Errorf("Available = %d, want 2", got)
	}

	// Request more than available
	if got := c.TryAcquire(3); got != 2 {
		t.Errorf("TryAcquire(3) = %d, want 2", got)
	}
	if got := c.InUse(); got != 4 {
		t.Errorf("InUse = %d, want 4", got)
	}

	// At capacity
	if got := c.TryAcquire(1); got != 0 {
		t.Errorf("TryAcquire(1) = %d, want 0", got)
	}
}

func TestCapacity_Release(t *testing.T) {
	c := NewCapacity(4)
	c.TryAcquire(3)
	c.Release(2)

	if got := c.InUse(); got != 1 {
		t.Errorf("InUse = %d, want 1", got)
	}
	if got := c.Available(); got != 3 {
		t.Errorf("Available = %d, want 3", got)
	}
}

func TestCapacity_ReleaseFloor(t *testing.T) {
	c := NewCapacity(4)
	c.Release(5) // underflow protection
	if got := c.InUse(); got != 0 {
		t.Errorf("InUse = %d, want 0", got)
	}
}

func TestCapacity_Reconcile(t *testing.T) {
	c := NewCapacity(4)
	c.TryAcquire(3)
	c.Reconcile(1) // actual VMs is 1
	if got := c.InUse(); got != 1 {
		t.Errorf("InUse = %d, want 1", got)
	}
}
