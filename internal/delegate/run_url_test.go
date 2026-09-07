package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunURLFor_EmptyPublicURL pins the "no wrong fallbacks" rule: an
// unconfigured public URL must render the {{RUN_URL}} placeholder empty,
// never a fabricated localhost link.
func TestRunURLFor_EmptyPublicURL(t *testing.T) {
	s := &Spawner{}
	if got := s.runURLFor("org-1", "run-1"); got != "" {
		t.Errorf("runURLFor with no public URL = %q, want empty", got)
	}
}

// TestSetPublicURL_TrimsTrailingSlash pins parity with Server.SetDeployConfig
// (internal/server/auth_handlers.go), which also trims trailing slashes: a
// TF_PUBLIC_URL configured with a trailing "/" must not produce a
// double-slash in the concatenated {{RUN_URL}} ("...com//runs/...").
func TestSetPublicURL_TrimsTrailingSlash(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := &Spawner{}
	s.SetPublicURL("http://localhost:3000/")

	want := "http://localhost:3000/runs/run-1"
	if got := s.runURLFor("org-1", "run-1"); got != want {
		t.Errorf("runURLFor after trailing-slash publicURL = %q, want %q", got, want)
	}
}
