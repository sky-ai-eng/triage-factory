package ctlbus

import "sync"

// Doorbells routes a tf_ctl doorbell to whatever in this process listens for
// the id it names. The process's tf_ctl listener rings it, and each waiter
// listens for its own id, so a doorbell wakes the one wait it is about
// instead of every wait in the process.
type Doorbells struct {
	mu      sync.Mutex
	waiting map[string]chan struct{}
}

// NewDoorbells returns an empty set of doorbells.
func NewDoorbells() *Doorbells {
	return &Doorbells{waiting: make(map[string]chan struct{})}
}

// Listen registers for the doorbell of id and returns the channel it rings
// on, and the function that stops listening. One listener per id: only the
// waiter that owns the id waits on it. A nil set of doorbells never rings,
// and its waiter reads on its poll alone.
func (d *Doorbells) Listen(id string) (<-chan struct{}, func()) {
	if d == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.waiting[id] = ch
	d.mu.Unlock()
	return ch, func() {
		d.mu.Lock()
		if d.waiting[id] == ch {
			delete(d.waiting, id)
		}
		d.mu.Unlock()
	}
}

// Ring wakes whatever is listening for id, and does nothing when nothing is:
// every process hears every tf_ctl message, and at most one of them is
// waiting on the id. It never blocks, because the tf_ctl read loop calls it.
// A nil set of doorbells (a process with nothing to wait on) ignores every
// ring.
func (d *Doorbells) Ring(id string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	ch := d.waiting[id]
	d.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}
