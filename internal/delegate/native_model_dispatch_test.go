package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
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
				func(context.Context, string, string) (domain.TeamModels, error) {
					return domain.NewTeamModels(tc.teamDefault, domain.ModelSet{}), nil
				})

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

// A pinned step outside the team's enable-set fails the delegation by name, and
// leaves nothing behind to reap. The pin was legal when it was saved — the
// catalog is the save-time gate — and became illegal when the set narrowed
// afterwards, which is the case a save-time check structurally cannot catch.
//
// The team default here IS enabled, so the only thing that can produce the
// refusal is the pin. And the same pin under a set that names it runs, which is
// the other half: the enable-set is membership, never a ceiling, so an enabled
// model costlier than the default dispatches on it.
func TestDelegate_StepPinOutsideTheEnabledSet(t *testing.T) {
	enabled := func(keys ...string) domain.TeamModels {
		return domain.NewTeamModels(domain.ModelHaiku,
			domain.TeamModelSet(keys, domain.OrgModelSet(nil, modelcatalog.DefaultEnabled())))
	}

	t.Run("outside the set fails", func(t *testing.T) {
		database := newDelegateTestDB(t)
		task, bpID := delegatableFixture(t, database, "pin-refused")
		if _, err := database.Exec(
			`UPDATE prompts SET model = ? WHERE id = 'capp-pin-refused'`, domain.ModelOpus,
		); err != nil {
			t.Fatalf("pin step model: %v", err)
		}

		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
		s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
			return enabled(domain.ModelHaiku), nil
		})

		_, err := s.Delegate(task, DelegateOpts{
			OrgID:               runmode.LocalDefaultOrgID,
			ExplicitBlueprintID: bpID,
			TriggerType:         "manual",
		})
		if !errors.Is(err, domain.ErrModelNotEnabled) {
			t.Fatalf("Delegate = %v, want ErrModelNotEnabled", err)
		}
		if !strings.Contains(err.Error(), domain.ModelOpus) {
			t.Errorf("error %q does not name the pinned model", err)
		}
		// Nothing durable was written: the refusal lands before the firing's
		// commit point, so there is no blueprint_run to reap and no queued
		// conversation for a claim to pick up.
		if n := countBlueprintRuns(t, database, task.ID); n != 0 {
			t.Errorf("a refused delegation minted %d blueprint_run(s)", n)
		}
		var queued int
		if err := database.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&queued); err != nil {
			t.Fatalf("count conversations: %v", err)
		}
		if queued != 0 {
			t.Errorf("a refused delegation enqueued %d conversation(s)", queued)
		}
	})

	t.Run("an enabled costlier pin runs", func(t *testing.T) {
		database := newDelegateTestDB(t)
		task, bpID := delegatableFixture(t, database, "pin-allowed")
		if _, err := database.Exec(
			`UPDATE prompts SET model = ? WHERE id = 'capp-pin-allowed'`, domain.ModelOpus,
		); err != nil {
			t.Fatalf("pin step model: %v", err)
		}

		s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
		s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
			return enabled(domain.ModelHaiku, domain.ModelOpus), nil
		})

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
		if got.String != domain.ModelOpus {
			t.Errorf("conversation model = %q, want the enabled pin %q", got.String, domain.ModelOpus)
		}
	})
}
