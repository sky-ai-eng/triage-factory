package delegate

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// What a step is dispatched on, end to end from the team default. Stored
// configuration carries concrete model ids and nothing translates between the
// settings row and the provider, so the id that lands on the conversation — the
// one the native loop puts on the wire and the ledger prices against — has to be
// the stored one, verbatim.
//
// The alias shim that used to translate at native dispatch pinned this about
// itself; with it gone the property is asserted here instead, one layer up,
// where the value actually travels — and where the conversation row the UI and
// the ledger read is written.
func TestDelegate_StepRunsOnTheStoredModelID(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teamDefault string
		stepPin     string
		want        string
	}{
		{"unset step inherits the team default", domain.ModelSonnet, "", domain.ModelSonnet},
		{"a pin is carried through as stored", domain.ModelOpus, domain.ModelHaiku, domain.ModelHaiku},
		// A pin costlier than the team default, and one no Anthropic tier
		// ranks: both are the person's choice and both reach the wire.
		{"a costlier pin is carried through", domain.ModelHaiku, domain.ModelOpus, domain.ModelOpus},
		{"a pin outside the tier ladder is carried through", domain.ModelSonnet, "claude-fable-5", "claude-fable-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			task, bpID := delegatableFixture(t, database, "wire")
			if tc.stepPin != "" {
				if _, err := database.Exec(
					`UPDATE prompts SET model = ? WHERE id = 'capp-wire'`, tc.stepPin,
				); err != nil {
					t.Fatalf("pin step model: %v", err)
				}
			}

			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
			s.SetRunCredentialResolvers(nil, nil,
				func(context.Context, string, string) string { return tc.teamDefault })

			brID, err := s.Delegate(task, DelegateOpts{
				OrgID:               runmode.LocalDefaultOrgID,
				ExplicitBlueprintID: bpID,
				TriggerType:         "manual",
			})
			if err != nil {
				t.Fatalf("Delegate: %v", err)
			}

			var got sql.NullString
			if err := database.QueryRow(
				`SELECT model FROM conversations WHERE blueprint_run_id = ?`, brID,
			).Scan(&got); err != nil {
				t.Fatalf("read step-0 conversation: %v", err)
			}
			if got.String != tc.want {
				t.Errorf("conversation model = %q, want %q", got.String, tc.want)
			}
		})
	}
}
