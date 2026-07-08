package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunURLFor_EmptyPublicURL pins the "no wrong fallbacks" rule (TFAC-591):
// an unconfigured public URL must render the {{RUN_URL}} placeholder empty,
// never a fabricated localhost link.
func TestRunURLFor_EmptyPublicURL(t *testing.T) {
	s := &Spawner{}
	if got := s.runURLFor("org-1", "run-1"); got != "" {
		t.Errorf("runURLFor with no public URL = %q, want empty", got)
	}
}

// TestRunURLFor_LocalMode pins the local-mode route shape
// (frontend/src/main.tsx "/runs/:runID" — no org segment).
func TestRunURLFor_LocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := &Spawner{}
	s.SetPublicURL("http://localhost:3000")

	want := "http://localhost:3000/runs/run-1"
	if got := s.runURLFor("org-1", "run-1"); got != want {
		t.Errorf("runURLFor(local) = %q, want %q", got, want)
	}
}

// TestRunURLFor_MultiMode pins the multi-mode route shape
// (frontend/src/main.tsx "/orgs/:org_id/runs/:runID").
func TestRunURLFor_MultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := &Spawner{}
	s.SetPublicURL("https://tf.example.com")

	want := "https://tf.example.com/orgs/org-1/runs/run-1"
	if got := s.runURLFor("org-1", "run-1"); got != want {
		t.Errorf("runURLFor(multi) = %q, want %q", got, want)
	}
}
