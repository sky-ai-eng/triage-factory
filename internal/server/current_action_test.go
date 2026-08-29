package server

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestCurrentAction covers every row of the composition table plus the three
// ways it declines to answer. Both runtimes' spellings are exercised on every
// tool that has two, because the whole point of the field is that a run reads
// the same whichever engine drove it.
func TestCurrentAction(t *testing.T) {
	call := func(name string, input map[string]any) []domain.ToolCall {
		return []domain.ToolCall{{ID: "t1", Name: name, Input: input}}
	}

	cases := []struct {
		name  string
		calls []domain.ToolCall
		want  string
	}{
		// bash — the authored summary wins over the command, under both
		// spellings, because a shell command is the one argument that is not
		// legible as-is.
		{"bash native description", call("bash", map[string]any{
			"command": "go test ./internal/sandbox -run TestSampler -count=20", "description": "Reproducing the flake",
		}), "Reproducing the flake"},
		{"bash SDK description", call("Bash", map[string]any{
			"command": "go test ./internal/sandbox -run TestSampler -count=20", "description": "Reproducing the flake",
		}), "Reproducing the flake"},
		{"bash native command fallback", call("bash", map[string]any{
			"command": "go build ./...",
		}), "Running go build ./..."},
		{"bash SDK command fallback", call("Bash", map[string]any{
			"command": "go build ./...",
		}), "Running go build ./..."},
		{"bash blank description falls through to the command", call("bash", map[string]any{
			"command": "go vet ./...", "description": "   ",
		}), "Running go vet ./..."},
		{"bash takes only the command's first line", call("bash", map[string]any{
			"command": "cat <<'EOF' > f.txt\nline two\nline three\nEOF",
		}), "Running cat <<'EOF' > f.txt"},
		// The past-tense summary is never read here: this line describes a
		// call in flight, and the row may well carry both.
		{"bash ignores description_past", call("bash", map[string]any{
			"command": "go test ./...", "description": "Vetting the sandbox", "description_past": "Vetted the sandbox",
		}), "Vetting the sandbox"},

		// The self-describing tools, native spelling (path) and SDK spelling
		// (file_path).
		{"read native", call("read", map[string]any{"path": "internal/server/agent.go"}), "Reading internal/server/agent.go"},
		{"read SDK", call("Read", map[string]any{"file_path": "internal/server/agent.go"}), "Reading internal/server/agent.go"},
		{"write native", call("write", map[string]any{"path": "docs/notes.md"}), "Writing docs/notes.md"},
		{"write SDK", call("Write", map[string]any{"file_path": "docs/notes.md"}), "Writing docs/notes.md"},
		{"edit native", call("edit", map[string]any{"path": "main.go"}), "Editing main.go"},
		{"edit SDK", call("Edit", map[string]any{"file_path": "main.go"}), "Editing main.go"},
		{"grep native", call("grep", map[string]any{"pattern": "enrichConversations"}), "Searching enrichConversations"},
		{"grep SDK", call("Grep", map[string]any{"pattern": "enrichConversations"}), "Searching enrichConversations"},
		{"find native", call("find", map[string]any{"pattern": "**/*.sql"}), "Finding **/*.sql"},
		{"find SDK spelling is Glob", call("Glob", map[string]any{"pattern": "**/*.sql"}), "Finding **/*.sql"},
		{"ls", call("ls", map[string]any{"path": "internal/db"}), "Listing internal/db"},
		{"ls with no path is the working directory, which names nothing", call("ls", map[string]any{}), ""},

		// The two tools whose own argument is already the sentence.
		{"stop_blueprint reads its reason", call("stop_blueprint", map[string]any{
			"type": "abort", "reason": "The base branch does not build", "summary": "…",
		}), "The base branch does not build"},
		{"Task reads its description", call("Task", map[string]any{
			"description": "Audit the egress lanes", "prompt": "…",
		}), "Audit the egress lanes"},

		// The last call of a turn is the one still in flight.
		{"last call of a batched turn wins", []domain.ToolCall{
			{ID: "a", Name: "read", Input: map[string]any{"path": "a.go"}},
			{ID: "b", Name: "edit", Input: map[string]any{"path": "b.go"}},
		}, "Editing b.go"},

		// Omit rather than guess.
		{"no tool calls", nil, ""},
		{"empty tool calls", []domain.ToolCall{}, ""},
		{"unknown tool", call("WebFetch", map[string]any{"url": "https://example.com"}), ""},
		{"known tool, missing argument", call("read", map[string]any{}), ""},
		{"known tool, blank argument", call("grep", map[string]any{"pattern": "  "}), ""},
		{"known tool, non-string argument", call("edit", map[string]any{"path": 42}), ""},
		{"bash with neither summary nor command", call("bash", map[string]any{"timeout": 30}), ""},
		{"nil input map", []domain.ToolCall{{ID: "t1", Name: "read"}}, ""},

		// Single line: a multi-line authored summary is flattened rather than
		// truncated at the break, since the break is not where the sentence ends.
		{"multi-line summary flattens", call("bash", map[string]any{
			"description": "Reproducing\n  the flake",
		}), "Reproducing the flake"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentAction(tc.calls, ""); got != tc.want {
				t.Errorf("currentAction = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCurrentAction_Cap pins the length bound: a pathological argument is cut
// to the cap with an ellipsis standing in for what was dropped, and an
// argument that merely reaches the cap is left whole.
func TestCurrentAction_Cap(t *testing.T) {
	long := strings.Repeat("a", 400)
	got := currentAction([]domain.ToolCall{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": long}}}, "")
	if n := len([]rune(got)); n != currentActionMaxLen {
		t.Fatalf("capped length = %d runes, want %d", n, currentActionMaxLen)
	}
	if !strings.HasPrefix(got, "Searching aaa") || !strings.HasSuffix(got, "…") {
		t.Errorf("capped line = %q, want the composed head plus an ellipsis", got)
	}

	// Exactly at the cap: whole, no ellipsis. "Searching " is 10 runes.
	atCap := strings.Repeat("b", currentActionMaxLen-len("Searching "))
	got = currentAction([]domain.ToolCall{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": atCap}}}, "")
	if got != "Searching "+atCap {
		t.Errorf("line at exactly the cap was altered: got %d runes, want %d untouched", len([]rune(got)), currentActionMaxLen)
	}

	// Multi-byte runes are counted as runes, not bytes — a cut mid-rune would
	// put a replacement character on the wire.
	wide := strings.Repeat("あ", 400)
	got = currentAction([]domain.ToolCall{{ID: "t1", Name: "grep", Input: map[string]any{"pattern": wide}}}, "")
	if n := len([]rune(got)); n != currentActionMaxLen {
		t.Errorf("capped multi-byte length = %d runes, want %d", n, currentActionMaxLen)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("capped multi-byte line carries a replacement character: %q", got)
	}
}

// TestCurrentAction_WorktreeStrip pins the path collapse: the agent's absolute
// worktree paths read worktree-relative on the composed line, under the same
// semantics the client's transcript helper applies (frontend/src/lib/
// worktree.ts) — /private symlink variants both directions, paths embedded in
// bash commands, and the bare root as ".".
func TestCurrentAction_WorktreeStrip(t *testing.T) {
	const wt = "/var/folders/kx/abc/T/triagefactory-runs/r1"
	call := func(name string, input map[string]any) []domain.ToolCall {
		return []domain.ToolCall{{ID: "t1", Name: name, Input: input}}
	}

	cases := []struct {
		name     string
		calls    []domain.ToolCall
		worktree string
		want     string
	}{
		{"read under the root", call("Read", map[string]any{
			"file_path": wt + "/internal/server/agent.go",
		}), wt, "Reading internal/server/agent.go"},
		{"agent path carries /private the stored root omits", call("Edit", map[string]any{
			"file_path": "/private" + wt + "/main.go",
		}), wt, "Editing main.go"},
		{"stored root carries /private the agent path omits", call("Edit", map[string]any{
			"file_path": wt + "/main.go",
		}), "/private" + wt, "Editing main.go"},
		{"path embedded in a bash command", call("bash", map[string]any{
			"command": "go test " + wt + "/internal/routing/...",
		}), wt, "Running go test internal/routing/..."},
		{"bare root is the working directory", call("bash", map[string]any{
			"command": "cd " + wt,
		}), wt, "Running cd ."},
		{"trailing slash on the stored root", call("Read", map[string]any{
			"file_path": wt + "/go.mod",
		}), wt + "/", "Reading go.mod"},
		{"no worktree leaves the line alone", call("Read", map[string]any{
			"file_path": wt + "/go.mod",
		}), "", "Reading " + wt + "/go.mod"},
		{"a path outside the root is not touched", call("Read", map[string]any{
			"file_path": "/etc/hosts",
		}), wt, "Reading /etc/hosts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentAction(tc.calls, tc.worktree); got != tc.want {
				t.Errorf("currentAction = %q, want %q", got, tc.want)
			}
		})
	}

	// The strip runs BEFORE the cap — the reason it lives on this side of the
	// wire at all. A line whose only excess is the worktree prefix arrives
	// whole; stripped after capping it would have lost its tail to an
	// ellipsis spent on the prefix.
	deep := wt + "/" + strings.Repeat("d/", 40) + "leaf.go"
	got := currentAction(call("Read", map[string]any{"file_path": deep}), wt)
	want := "Reading " + strings.Repeat("d/", 40) + "leaf.go"
	if got != want {
		t.Errorf("strip-then-cap: got %q, want %q", got, want)
	}
	if strings.ContainsRune(got, '…') {
		t.Errorf("prefix spent the cap: %q", got)
	}
}
