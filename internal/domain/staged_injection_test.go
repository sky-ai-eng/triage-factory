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

// TestStagedInjectionBlock_ABodyOfSeveralLinesIsOneBullet: a body that lists
// entries on lines of its own stays one bullet, so the block has as many
// top-level bullets as injections.
func TestStagedInjectionBlock_ABodyOfSeveralLinesIsOneBullet(t *testing.T) {
	block := StagedInjectionBlock([]StagedInjection{
		{Body: "a listing\n- entry one\n- entry two"},
		{Body: "another update"},
	})
	bullets := 0
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "- ") {
			bullets++
		}
	}
	if bullets != 2 {
		t.Errorf("block has %d top-level bullets, want one per injection (2): %q", bullets, block)
	}
}
