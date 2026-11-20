package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestShippedSync_Postgres_WaitsForAStepsWriteOnTheBlueprint: the drift sync
// holds a shipped blueprint before it reads its steps, as every steps write
// does. A sync that runs while a user's steps write holds the blueprint waits
// for it, then reads the user_modified stamp that write committed and leaves
// the unit alone, rather than writing shipped content over the edit.
func TestShippedSync_Postgres_WaitsForAStepsWriteOnTheBlueprint(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID := seedPgOrgForBlueprints(t, h)
	seedPgDefaultTeam(t, h, orgID, userID)
	teamID := firstTeamForOrg(t, h, orgID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	shipped := func(body string) ([]domain.Prompt, []domain.SeedBlueprint) {
		return []domain.Prompt{{SystemSlug: "ci", Name: "CI Fix", Body: body, Source: "system"}},
			[]domain.SeedBlueprint{{SystemSlug: "ci", Name: "CI Fix", StepPromptSlugs: []string{"ci"}}}
	}
	prompts, blueprints := shipped("old body")
	if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, blueprints); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	var blueprintID, promptID string
	if err := h.AdminDB.QueryRow(`
		SELECT b.id, bs.step_prompt_id FROM blueprints b
		JOIN blueprint_steps bs ON bs.blueprint_id = b.id AND bs.org_id = b.org_id
		WHERE b.org_id = $1 AND b.team_id = $2 AND b.system_slug = 'ci'
	`, orgID, teamID).Scan(&blueprintID, &promptID); err != nil {
		t.Fatalf("read the shipped blueprint: %v", err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	var once bool
	free := func() {
		if !once {
			once = true
			close(release)
		}
	}
	t.Cleanup(free)
	edit := make(chan error, 1)
	go func() {
		edit <- stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			if _, err := tx.Blueprints.ReplaceSteps(ctx, orgID, blueprintID, []string{promptID}, []string{"the user's brief"}); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-edit:
		t.Fatalf("the user's steps write ended before holding the blueprint: %v", err)
	}

	prompts, blueprints = shipped("new body")
	synced := make(chan error, 1)
	go func() { synced <- stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, blueprints) }()
	select {
	case err := <-synced:
		t.Fatalf("the sync answered while a steps write held the blueprint: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	free()
	if err := <-edit; err != nil {
		t.Fatalf("the user's steps write: %v", err)
	}
	select {
	case err := <-synced:
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sync never answered after the steps write committed")
	}

	var body, brief string
	if err := h.AdminDB.QueryRow(`SELECT body FROM prompts WHERE id = $1`, promptID).Scan(&body); err != nil {
		t.Fatalf("read the step prompt: %v", err)
	}
	if body != "old body" {
		t.Errorf("step prompt body after the sync = %q; want the unit left alone, %q", body, "old body")
	}
	if err := h.AdminDB.QueryRow(`SELECT brief FROM blueprint_steps WHERE blueprint_id = $1 AND step_index = 0`, blueprintID).Scan(&brief); err != nil {
		t.Fatalf("read the step: %v", err)
	}
	if brief != "the user's brief" {
		t.Errorf("step brief after the sync = %q; want the user's", brief)
	}
}
