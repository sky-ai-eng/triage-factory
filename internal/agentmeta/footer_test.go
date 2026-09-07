package agentmeta

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBuild_KindNounRendersInDisclaimer pins the noun parameterization so a
// future "issue" or "comment" surface gets its own phrasing.
func TestBuild_KindNounRendersInDisclaimer(t *testing.T) {
	for _, kind := range []string{"review", "PR", "issue"} {
		got := Build(kind, "")
		want := "This " + kind + " was partially generated"
		if !strings.Contains(got, want) {
			t.Errorf("kind=%q: missing %q in %q", kind, want, got)
		}
	}
}

// TestBuild_OpensWithRule pins the concatenation contract: a caller appends the
// footer to a body verbatim, so the footer must supply its own separation.
func TestBuild_OpensWithRule(t *testing.T) {
	if got := Build("PR", ""); !strings.HasPrefix(got, "\n\n---\n") {
		t.Errorf("Build = %q, want a leading blank line and rule", got)
	}
}

// TestBuild_LinksTheRunWhenKnown pins that a run URL renders as a markdown link
// under the disclosure and that nothing else rides along: no spend, no elapsed
// time, no model name — those live on the page the link points at.
func TestBuild_LinksTheRunWhenKnown(t *testing.T) {
	got := Build("PR", "https://tf.example.com/runs/run-1")
	if !strings.Contains(got, "[View the run](https://tf.example.com/runs/run-1)") {
		t.Errorf("footer lacks the run link: %q", got)
	}
	for _, banned := range []string{"Time:", "Cost:", "Model:", "$"} {
		if strings.Contains(got, banned) {
			t.Errorf("footer carries %q, which belongs on the run page: %q", banned, got)
		}
	}
}

// TestBuild_NoLinkWithoutURL pins the "no wrong fallbacks" rule: an
// unconfigured public URL yields a footer with no link at all, never a
// fabricated one.
func TestBuild_NoLinkWithoutURL(t *testing.T) {
	got := Build("review", "")
	if strings.Contains(got, "View the run") || strings.Count(got, "](") != 1 {
		t.Errorf("footer without a run URL must carry only the Triage Factory link: %q", got)
	}
}

// TestBuild_Deterministic pins that two calls with the same inputs yield the
// same bytes — a retried create or a second posting surface cannot produce a
// footer that differs from the first.
func TestBuild_Deterministic(t *testing.T) {
	a := Build("PR", "https://tf.example.com/runs/run-1")
	b := Build("PR", "https://tf.example.com/runs/run-1")
	if a != b {
		t.Errorf("Build is not deterministic:\n%q\n%q", a, b)
	}
}

// TestRunURL_EmptyPublicURL pins that an unconfigured public URL renders no
// link, never a fabricated localhost one.
func TestRunURL_EmptyPublicURL(t *testing.T) {
	if got := RunURL("", "org-1", "run-1"); got != "" {
		t.Errorf("RunURL with no public URL = %q, want empty", got)
	}
}

// TestRunURL_LocalMode pins the local-mode route shape
// (frontend/src/main.tsx "/runs/:conversationID" — no org segment).
func TestRunURL_LocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	want := "http://localhost:3000/runs/run-1"
	if got := RunURL("http://localhost:3000", "org-1", "run-1"); got != want {
		t.Errorf("RunURL(local) = %q, want %q", got, want)
	}
}

// TestRunURL_MultiMode pins the multi-mode route shape
// (frontend/src/main.tsx "/orgs/:org_id/runs/:conversationID").
func TestRunURL_MultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	want := "https://tf.example.com/orgs/org-1/runs/run-1"
	if got := RunURL("https://tf.example.com", "org-1", "run-1"); got != want {
		t.Errorf("RunURL(multi) = %q, want %q", got, want)
	}
}
