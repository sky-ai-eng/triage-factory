package delegate

import (
	"sync"
	"time"
)

// WarmWaits is this executor's set of engagements waiting warm: each one's
// turn ended and it holds its claim, in phase domain.ClaimPhaseAwaitingMessage,
// while it waits for a person's message.
// An engagement registers when its wait begins and unregisters when the wait
// ends, however it ends. The registry is how the rest of the executor reaches
// a waiting engagement without holding a reference to it: a person's message
// rings its wake channel, and reclaiming its slot signals its let-go channel.
//
// It is local to the process because a wait is: the engagement it names is a
// goroutine here, and the claim it holds is what tells every other process
// that the conversation is taken.
type WarmWaits struct {
	mu      sync.Mutex
	entries map[string]*WarmWait
	// seq orders entries by when their wait began. Registration happens the
	// moment a wait begins, so registration order is wait order, and a
	// counter cannot tie the way two clock readings can.
	seq uint64
}

// WarmWait is one engagement's registration. Its owner selects on Wake and
// LetGo for the length of its window and calls Unregister when the wait ends.
type WarmWait struct {
	registry       *WarmWaits
	conversationID string
	orgID          string
	waitingSince   time.Time
	seq            uint64
	wake           chan struct{}
	letGo          chan struct{}
}

// NewWarmWaits returns an empty registry.
func NewWarmWaits() *WarmWaits {
	return &WarmWaits{entries: make(map[string]*WarmWait)}
}

// Register records that the engagement on conversationID, in orgID, is now
// waiting warm, and returns its handle. One engagement holds a conversation
// at a time, so a registration already present for conversationID is stale:
// it is let go and replaced.
func (r *WarmWaits) Register(conversationID, orgID string) *WarmWait {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.entries[conversationID]; ok {
		r.letGoLocked(old)
	}
	r.seq++
	w := &WarmWait{
		registry:       r,
		conversationID: conversationID,
		orgID:          orgID,
		waitingSince:   time.Now(),
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
func (r *WarmWaits) Wake(conversationID string) bool {
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
func (r *WarmWaits) LetGoOldest() bool {
	return r.letGoOldestWhere(func(*WarmWait) bool { return true })
}

// LetGoOldestInOrg is LetGoOldest over the engagements of one org.
func (r *WarmWaits) LetGoOldestInOrg(orgID string) bool {
	return r.letGoOldestWhere(func(w *WarmWait) bool { return w.orgID == orgID })
}

// LetGoAll tells every waiting engagement to end its window now, and
// unregisters them all.
func (r *WarmWaits) LetGoAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.entries {
		r.letGoLocked(w)
	}
}

// Len is how many engagements are waiting.
func (r *WarmWaits) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *WarmWaits) letGoOldestWhere(match func(*WarmWait) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var oldest *WarmWait
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

func (r *WarmWaits) letGoLocked(w *WarmWait) {
	ringOnce(w.letGo)
	delete(r.entries, w.conversationID)
}

// Wake receives when a person's message may be waiting.
func (w *WarmWait) Wake() <-chan struct{} { return w.wake }

// LetGo receives when the engagement should end its window now.
func (w *WarmWait) LetGo() <-chan struct{} { return w.letGo }

// OrgID is the org the waiting engagement's conversation belongs to.
func (w *WarmWait) OrgID() string { return w.orgID }

// WaitingSince is when the engagement registered, which is when its turn ended.
func (w *WarmWait) WaitingSince() time.Time { return w.waitingSince }

// Unregister takes the engagement out of the registry, after which nothing
// rings its channels. Calling it again, or after the registry let it go, does
// nothing.
func (w *WarmWait) Unregister() {
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
