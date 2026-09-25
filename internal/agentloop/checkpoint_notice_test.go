package agentloop

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestAfterToolBatch_ReportsTheLastResultAndSkipsATerminatingBatch: the hook
// fires once per batch that leaves the run going, after every result is
// persisted, with the assembly key of the batch's last result — and not for a
// batch that ends the run, where there is no next call to overlap with.
func TestAfterToolBatch_ReportsTheLastResultAndSkipsATerminatingBatch(t *testing.T) {
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{
			{ID: "c1", Name: "bash", Input: map[string]any{"command": "make"}},
			{ID: "c2", Name: "bash", Input: map[string]any{"command": "make test"}},
		}},
		{calls: []domain.ToolCall{{ID: "c3", Name: ToolStopBlueprint, Input: map[string]any{"type": "abort", "reason": "r", "summary": "s"}}}},
	}}
	e := newTestEngine(tr, p, newScriptedToolHost())
	type seen struct {
		position float64
		results  int
	}
	var calls []seen
	e.Hooks.AfterToolBatch = func(_ context.Context, position float64) {
		calls = append(calls, seen{position: position, results: len(tr.toolResults())})
	}
	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}

	if len(calls) != 1 {
		t.Fatalf("AfterToolBatch fired %d times, want once: for the working batch and not the terminating one", len(calls))
	}
	if calls[0].results != 2 {
		t.Errorf("the hook saw %d results persisted, want both of the batch's", calls[0].results)
	}
	c2 := tr.find(func(m domain.Message) bool { return m.Role == "tool" && m.ToolCallID == "c2" })
	if c2 == nil {
		t.Fatal("no result for c2")
	}
	if want := assemblyKey(*c2); calls[0].position != want {
		t.Errorf("position = %v, want %v — the assembly key of the batch's last result", calls[0].position, want)
	}
}

// TestCheckpointRestoredNotice pins what a restore from a mid-engagement
// checkpoint tells the agent: part of the interrupted engagement's work is
// present, and exactly the tool calls after the checkpoint are not.
func TestCheckpointRestoredNotice(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "go"},
		{ID: 2, Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "a", Name: "bash", Input: map[string]any{"command": "git status"}}}},
		{ID: 3, Role: "tool", ToolCallID: "a"},
		// The checkpoint covers everything up to row 3.
		{ID: 4, Role: "assistant", ToolCalls: []domain.ToolCall{
			{ID: "b", Name: "bash", Input: map[string]any{"command": "make   test\n  -j4"}},
			{ID: "c", Name: "write", Input: map[string]any{"path": "src/main.go"}},
		}},
		{ID: 5, Role: "tool", ToolCallID: "b"},
		{ID: 6, Role: "tool", ToolCallID: "c"},
		{ID: 7, Role: "assistant", ToolCalls: []domain.ToolCall{
			{ID: "d", Name: ToolStopBlueprint, Input: map[string]any{"type": "abort"}},
			{ID: "e", Name: "edit", Input: map[string]any{"path": "README.md"}},
		}},
	}

	t.Run("names the first call after the checkpoint and counts the rest", func(t *testing.T) {
		n := checkpointRestoredNotice(rows, 3, false, true)
		if !strings.Contains(n, "restored from a checkpoint taken during the interrupted engagement") {
			t.Errorf("the notice must say the restore is a checkpoint of the interrupted engagement: %q", n)
		}
		// b, c and e: the flow-control call touches no workspace and is not one.
		if !strings.Contains(n, "`bash: make test -j4` and the 2 tool calls after it came after the checkpoint") {
			t.Errorf("the notice must name the first call after the checkpoint and count the rest: %q", n)
		}
		if strings.Contains(n, "git status") {
			t.Errorf("a call before the checkpoint is present in the tree and must not be named: %q", n)
		}
		if strings.Contains(n, "during the engagement that was interrupted — are not present") {
			t.Errorf("the generic body's claim is false after a checkpoint: %q", n)
		}
	})

	t.Run("one call after", func(t *testing.T) {
		n := checkpointRestoredNotice(rows[:5], 3, false, true)
		if !strings.Contains(n, "and the tool call after it came after the checkpoint") {
			t.Errorf("two calls after the checkpoint read as the first and the one after it: %q", n)
		}
		single := []domain.Message{rows[0], rows[1], rows[2], {ID: 4, Role: "assistant", ToolCalls: rows[3].ToolCalls[:1]}}
		n = checkpointRestoredNotice(single, 3, false, true)
		if !strings.Contains(n, "`bash: make test -j4` came after the checkpoint; whatever it changed") {
			t.Errorf("a single call is named alone: %q", n)
		}
	})

	t.Run("nothing after the checkpoint", func(t *testing.T) {
		n := checkpointRestoredNotice(rows[:3], 3, false, true)
		if !strings.Contains(n, "reflects every tool call above") {
			t.Errorf("a checkpoint after the last call leaves nothing missing, and the notice says so: %q", n)
		}
	})

	t.Run("a summary written since may cover work the tree lacks", func(t *testing.T) {
		summarized := append(append([]domain.Message(nil), rows[:3]...),
			domain.Message{ID: 9, Role: "user", Subtype: domain.MessageSubtypeInjectionCompactionResult, Content: "summary"})
		n := checkpointRestoredNotice(summarized, 3, false, true)
		if !strings.Contains(n, "summary above was written after this checkpoint") {
			t.Errorf("calls a compaction took out of the window cannot be named, so the notice must say so: %q", n)
		}
		if n := checkpointRestoredNotice(rows, 3, false, true); strings.Contains(n, "summary above") {
			t.Errorf("no compaction since the checkpoint, no summary sentence: %q", n)
		}
	})

	t.Run("the executor sentence still leads", func(t *testing.T) {
		n := checkpointRestoredNotice(rows, 3, true, true)
		if !strings.HasPrefix(n, "<system-note>\n"+executorChangedSentence) {
			t.Errorf("a move is its own fact, stated first: %q", n)
		}
	})
}

func TestDescribeToolCall(t *testing.T) {
	long := strings.Repeat("x", 200)
	cases := []struct {
		call domain.ToolCall
		want string
	}{
		{domain.ToolCall{Name: "bash", Input: map[string]any{"command": "echo `date`\n  && ls"}}, "bash: echo 'date' && ls"},
		{domain.ToolCall{Name: "read", Input: map[string]any{"path": "a/b.go"}}, "read: a/b.go"},
		{domain.ToolCall{Name: "grep", Input: map[string]any{"pattern": "TODO", "path": "src"}}, "grep: TODO"},
		{domain.ToolCall{Name: "ls"}, "ls"},
		{domain.ToolCall{Name: "bash", Input: map[string]any{"command": "   "}}, "bash"},
		{domain.ToolCall{Name: "bash", Input: map[string]any{"command": long}}, "bash: " + strings.Repeat("x", describedCallMaxRunes-1) + "…"},
	}
	for _, tc := range cases {
		if got := describeToolCall(tc.call); got != tc.want {
			t.Errorf("describeToolCall(%v) = %q, want %q", tc.call.Input, got, tc.want)
		}
	}
}

// TestInterruptedToolResult_AfterACheckpoint: a call left dangling after the
// checkpoint the tree was restored from had no effect on that tree, which is
// the one restore where the workspace half of the answer is known.
func TestInterruptedToolResult_AfterACheckpoint(t *testing.T) {
	got := interruptedToolResult(domain.WorkspaceProvenanceRehydrated, false, true)
	if !strings.Contains(got, "restored to a checkpoint taken before this call ran") || !strings.Contains(got, "none of its effects on the workspace are present") {
		t.Errorf("a call after the checkpoint must be said to have left no trace in the tree: %q", got)
	}
	if !strings.Contains(got, "outside the workspace") {
		t.Errorf("what it did outside the tree is still unknown, and must still be verified: %q", got)
	}
	if got := interruptedToolResult(domain.WorkspaceProvenanceWarm, false, true); strings.Contains(got, "checkpoint") {
		t.Errorf("a warm tree was not restored from anything: %q", got)
	}
	if got := interruptedToolResult(domain.WorkspaceProvenanceRehydrated, false, false); !strings.Contains(got, "restored to its last snapshot point") {
		t.Errorf("without a checkpoint position the restore reads as before: %q", got)
	}
}

// TestRun_RestoreFromACheckpointNamesTheCallsAfterIt drives the repair pass: a
// successor on a tree restored from a checkpoint at the first batch is told
// exactly the calls after it, and the call left dangling is answered as having
// left the restored tree untouched.
func TestRun_RestoreFromACheckpointNamesTheCallsAfterIt(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "git checkout -b fix"}}}},
		domain.Message{ConversationID: "conv", Role: "tool", ToolCallID: "c1", Content: "ok"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{
			{ID: "c2", Name: "edit", Input: map[string]any{"path": "main.go"}},
			{ID: "c3", Name: "bash", Input: map[string]any{"command": "go test ./..."}},
		}},
		domain.Message{ConversationID: "conv", Role: "tool", ToolCallID: "c2", Content: "ok"},
	)
	asOf := 3.0 // c1's result row
	params := workspaceParams(domain.WorkspaceProvenanceRehydrated, false)
	params.WorkspaceAsOf = &asOf

	p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}
	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), params); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}

	notice := tr.find(func(m domain.Message) bool { return m.Subtype == domain.MessageSubtypeInjectionExecutorChanged })
	if notice == nil {
		t.Fatal("a tree restored from a checkpoint with work since must be explained")
	}
	if !strings.Contains(notice.Content, "`edit: main.go` and the tool call after it came after the checkpoint") {
		t.Errorf("the notice must name exactly the calls after the checkpoint: %q", notice.Content)
	}
	c3 := tr.find(func(m domain.Message) bool { return m.Role == "tool" && m.ToolCallID == "c3" })
	if c3 == nil || !strings.Contains(c3.Content, "checkpoint taken before this call ran") {
		t.Fatalf("the dangling call must be answered as absent from the restored tree: %+v", c3)
	}
	// And the model reads the notice on its first call.
	if len(p.requests) == 0 {
		t.Fatal("no request was made")
	}
	var sent bool
	for _, r := range p.requests[0].Rows {
		if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged {
			sent = true
		}
	}
	if !sent {
		t.Error("the first request must carry the restore notice")
	}
}
