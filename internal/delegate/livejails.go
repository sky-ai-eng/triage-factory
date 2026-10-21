package delegate

import (
	"sort"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
)

// liveJails is every jail this process is running an engagement in right
// now, and the claim each one bills to. It exists so an observer on a timer —
// the resource sampler — can ask "what jails are live?" without holding a
// reference to any engagement. The engagement that launched a jail registers
// it, so the registry needs nothing from a backend beyond cell.Jail.
//
// The zero value is ready to use.
type liveJails struct {
	mu      sync.Mutex
	nextKey uint64
	byKey   map[uint64]liveJail
}

// liveJail is one registered jail and the claim that pays for it.
type liveJail struct {
	claimID string
	jail    cell.Jail
}

// add records jail as live for claimID and returns the release that drops it
// again. A launch with no claim to attribute is not recorded at all, since
// its usage has nowhere to go; its release is a no-op, so a caller can
// register unconditionally and defer the release the same way.
//
// The release is idempotent: an entry outliving its jail would have the
// sampler read a jail that is gone on every tick from then on.
func (r *liveJails) add(claimID string, jail cell.Jail) (release func()) {
	if claimID == "" || jail == nil {
		return func() {}
	}
	r.mu.Lock()
	if r.byKey == nil {
		r.byKey = make(map[uint64]liveJail)
	}
	r.nextKey++
	key := r.nextKey
	r.byKey[key] = liveJail{claimID: claimID, jail: jail}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.byKey, key)
			r.mu.Unlock()
		})
	}
}

// snapshot returns every live jail, ordered by claim id so repeated reads are
// stable. Empty is the common answer and the cheap one: a control pod never
// launches a jail, and an idle executor has none live.
func (r *liveJails) snapshot() []liveJail {
	r.mu.Lock()
	out := make([]liveJail, 0, len(r.byKey))
	for _, j := range r.byKey {
		out = append(out, j)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].claimID < out[j].claimID })
	return out
}
