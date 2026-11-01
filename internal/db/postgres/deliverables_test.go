package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestDeliverableStore_Postgres_RoundTrip drives Upsert + ListByConversation against
// the AdminDB-wired store (RLS bypassed — behavior, not auth). Pins that
// every field round-trips and an empty id is server-generated. TFAC-455.
func TestDeliverableStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	in := domain.Deliverable{
		ConversationID: conversationID,
		OrgID:          orgID,
		TeamID:         teamID,
		Provider:       domain.ProviderGitHub,
		Scope:          "https://github.com",
		Kind:           domain.DeliverableKindPullRequest,
		Target:         "octo/repo#123",
		ExternalID:     "123",
		URL:            "https://github.com/octo/repo/pull/123",
		State:          domain.DeliverableStatePROpen,
		DedupKey:       domain.ObjectKey("github", "pull_request", "octo/repo", "refs/heads/feat"),
		DetailsJSON:    `{"draft":false}`,
	}
	out, err := stores.Deliverables.Upsert(ctx, orgID, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if out.ID == "" {
		t.Error("expected server-generated id")
	}
	if out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() {
		t.Error("expected timestamps populated")
	}
	if out.ConversationID != conversationID || out.TeamID != teamID || out.Provider != "github" ||
		out.Kind != "pull_request" || out.Target != "octo/repo#123" || out.ExternalID != "123" ||
		out.URL != in.URL || out.State != "open" || out.DedupKey != in.DedupKey ||
		out.DetailsJSON != `{"draft":false}` {
		t.Errorf("round-trip mismatch: %+v", out)
	}

	got, _, err := stores.Deliverables.ListByConversation(ctx, orgID, conversationID, db.Unwindowed)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(got) != 1 || got[0].ID != out.ID {
		t.Errorf("ListByConversation = %+v, want the one upserted row", got)
	}
}

// TestDeliverableStore_Postgres_UpsertDedup pins that the same dedup_key
// collapses to one row and the second upsert updates the mutable fields.
func TestDeliverableStore_Postgres_UpsertDedup(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ObjectKey("github", "pull_request", "octo/repo", "refs/heads/feat")
	first, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
		State: domain.DeliverableStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	second, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#7",
		ExternalID: "7", URL: "https://github.com/octo/repo/pull/7",
		State: domain.DeliverableStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("dedup failed: %s vs %s", first.ID, second.ID)
	}
	var count int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM deliverables WHERE dedup_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row, got %d", count)
	}
	if second.State != "open" || second.ExternalID != "7" || second.Target != "octo/repo#7" {
		t.Errorf("second upsert did not update fields: %+v", second)
	}
	if second.UpdatedAt.Before(second.CreatedAt) {
		t.Errorf("updated_at %v before created_at %v", second.UpdatedAt, second.CreatedAt)
	}
}

// TestDeliverableStore_Postgres_InsertIfAbsent mirrors the SQLite conformance for
// the reconciler-backstop write: first insert lands, a second on the same
// dedup_key is a no-op that preserves the existing row's advanced state.
func TestDeliverableStore_Postgres_InsertIfAbsent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ObjectKey("github", "pull_request", "octo/repo#7", "")
	base := domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#7", ExternalID: "7",
		DedupKey: key,
	}

	first := base
	first.State = domain.DeliverableStatePROpen
	inserted, err := stores.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, first)
	if err != nil {
		t.Fatalf("first InsertDeliverableIfAbsentSystem: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported not-inserted; want true")
	}

	if _, err := h.AdminDB.Exec(`UPDATE deliverables SET state = $1 WHERE dedup_key = $2`, domain.DeliverableStatePRMerged, key); err != nil {
		t.Fatalf("advance state: %v", err)
	}

	second := base
	second.State = domain.DeliverableStatePROpen
	inserted, err = stores.Deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, second)
	if err != nil {
		t.Fatalf("second InsertDeliverableIfAbsentSystem: %v", err)
	}
	if inserted {
		t.Error("second insert reported inserted; want false")
	}

	var count int
	var state string
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*), MAX(state) FROM deliverables WHERE dedup_key = $1`, key).Scan(&count, &state); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want exactly 1 row, got %d", count)
	}
	if state != domain.DeliverableStatePRMerged {
		t.Errorf("insert-if-absent clobbered state to %q; want merged preserved", state)
	}
}

// TestDeliverableStore_Postgres_PendingToReal pins the pending→real PR
// transition: keyed on the branch ref, the real PR upserts onto the same
// row (pending→open, url/external_id filled).
func TestDeliverableStore_Postgres_PendingToReal(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ObjectKey("github", "pull_request", "octo/repo", "refs/heads/feat")
	pending, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
		State: domain.DeliverableStatePRPending, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("pending Upsert: %v", err)
	}
	if pending.State != "pending" || pending.ExternalID != "" || pending.URL != "" {
		t.Fatalf("pending row malformed: %+v", pending)
	}
	real, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#42",
		ExternalID: "42", URL: "https://github.com/octo/repo/pull/42",
		State: domain.DeliverableStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("real Upsert: %v", err)
	}
	if real.ID != pending.ID {
		t.Errorf("pending→real minted a new row: %s vs %s", pending.ID, real.ID)
	}
	if real.State != "open" || real.ExternalID != "42" || real.URL == "" {
		t.Errorf("real row did not transition: %+v", real)
	}
	var count int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM deliverables`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row after pending→real, got %d", count)
	}
}

// TestDeliverableStore_Postgres_RLS_TeamScoped pins the production RLS layer:
// a deliverable scoped to teamA is visible to teamA members (alice, bob) and
// invisible to a same-org member of a different team (carol). Mirrors the
// conversations team-visibility scoping. TFAC-455.
func TestDeliverableStore_Postgres_RLS_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	bob := pgtest.SeedUser(t, h, "bob")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")

	conversationID := seedPgDeliverableConversation(t, h, orgA, teamA, alice)
	// Seed a teamA-scoped deliverable via admin (bypass RLS for setup).
	if _, err := h.AdminDB.Exec(`
		INSERT INTO deliverables (org_id, conversation_id, team_id, provider, scope, kind, target, state, dedup_key)
		VALUES ($1, $2, $3, 'github', 'https://github.com', 'pull_request', 'octo/repo', 'open', 'github:pull_request:octo/repo#1')
	`, orgA, conversationID, teamA); err != nil {
		t.Fatalf("seed deliverable: %v", err)
	}
	ctx := context.Background()

	// Alice (teamA) and bob (teamA) see it.
	for _, u := range []struct {
		name, id string
	}{{"alice", alice}, {"bob", bob}} {
		err := h.WithUser(t, u.id, orgA, func(tx *sql.Tx) error {
			rows, _, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.ListByConversation(ctx, orgA, conversationID, db.Unwindowed)
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				t.Errorf("%s saw %d teamA deliverables, want 1", u.name, len(rows))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s path: %v", u.name, err)
		}
	}

	// Carol (teamB, same org, different team) sees zero — deliverables_select
	// gates on user_in_team(team_id).
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		rows, _, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.ListByConversation(ctx, orgA, conversationID, db.Unwindowed)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("carol (different team) saw %d teamA deliverables — RLS leaked across teams", len(rows))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol path: %v", err)
	}
}

// TestDeliverableStore_Postgres_RLS_WritePath exercises the deliverables_insert
// policy under tf_app (the read test bypasses it via AdminDB at setup): a
// teamA member with write access can Upsert a teamA deliverable, but a member
// of a different team is rejected by the WITH CHECK
// (user_can_write_team(team_id)). Without this the write policy could be
// misconfigured and the read-only suite would still pass. TFAC-455.
func TestDeliverableStore_Postgres_RLS_WritePath(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	ctx := context.Background()

	// conversation_id left empty (NULL) so this isolates deliverables_insert, not the
	// conversations FK/RLS interplay.
	mkDeliverable := func(key string) domain.Deliverable {
		return domain.Deliverable{
			OrgID: orgA, TeamID: teamA,
			Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
			State: domain.DeliverableStatePROpen, DedupKey: key,
		}
	}

	// Alice (teamA, founder → can write) succeeds through the app pool.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.Upsert(ctx, orgA, mkDeliverable("github:pull_request:octo/repo:refs/heads/a"))
		return err
	}); err != nil {
		t.Fatalf("alice (teamA writer) Upsert under RLS: %v", err)
	}
	var landed int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM deliverables WHERE team_id = $1`, teamA).Scan(&landed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if landed != 1 {
		t.Errorf("alice's deliverable didn't land: count = %d, want 1", landed)
	}

	// Carol (teamB) writing a teamA-scoped deliverable is rejected by the
	// deliverables_insert WITH CHECK.
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, e := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.Upsert(ctx, orgA, mkDeliverable("github:pull_request:octo/repo:refs/heads/b"))
		return e
	})
	pgtest.AssertRLSViolation(t, err)
}

// TestDeliverableStore_Postgres_RLS_InsertIfAbsent runs the app-pool
// InsertDeliverableIfAbsent under tf_app, the path a manual run's pull request
// create takes: a team writer inserts a deliverable of their team's, a member
// of another team is refused by the deliverables_insert WITH CHECK, and a
// writer whose key another team's row already holds inserts nothing, with no
// error and that row left as it was.
func TestDeliverableStore_Postgres_RLS_InsertIfAbsent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	ctx := context.Background()

	mkDeliverable := func(teamID, key, state string) domain.Deliverable {
		return domain.Deliverable{
			OrgID: orgA, TeamID: teamID,
			Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#7",
			State: state, DedupKey: key,
		}
	}
	insertAs := func(user string, a domain.Deliverable) (bool, error) {
		var inserted bool
		err := h.WithUser(t, user, orgA, func(tx *sql.Tx) error {
			var e error
			inserted, e = pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.InsertDeliverableIfAbsent(ctx, orgA, a)
			return e
		})
		return inserted, err
	}

	if inserted, err := insertAs(alice, mkDeliverable(teamA, "github:pull_request:PR_a", domain.DeliverableStatePRDraft)); err != nil || !inserted {
		t.Fatalf("alice (teamA writer) InsertDeliverableIfAbsent under RLS = %v, %v; want an insert", inserted, err)
	}

	_, err := insertAs(carol, mkDeliverable(teamA, "github:pull_request:PR_c", domain.DeliverableStatePRDraft))
	pgtest.AssertRLSViolation(t, err)

	admin := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	theirs, err := admin.Deliverables.UpsertSystem(ctx, orgA, mkDeliverable(teamB, "github:pull_request:PR_b", domain.DeliverableStatePRDraft))
	if err != nil {
		t.Fatalf("seed teamB's deliverable: %v", err)
	}
	if inserted, err := insertAs(alice, mkDeliverable(teamA, "github:pull_request:PR_b", domain.DeliverableStatePROpen)); err != nil || inserted {
		t.Errorf("alice InsertDeliverableIfAbsent on teamB's key = %v, %v; want no insert and no error", inserted, err)
	}
	var team, state string
	if err := h.AdminDB.QueryRow(`SELECT team_id, state FROM deliverables WHERE id = $1`, theirs.ID).Scan(&team, &state); err != nil {
		t.Fatalf("read teamB's deliverable: %v", err)
	}
	if team != teamB || state != domain.DeliverableStatePRDraft {
		t.Errorf("teamB's deliverable = team %s state %s, want it unchanged (team %s, draft)", team, state, teamB)
	}
}

// TestDeliverableStore_Postgres_SurvivesConversationPurge pins the
// audit-ledger invariant: a deliverable whose conversation was purged is
// detached (conversation_id NULL), not deleted, and still reads back as the
// team's.
func TestDeliverableStore_Postgres_SurvivesConversationPurge(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	art, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#3",
		State: domain.DeliverableStatePROpen, DedupKey: domain.PullRequestDedupKey("PR_3"),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate a conversation purge: the FK is ON DELETE SET NULL, so deleting
	// the conversation detaches the deliverable rather than cascading it away.
	if _, err := h.AdminDB.Exec(`DELETE FROM conversations WHERE id = $1`, conversationID); err != nil {
		t.Fatalf("purge conversation: %v", err)
	}
	var nullConversation sql.NullString
	if err := h.AdminDB.QueryRow(`SELECT conversation_id FROM deliverables WHERE id = $1`, art.ID).Scan(&nullConversation); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nullConversation.Valid {
		t.Fatalf("conversation_id should be NULL after conversation purge, got %q", nullConversation.String)
	}

	got, err := stores.Deliverables.Get(ctx, orgID, art.ID)
	if err != nil || got == nil {
		t.Fatalf("Get the detached deliverable = %v, %v", got, err)
	}
	if got.ConversationID != "" || got.TeamID != teamID {
		t.Errorf("detached row = {conversation %q, team %q}, want {\"\", %s}", got.ConversationID, got.TeamID, teamID)
	}
}

// TestDeliverableStore_Postgres_UpsertPreservesExternalIDAndURL pins that an
// upsert leaving external_id/url empty does NOT blank values an earlier upsert
// stored. They are the backing object's stable coordinates and only fill in;
// a reconciliation pass or a URL-less mutation must not erase them. State
// still follows the latest writer.
func TestDeliverableStore_Postgres_UpsertPreservesExternalIDAndURL(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.PullRequestDedupKey("PR_1")
	if _, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#1",
		ExternalID: "1", URL: "https://github.com/octo/repo/pull/1",
		State: domain.DeliverableStatePRDraft, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	out, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#1",
		State: domain.DeliverableStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if out.ExternalID != "1" || out.URL != "https://github.com/octo/repo/pull/1" {
		t.Errorf("external_id/url were blanked by an empty upsert: ext=%q url=%q", out.ExternalID, out.URL)
	}
	if out.State != domain.DeliverableStatePROpen {
		t.Errorf("state should follow the latest writer, got %q", out.State)
	}

	// A non-empty value still overwrites.
	out, err = stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#1",
		ExternalID: "1", URL: "https://github.com/octo/repo/pull/1/files",
		State: domain.DeliverableStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if out.URL != "https://github.com/octo/repo/pull/1/files" {
		t.Errorf("non-empty url did not overwrite: %q", out.URL)
	}
}

// TestDeliverableStore_Postgres_UpsertPreservesTargetOnEmpty pins that an
// upsert leaving target empty does NOT blank the target an earlier upsert
// stored — a review's submit stamps its id and link and need not restate the
// pull request. A non-empty target still overwrites.
func TestDeliverableStore_Postgres_UpsertPreservesTargetOnEmpty(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	key := domain.ReviewDedupKey("PR_7", conversationID)
	if _, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "review", Target: "octo/repo#7",
		URL:   "https://github.com/octo/repo/pull/7",
		State: domain.DeliverableStateReviewPending, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A write that carries the review's id but no target or link.
	out, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "review", ExternalID: "555",
		State: domain.DeliverableStateReviewSubmitted, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if out.Target != "octo/repo#7" || out.URL != "https://github.com/octo/repo/pull/7" {
		t.Errorf("empty upsert blanked stable coordinates: target=%q url=%q", out.Target, out.URL)
	}
	if out.State != domain.DeliverableStateReviewSubmitted {
		t.Errorf("state should follow the latest writer, got %q", out.State)
	}

	// A non-empty target still overwrites.
	out, err = stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: "github", Scope: "https://github.com", Kind: "review", Target: "octo/repo#8",
		State: domain.DeliverableStateReviewSubmitted, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if out.Target != "octo/repo#8" {
		t.Errorf("non-empty target did not overwrite: %q", out.Target)
	}
}

// TestDeliverableStore_Postgres_UpsertSystem_BypassesRLS pins the admin-pool
// write path the event-triggered exec choke point depends on (TFAC-459): a
// conversation with no creator user has no JWT-claims context, so its deliverable insert
// is unreachable through tf_app (deliverables_insert demands a team-writing
// user). UpsertSystem routes to the admin pool and lands the row; the app-pool
// Upsert on the same bare (claims-less) connection is rejected, proving the
// System variant is doing real pool-routing work, not riding ambient access.
func TestDeliverableStore_Postgres_UpsertSystem_BypassesRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "alice")

	// app = AppDB (authenticator, RLS-active, no claims set here); admin =
	// AdminDB (BYPASSRLS). This is the production pool split.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	// conversation_id left empty (NULL) so this isolates the write-policy/pool routing
	// from the conversations FK.
	art := domain.Deliverable{
		OrgID: orgID, TeamID: teamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindPullRequest,
		Target: "octo/repo#1", ExternalID: "1", State: domain.DeliverableStatePRDraft,
		DedupKey: domain.PullRequestDedupKey("PR_1"),
	}

	// The app-pool write with no claims context fails — exactly why the
	// event-triggered writer needs the admin path.
	if _, err := stores.Deliverables.Upsert(ctx, orgID, art); err == nil {
		t.Fatal("expected the claims-less app-pool Upsert to be rejected")
	}

	// UpsertSystem (admin pool) lands the row.
	out, err := stores.Deliverables.UpsertSystem(ctx, orgID, art)
	if err != nil {
		t.Fatalf("UpsertSystem: %v", err)
	}
	if out.ID == "" || out.Target != "octo/repo#1" || out.State != domain.DeliverableStatePRDraft {
		t.Errorf("UpsertSystem row malformed: %+v", out)
	}

	var count int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM deliverables WHERE team_id = $1 AND dedup_key = 'github:pull_request:PR_1'`, teamID,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("UpsertSystem did not land the row: count = %d, want 1", count)
	}
}

// TestDeliverableStore_Postgres_ListNonTerminalBySystem pins the reconciler's
// org-wide admin read: on the admin pool it returns EXACTLY the reconcilable
// non-terminal rows for the org (PR draft|open —
// domain.IsReconcilableNonTerminal), excludes terminal / unreconcilable
// states, and never crosses org boundaries.
func TestDeliverableStore_Postgres_ListNonTerminalBySystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, _, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, _, teamB := pgtest.SeedOrgWithUser(t, h, "bob")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seeds := []struct{ kind, state string }{
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRPending},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRDraft},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePROpen},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRMerged},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRClosed},
		{domain.DeliverableKindReview, domain.DeliverableStateReviewPending},
		{domain.DeliverableKindReview, domain.DeliverableStateReviewSubmitted},
		{domain.DeliverableKindReview, domain.DeliverableStateReviewDismissed},
	}
	want := map[string]bool{}
	for i, s := range seeds {
		key := fmt.Sprintf("a%d", i)
		if _, err := stores.Deliverables.UpsertSystem(ctx, orgA, domain.Deliverable{
			OrgID: orgA, TeamID: teamA, Provider: "github", Scope: "https://github.com", Kind: s.kind, Target: "octo/repo", State: s.state, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.state, err)
		}
		if domain.IsReconcilableNonTerminal(s.kind, s.state) {
			want[key] = true
		}
	}
	// Org B owns a non-terminal deliverable that must never surface in org A's list.
	if _, err := stores.Deliverables.UpsertSystem(ctx, orgB, domain.Deliverable{
		OrgID: orgB, TeamID: teamB, Provider: "github", Scope: "https://github.com", Kind: domain.DeliverableKindPullRequest,
		Target: "octo/repo", State: domain.DeliverableStatePROpen, DedupKey: "b0",
	}); err != nil {
		t.Fatalf("seed org B: %v", err)
	}

	got, err := stores.Deliverables.ListNonTerminalBySystem(ctx, orgA)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	gotKeys := map[string]bool{}
	for _, a := range got {
		if a.OrgID != orgA {
			t.Errorf("cross-org leak: row %q belongs to org %q, not %q", a.DedupKey, a.OrgID, orgA)
		}
		gotKeys[a.DedupKey] = true
	}
	if len(gotKeys) != len(want) {
		t.Errorf("returned %d rows, want %d: got=%v want=%v", len(gotKeys), len(want), gotKeys, want)
	}
	for k := range want {
		if !gotKeys[k] {
			t.Errorf("dropped reconcilable row %q", k)
		}
	}
	for k := range gotKeys {
		if !want[k] {
			t.Errorf("returned terminal/unreconcilable row %q (SQL predicate disagrees with domain.IsReconcilableNonTerminal)", k)
		}
	}
}

// TestDeliverableStore_Postgres_CountByConversation pins the batched
// per-conversation count (TFAC-465) on the admin pool: counts keyed by
// conversation id across a multi-conversation batch, a conversation with no
// deliverables absent from the map, and an empty conversationIDs a no-op. Doubles
// as the proof that the []string→ANY($2) bind and GROUP BY work against the
// uuid conversation_id column.
func TestDeliverableStore_Postgres_CountByConversation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	convA := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	convB := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	convC := seedPgDeliverableConversation(t, h, orgID, teamID, userID) // no deliverables
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seed := func(conversationID, key string) {
		if _, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
			Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
			State: domain.DeliverableStatePRDraft, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(convA, "a1")
	seed(convA, "a2")
	seed(convB, "b1")

	counts, err := stores.Deliverables.CountByConversation(ctx, orgID, []string{convA, convB, convC})
	if err != nil {
		t.Fatalf("CountByConversation: %v", err)
	}
	if counts[convA] != 2 {
		t.Errorf("convA count = %d, want 2", counts[convA])
	}
	if counts[convB] != 1 {
		t.Errorf("convB count = %d, want 1", counts[convB])
	}
	if _, ok := counts[convC]; ok {
		t.Errorf("convC (no deliverables) should be absent, got %d", counts[convC])
	}

	empty, err := stores.Deliverables.CountByConversation(ctx, orgID, nil)
	if err != nil {
		t.Fatalf("CountByConversation(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("CountByConversation(nil) = %v, want empty non-nil map", empty)
	}
}

// TestDeliverableStore_Postgres_CountByConversation_TeamScoped pins that
// CountByConversation runs on the RLS-active app pool: a same-org member of a
// different team counts zero for a conversation whose deliverables belong to
// another team — the per-card count can't leak across the tenancy boundary.
// Mirrors the ListByTeam RLS test. TFAC-465.
func TestDeliverableStore_Postgres_CountByConversation_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	conversationID := seedPgDeliverableConversation(t, h, orgA, teamA, alice)

	// Two teamA-scoped deliverables on the conversation (admin insert bypasses RLS for setup).
	for _, key := range []string{"github:pull_request:PR_1", "github:pull_request:PR_2"} {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO deliverables (org_id, conversation_id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES ($1, $2, $3, 'github', 'https://github.com', 'pull_request', 'octo/repo', 'draft', $4)
		`, orgA, conversationID, teamA, key); err != nil {
			t.Fatalf("seed deliverable: %v", err)
		}
	}
	ctx := context.Background()

	// Alice (teamA) counts both.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		counts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.CountByConversation(ctx, orgA, []string{conversationID})
		if err != nil {
			return err
		}
		if counts[conversationID] != 2 {
			t.Errorf("alice counted %d, want 2", counts[conversationID])
		}
		return nil
	}); err != nil {
		t.Fatalf("alice path: %v", err)
	}

	// Carol (teamB, same org, different team) counts zero — deliverables_select
	// gates on user_in_team(team_id), so the rows are invisible to her.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		counts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.CountByConversation(ctx, orgA, []string{conversationID})
		if err != nil {
			return err
		}
		if _, ok := counts[conversationID]; ok {
			t.Errorf("carol (different team) counted %d — RLS leaked across teams", counts[conversationID])
		}
		return nil
	}); err != nil {
		t.Fatalf("carol path: %v", err)
	}
}

// TestDeliverableStore_Postgres_ListByConversations pins the batched
// multi-conversation read on the admin pool (TFAC-465): deliverables for the given
// conversations come back in one slice, each carrying its ConversationID for
// grouping; a conversation with no deliverables contributes nothing and an empty
// conversationIDs is a no-op. Doubles as proof the []string→ANY
// uuid[] bind works for the list variant.
func TestDeliverableStore_Postgres_ListByConversations(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	convA := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	convB := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	convC := seedPgDeliverableConversation(t, h, orgID, teamID, userID) // no deliverables
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	seed := func(conversationID, key string) {
		if _, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
			Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
			State: domain.DeliverableStatePRDraft, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(convA, "a1")
	seed(convA, "a2")
	seed(convB, "b1")

	arts, err := stores.Deliverables.ListByConversations(ctx, orgID, []string{convA, convB, convC})
	if err != nil {
		t.Fatalf("ListByConversations: %v", err)
	}
	byConversation := map[string]int{}
	for _, a := range arts {
		if a.ConversationID == "" {
			t.Errorf("deliverable %s came back without its ConversationID", a.ID)
		}
		byConversation[a.ConversationID]++
	}
	if byConversation[convA] != 2 || byConversation[convB] != 1 {
		t.Errorf("grouped counts = %v, want convA=2 convB=1", byConversation)
	}
	if _, ok := byConversation[convC]; ok {
		t.Errorf("convC (no deliverables) contributed %d rows, want 0", byConversation[convC])
	}

	empty, err := stores.Deliverables.ListByConversations(ctx, orgID, nil)
	if err != nil {
		t.Fatalf("ListByConversations(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByConversations(nil) = %v, want empty", empty)
	}
}

// TestDeliverableStore_Postgres_ListByConversations_TeamScoped pins that
// ListByConversations runs on the RLS-active app pool — symmetric to
// CountByConversation_TeamScoped, the defense-in-depth scoping guarantee at the
// store layer. A same-org member of a different team sees no deliverables for a
// conversation whose deliverables belong to another team, even passing the
// conversation id directly. ListByConversations feeds the conversation list's
// approval signal (has_unresolved_deliverables, pending_deliverable_ids), so a
// leak here would mis-surface another team's approval card. TFAC-465.
func TestDeliverableStore_Postgres_ListByConversations_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	carol := pgtest.SeedUser(t, h, "carol")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "member")
	conversationID := seedPgDeliverableConversation(t, h, orgA, teamA, alice)

	// Two teamA-scoped deliverables on the conversation (admin insert bypasses RLS for setup).
	for _, key := range []string{"github:pull_request:PR_1", "github:pull_request:PR_2"} {
		if _, err := h.AdminDB.Exec(`
			INSERT INTO deliverables (org_id, conversation_id, team_id, provider, scope, kind, target, state, dedup_key)
			VALUES ($1, $2, $3, 'github', 'https://github.com', 'pull_request', 'octo/repo', 'draft', $4)
		`, orgA, conversationID, teamA, key); err != nil {
			t.Fatalf("seed deliverable: %v", err)
		}
	}
	ctx := context.Background()

	// Alice (teamA) sees both.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		arts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.ListByConversations(ctx, orgA, []string{conversationID})
		if err != nil {
			return err
		}
		if len(arts) != 2 {
			t.Errorf("alice saw %d deliverables, want 2", len(arts))
		}
		return nil
	}); err != nil {
		t.Fatalf("alice path: %v", err)
	}

	// Carol (teamB, same org, different team) sees zero — deliverables_select gates
	// on user_in_team(team_id), so the rows are invisible to her.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		arts, err := pgstore.NewForTx(tx, pgtest.SecretKey).Deliverables.ListByConversations(ctx, orgA, []string{conversationID})
		if err != nil {
			return err
		}
		if len(arts) != 0 {
			t.Errorf("carol (different team) saw %d deliverables — RLS leaked across teams", len(arts))
		}
		return nil
	}); err != nil {
		t.Fatalf("carol path: %v", err)
	}
}

// TestDeliverableStore_Postgres_ListPendingReviewsByEntity pins the new-commits
// notifier's lookup (TFAC-501): only PENDING review deliverables about the PR's
// entity come back, whatever name their target spells; resolved reviews,
// other PRs' drafts and non-review kinds are excluded.
func TestDeliverableStore_Postgres_ListPendingReviewsByEntity(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	entity := func(key, nodeID string) string {
		t.Helper()
		e, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, domain.ProviderGitHub, "https://github.com", key, nodeID, "pr", "t", "")
		if err != nil {
			t.Fatalf("seed entity %s: %v", key, err)
		}
		return e.ID
	}
	pr7, other7 := entity("octo/repo#7", "PR_kw7"), entity("octo/other#7", "PR_kwOTHER7")
	mk := func(kind, state, target, dedup, entityID string) {
		if _, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
			Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: kind, State: state,
			Target: target, DedupKey: dedup, EntityID: entityID,
		}); err != nil {
			t.Fatalf("seed deliverable %s/%s: %v", kind, state, err)
		}
	}
	// Two pending reviews of the PR (different conversations → different
	// dedup keys), one under a name the repository has since given up, plus
	// noise that must NOT match: resolved drafts, another PR's draft, a draft
	// linked to no entity, and a PR row.
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewPending, "octo/repo#7", domain.ReviewDedupKey("PR_kw7", "conv-a"), pr7)
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewPending, "octo/old-repo#7", domain.ReviewDedupKey("PR_kw7", "conv-b"), pr7)
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewSubmitted, "octo/repo#7", domain.ReviewDedupKey("PR_kw7", "conv-s"), pr7)
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewPending, "octo/repo#7", domain.ReviewDedupKey("PR_kwOTHER7", "conv-a"), other7)
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewPending, "octo/repo#77", domain.ReviewDedupKey("PR_kw77", "conv-a"), "")
	mk(domain.DeliverableKindPullRequest, domain.DeliverableStatePROpen, "octo/repo#7", domain.PullRequestDedupKey("PR_kw7"), pr7)

	got, err := stores.Deliverables.ListPendingReviewsByEntitySystem(ctx, orgID, pr7)
	if err != nil {
		t.Fatalf("ListPendingReviewsByEntitySystem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pending reviews of PR_kw7, got %d: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Kind != domain.DeliverableKindReview || a.State != domain.DeliverableStateReviewPending || a.EntityID != pr7 {
			t.Errorf("unexpected deliverable: kind=%s state=%s target=%s", a.Kind, a.State, a.Target)
		}
	}
}

// TestDeliverableStore_Postgres_TransitionReviewState pins the review CAS: exactly
// one of two same-from transitions wins; the loser (and a wrong-kind row)
// reports false without touching anything; the stamp trio applies
// preserve-on-empty in the same write.
func TestDeliverableStore_Postgres_TransitionReviewState(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	rev, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindReview,
		State: domain.DeliverableStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-cas", DetailsJSON: `{"number":7}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	pr, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindPullRequest,
		State: domain.DeliverableStateReviewPending, Target: "octo/repo#7", DedupKey: "pr-cas",
	})
	if err != nil {
		t.Fatalf("seed pr: %v", err)
	}

	won, err := stores.Deliverables.TransitionReviewState(ctx, orgID, rev.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}
	won, err = stores.Deliverables.TransitionReviewState(ctx, orgID, rev.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("second claim = (%v, %v), want (false, nil) — the CAS must admit one winner", won, err)
	}
	got, _ := stores.Deliverables.Get(ctx, orgID, rev.ID)
	if got.State != domain.DeliverableStateReviewSubmitted || got.DetailsJSON != `{"number":7}` || got.URL != "" {
		t.Fatalf("post-claim row = state %q details %q url %q, want submitted/original details/empty url", got.State, got.DetailsJSON, got.URL)
	}

	won, err = stores.Deliverables.TransitionReviewState(ctx, orgID, rev.ID,
		domain.DeliverableStateReviewSubmitted, domain.DeliverableStateReviewSubmitted,
		"12345", "https://github.com/octo/repo/pull/7#pullrequestreview-12345", `{"number":7,"review_event":"COMMENT"}`)
	if err != nil || !won {
		t.Fatalf("stamp = (%v, %v), want (true, nil)", won, err)
	}
	got, _ = stores.Deliverables.Get(ctx, orgID, rev.ID)
	if got.ExternalID != "12345" || got.URL != "https://github.com/octo/repo/pull/7#pullrequestreview-12345" ||
		got.DetailsJSON != `{"number":7,"review_event":"COMMENT"}` {
		t.Fatalf("post-stamp row = %+v, want the posted review's coordinates", got)
	}

	won, err = stores.Deliverables.TransitionReviewState(ctx, orgID, pr.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("pr transition = (%v, %v), want (false, nil) — CAS is review-only", won, err)
	}
}

// TestDeliverableStore_Postgres_UpdateReviewDetailsIfPending pins the guarded
// draft write: it lands only while the review is still pending and refuses —
// leaving the row untouched — once the review resolved.
func TestDeliverableStore_Postgres_UpdateReviewDetailsIfPending(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgDeliverableConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	rev, err := stores.Deliverables.Upsert(ctx, orgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: orgID, TeamID: teamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindReview,
		State: domain.DeliverableStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-details", DetailsJSON: `{"v":1}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}

	updated, err := stores.Deliverables.UpdateReviewDetailsIfPendingSystem(ctx, orgID, rev.ID, `{"v":2}`)
	if err != nil || !updated {
		t.Fatalf("pending update = (%v, %v), want (true, nil)", updated, err)
	}
	got, _ := stores.Deliverables.Get(ctx, orgID, rev.ID)
	if got.DetailsJSON != `{"v":2}` {
		t.Fatalf("details = %q, want {\"v\":2}", got.DetailsJSON)
	}

	if won, err := stores.Deliverables.TransitionReviewState(ctx, orgID, rev.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", ""); err != nil || !won {
		t.Fatalf("resolve: (%v, %v)", won, err)
	}
	updated, err = stores.Deliverables.UpdateReviewDetailsIfPendingSystem(ctx, orgID, rev.ID, `{"v":3}`)
	if err != nil || updated {
		t.Fatalf("post-resolve update = (%v, %v), want (false, nil)", updated, err)
	}
	got, _ = stores.Deliverables.Get(ctx, orgID, rev.ID)
	if got.DetailsJSON != `{"v":2}` || got.State != domain.DeliverableStateReviewSubmitted {
		t.Fatalf("row after refused write = state %q details %q, want submitted/{\"v\":2} untouched", got.State, got.DetailsJSON)
	}
}

// seedPgDeliverableConversation mints a minimal conversation the
// deliverables.conversation_id FK can point at. origin is non-'blueprint' so
// conversations_origin_requires_parents doesn't demand a parent chain;
// trigger_type='manual' needs a non-NULL creator.
func seedPgDeliverableConversation(t *testing.T, h *pgtest.Harness, orgID, teamID, userID string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'running', 'team')
	`, id, orgID, teamID, userID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return id
}
