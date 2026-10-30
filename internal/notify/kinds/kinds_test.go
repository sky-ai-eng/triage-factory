package kinds_test

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/notify"
	_ "github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// TestKindsConformance renders every registered kind's Example for every
// recipient its audience can produce and checks what each must have.
func TestKindsConformance(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range notify.Kinds() {
		if seen[k.Name()] {
			t.Errorf("two kinds are named %q", k.Name())
		}
		seen[k.Name()] = true
		examples := notify.Examples(k)
		if len(examples) == 0 {
			t.Errorf("%s: its audience produces no recipient", k.Name())
		}
		for _, ex := range examples {
			if strings.TrimSpace(ex.Content.Title) == "" {
				t.Errorf("%s: an empty title for %+v", k.Name(), ex.Recipient)
			}
			if ex.Priority.Rank() == 0 {
				t.Errorf("%s: priority %q for %+v", k.Name(), ex.Priority, ex.Recipient)
			}
		}
	}
}
