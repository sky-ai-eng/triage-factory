package exec

import (
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// stubLocalStores replaces the local-state opener for the duration of a test
// and reports whether it was reached. The returned stores are empty: every
// invocation under test takes a help route, which never touches them.
func stubLocalStores(t *testing.T) *bool {
	t.Helper()
	var opened bool
	saved := openLocalStores
	openLocalStores = func() (db.Stores, func() error, error) {
		opened = true
		return db.Stores{}, func() error { return nil }, nil
	}
	t.Cleanup(func() { openLocalStores = saved })
	return &opened
}

// TestHandleSandboxed_NeverOpensLocalState is the structural guard on the
// jailed path. The signature is the real one — HandleSandboxed is handed a
// client and never the means to build stores — and this pins that the body
// stays that way: if a verb, a help route, or anything added later reaches for
// local state, the stub fails the test instead of silently minting a SQLite
// file inside the agent's worktree.
//
// nil is a legitimate client here: help routes return before the dispatcher
// asks for one.
func TestHandleSandboxed_NeverOpensLocalState(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--help"},
		{"gh", "--help"},
		{"gh", "pr", "--help"},
		{"jira", "--help"},
		{"workspace", "--help"},
		{"memory", "--help"},
	} {
		name := "bare"
		if len(args) > 0 {
			name = args[0] + "/" + args[len(args)-1]
		}
		t.Run(name, func(t *testing.T) {
			opened := stubLocalStores(t)
			withStdoutDiscarded(t, func() { HandleSandboxed(nil, args) })
			if *opened {
				t.Error("the jailed path opened local state; a jailed process has none to open")
			}
		})
	}
}

// TestHandle_OpensLocalStateBeforeAnyVerb is the other side of the same coin:
// the host CLI keeps its eager open, so migrations still run before the first
// store read on every invocation. Only the bare help route short-circuits ahead
// of it, exactly as it always has.
func TestHandle_OpensLocalStateBeforeAnyVerb(t *testing.T) {
	t.Run("verb opens", func(t *testing.T) {
		opened := stubLocalStores(t)
		withStdoutDiscarded(t, func() { Handle([]string{"gh", "--help"}) })
		if !*opened {
			t.Error("the host path skipped the local-state open; its client reads that DB")
		}
	})

	t.Run("bare help short-circuits", func(t *testing.T) {
		opened := stubLocalStores(t)
		withStdoutDiscarded(t, func() { Handle([]string{"--help"}) })
		if *opened {
			t.Error("bare help opened local state; it never has")
		}
	})
}

// TestOpenLocalStores_ErrorIsReaderFacing pins the stderr wording the host path
// prints when the open fails. The message moved from the call site into the
// opener, and it is what an operator — or an agent reading a tool result — has
// to diagnose from, so it stays byte-for-byte what it always was.
func TestOpenLocalStores_ErrorIsReaderFacing(t *testing.T) {
	t.Setenv("HOME", "") // an unresolvable state root is the one failure a test can force
	_, _, err := openLocalStores()
	if err == nil {
		t.Skip("this platform resolved a state root without HOME; nothing to assert")
	}
	if !strings.HasPrefix(err.Error(), "error opening database: ") {
		t.Errorf("error = %q; want the \"error opening database: \" prefix", err)
	}
}

// withStdoutDiscarded runs fn with stdout pointed at /dev/null, so the help
// dumps under test don't drown the test output.
func withStdoutDiscarded(t *testing.T, fn func()) {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	saved := os.Stdout
	os.Stdout = devNull
	defer func() {
		os.Stdout = saved
		_ = devNull.Close()
	}()
	fn()
}
