package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// TestNativeWireModel_PinsArePriced pins every alias to a concrete id that
// the pricing datasheet can cost. A datasheet refresh that retires a pinned
// id must fail here, loudly — the alternative is a run whose every assistant
// row persists with a NULL cost stamp.
func TestNativeWireModel_PinsArePriced(t *testing.T) {
	usage := inference.Usage{InputTokens: 1000, OutputTokens: 1000}
	for _, alias := range []string{"haiku", "sonnet", "opus"} {
		resolved := nativeWireModel(alias)
		if resolved == alias {
			t.Errorf("alias %q did not resolve to a concrete id", alias)
			continue
		}
		cost, ok := inference.CostForUsage(resolved, usage)
		if !ok {
			t.Errorf("pinned id %q (for %q) has no pricing row", resolved, alias)
		}
		if ok && cost <= 0 {
			t.Errorf("pinned id %q priced nonpositive: %v", resolved, cost)
		}
	}
}

// TestNativeWireModel_PassthroughForConcreteIDs is the removability property:
// when stored configuration carries concrete ids, this function becomes a
// no-op, and deleting it changes nothing.
func TestNativeWireModel_PassthroughForConcreteIDs(t *testing.T) {
	for _, id := range []string{
		"claude-sonnet-5",
		"claude-opus-4-8",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"",
	} {
		if got := nativeWireModel(id); got != id {
			t.Errorf("nativeWireModel(%q) = %q, want passthrough", id, got)
		}
	}
}
