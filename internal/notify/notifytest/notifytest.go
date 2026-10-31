// Package notifytest records the notify verbs a producer calls, for its tests:
//
//	rec := notifytest.Record(t)
//	// … run the producer …
//	rec.Active(t, orgID, kinds.LinearTeamUnreadable)
//
// A recorder models what the verbs leave open, per org and kind, from the
// calls alone: it resolves no audience, renders no copy and writes nothing.
// Every recorder sees every call made while it is installed, and its
// assertions read only the org they are given, so parallel tests that each use
// their own org do not see each other.
package notifytest

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
)

// Occurrence is one open occurrence as the recorder models it.
type Occurrence struct {
	Key string
	// Params is the P of the call that last raised it.
	Params any
	// Seen counts the raises since it opened.
	Seen int
}

// Recorder holds the occurrences the verbs left open.
type Recorder struct {
	mu sync.Mutex
	// open is org → kind name → key.
	open  map[string]map[string]map[string]Occurrence
	kinds map[string]sink.Kind
}

var installed = struct {
	mu        sync.Mutex
	recorders map[*Recorder]struct{}
	prev      sink.Sink
}{recorders: map[*Recorder]struct{}{}}

// Record installs a recorder for the rest of the test. The verbs reach it in
// place of whatever was installed before, which is restored when the last
// recorder's test ends.
func Record(t testing.TB) *Recorder {
	t.Helper()
	r := &Recorder{open: map[string]map[string]map[string]Occurrence{}, kinds: map[string]sink.Kind{}}
	installed.mu.Lock()
	if len(installed.recorders) == 0 {
		installed.prev = sink.Swap(fanout{})
	}
	installed.recorders[r] = struct{}{}
	installed.mu.Unlock()
	t.Cleanup(func() {
		installed.mu.Lock()
		defer installed.mu.Unlock()
		delete(installed.recorders, r)
		if len(installed.recorders) == 0 {
			sink.Swap(installed.prev)
			installed.prev = nil
		}
	})
	return r
}

// Isolate puts back, when the test ends, whatever notify had installed when
// it was called: for a test that runs notify.Install itself.
func Isolate(t testing.TB) {
	t.Helper()
	prev := sink.Load()
	t.Cleanup(func() { sink.Swap(prev) })
}

// Open returns the kind's open occurrences in orgID, ordered by key.
func (r *Recorder) Open(orgID string, k notify.Kind) []Occurrence {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Occurrence
	for _, occ := range r.open[orgID][k.Name()] {
		out = append(out, occ)
	}
	slices.SortFunc(out, func(a, b Occurrence) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Active fails t unless the kind has an open occurrence in orgID, or, given
// keys, unless each of them is open.
func (r *Recorder) Active(t testing.TB, orgID string, k notify.Kind, keys ...string) {
	t.Helper()
	open := r.Open(orgID, k)
	if len(keys) == 0 {
		if len(open) == 0 {
			t.Errorf("notifytest: %s has nothing open in org %s", k.Name(), orgID)
		}
		return
	}
	for _, key := range keys {
		if !slices.ContainsFunc(open, func(o Occurrence) bool { return o.Key == key }) {
			t.Errorf("notifytest: %s key %q is not open in org %s; open: %v", k.Name(), key, orgID, keysOf(open))
		}
	}
}

// Inactive fails t if the kind has anything open in orgID.
func (r *Recorder) Inactive(t testing.TB, orgID string, k notify.Kind) {
	t.Helper()
	if open := r.Open(orgID, k); len(open) > 0 {
		t.Errorf("notifytest: %s has %v open in org %s; want nothing", k.Name(), keysOf(open), orgID)
	}
}

func keysOf(occs []Occurrence) []string {
	out := make([]string, len(occs))
	for i, o := range occs {
		out[i] = o.Key
	}
	return out
}

func (r *Recorder) raise(k sink.Kind, orgID string, p any) {
	byKind := r.open[orgID]
	if byKind == nil {
		byKind = map[string]map[string]Occurrence{}
		r.open[orgID] = byKind
	}
	byKey := byKind[k.Name()]
	if byKey == nil {
		byKey = map[string]Occurrence{}
		byKind[k.Name()] = byKey
	}
	key := k.Key(p)
	occ := byKey[key]
	byKey[key] = Occurrence{Key: key, Params: p, Seen: occ.Seen + 1}
	r.kinds[k.Name()] = k
}

func (r *Recorder) set(k sink.Kind, orgID string, p any, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if active {
		r.raise(k, orgID, p)
		return
	}
	delete(r.open[orgID][k.Name()], k.Key(p))
}

func (r *Recorder) sync(k sink.Kind, orgID string, active, keep []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// One raise per key, with the last member's params, as the engine writes.
	latest := map[string]any{}
	var keys []string
	for _, p := range active {
		key := k.Key(p)
		if _, seen := latest[key]; !seen {
			keys = append(keys, key)
		}
		latest[key] = p
	}
	kept := map[string]bool{}
	for _, p := range keep {
		kept[k.Key(p)] = true
	}
	for key := range r.open[orgID][k.Name()] {
		if _, raised := latest[key]; !raised && !kept[key] {
			delete(r.open[orgID][k.Name()], key)
		}
	}
	for _, key := range keys {
		r.raise(k, orgID, latest[key])
	}
}

func (r *Recorder) post(k sink.Kind, orgID string, p any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raise(k, orgID, p)
}

// clearSource drops the source's conditions; ClearSource leaves notices to
// expire, and an empty source names no kind's.
func (r *Recorder) clearSource(orgID, source string) {
	if source == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.open[orgID] {
		if k := r.kinds[name]; k.Source() == source && k.Lifecycle() == domain.NotificationCondition {
			delete(r.open[orgID], name)
		}
	}
}

// fanout is the installed sink: it hands every call to every recorder.
type fanout struct{}

func recorders() []*Recorder {
	installed.mu.Lock()
	defer installed.mu.Unlock()
	out := make([]*Recorder, 0, len(installed.recorders))
	for r := range installed.recorders {
		out = append(out, r)
	}
	return out
}

func (fanout) Set(_ context.Context, k sink.Kind, orgID string, p any, active bool) {
	for _, r := range recorders() {
		r.set(k, orgID, p, active)
	}
}

func (fanout) Sync(_ context.Context, k sink.Kind, orgID string, active, keep []any) {
	for _, r := range recorders() {
		r.sync(k, orgID, active, keep)
	}
}

func (fanout) Post(_ context.Context, k sink.Kind, orgID string, p any) {
	for _, r := range recorders() {
		r.post(k, orgID, p)
	}
}

func (fanout) ClearSource(_ context.Context, orgID, source string) {
	for _, r := range recorders() {
		r.clearSource(orgID, source)
	}
}
