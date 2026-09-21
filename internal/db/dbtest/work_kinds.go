package dbtest

import (
	"regexp"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// workKindNameRE is the shape a registered kind's name must have: it is a
// path segment on the operator surface and a metric label, so it is a bare
// lower-case identifier and nothing else.
var workKindNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// AssertWorkKindRegistry pins what every registered work kind owes the
// surfaces that iterate the registry: a bare lower-case identifier for a
// name, unique across the registry, and a Kind that carries a metrics
// observer — a kind registered without one would sit on the panel and never
// count. Both dialects' store tests run it over the bundle they build.
func AssertWorkKindRegistry(t *testing.T, kinds []db.WorkKindHandle) {
	t.Helper()
	if len(kinds) == 0 {
		t.Fatal("no work kinds registered; the event queue must be on the registry")
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		name := k.Name()
		if !workKindNameRE.MatchString(name) {
			t.Errorf("work kind %q is not a bare lower-case identifier", name)
		}
		if seen[name] {
			t.Errorf("work kind %q is registered twice", name)
		}
		seen[name] = true
		if k.Label() == "" {
			t.Errorf("work kind %q has no label", name)
		}
		if k.Kind().Observer == nil {
			t.Errorf("work kind %q carries no metrics observer", name)
		}
		if err := k.Kind().Validate(); err != nil {
			t.Errorf("work kind %q: %v", name, err)
		}
		if k.Conn() == nil {
			t.Errorf("work kind %q has no connection", name)
		}
		if k.Objective().OldestReadyAge <= 0 {
			t.Errorf("work kind %q declares no oldest-ready-age objective", name)
		}
		if !k.Controls().Redrive {
			t.Errorf("work kind %q does not offer redrive, which every kind offers", name)
		}
	}
}
