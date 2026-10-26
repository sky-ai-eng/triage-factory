package commitsigner

import "sync"

// The kinds of the tf_ctl messages the signing queue rides (ctlbus.Message).
const (
	// KindSignRequest rings every control pod: a request was queued.
	KindSignRequest = "sign_request"
	// KindSignDone rings the executor waiting on a request: it was decided.
	KindSignDone = "sign_done"
)

// Doorbells routes sign_done notifications to the requests an executor is
// waiting on. One per executor process; the tf_ctl listener rings it, and
// each waiting request listens for its own row.
type Doorbells struct {
	mu      sync.Mutex
	waiting map[string]chan struct{}
}

// NewDoorbells returns an empty set of doorbells.
func NewDoorbells() *Doorbells {
	return &Doorbells{waiting: make(map[string]chan struct{})}
}

// Listen registers for the doorbell of signature id and returns the channel
// it rings on, and the function that stops listening. One listener per id:
// only the request that inserted the row waits on it. A nil set of doorbells
// never rings, and its waiter reads the row on the poll alone.
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

// Ring wakes whatever is listening for signature id, and does nothing when
// nothing is: every executor hears every sign_done, and only one of them
// asked. It never blocks, because the tf_ctl read loop calls it. A nil set of
// doorbells (a process that queues no requests) ignores every ring.
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
