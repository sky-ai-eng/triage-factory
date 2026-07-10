package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedAgentToolsCache pre-fires the spawner's scan Once and plants a
// known cache value, so the tests below observe the mode gate rather
// than whatever ~/.claude/agents happens to exist on the test host.
func seedAgentToolsCache(s *Spawner, tools string) {
	s.agentToolsOnce.Do(func() { s.agentToolsCache = tools })
}

func TestCollectExtraTools_LocalModeMergesAgentTools(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := &Spawner{}
	seedAgentToolsCache(s, "Bash(gh:*)")

	got := s.collectExtraTools("WebFetch")
	if got != "WebFetch,Bash(gh:*)" {
		t.Fatalf("collectExtraTools = %q, want %q", got, "WebFetch,Bash(gh:*)")
	}
}

func TestCollectExtraTools_MultiModeIgnoresAgentTools(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := &Spawner{}
	// Even with a populated cache (as if the scan had already run), multi
	// mode must not merge orchestrator-host agent tools into any tenant's
	// --allowedTools.
	seedAgentToolsCache(s, "Bash(gh:*)")

	if got := s.collectExtraTools("WebFetch"); got != "WebFetch" {
		t.Fatalf("collectExtraTools = %q, want prompt tools only (%q)", got, "WebFetch")
	}
	if got := s.collectExtraTools(""); got != "" {
		t.Fatalf("collectExtraTools with empty prompt tools = %q, want empty", got)
	}
	if got := s.cachedAgentTools(); got != "" {
		t.Fatalf("cachedAgentTools = %q in multi mode, want empty", got)
	}
}
