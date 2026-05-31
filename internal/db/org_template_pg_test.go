package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestOrgTemplate_Postgres_ForwardOnlyAndIsolation is the SKY-381 multi-tenant
// acceptance pin, on the real two-pool Postgres store:
//
//   - an org admin can edit their org's template (RLS allows the write);
//   - a non-admin member cannot (RLS denies it — defense in depth behind the
//     handler's requireOrgAdminRole gate);
//   - org A's edited template flows to A's *next* new team;
//   - the team that already existed when the edit landed is untouched
//     (forward-only);
//   - org B is unaffected by A's edit (cross-org isolation).
//
// Skips when Docker is unavailable (pgtest harness).
func TestOrgTemplate_Postgres_ForwardOnlyAndIsolation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, ownerA, founderTeamA := pgtest.SeedOrgWithUser(t, h, "orgA-owner")
	orgB, _, founderTeamB := pgtest.SeedOrgWithUser(t, h, "orgB-owner")

	// A plain member of org A (not an admin) — used for the RLS-denial check.
	memberA := pgtest.SeedUser(t, h, "orgA-member")
	pgtest.AddOrgMember(t, h, memberA, orgA, founderTeamA, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	// Org-create seeds each org's template + founder team from the shipped lists.
	if err := db.BootstrapNewOrg(ctx, stores, orgA, founderTeamA, ai.ShippedPrompts()); err != nil {
		t.Fatalf("BootstrapNewOrg A: %v", err)
	}
	if err := db.BootstrapNewOrg(ctx, stores, orgB, founderTeamB, ai.ShippedPrompts()); err != nil {
		t.Fatalf("BootstrapNewOrg B: %v", err)
	}

	const customSlug = "tmpl-custom-A"

	// A non-admin member's template write is refused by RLS.
	memberErr := h.WithUser(t, memberA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx).OrgTemplate.CreatePrompt(ctx, orgA, domain.Prompt{
			ID: uuid.New().String(), SystemSlug: "tmpl-member-attempt", Name: "Nope", Body: "x", Source: "user",
		})
	})
	if memberErr == nil {
		t.Error("non-admin member created an org_template_prompts row; RLS should refuse it")
	}

	// The org admin (owner) edits the template — adds a house-rules prompt.
	if err := h.WithUser(t, ownerA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx).OrgTemplate.CreatePrompt(ctx, orgA, domain.Prompt{
			ID: uuid.New().String(), SystemSlug: customSlug, Name: "Org A House Rules", Body: "house rules", Source: "user",
		})
	}); err != nil {
		t.Fatalf("admin CreatePrompt: %v", err)
	}

	// A 2nd team in org A, created AFTER the edit, copies the edited template.
	team2A := pgtest.SeedTeam(t, h, orgA, "team2-a")
	if err := db.BootstrapNewTeam(ctx, stores, orgA, team2A); err != nil {
		t.Fatalf("BootstrapNewTeam A team2: %v", err)
	}

	// A team in org B, to confirm B never sees A's edit.
	team2B := pgtest.SeedTeam(t, h, orgB, "team2-b")
	if err := db.BootstrapNewTeam(ctx, stores, orgB, team2B); err != nil {
		t.Fatalf("BootstrapNewTeam B team2: %v", err)
	}

	countPrompt := func(org, team, slug string) int {
		t.Helper()
		var n int
		if err := h.AdminDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`, org, team, slug,
		).Scan(&n); err != nil {
			t.Fatalf("count prompt: %v", err)
		}
		return n
	}

	// Flows forward: A's new team carries the edit.
	if got := countPrompt(orgA, team2A, customSlug); got != 1 {
		t.Errorf("org A's new team has %d copies of the template-added prompt; want 1", got)
	}
	// Forward-only: A's founder team (pre-existing) is untouched.
	if got := countPrompt(orgA, founderTeamA, customSlug); got != 0 {
		t.Errorf("org A's pre-existing founder team gained %d copies of the post-create template edit; want 0 (forward-only)", got)
	}
	// Cross-org isolation: org B's new team never sees A's edit.
	if got := countPrompt(orgB, team2B, customSlug); got != 0 {
		t.Errorf("org B's new team has %d copies of org A's template prompt; want 0 (cross-org isolation)", got)
	}
	// And org B's template itself never received A's row.
	var bTemplateCustom int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_template_prompts WHERE org_id = $1 AND system_slug = $2`, orgB, customSlug,
	).Scan(&bTemplateCustom); err != nil {
		t.Fatalf("count org B template: %v", err)
	}
	if bTemplateCustom != 0 {
		t.Errorf("org B's template has %d copies of org A's custom slug; want 0", bTemplateCustom)
	}
}
