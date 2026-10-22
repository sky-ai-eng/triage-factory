package delegate

import (
	"context"
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
}

func (r *overrunRecorder) hook(stallCause) bool {
	r.offers.Add(1)
	return r.handled
}

// A tool call past its deadline is offered to the overrun hook once. A call the
// hook ended but that is still running past its grace is the stall it would
// have been without the hook, and one the hook could not end, such as a
// restart that is itself stuck, is a stall at once.
func TestActivityTracker_AnOverrunTheHookDoesNotEndIsAStall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handled bool
		grace   time.Duration
	}{
		{"ended but still running past its grace", true, 40 * time.Millisecond},
		{"could not be ended", false, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newStallRecorder()
			a := newActivityTracker(time.Hour, rec.stalled)
			a.overrunGrace = tc.grace
			defer a.stop()
			hook := &overrunRecorder{handled: tc.handled}
			defer a.onToolOverrun(hook.hook)()

			defer a.begin("tool:bash", 30*time.Millisecond)()
			if cause := rec.waitFired(t, 5*time.Second); cause.op != "tool:bash" {
				t.Errorf("stall op = %q, want tool:bash", cause.op)
			}
			if n := hook.offers.Load(); n != 1 {
				t.Errorf("hook offers = %d, want the call offered once", n)
			}
		})
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
