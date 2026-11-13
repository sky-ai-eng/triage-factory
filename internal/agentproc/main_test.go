package agentproc

import (
	"fmt"
	"os"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// cleanups lets paths.SetForTest, which wants a *testing.T, run from TestMain.
type cleanups struct{ fns []func() }

func (c *cleanups) Helper()          {}
func (c *cleanups) Cleanup(f func()) { c.fns = append(c.fns, f) }

// TestMain points the state root at a scratch directory for the whole package.
// Spawning a command writes under the state root on Windows (the args file and
// the agent-visible copy of the running binary), and a test must never put its
// own test binary where the user's real install keeps TF's.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentproc-state-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentproc tests: create state root:", err)
		os.Exit(1)
	}
	c := &cleanups{}
	paths.SetForTest(c, dir)
	code := m.Run()
	for _, f := range c.fns {
		f()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
