package agentloop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// budgetParams is testParams with a per-command memory budget configured.
func budgetParams(mb int) Params {
	p := testParams()
	p.BashMemBudgetMB = mb
	return p
}

// oneToolCallThenDone is the shortest script that reaches the jail: a turn
// that calls a tool, then a turn that concludes.
func oneToolCallThenDone() *scriptedProvider {
	return &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "make"}}}},
		{text: "done"},
	}}
}

// TestRun_ConfiguresTheToolHostBeforeAnyToolCall pins the frame order the
// budget depends on. A configure that arrived after the first `bash` would
// leave exactly the command most likely to need bounding unbounded.
func TestRun_ConfiguresTheToolHostBeforeAnyToolCall(t *testing.T) {
	tr := newMemTranscript(pendingUser("build it"))
	host := newScriptedToolHost()
	e := newTestEngine(tr, oneToolCallThenDone(), host)

	if got := e.Run(context.Background(), budgetParams(2048)); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	calls := host.calls()
	want := []string{toolHostConfigureTool, "bash"}
	if len(calls) != len(want) {
		t.Fatalf("host saw %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("host saw %v, want %v", calls, want)
		}
	}
	args, ok := host.argsFor(toolHostConfigureTool)
	if !ok {
		t.Fatal("the configure frame carried no args")
	}
	if got := args[bashMemBudgetArg]; got != 2048 {
		t.Errorf("%s = %v, want 2048", bashMemBudgetArg, got)
	}
}

// TestRun_ConfiguresOncePerEngagement — the host holds the budget for the life
// of the connection, so a second frame would be noise on every turn.
func TestRun_ConfiguresOncePerEngagement(t *testing.T) {
	tr := newMemTranscript(pendingUser("build it"))
	host := newScriptedToolHost()
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "ls"}}},
		{calls: []domain.ToolCall{{ID: "c2", Name: "bash", Input: map[string]any{"command": "make"}}}},
		{text: "done"},
	}}
	e := newTestEngine(tr, p, host)

	if got := e.Run(context.Background(), budgetParams(1024)); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	configures := 0
	for _, name := range host.calls() {
		if name == toolHostConfigureTool {
			configures++
		}
	}
	if configures != 1 {
		t.Fatalf("sent %d configure frames across %v, want exactly 1", configures, host.calls())
	}
}

// TestRun_NoBudgetSendsNoConfigureFrame is the negative-space AC: at zero the
// feature is not merely inert, it is absent from the wire.
func TestRun_NoBudgetSendsNoConfigureFrame(t *testing.T) {
	tr := newMemTranscript(pendingUser("build it"))
	host := newScriptedToolHost()
	e := newTestEngine(tr, oneToolCallThenDone(), host)

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	for _, name := range host.calls() {
		if name == toolHostConfigureTool {
			t.Fatalf("an unbudgeted engagement sent a configure frame: %v", host.calls())
		}
	}
}

// TestRun_ConfiguresTheBashTimeLimits: the limits ride the same frame as the
// budget, each in whole seconds, and a limit at zero is left out of it, so a
// session with only time limits still gets a frame and one without any policy
// still gets none.
func TestRun_ConfiguresTheBashTimeLimits(t *testing.T) {
	tests := []struct {
		name   string
		params func(*Params)
		want   map[string]any
	}{
		{
			name: "every policy",
			params: func(p *Params) {
				p.BashMemBudgetMB = 2048
				p.BashDefaultTimeout = 10 * time.Minute
				p.BashMaxTimeout = 29 * time.Minute
			},
			want: map[string]any{bashMemBudgetArg: 2048, bashDefaultTimeoutArg: 600, bashMaxTimeoutArg: 1740},
		},
		{
			name: "time limits alone",
			params: func(p *Params) {
				p.BashDefaultTimeout = 1500 * time.Millisecond
				p.BashMaxTimeout = 2 * time.Second
			},
			want: map[string]any{bashDefaultTimeoutArg: 1, bashMaxTimeoutArg: 2},
		},
		{
			name: "a limit under a second is no limit",
			params: func(p *Params) {
				p.BashMemBudgetMB = 512
				p.BashMaxTimeout = 500 * time.Millisecond
			},
			want: map[string]any{bashMemBudgetArg: 512},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("build it"))
			host := newScriptedToolHost()
			params := testParams()
			tc.params(&params)
			if got := newTestEngine(tr, oneToolCallThenDone(), host).Run(context.Background(), params); got.Kind != ResultConcluded {
				t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
			}
			args, ok := host.argsFor(toolHostConfigureTool)
			if !ok {
				t.Fatalf("no configure frame among %v", host.calls())
			}
			if len(args) != len(tc.want) {
				t.Fatalf("frame args = %v, want %v", args, tc.want)
			}
			for k, v := range tc.want {
				if args[k] != v {
					t.Errorf("%s = %v, want %v", k, args[k], v)
				}
			}
		})
	}
}

// TestRun_ConfigureFailureIsToleratedWithOneLogLine — every way the frame can
// fail means the same thing (this run has no per-command budget), and none of
// them may stop the engagement. Version skew between the loop and the harness
// is the case that matters: an older host answers the non-fatal unknown_tool.
func TestRun_ConfigureFailureIsToleratedWithOneLogLine(t *testing.T) {
	tests := []struct {
		name string
		arm  func(*scriptedToolHost)
	}{
		{
			name: "an older harness answers unknown_tool",
			arm: func(h *scriptedToolHost) {
				h.answers[toolHostConfigureTool] = ToolOutcome{
					Protocol: &ProtocolError{Kind: protoUnknownTool, Message: "unknown tool: _configure"},
				}
			},
		},
		{
			name: "the host rejects the args",
			arm: func(h *scriptedToolHost) {
				h.answers[toolHostConfigureTool] = ToolOutcome{ToolError: "bad budget"}
			},
		},
		{
			name: "the frame never lands",
			arm: func(h *scriptedToolHost) {
				h.errs[toolHostConfigureTool] = errors.New("write tool request: broken pipe")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("build it"))
			host := newScriptedToolHost()
			tc.arm(host)
			log := &recordingLogger{}
			e := newTestEngine(tr, oneToolCallThenDone(), host)
			e.Log = log

			got := e.Run(context.Background(), budgetParams(2048))
			if got.Kind != ResultConcluded {
				t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
			}
			// The tool call after the failed frame still ran: the failure cost
			// the budget, not the engagement.
			if _, ok := host.argsFor("bash"); !ok {
				t.Fatalf("the engagement never reached the jail: %v", host.calls())
			}
			// Exactly one line, and the line about the budget: a count alone
			// would pass on some unrelated warn while the failure this test is
			// about went unmentioned.
			if len(log.warns) != 1 {
				t.Fatalf("logged %v, want exactly one warn", log.warns)
			}
			if !strings.Contains(log.warns[0], "without a per-command memory budget or time limits") {
				t.Errorf("warn = %q, want the one naming the lost budget", log.warns[0])
			}
		})
	}
}
