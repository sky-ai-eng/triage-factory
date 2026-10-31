package kinds_test

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// TestKindsConformance renders every registered kind's Example for every
// recipient its audience can produce and checks what each must have. Unique
// names need no check here: a duplicate panics at init, before any test runs.
// The action checks are the write's own (Example.Err): at most two actions, a
// notice carries one, and every Link route has a page in the recipient's
// mode.
func TestKindsConformance(t *testing.T) {
	for _, k := range notify.Kinds() {
		for _, ex := range notify.Examples(k) {
			if strings.TrimSpace(ex.Content.Title) == "" {
				t.Errorf("%s: an empty title for %+v", k.Name(), ex.Recipient)
			}
			if ex.Priority.Rank() == 0 {
				t.Errorf("%s: priority %q for %+v", k.Name(), ex.Priority, ex.Recipient)
			}
			if ex.Err != nil {
				t.Errorf("%s: for %+v (local %v), a write would be refused: %v", k.Name(), ex.Recipient, ex.Local, ex.Err)
			}
		}
	}
}

// TestEventConformance holds the event rendering to the same checks, for a
// team row, a personal row and local mode's one user. Every row links to the
// entity and to its rule, so a row whose entity link cannot be shown still
// carries an action.
func TestEventConformance(t *testing.T) {
	exs := kinds.Event.Examples()
	if len(exs) != 3 {
		t.Fatalf("got %d examples; want a team row, a personal row and local mode's", len(exs))
	}
	for _, ex := range exs {
		if strings.TrimSpace(ex.Content.Title) == "" {
			t.Errorf("an empty title for %+v", ex.Recipient)
		}
		if ex.Err != nil {
			t.Errorf("for %+v (local %v), a write would be refused: %v", ex.Recipient, ex.Local, ex.Err)
		}
	}
	for _, ex := range exs {
		if n := len(ex.Content.Actions); n != 2 {
			t.Errorf("%+v row has %d actions; want the entity link and the rule link", ex.Recipient, n)
		}
	}
}
