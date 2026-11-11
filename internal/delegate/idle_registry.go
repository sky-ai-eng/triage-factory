package delegate

import (
	"sync"
	"time"
)

// IdleRegistry is this executor's set of engagements waiting warm: each one's
// turn ended and it holds its claim while it waits for a person's message.
// An engagement registers when its wait begins and unregisters when the wait
// ends, however it ends. The registry is how the rest of the executor reaches
// a waiting engagement without holding a reference to it: a person's message
// rings its wake channel, and reclaiming its slot signals its let-go channel.
//
// It is local to the process because a wait is: the engagement it names is a
// goroutine here, and the claim it holds is what tells every other process
// that the conversation is taken.
type IdleRegistry struct {
	mu      sync.Mutex
	entries map[string]*IdleWait
	// seq orders entries by when they went idle. Registration happens the
	// moment a wait begins, so registration order is idle order, and a
	// counter cannot tie the way two clock readings can.
	seq uint64
}

// IdleWait is one engagement's registration. Its owner selects on Wake and
// LetGo for the length of its window and calls Unregister when the wait ends.
type IdleWait struct {
	registry       *IdleRegistry
	conversationID string
	orgID          string
	idleSince      time.Time
	seq            uint64
	wake           chan struct{}
	letGo          chan struct{}
}

// NewIdleRegistry returns an empty registry.
func NewIdleRegistry() *IdleRegistry {
	return &IdleRegistry{entries: make(map[string]*IdleWait)}
}

// Register records that the engagement on conversationID, in orgID, is now
// waiting warm, and returns its handle. One engagement holds a conversation
// at a time, so a registration already present for conversationID is stale:
// it is let go and replaced.
func (r *IdleRegistry) Register(conversationID, orgID string) *IdleWait {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.entries[conversationID]; ok {
		r.letGoLocked(old)
	}
	r.seq++
	w := &IdleWait{
		registry:       r,
		conversationID: conversationID,
		orgID:          orgID,
		idleSince:      time.Now(),
		seq:            r.seq,
		// Capacity 1, so a ring never blocks the ringer, and a second ring
		// before the owner reads the first is the same news.
		wake:  make(chan struct{}, 1),
		letGo: make(chan struct{}, 1),
	}
	r.entries[conversationID] = w
	return w
}

// Wake rings the wake channel of the engagement waiting on conversationID: a
// person's message may be waiting for it. It reports whether an engagement
// was registered.
func (r *IdleRegistry) Wake(conversationID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.entries[conversationID]
	if !ok {
		return false
	}
	ringOnce(w.wake)
	return true
}

// LetGoOldest tells the engagement that has waited longest to end its window
// now, and unregisters it. It reports whether any engagement was waiting.
func (r *IdleRegistry) LetGoOldest() bool {
	return r.letGoOldestWhere(func(*IdleWait) bool { return true })
}

// LetGoOldestInOrg is LetGoOldest over the engagements of one org.
func (r *IdleRegistry) LetGoOldestInOrg(orgID string) bool {
	return r.letGoOldestWhere(func(w *IdleWait) bool { return w.orgID == orgID })
}

// LetGoAll tells every waiting engagement to end its window now, and
// unregisters them all.
func (r *IdleRegistry) LetGoAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.entries {
		r.letGoLocked(w)
	}
}

// Len is how many engagements are waiting.
func (r *IdleRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *IdleRegistry) letGoOldestWhere(match func(*IdleWait) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var oldest *IdleWait
	for _, w := range r.entries {
		if match(w) && (oldest == nil || w.seq < oldest.seq) {
			oldest = w
		}
	}
	if oldest == nil {
		return false
	}
	r.letGoLocked(oldest)
	return true
}

func (r *IdleRegistry) letGoLocked(w *IdleWait) {
	ringOnce(w.letGo)
	delete(r.entries, w.conversationID)
}

// Wake receives when a person's message may be waiting.
func (w *IdleWait) Wake() <-chan struct{} { return w.wake }

// LetGo receives when the engagement should end its window now.
func (w *IdleWait) LetGo() <-chan struct{} { return w.letGo }

// OrgID is the org the waiting engagement's conversation belongs to.
func (w *IdleWait) OrgID() string { return w.orgID }

// IdleSince is when the engagement registered, which is when its turn ended.
func (w *IdleWait) IdleSince() time.Time { return w.idleSince }

// Unregister takes the engagement out of the registry, after which nothing
// rings its channels. Calling it again, or after the registry let it go, does
// nothing.
func (w *IdleWait) Unregister() {
	r := w.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[w.conversationID] == w {
		delete(r.entries, w.conversationID)
	}
}

func ringOnce(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
