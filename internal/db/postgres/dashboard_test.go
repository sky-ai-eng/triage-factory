package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestDashboardStore_Postgres runs the shared DashboardStore
// conformance suite against the Postgres impl. The seeder casts
// the marshaled JSON to JSONB so it lands in the typed column;
// snapshot_json is JSONB in D3.
func TestDashboardStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunDashboardStoreConformance(t, func(t *testing.T) (db.DashboardStore, string, dbtest.PRSnapshotSeederForDashboard) {
		t.Helper()
		h.Reset(t)
		orgID, _ := seedPgOrgAndUserForDashboard(t, h)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		seed := func(t *testing.T, snap domain.PRSnapshot) {
			t.Helper()
			seedPgPRSnapshot(t, h.AdminDB, orgID, snap)
		}
		return stores.Dashboard, orgID, seed
	})
}

// TestDashboardStore_Postgres_OnlyCountsRequestingUser is the org-wide
// regression guard for SKY-367: the dashboard query reads every GitHub
// snapshot in the org (entities are org-wide, no team column), and the
// per-user filtering happens in Go on snap.Author / review author. Once
// org-wide polling lands, many other users' PRs share the org; this pins
// that another user's PRs and reviews never bleed into the requesting
// user's stats or PR list. If a refactor ever drops the username filter
// (e.g. pushing it into SQL incorrectly, or removing it), this fails.
func TestDashboardStore_Postgres_OnlyCountsRequestingUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _ := seedPgOrgAndUserForDashboard(t, h)
	stores := pgstore.New(h.AdminDB, h.AdminDB)
	ctx := context.Background()

	const me = "aidan"
	now := time.Now().UTC().Format(time.RFC3339)

	// My work.
	seedPgPRSnapshot(t, h.AdminDB, orgID, domain.PRSnapshot{Number: 1, Author: me, State: "MERGED", Merged: true, MergedAt: now, Repo: "owner/repo"})
	seedPgPRSnapshot(t, h.AdminDB, orgID, domain.PRSnapshot{Number: 2, Author: me, State: "OPEN", Repo: "owner/repo"})
	// A teammate's open PR I reviewed — counts as a review given, not as my PR.
	seedPgPRSnapshot(t, h.AdminDB, orgID, domain.PRSnapshot{
		Number: 3, Author: "other-user", State: "OPEN", Repo: "owner/repo",
		Reviews: []domain.ReviewState{{Author: me}},
	})
	// Pure other-user PRs that share the org — must be invisible to me.
	seedPgPRSnapshot(t, h.AdminDB, orgID, domain.PRSnapshot{Number: 4, Author: "other-user", State: "OPEN", Repo: "owner/repo"})
	seedPgPRSnapshot(t, h.AdminDB, orgID, domain.PRSnapshot{Number: 5, Author: "third-user", State: "MERGED", Merged: true, MergedAt: now, Repo: "owner/repo"})

	stats, err := stores.Dashboard.Stats(ctx, orgID, me, 30)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Merged != 1 {
		t.Errorf("Merged = %d, want 1 (third-user's merged PR must not count)", stats.Merged)
	}
	if stats.Awaiting != 1 {
		t.Errorf("Awaiting = %d, want 1 (only my open PR; other users' open PRs excluded)", stats.Awaiting)
	}
	if stats.ReviewsGiven != 1 {
		t.Errorf("ReviewsGiven = %d, want 1 (my review on other-user's PR)", stats.ReviewsGiven)
	}

	prs, err := stores.Dashboard.PRs(ctx, orgID, me)
	if err != nil {
		t.Fatalf("PRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("len(prs) = %d, want 2 (only my PRs, even with 3 other users' PRs in the org)", len(prs))
	}
	for _, p := range prs {
		if p.Author != me {
			t.Errorf("PRs returned another user's PR (#%d by %s) — org-wide leak", p.Number, p.Author)
		}
	}
}

func seedPgOrgAndUserForDashboard(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("dash-conf-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "Dash Conformance User"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Dash Conformance Org "+orgID[:8], "dash-"+orgID[:8], userID); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

func seedPgPRSnapshot(t *testing.T, conn *sql.DB, orgID string, snap domain.PRSnapshot) {
	t.Helper()
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("dashboard-conformance-%d-%d", snap.Number, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at, last_polled_at)
		VALUES ($1, $2, 'github', $3, 'pr', $4, $5, $6::jsonb, $7, $7)
	`, entityID, orgID, sourceID, snap.Title, snap.URL, string(blob), now); err != nil {
		t.Fatalf("seed entity for snapshot %d: %v", snap.Number, err)
	}
}
