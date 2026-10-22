package delegate

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// overrunRecorder is an overrun hook that counts its offers and answers with
// handled.
type overrunRecorder struct {
	offers  atomic.Int32
	handled bool
	op      atomic.Value
}

func (r *overrunRecorder) hook(c stallCause) bool {
	r.offers.Add(1)
	r.op.Store(c.op)
	return r.handled
}

// A tool call past its deadline goes to the hook first, once. A call the hook
// ended that returns within the grace is never a stall.
func TestActivityTracker_AToolCallPastItsDeadlineIsOfferedToTheOverrunHookOnce(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	a.overrunGrace = time.Hour
	defer a.stop()
	hook := &overrunRecorder{handled: true}
	defer a.onToolOverrun(hook.hook)()

	end := a.begin("tool:bash", 30*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for hook.offers.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hook.offers.Load() != 1 || hook.op.Load() != "tool:bash" {
		t.Fatalf("hook offers = %d (op %v), want one for tool:bash", hook.offers.Load(), hook.op.Load())
	}
	rec.assertQuietFor(t, 150*time.Millisecond, "a call the hook ended is not a stall while its grace lasts")
	end()
	rec.assertQuietFor(t, 100*time.Millisecond, "the call returned")
	if n := hook.offers.Load(); n != 1 {
		t.Errorf("hook offers = %d, want the one call offered once", n)
	}
}

// The same call is never ended twice: past the grace it is the stall it
// would have been without the hook.
func TestActivityTracker_AToolCallStillRunningAfterItsGraceIsAStall(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	a.overrunGrace = 40 * time.Millisecond
	defer a.stop()
	hook := &overrunRecorder{handled: true}
	defer a.onToolOverrun(hook.hook)()

	defer a.begin("tool:bash", 30*time.Millisecond)()
	cause := rec.waitFired(t, 5*time.Second)
	if cause.op != "tool:bash" {
		t.Errorf("stall op = %q, want tool:bash", cause.op)
	}
	if n := hook.offers.Load(); n != 1 {
		t.Errorf("hook offers = %d, want 1", n)
	}
}

// A hook with nothing to end, such as a restart that is itself stuck, leaves
// the stall to be decided at once rather than after a grace.
func TestActivityTracker_AnOverrunTheHookCannotEndIsAStallAtOnce(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	a.overrunGrace = time.Hour
	defer a.stop()
	hook := &overrunRecorder{handled: false}
	defer a.onToolOverrun(hook.hook)()

	defer a.begin("tool:bash", 30*time.Millisecond)()
	if cause := rec.waitFired(t, 5*time.Second); cause.op != "tool:bash" {
		t.Errorf("stall op = %q, want tool:bash", cause.op)
	}
}

// Only a tool call is offered; every other operation stalls as it always has.
func TestActivityTracker_OnlyAToolCallIsOfferedToTheOverrunHook(t *testing.T) {
	rec := newStallRecorder()
	a := newActivityTracker(time.Hour, rec.stalled)
	defer a.stop()
	hook := &overrunRecorder{handled: true}
	defer a.onToolOverrun(hook.hook)()

	defer a.begin("clone", 30*time.Millisecond)()
	if cause := rec.waitFired(t, 5*time.Second); cause.op != "clone" {
		t.Errorf("stall op = %q, want clone", cause.op)
	}
	if n := hook.offers.Load(); n != 0 {
		t.Errorf("hook offers = %d, want none for a clone", n)
	}
}

// stuckToolHost holds every call until its jail is stopped, then fails it the
// way a socket to a dead jail does.
type stuckToolHost struct{ jail *fakeJail }

func (h stuckToolHost) Call(string, map[string]any) (agentloop.ToolOutcome, error) {
	for !h.jail.Exited() {
		time.Sleep(5 * time.Millisecond)
	}
	return agentloop.ToolOutcome{}, io.EOF
}
func (stuckToolHost) Close() error { return nil }

// jailOneStuck makes jail 1's tool host stuck and every later jail's answer
// with its number.
func jailOneStuck(jail cell.Jail, _ net.Conn) agentloop.ToolHost {
	fj := jail.(*fakeJail)
	if fj.n == 1 {
		return stuckToolHost{jail: fj}
	}
	return jailNamedToolHost{n: fj.n, closed: new(bool)}
}

// endOverrunCall ends the call in flight by stopping its jail, and the call
// learns why; with no call in flight it does nothing and says so.
func TestRestartableJail_EndingAnOverrunCallStopsItsJailAndSaysWhy(t *testing.T) {
	c := &fakeCell{}
	j, err := launchRestartableJail(context.Background(), c, cell.LaunchSpec{RunTree: "/tree"}, jailOneStuck)
	if err != nil {
		t.Fatalf("launchRestartableJail: %v", err)
	}
	if j.endOverrunCall(30 * time.Minute) {
		t.Fatal("ended a call with none in flight")
	}
	if c.jails[0].Exited() {
		t.Fatal("stopped the jail with no call in flight")
	}

	done := make(chan error, 1)
	go func() {
		_, err := j.toolHost().Call("bash", nil)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !j.endOverrunCall(30*time.Minute) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var callErr error
	select {
	case callErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end once its jail was stopped")
	}
	var lost *agentloop.ToolHostLostError
	if !errors.As(callErr, &lost) || !strings.Contains(lost.Notice, "ran past the 30-minute limit") {
		t.Fatalf("call error = %v, want the overrun named", callErr)
	}
	if !errors.Is(callErr, io.EOF) {
		t.Errorf("call error = %v, want the socket's failure kept in the chain", callErr)
	}
}

// The whole backstop through the real engine: a tool call that never returns
// is ended at its deadline by stopping its jail, the loop restarts the jail,
// the call is answered with why it ended, and the conversation goes on to
// conclude. Nothing parks it and nothing asks for a person.
func TestNativeToolOverrun_TheJailIsRestartedAndTheLoopGoesOn(t *testing.T) {
	f := newStallFixture(t, "r-tool-overrun", activityTimings{idle: time.Hour, toolCall: 100 * time.Millisecond})
	c := &fakeCell{}
	launched, err := launchRestartableJail(context.Background(), c, cell.LaunchSpec{RunTree: "/tree"}, jailOneStuck)
	if err != nil {
		t.Fatalf("launchRestartableJail: %v", err)
	}
	defer f.s.activityFor(f.conversationID).onToolOverrun(func(stallCause) bool {
		return launched.endOverrunCall(100 * time.Millisecond)
	})()

	provider := &scriptedGateProvider{turns: []gateTurn{
		{calls: []domain.ToolCall{bashCall("c1", "npm run dev")}},
		{text: "carried on"},
	}}
	result := f.runNative(t, provider, launched.toolHost())
	if result.Kind != agentloop.ResultConcluded {
		t.Fatalf("result = %v (err %v), want the loop to conclude after the restart", result.Kind, result.Err)
	}
	if len(c.jails) != 2 {
		t.Errorf("jails launched = %d, want the stuck one and its replacement", len(c.jails))
	}

	var answer string
	for _, m := range allRows(t, f.s, f.conversationID) {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			answer = m.Content
		}
	}
	for _, want := range []string{"This tool call ran past the 100ms limit on a single tool call", "The sandbox has been restarted"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the stuck call's answer = %q, want it to say %q", answer, want)
		}
	}
	var intent bool
	if err := f.database.QueryRow(`SELECT stop_requested_at IS NOT NULL FROM conversations WHERE id = ?`, f.conversationID).Scan(&intent); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if intent {
		t.Error("the overrun wrote a stop intent; the conversation would park for a person")
	}
	if cause := context.Cause(f.claimCtx); cause != nil {
		t.Errorf("the claim was cancelled (%v); the overrun must not stop the engagement", cause)
	}
}
