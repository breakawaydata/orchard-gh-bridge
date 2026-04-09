package bridge

import "sync"

// Capacity tracks global VM slots across all scale sets.
type Capacity struct {
	mu      sync.Mutex
	current int
	max     int
}

func NewCapacity(max int) *Capacity {
	return &Capacity{max: max}
}

// TryAcquire attempts to reserve n slots. Returns the actual number acquired,
// which may be less than n if capacity is limited. Non-blocking.
func (c *Capacity) TryAcquire(n int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	available := c.max - c.current
	if available <= 0 {
		return 0
	}
	acquired := min(n, available)
	c.current += acquired
	return acquired
}

// Release returns n slots to the pool.
func (c *Capacity) Release(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.current -= n
	if c.current < 0 {
		c.current = 0
	}
}

// Available returns current available slot count.
func (c *Capacity) Available() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max - c.current
}

// InUse returns current number of slots in use.
func (c *Capacity) InUse() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Reconcile sets the in-use count to the given value. Used by the cleanup
// goroutine to correct drift between tracked and actual VM counts.
func (c *Capacity) Reconcile(actual int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = min(actual, c.max)
}

// SetMax updates the maximum capacity (e.g. when workers come/go).
func (c *Capacity) SetMax(max int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.max = max
}
