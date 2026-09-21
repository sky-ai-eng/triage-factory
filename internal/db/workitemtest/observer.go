package workitemtest

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// recorder is the suite's workitem.Observer: every call is rendered as one
// line, in order, so a subtest asserts the exact sequence the package
// reported for the dispositions it drove. Guarded because a claim under test
// may run on another goroutine.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *recorder) Claimed(orgID string, leased, reclaimed, cancelled, parked int) {
	r.record("claimed %s %d/%d/%d/%d", orgID, leased, reclaimed, cancelled, parked)
}
func (r *recorder) Completed(orgID string) { r.record("completed %s", orgID) }
func (r *recorder) Requeued(orgID string, outcome workitem.Outcome) {
	r.record("requeued %s %s", orgID, outcome)
}
func (r *recorder) Parked(orgID, reason string) { r.record("parked %s %s", orgID, reason) }
func (r *recorder) Deferred(orgID string)       { r.record("deferred %s", orgID) }
func (r *recorder) Cancelled(orgID string)      { r.record("cancelled %s", orgID) }
func (r *recorder) LeaseLost(orgID, op string)  { r.record("lease_lost %s %s", orgID, op) }
func (r *recorder) Redriven(orgID string)       { r.record("redriven %s", orgID) }
func (r *recorder) Superseded(orgID string)     { r.record("superseded %s", orgID) }

// take returns everything recorded since the last take, and clears it.
func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// requireObserved asserts the calls since the last check are exactly want,
// each rendered for the subtest's own org: "claimed 1/0/0/0", "completed",
// "requeued transient", "lease_lost complete".
func (e *env) requireObserved(want ...string) {
	e.t.Helper()
	got := e.obs.take()
	expect := make([]string, len(want))
	for i, w := range want {
		// The org is the second word of every rendering.
		verb, rest := w, ""
		for j := 0; j < len(w); j++ {
			if w[j] == ' ' {
				verb, rest = w[:j], w[j:]
				break
			}
		}
		expect[i] = verb + " " + e.org + rest
	}
	if len(got) == 0 && len(expect) == 0 {
		return
	}
	if !reflect.DeepEqual(got, expect) {
		e.t.Fatalf("observer saw\n  %q\nwant\n  %q", got, expect)
	}
}
