package domain

import (
	"strings"
	"testing"
)

// TestStagedInjectionBlock_BundlesInOrder: every non-empty body becomes a bullet, in
// the slice order the caller passes (oldest→newest), wrapped once.
func TestStagedInjectionBlock_BundlesInOrder(t *testing.T) {
	block := StagedInjectionBlock([]StagedInjection{
		{Body: "first thing happened"},
		{Body: "second thing happened"},
	})
	if !strings.HasPrefix(block, "<system-note>") || !strings.HasSuffix(block, "</system-note>") {
		t.Fatalf("block not wrapped in a single <system-note>: %q", block)
	}
	if strings.Count(block, "<system-note>") != 1 {
		t.Errorf("want exactly one <system-note> wrapper, got %d: %q", strings.Count(block, "<system-note>"), block)
	}
	first := strings.Index(block, "first thing happened")
	second := strings.Index(block, "second thing happened")
	if first < 0 || second < 0 || first > second {
		t.Errorf("bodies missing or out of order: first=%d second=%d in %q", first, second, block)
	}
	if !strings.Contains(block, "- first thing happened") {
		t.Errorf("body not rendered as a bullet: %q", block)
	}
}

// TestStagedInjectionBlock_EmptyAndBlankBodies: no injections (or only blank bodies) → "",
// so a resume with nothing pending prepends nothing.
func TestStagedInjectionBlock_EmptyAndBlankBodies(t *testing.T) {
	if got := StagedInjectionBlock(nil); got != "" {
		t.Errorf("nil injections: want empty, got %q", got)
	}
	if got := StagedInjectionBlock([]StagedInjection{{Body: ""}, {Body: ""}}); got != "" {
		t.Errorf("all-blank bodies: want empty, got %q", got)
	}
}

// TestPRNewCommitsInjection_NamesPRAndShortSHAs: the copy names the PR and both
// short SHAs and tells the agent to re-pull and reconcile.
func TestPRNewCommitsInjection_NamesPRAndShortSHAs(t *testing.T) {
	injection := PRNewCommitsInjection("owner/repo", 42,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	for _, want := range []string{"#42", "owner/repo", "aaaaaaaaaaaa", "bbbbbbbbbbbb", "Re-pull", "reconcile"} {
		if !strings.Contains(injection, want) {
			t.Errorf("injection missing %q: %q", want, injection)
		}
	}
	// Full 40-char SHAs must NOT leak — they're shortened to 12.
	if strings.Contains(injection, "aaaaaaaaaaaaa") { // 13 a's
		t.Errorf("SHA not shortened to 12 chars: %q", injection)
	}
}

func TestShortSHA(t *testing.T) {
	if got := ShortSHA("abcdef0123456789"); got != "abcdef012345" {
		t.Errorf("ShortSHA truncate: got %q want abcdef012345", got)
	}
	if got := ShortSHA("abc"); got != "abc" {
		t.Errorf("ShortSHA short value: got %q want abc", got)
	}
	if got := ShortSHA(""); got != "" {
		t.Errorf("ShortSHA empty: got %q want empty", got)
	}
}
