package agentloop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// restartableHost is a tool host whose jail a test kills: a call to a tool in
// dies fails as a dead tool host's would, until a Restart clears the set, and
// with alwaysDies every tool call fails. restartErr fails the restarts
// instead. onCall runs at the start of every call.
type restartableHost struct {
	mu         sync.Mutex
	dies       map[string]bool
	alwaysDies bool
	restartErr error
	onCall     func()
	restarts   int
	observed   []string
}

func (h *restartableHost) Call(name string, _ map[string]any) (ToolOutcome, error) {
	if h.onCall != nil {
		h.onCall()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observed = append(h.observed, name)
	if (h.alwaysDies && name != toolHostConfigureTool) || h.dies[name] {
		return ToolOutcome{}, io.EOF
	}
	return ToolOutcome{Content: "ok:" + name}, nil
}

func (h *restartableHost) Restart(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restartErr != nil {
		return h.restartErr
	}
	h.restarts++
	h.dies = nil
	return nil
}

func (*restartableHost) Close() error { return nil }

func (h *restartableHost) kill(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dies = map[string]bool{name: true}
}

func (h *restartableHost) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, o := range h.observed {
		if o == name {
			n++
		}
	}
	return n
}

// The socket says which side of the request a dead tool host died on: a write
// that failed never reached it, a read that failed after the write might
// have.
func TestSocketToolHost_MarksARequestThatNeverLeft(t *testing.T) {
	t.Run("the peer is gone before the call", func(t *testing.T) {
		// A unix socket, as the tool host's is: a write to one whose peer
		// has closed fails at once rather than being buffered.
		listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "tools.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			c, _ := listener.Accept()
			accepted <- c
		}()
		a, err := net.Dial("unix", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if peer := <-accepted; peer != nil {
			_ = peer.Close()
		}
		host := NewToolHost(a, time.Second)
		defer host.Close()
		_, err = host.Call("bash", nil)
		if !errors.Is(err, errRequestNotSent) {
			t.Fatalf("err = %v, want it marked as never sent", err)
		}
	})
	t.Run("the peer takes the request and dies", func(t *testing.T) {
		a, b := net.Pipe()
		go func() {
			_, _ = readFrame(b)
			_ = b.Close()
		}()
		host := NewToolHost(a, time.Second)
		defer host.Close()
		_, err := host.Call("bash", nil)
		if err == nil || errors.Is(err, errRequestNotSent) {
			t.Fatalf("err = %v, want a failure that does not claim the call never ran", err)
		}
	})
}

// A tool host that died and is restarted costs the engagement nothing but the
// calls it took with it: the call it died in is told what is known about it,
// the calls after it that they never ran, both that the sandbox is back, and
// the loop goes on against the new tool host, configured again.
func TestRun_ADeadToolHostIsRestartedAndTheLoopGoesOn(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	host := &restartableHost{dies: map[string]bool{"bash": true}}
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}, {ID: "c2", Name: "read"}}},
		{calls: []domain.ToolCall{{ID: "c3", Name: "bash"}}},
		{text: "done"},
	}}
	got := newTestEngine(tr, p, host).Run(context.Background(), budgetParams(2048))
	if got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	if host.restarts != 1 {
		t.Errorf("restarts = %d, want 1", host.restarts)
	}
	if n := host.count(toolHostConfigureTool); n != 2 {
		t.Errorf("configure frames = %d, want 2: the new tool host needs the engagement's policy too", n)
	}
	if n := host.count("read"); n != 0 {
		t.Errorf("read reached the tool host %d times; a call after the one the jail died in is left to the model", n)
	}
	want := map[string]string{
		"c1": lostInFlightNotice + " " + restartedSentence,
		"c2": restartedRemainingNotice,
		"c3": "ok:bash",
	}
	results := tr.toolResults()
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want one per call", results)
	}
	for _, r := range results {
		if r.Content != want[r.ToolCallID] {
			t.Errorf("result for %s = %q, want %q", r.ToolCallID, r.Content, want[r.ToolCallID])
		}
	}
}

// A dead tool host the engine cannot restart ends the engagement with its
// tool host lost, never failed: every call is answered with what is known
// about it, and told the run goes on in a new sandbox.
func TestRun_ADeadToolHostThatCannotRestartIsLost(t *testing.T) {
	notSent := fmt.Errorf("agentloop: write tool request: %w: %w", errRequestNotSent, io.ErrClosedPipe)
	cases := []struct {
		name string
		host func() ToolHost
		// first is the running call's result.
		first  string
		reason string
	}{
		{
			name: "the host died after taking the request",
			host: func() ToolHost {
				h := newScriptedToolHost()
				h.errs["bash"] = io.EOF
				return h
			},
			first:  lostInFlightNotice + " " + newSandboxSentence,
			reason: lostSandboxReason,
		},
		{
			name: "the host was gone before the request left",
			host: func() ToolHost {
				h := newScriptedToolHost()
				h.errs["bash"] = notSent
				return h
			},
			first:  lostBeforeSentNotice + " " + newSandboxSentence,
			reason: lostSandboxReason,
		},
		{
			name: "the host refused the request and closed its connection",
			host: func() ToolHost {
				h := newScriptedToolHost()
				h.answers["bash"] = ToolOutcome{Protocol: &ProtocolError{Kind: protoRequestTooLarge, Message: "frame length 99 exceeds cap 16"}}
				return h
			},
			first: "This tool call was not executed: the tool host refused it and closed its connection " +
				"(tool host protocol error (request_too_large): frame length 99 exceeds cap 16). " + newSandboxSentence,
			reason: "the sandbox's tool host refused a request and closed its connection",
		},
		{
			name: "the restart failed",
			host: func() ToolHost {
				return &restartableHost{dies: map[string]bool{"bash": true}, restartErr: errors.New("launch failed")}
			},
			first:  lostInFlightNotice + " " + newSandboxSentence,
			reason: lostSandboxReason,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("go"))
			p := &scriptedProvider{turns: []scriptedTurn{
				{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}, {ID: "c2", Name: "read"}}},
			}}
			got := newTestEngine(tr, p, tc.host()).Run(context.Background(), testParams())
			if got.Kind != ResultToolHostLost {
				t.Fatalf("disposition = %v, want tool host lost (err: %v)", got.Kind, got.Err)
			}
			if lost, ok := lostToolHost(got.Err); !ok || lost.Reason != tc.reason {
				t.Errorf("Err = %+v, want a lost tool host with reason %q", got.Err, tc.reason)
			}
			results := tr.toolResults()
			if len(results) != 2 {
				t.Fatalf("both calls need results, got %+v", results)
			}
			if !results[0].IsError || results[0].Content != tc.first {
				t.Errorf("the running call's result = %q, want %q", results[0].Content, tc.first)
			}
			if !results[1].IsError || results[1].Content != fatalToolHostNotice {
				t.Errorf("the call after it = %q, want the never-executed notice", results[1].Content)
			}
		})
	}
}

// The restarts are bounded by calls completing, not counted over the
// engagement: a tool host that dies again before any call finishes is given
// maxConsecutiveRestarts new jails and no more, while one that does work
// between deaths is restarted every time.
func TestRun_ToolHostRestartsAreBoundedByCallsCompleting(t *testing.T) {
	t.Run("dying with nothing completing", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := &restartableHost{alwaysDies: true}
		var turns []scriptedTurn
		for i := 0; i <= maxConsecutiveRestarts; i++ {
			turns = append(turns, scriptedTurn{calls: []domain.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "bash"}}})
		}
		got := newTestEngine(tr, &scriptedProvider{turns: turns}, host).Run(context.Background(), testParams())
		if got.Kind != ResultToolHostLost {
			t.Fatalf("disposition = %v, want tool host lost (err: %v)", got.Kind, got.Err)
		}
		if host.restarts != maxConsecutiveRestarts {
			t.Errorf("restarts = %d, want %d", host.restarts, maxConsecutiveRestarts)
		}
		results := tr.toolResults()
		if last := results[len(results)-1]; !strings.HasSuffix(last.Content, newSandboxSentence) {
			t.Errorf("the call the engagement ended on = %q, want it told the run goes on in a new sandbox", last.Content)
		}
	})
	t.Run("dying with a call completing between", func(t *testing.T) {
		tr := newMemTranscript(pendingUser("go"))
		host := &restartableHost{}
		var turns []scriptedTurn
		for i := 0; i <= maxConsecutiveRestarts; i++ {
			turns = append(turns,
				scriptedTurn{calls: []domain.ToolCall{{ID: fmt.Sprintf("dies%d", i), Name: "bash"}}, onCall: func() { host.kill("bash") }},
				scriptedTurn{calls: []domain.ToolCall{{ID: fmt.Sprintf("works%d", i), Name: "read"}}},
			)
		}
		turns = append(turns, scriptedTurn{text: "done"})
		got := newTestEngine(tr, &scriptedProvider{turns: turns}, host).Run(context.Background(), testParams())
		if got.Kind != ResultConcluded {
			t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
		}
		if want := maxConsecutiveRestarts + 1; host.restarts != want {
			t.Errorf("restarts = %d, want %d", host.restarts, want)
		}
	})
}

// A stop kills the engagement's jail, so the call it lands in comes back as a
// dead tool host. That is the stop's to report: the engagement is cancelled,
// nothing is restarted, and its calls are left for the next claim's repair
// pass rather than told the run goes on.
func TestRun_AToolHostKilledByAStopIsTheStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := newMemTranscript(pendingUser("go"))
	host := &restartableHost{dies: map[string]bool{"bash": true}, onCall: cancel}
	p := &scriptedProvider{turns: []scriptedTurn{{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}}}}
	got := newTestEngine(tr, p, host).Run(ctx, testParams())
	if got.Kind != ResultCancelled {
		t.Fatalf("disposition = %v, want cancelled (err: %v)", got.Kind, got.Err)
	}
	if host.restarts != 0 {
		t.Errorf("restarts = %d, want none under a stop", host.restarts)
	}
	if results := tr.toolResults(); len(results) != 0 {
		t.Errorf("results written under a stop = %+v, want none", results)
	}
}
