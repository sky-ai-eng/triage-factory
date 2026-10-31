package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestDeliverableStore_SQLite_RoundTrip pins that Upsert inserts a row whose
// every field reads back intact, that an empty id is generated, and that
// the empty optional fields land as SQL NULL. TFAC-455.
func TestDeliverableStore_SQLite_RoundTrip(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	in := domain.Deliverable{
		ConversationID: conversationID,
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
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
	out, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, in)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if out.ID == "" {
		t.Error("expected a generated id, got empty string")
	}
	if out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() {
		t.Error("expected created_at/updated_at populated")
	}
	if out.ConversationID != conversationID || out.Provider != "github" || out.Kind != "pull_request" ||
		out.Target != "octo/repo#123" || out.ExternalID != "123" ||
		out.URL != in.URL || out.State != "open" || out.DedupKey != in.DedupKey ||
		out.DetailsJSON != `{"draft":false}` {
		t.Errorf("round-trip mismatch: %+v", out)
	}

	// Empty optionals → SQL NULL.
	if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		OrgID:    runmode.LocalDefaultOrgID,
		TeamID:   runmode.LocalDefaultTeamID,
		Provider: domain.ProviderGit,
		Scope:    "https://github.com",
		Kind:     domain.DeliverableKindBranch,
		Target:   "octo/repo",
		State:    domain.DeliverableStateBranchPushed,
		DedupKey: "git:branch:octo/repo:refs/heads/x",
	}); err != nil {
		t.Fatalf("Upsert branch: %v", err)
	}
	var extID, url, details sql.NullString
	var nullConversation sql.NullString
	if err := conn.QueryRow(
		`SELECT conversation_id, external_id, url, details_json FROM deliverables WHERE dedup_key = 'git:branch:octo/repo:refs/heads/x'`,
	).Scan(&nullConversation, &extID, &url, &details); err != nil {
		t.Fatalf("read back branch: %v", err)
	}
	if nullConversation.Valid || extID.Valid || url.Valid || details.Valid {
		t.Errorf("empty optionals should be NULL: conversation=%v ext=%v url=%v details=%v", nullConversation, extID, url, details)
	}
}

// TestDeliverableStore_SQLite_UpsertDedup pins that two upserts with the same
// dedup_key collapse to one row, and the second updates the mutable
// fields. TFAC-455.
func TestDeliverableStore_SQLite_UpsertDedup(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	key := domain.ObjectKey("github", "pull_request", "octo/repo", "refs/heads/feat")
	first, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
		State: domain.DeliverableStatePRDraft, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	second, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#7",
		ExternalID: "7", URL: "https://github.com/octo/repo/pull/7",
		State: domain.DeliverableStatePROpen, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("dedup failed: distinct ids %s vs %s", first.ID, second.ID)
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM deliverables WHERE dedup_key = ?`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row for dedup_key, got %d", count)
	}
	if second.State != "open" || second.ExternalID != "7" || second.Target != "octo/repo#7" {
		t.Errorf("second upsert did not update mutable fields: %+v", second)
	}
	if !second.UpdatedAt.Equal(second.CreatedAt) && second.UpdatedAt.Before(second.CreatedAt) {
		t.Errorf("updated_at %v should be >= created_at %v", second.UpdatedAt, second.CreatedAt)
	}
}

// TestDeliverableStore_SQLite_InsertIfAbsent pins the reconciler-backstop write:
// the first insert lands (inserted=true); a second on the same dedup_key is a
// no-op (inserted=false) that NEVER overwrites the existing row's state — so a
// backstop pass can't regress a PR the observation path already recorded as, say,
// merged, back to open.
func TestDeliverableStore_SQLite_InsertIfAbsent(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	key := domain.ObjectKey("github", "pull_request", "octo/repo#7", "")
	base := domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo#7", ExternalID: "7",
		DedupKey: key,
	}

	first := base
	first.State = domain.DeliverableStatePROpen
	inserted, err := stores.Deliverables.InsertDeliverableIfAbsentSystem(ctx, runmode.LocalDefaultOrgID, first)
	if err != nil {
		t.Fatalf("first InsertDeliverableIfAbsentSystem: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported not-inserted; want true")
	}

	// Advance the row out-of-band the way the observation path would.
	if _, err := conn.Exec(`UPDATE deliverables SET state = ? WHERE dedup_key = ?`, domain.DeliverableStatePRMerged, key); err != nil {
		t.Fatalf("advance state: %v", err)
	}

	second := base
	second.State = domain.DeliverableStatePROpen // a stale backstop view
	inserted, err = stores.Deliverables.InsertDeliverableIfAbsentSystem(ctx, runmode.LocalDefaultOrgID, second)
	if err != nil {
		t.Fatalf("second InsertDeliverableIfAbsentSystem: %v", err)
	}
	if inserted {
		t.Error("second insert reported inserted; want false (row already present)")
	}

	var count int
	var state string
	if err := conn.QueryRow(`SELECT COUNT(*), MAX(state) FROM deliverables WHERE dedup_key = ?`, key).Scan(&count, &state); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want exactly 1 row, got %d", count)
	}
	if state != domain.DeliverableStatePRMerged {
		t.Errorf("insert-if-absent clobbered state to %q; want merged preserved", state)
	}
}

// TestDeliverableStore_SQLite_PendingToReal pins the pending→real PR
// transition: a pending PR keyed on the branch ref, then the real PR keyed
// on the same ref, collapse to one row that flips pending→open with
// url/external_id filled. TFAC-455.
func TestDeliverableStore_SQLite_PendingToReal(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	// The branch ref is the stable anchor across the transition.
	key := domain.ObjectKey("github", "pull_request", "octo/repo", "refs/heads/feat")

	pending, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "pull_request", Target: "octo/repo",
		State: domain.DeliverableStatePRPending, DedupKey: key,
		// No external_id / url yet — the PR doesn't exist.
	})
	if err != nil {
		t.Fatalf("pending Upsert: %v", err)
	}
	if pending.State != "pending" || pending.ExternalID != "" || pending.URL != "" {
		t.Fatalf("pending row malformed: %+v", pending)
	}

	real, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
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
	if err := conn.QueryRow(`SELECT COUNT(*) FROM deliverables`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 row after pending→real, got %d", count)
	}
}

// TestDeliverableStore_SQLite_UpsertPreservesExternalIDAndURL pins that an
// upsert leaving external_id/url empty does NOT blank values an earlier upsert
// stored — they only ever fill in (PR number / issue key, html link). A
// reconciliation pass, or a Jira mutation whose conversation can't compute the
// browse URL, must not erase them. State/target still follow the latest writer.
func TestDeliverableStore_SQLite_UpsertPreservesExternalIDAndURL(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	key := domain.ObjectKey("jira", "issue", "SKY-1", "")
	if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "jira", Scope: "https://acme.atlassian.net", Kind: "issue", Target: "SKY-1",
		ExternalID: "SKY-1", URL: "https://jira.example.com/browse/SKY-1",
		State: domain.DeliverableStateIssueCreated, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A later mutation that can't supply external_id/url (both empty).
	out, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "jira", Scope: "https://acme.atlassian.net", Kind: "issue", Target: "SKY-1",
		State: domain.DeliverableStateIssueUpdated, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if out.ExternalID != "SKY-1" || out.URL != "https://jira.example.com/browse/SKY-1" {
		t.Errorf("external_id/url were blanked by an empty upsert: ext=%q url=%q", out.ExternalID, out.URL)
	}
	if out.State != domain.DeliverableStateIssueUpdated {
		t.Errorf("state should still follow the latest writer, got %q", out.State)
	}

	// A non-empty value still overwrites (intentional change path).
	out, err = stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "jira", Scope: "https://acme.atlassian.net", Kind: "issue", Target: "SKY-1",
		ExternalID: "SKY-1", URL: "https://jira.example.com/browse/SKY-1?focusedId=9",
		State: domain.DeliverableStateIssueUpdated, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if out.URL != "https://jira.example.com/browse/SKY-1?focusedId=9" {
		t.Errorf("non-empty url did not overwrite: %q", out.URL)
	}
}

// TestDeliverableStore_SQLite_UpsertPreservesTargetOnEmpty pins that an upsert
// leaving target empty does NOT blank the resource key an earlier upsert
// stored — a GitHub comment-update/delete knows only the comment id, not the
// PR number, so it can't recompute owner/repo#N and must inherit it. A
// non-empty target still migrates (the pending→real PR path).
func TestDeliverableStore_SQLite_UpsertPreservesTargetOnEmpty(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	key := domain.ObjectKey("github", "comment", "555", "")
	if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "comment", Target: "octo/repo#7",
		ExternalID: "555", URL: "https://github.com/octo/repo/pull/7#issuecomment-555",
		State: domain.DeliverableStateCommentPosted, DedupKey: key,
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// A comment-delete that only carries the id (empty target/url).
	out, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "comment", ExternalID: "555",
		State: domain.DeliverableStateCommentDeleted, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if out.Target != "octo/repo#7" || out.URL != "https://github.com/octo/repo/pull/7#issuecomment-555" {
		t.Errorf("empty upsert blanked stable coordinates: target=%q url=%q", out.Target, out.URL)
	}
	if out.State != domain.DeliverableStateCommentDeleted {
		t.Errorf("state should follow the latest writer, got %q", out.State)
	}

	// A non-empty target still overwrites (migration path).
	out, err = stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "github", Scope: "https://github.com", Kind: "comment", Target: "octo/repo#8",
		State: domain.DeliverableStateCommentPosted, DedupKey: key,
	})
	if err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if out.Target != "octo/repo#8" {
		t.Errorf("non-empty target did not overwrite: %q", out.Target)
	}
}

// TestDeliverableStore_SQLite_ListByConversationAndTeam pins the two read paths
// and their newest-first ordering + the team Limit. TFAC-455.
func TestDeliverableStore_SQLite_ListByConversationAndTeam(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	for i, k := range []string{"a", "b", "c"} {
		if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Scope: "https://github.com", Kind: "comment", Target: "octo/repo",
			State: domain.DeliverableStateCommentPosted, DedupKey: "k" + k,
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	byConversation, _, err := stores.Deliverables.ListByConversation(ctx, runmode.LocalDefaultOrgID, conversationID, db.Unwindowed)
	if err != nil {
		t.Fatalf("ListByConversation: %v", err)
	}
	if len(byConversation) != 3 {
		t.Errorf("ListByConversation len = %d, want 3", len(byConversation))
	}

	byTeam, _, err := stores.Deliverables.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.DeliverableListOpts{})
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	if len(byTeam) != 3 {
		t.Errorf("ListByTeam len = %d, want 3", len(byTeam))
	}

	limited, _, err := stores.Deliverables.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.DeliverableListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("ListByTeam limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("ListByTeam(Limit:2) len = %d, want 2", len(limited))
	}
}

// TestDeliverableStore_SQLite_CountByConversation pins the batched
// per-conversation count the conversation-list path uses for deliverable_count
// (TFAC-465): counts keyed by conversation id, a conversation with no deliverables
// absent from the map (not 0-valued), unknown ids absent, and
// an empty conversationIDs a no-op returning an empty map.
func TestDeliverableStore_SQLite_CountByConversation(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	convA := seedDeliverableConversation(t, conn)
	convB := seedDeliverableConversationWithID(t, conn, "88888888-8888-8888-8888-888888888888")
	convC := seedDeliverableConversationWithID(t, conn, "77777777-7777-7777-7777-777777777777") // no deliverables

	seed := func(conversationID, key string) {
		if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Scope: "https://github.com", Kind: "comment", Target: "octo/repo",
			State: domain.DeliverableStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(convA, "a1")
	seed(convA, "a2")
	seed(convA, "a3")
	seed(convB, "b1")

	counts, err := stores.Deliverables.CountByConversation(ctx, runmode.LocalDefaultOrgID, []string{convA, convB, convC, "nonexistent"})
	if err != nil {
		t.Fatalf("CountByConversation: %v", err)
	}
	if counts[convA] != 3 {
		t.Errorf("convA count = %d, want 3", counts[convA])
	}
	if counts[convB] != 1 {
		t.Errorf("convB count = %d, want 1", counts[convB])
	}
	if _, ok := counts[convC]; ok {
		t.Errorf("convC (no deliverables) should be absent from the map, got %d", counts[convC])
	}
	if _, ok := counts["nonexistent"]; ok {
		t.Error("unknown conversation id should be absent from the map")
	}

	// Empty conversationIDs is a no-op — empty, non-nil map, no query.
	empty, err := stores.Deliverables.CountByConversation(ctx, runmode.LocalDefaultOrgID, nil)
	if err != nil {
		t.Fatalf("CountByConversation(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("CountByConversation(nil) = %v, want empty non-nil map", empty)
	}
}

// TestDeliverableStore_SQLite_ListByConversations pins the batched
// multi-conversation read the conversation-list path uses for pending_kind
// (TFAC-465): deliverables for the given conversations come back in one slice,
// each carrying its ConversationID for grouping; a conversation with no
// deliverables contributes nothing and an empty conversationIDs is a no-op.
func TestDeliverableStore_SQLite_ListByConversations(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	convA := seedDeliverableConversation(t, conn)
	convB := seedDeliverableConversationWithID(t, conn, "88888888-8888-8888-8888-888888888888")
	convC := seedDeliverableConversationWithID(t, conn, "77777777-7777-7777-7777-777777777777") // no deliverables

	seed := func(conversationID, key string) {
		if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Scope: "https://github.com", Kind: "comment", Target: "octo/repo",
			State: domain.DeliverableStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(convA, "a1")
	seed(convA, "a2")
	seed(convB, "b1")

	arts, err := stores.Deliverables.ListByConversations(ctx, runmode.LocalDefaultOrgID, []string{convA, convB, convC})
	if err != nil {
		t.Fatalf("ListByConversations: %v", err)
	}
	byConversation := map[string]int{}
	for _, a := range arts {
		if a.ConversationID == "" {
			t.Errorf("deliverable %s came back without its ConversationID (can't group)", a.ID)
		}
		byConversation[a.ConversationID]++
	}
	if byConversation[convA] != 2 || byConversation[convB] != 1 {
		t.Errorf("grouped counts = %v, want convA=2 convB=1", byConversation)
	}
	if byConversation[convC] != 0 {
		t.Errorf("convC (no deliverables) contributed %d rows, want 0", byConversation[convC])
	}

	empty, err := stores.Deliverables.ListByConversations(ctx, runmode.LocalDefaultOrgID, nil)
	if err != nil {
		t.Fatalf("ListByConversations(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByConversations(nil) = %v, want empty", empty)
	}
}

// TestDeliverableStore_SQLite_ListByTeam_IncludesDetached pins the
// audit-ledger invariant: a deliverable whose conversation was purged
// (conversation_id NULL) is still the team's and must come back from
// ListByTeam. Guards against a future `AND conversation_id IS NOT NULL`
// creeping into the query. TFAC-455.
func TestDeliverableStore_SQLite_ListByTeam_IncludesDetached(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	art, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
		Provider: "git", Scope: "https://github.com", Kind: "branch", Target: "octo/repo",
		State: domain.DeliverableStateBranchPushed, DedupKey: "git:branch:octo/repo:refs/heads/x",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate a conversation purge: the FK is ON DELETE SET NULL, so deleting
	// the conversation detaches the deliverable rather than cascading it away.
	if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, conversationID); err != nil {
		t.Fatalf("purge conversation: %v", err)
	}
	var nullConversation sql.NullString
	if err := conn.QueryRow(`SELECT conversation_id FROM deliverables WHERE id = ?`, art.ID).Scan(&nullConversation); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if nullConversation.Valid {
		t.Fatalf("conversation_id should be NULL after conversation purge, got %q", nullConversation.String)
	}

	rows, _, err := stores.Deliverables.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.DeliverableListOpts{})
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != art.ID {
		t.Errorf("ListByTeam dropped the detached deliverable: %+v", rows)
	}
	if rows[0].ConversationID != "" {
		t.Errorf("detached row ConversationID = %q, want empty", rows[0].ConversationID)
	}
}

// TestDeliverableStore_SQLite_DropsRunDeliverables pins that the forward
// migration retired the dead run_deliverables table and created deliverables.
func TestDeliverableStore_SQLite_DropsRunDeliverables(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'run_deliverables'`,
	).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 0 {
		t.Errorf("run_deliverables still present after migration; want dropped")
	}
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'deliverables'`,
	).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 1 {
		t.Errorf("deliverables table missing after migration")
	}
}

// TestDeliverableStore_SQLite_ListNonTerminal pins the reconciler's working set
// (TFAC-464): seed one deliverable in every state across kinds, then assert
// ListNonTerminalBySystem returns EXACTLY the rows domain.IsReconcilableNonTerminal
// marks — PR draft|open, review pending, branch pushed — and nothing terminal
// (merged/closed/submitted/dismissed/deleted) or unreconcilable (PR pending,
// comment, issue). Doubles as the SQL-predicate ⇔ domain-predicate drift guard.
func TestDeliverableStore_SQLite_ListNonTerminal(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	// (kind, state) across the whole lifecycle space.
	seeds := []struct{ kind, state string }{
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRPending}, // unreconcilable: no node id yet
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRDraft},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePROpen},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRMerged},
		{domain.DeliverableKindPullRequest, domain.DeliverableStatePRClosed},
		{domain.DeliverableKindReview, domain.DeliverableStateReviewPending},
		{domain.DeliverableKindReview, domain.DeliverableStateReviewSubmitted},
		{domain.DeliverableKindReview, domain.DeliverableStateReviewDismissed},
		{domain.DeliverableKindBranch, domain.DeliverableStateBranchPushed},
		{domain.DeliverableKindBranch, domain.DeliverableStateBranchDeleted},
		{domain.DeliverableKindComment, domain.DeliverableStateCommentPosted},
		{domain.DeliverableKindIssue, domain.DeliverableStateIssueCreated},
	}
	want := map[string]bool{}
	for i, s := range seeds {
		key := "k" + string(rune('a'+i)) // unique dedup_key per seed
		if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: "github", Scope: "https://github.com", Kind: s.kind, Target: "octo/repo", State: s.state, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.state, err)
		}
		if domain.IsReconcilableNonTerminal(s.kind, s.state) {
			want[key] = true
		}
	}

	got, err := stores.Deliverables.ListNonTerminalBySystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	gotKeys := map[string]bool{}
	for _, a := range got {
		gotKeys[a.DedupKey] = true
	}
	if len(gotKeys) != len(want) {
		t.Errorf("returned %d rows, want %d: got=%v want=%v", len(gotKeys), len(want), gotKeys, want)
	}
	for k := range want {
		if !gotKeys[k] {
			t.Errorf("ListNonTerminalBySystem dropped reconcilable row %q", k)
		}
	}
	for k := range gotKeys {
		if !want[k] {
			t.Errorf("ListNonTerminalBySystem returned terminal/unreconcilable row %q (SQL predicate disagrees with domain.IsReconcilableNonTerminal)", k)
		}
	}
}

// TestDeliverableStore_SQLite_ListByOrgSystem pins the bot-activity audit feed's
// org source (TFAC-483): it returns ALL states — terminal included (merged PR,
// dismissed review) — in contrast to ListNonTerminalBySystem's reconciler
// working set, newest-first, and honors the provider/kind/state/time filters and
// the limit/offset paging from DeliverableListOpts. A _time_format=sqlite conn so
// the bound Since/Until time.Time compares against created_at correctly.
func TestDeliverableStore_SQLite_ListByOrgSystem(t *testing.T) {
	conn := newSQLiteForDeliverableTestTimed(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	// (provider, kind, state, created_at) across the lifecycle, including
	// terminal rows the reconciler set would hide.
	seeds := []struct {
		provider, kind, state, when string
	}{
		{domain.ProviderGit, domain.DeliverableKindBranch, domain.DeliverableStateBranchPushed, "2026-06-01 00:00:00"},
		{domain.ProviderGitHub, domain.DeliverableKindPullRequest, domain.DeliverableStatePROpen, "2026-06-02 00:00:00"},
		{domain.ProviderGitHub, domain.DeliverableKindPullRequest, domain.DeliverableStatePRMerged, "2026-06-03 00:00:00"},   // terminal
		{domain.ProviderGitHub, domain.DeliverableKindReview, domain.DeliverableStateReviewDismissed, "2026-06-04 00:00:00"}, // terminal
		{domain.ProviderJira, domain.DeliverableKindComment, domain.DeliverableStateCommentPosted, "2026-06-05 00:00:00"},
	}
	for i, s := range seeds {
		out, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: s.provider, Scope: "https://github.com", Kind: s.kind, Target: "octo/repo", State: s.state, DedupKey: "k" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatalf("seed %s/%s: %v", s.kind, s.state, err)
		}
		// Pin created_at so the newest-first order + the time window are deterministic.
		if _, err := conn.Exec(`UPDATE deliverables SET created_at = ? WHERE id = ?`, s.when, out.ID); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
	}

	// No opts: every row, terminal included, newest-first.
	all, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("ListByOrgSystem returned %d rows, want all 5 (terminal included)", len(all))
	}
	states := map[string]bool{}
	for _, a := range all {
		states[a.State] = true
	}
	if !states[domain.DeliverableStatePRMerged] || !states[domain.DeliverableStateReviewDismissed] {
		t.Errorf("feed dropped terminal rows: states=%v (want merged + dismissed present)", states)
	}
	// Contrast the reconciler set: it hides exactly those terminal rows.
	nonTerminal, err := stores.Deliverables.ListNonTerminalBySystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListNonTerminalBySystem: %v", err)
	}
	if len(nonTerminal) != 2 {
		t.Errorf("ListNonTerminalBySystem returned %d, want 2 (branch pushed + PR open) — the contrast", len(nonTerminal))
	}
	// Newest-first: comment (06-05) leads, branch (06-01) trails.
	if all[0].State != domain.DeliverableStateCommentPosted || all[4].State != domain.DeliverableStateBranchPushed {
		t.Errorf("order not newest-first: first=%q last=%q", all[0].State, all[4].State)
	}

	// Filters.
	gh, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{Provider: domain.ProviderGitHub})
	if err != nil {
		t.Fatalf("filter provider: %v", err)
	}
	if len(gh) != 3 {
		t.Errorf("provider=github returned %d, want 3 (PR open + PR merged + review)", len(gh))
	}
	prs, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{Kind: domain.DeliverableKindPullRequest})
	if err != nil {
		t.Fatalf("filter kind: %v", err)
	}
	if len(prs) != 2 {
		t.Errorf("kind=pull_request returned %d, want 2 (open + merged)", len(prs))
	}
	merged, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{State: domain.DeliverableStatePRMerged})
	if err != nil {
		t.Fatalf("filter state: %v", err)
	}
	if len(merged) != 1 || merged[0].State != domain.DeliverableStatePRMerged {
		t.Errorf("state=merged returned %+v, want exactly the merged PR", merged)
	}

	// Time window [06-03, 06-05): merged (06-03) + review (06-04), excluding the
	// open PR before it and the comment at the exclusive upper bound.
	windowed, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{
		Since: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("filter window: %v", err)
	}
	if len(windowed) != 2 {
		t.Fatalf("time window returned %d, want 2 (merged + review)", len(windowed))
	}
	if windowed[0].State != domain.DeliverableStateReviewDismissed || windowed[1].State != domain.DeliverableStatePRMerged {
		t.Errorf("windowed order = [%q, %q], want [dismissed, merged] (newest-first)", windowed[0].State, windowed[1].State)
	}

	// Paging: page 1 (limit 2) → comment + review; page 2 (offset 2) → merged + open.
	page1, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page2, _, err := stores.Deliverables.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, db.DeliverableListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("paging sizes = %d, %d, want 2, 2", len(page1), len(page2))
	}
	if page1[0].State != domain.DeliverableStateCommentPosted || page2[0].State != domain.DeliverableStatePRMerged {
		t.Errorf("paging boundary wrong: page1[0]=%q page2[0]=%q", page1[0].State, page2[0].State)
	}
}

// TestDeliverableStore_SQLite_ListByTeam_Filters pins that the shared filter/paging
// helper applies to the team-scoped path too (not just ListByOrgSystem): a
// provider filter and an offset both narrow ListByTeam's result. TFAC-483.
func TestDeliverableStore_SQLite_ListByTeam_Filters(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	seed := func(provider, kind, key string) {
		if _, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
			ConversationID: conversationID, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: provider, Scope: "https://github.com", Kind: kind, Target: "octo/repo",
			State: domain.DeliverableStateCommentPosted, DedupKey: key,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(domain.ProviderGitHub, domain.DeliverableKindComment, "g1")
	seed(domain.ProviderGitHub, domain.DeliverableKindComment, "g2")
	seed(domain.ProviderJira, domain.DeliverableKindComment, "j1")

	gh, _, err := stores.Deliverables.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.DeliverableListOpts{Provider: domain.ProviderGitHub})
	if err != nil {
		t.Fatalf("ListByTeam provider filter: %v", err)
	}
	if len(gh) != 2 {
		t.Errorf("provider=github returned %d, want 2", len(gh))
	}

	// limit 2 then offset 2 walks the 3-row team feed without overlap.
	offset, _, err := stores.Deliverables.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, db.DeliverableListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListByTeam offset: %v", err)
	}
	if len(offset) != 1 {
		t.Errorf("limit 2 offset 2 over 3 rows returned %d, want 1", len(offset))
	}
}

// newSQLiteForDeliverableTestTimed is newSQLiteForDeliverableTest with
// _time_format=sqlite, so a bound time.Time (the Since/Until filters) serializes
// to a datetime()-parseable form matching CURRENT_TIMESTAMP — the same reason
// the spend store's SQLite test uses it. Used by the time-window filter test.
func newSQLiteForDeliverableTestTimed(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// TestDeliverableStore_SQLite_ListPendingReviewsByEntity pins the new-commits
// notifier's lookup (TFAC-501): only PENDING review deliverables about the PR's
// entity come back, whatever name their target spells; resolved reviews,
// other PRs' drafts and non-review kinds are excluded.
func TestDeliverableStore_SQLite_ListPendingReviewsByEntity(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)
	org := runmode.LocalDefaultOrgID

	entity := func(key, nodeID string) string {
		t.Helper()
		e, _, err := stores.Entities.FindOrCreateSystem(ctx, org, domain.ProviderGitHub, "https://github.com", key, nodeID, "pr", "t", "")
		if err != nil {
			t.Fatalf("seed entity %s: %v", key, err)
		}
		return e.ID
	}
	pr7, other7 := entity("octo/repo#7", "PR_kw7"), entity("octo/other#7", "PR_kwOTHER7")
	mk := func(kind, state, target, dedup, entityID string) {
		if _, err := stores.Deliverables.Upsert(ctx, org, domain.Deliverable{
			ConversationID: conversationID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
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
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewDismissed, "octo/repo#7", domain.ReviewDedupKey("PR_kw7", "conv-d"), pr7)
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewPending, "octo/repo#7", domain.ReviewDedupKey("PR_kwOTHER7", "conv-a"), other7)
	mk(domain.DeliverableKindReview, domain.DeliverableStateReviewPending, "octo/repo#77", domain.ReviewDedupKey("PR_kw77", "conv-a"), "")
	mk(domain.DeliverableKindPullRequest, domain.DeliverableStatePROpen, "octo/repo#7", domain.PullRequestDedupKey("PR_kw7"), pr7)

	got, err := stores.Deliverables.ListPendingReviewsByEntitySystem(ctx, org, pr7)
	if err != nil {
		t.Fatalf("ListPendingReviewsByEntitySystem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pending reviews of PR_kw7, got %d: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Kind != domain.DeliverableKindReview || a.State != domain.DeliverableStateReviewPending || a.EntityID != pr7 {
			t.Errorf("unexpected deliverable in result: kind=%s state=%s target=%s", a.Kind, a.State, a.Target)
		}
	}
}

// TestDeliverableStore_SQLite_TransitionReviewState pins the review CAS: exactly
// one of two same-from transitions wins; the loser (and a wrong-from or
// wrong-kind row) reports false without touching anything; the stamp trio
// (external_id/url/details_json) applies preserve-on-empty in the same write.
func TestDeliverableStore_SQLite_TransitionReviewState(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)
	org := runmode.LocalDefaultOrgID

	rev, err := stores.Deliverables.Upsert(ctx, org, domain.Deliverable{
		ConversationID: conversationID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindReview,
		State: domain.DeliverableStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-cas", DetailsJSON: `{"number":7}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	pr, err := stores.Deliverables.Upsert(ctx, org, domain.Deliverable{
		ConversationID: conversationID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindPullRequest,
		State: domain.DeliverableStateReviewPending, Target: "octo/repo#7", DedupKey: "pr-cas",
	})
	if err != nil {
		t.Fatalf("seed pr: %v", err)
	}

	// The claim: pending → submitted, no stamp yet.
	won, err := stores.Deliverables.TransitionReviewState(ctx, org, rev.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || !won {
		t.Fatalf("claim = (%v, %v), want (true, nil)", won, err)
	}
	// The losing racer: same from-state, row already moved.
	won, err = stores.Deliverables.TransitionReviewState(ctx, org, rev.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("second claim = (%v, %v), want (false, nil) — the CAS must admit one winner", won, err)
	}
	// The claim alone must not have blanked details (preserve-on-empty).
	got, _ := stores.Deliverables.Get(ctx, org, rev.ID)
	if got.State != domain.DeliverableStateReviewSubmitted || got.DetailsJSON != `{"number":7}` || got.URL != "" {
		t.Fatalf("post-claim row = state %q details %q url %q, want submitted/original details/empty url", got.State, got.DetailsJSON, got.URL)
	}

	// The stamp: submitted → submitted carrying the posted review's coordinates.
	won, err = stores.Deliverables.TransitionReviewState(ctx, org, rev.ID,
		domain.DeliverableStateReviewSubmitted, domain.DeliverableStateReviewSubmitted,
		"12345", "https://github.com/octo/repo/pull/7#pullrequestreview-12345", `{"number":7,"review_event":"COMMENT"}`)
	if err != nil || !won {
		t.Fatalf("stamp = (%v, %v), want (true, nil)", won, err)
	}
	got, _ = stores.Deliverables.Get(ctx, org, rev.ID)
	if got.ExternalID != "12345" || got.URL != "https://github.com/octo/repo/pull/7#pullrequestreview-12345" ||
		got.DetailsJSON != `{"number":7,"review_event":"COMMENT"}` {
		t.Fatalf("post-stamp row = %+v, want the posted review's coordinates", got)
	}

	// Kind guard: a non-review row never transitions, even from a matching state.
	won, err = stores.Deliverables.TransitionReviewState(ctx, org, pr.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("pr transition = (%v, %v), want (false, nil) — CAS is review-only", won, err)
	}
	// Missing row.
	won, err = stores.Deliverables.TransitionReviewState(ctx, org, "00000000-0000-0000-0000-000000000000",
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || won {
		t.Fatalf("missing-row transition = (%v, %v), want (false, nil)", won, err)
	}
}

// TestDeliverableStore_SQLite_UpdateReviewDetailsIfPending pins the guarded draft
// write: it lands only while the review is still pending, and reports false —
// leaving the row untouched — once the review resolved, so a stale writer can't
// resurrect a submitted review's draft state.
func TestDeliverableStore_SQLite_UpdateReviewDetailsIfPending(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)
	org := runmode.LocalDefaultOrgID

	rev, err := stores.Deliverables.Upsert(ctx, org, domain.Deliverable{
		ConversationID: conversationID, OrgID: org, TeamID: runmode.LocalDefaultTeamID,
		Provider: domain.ProviderGitHub, Scope: "https://github.com", Kind: domain.DeliverableKindReview,
		State: domain.DeliverableStateReviewPending, Target: "octo/repo#7",
		DedupKey: "rev-details", DetailsJSON: `{"v":1}`,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}

	updated, err := stores.Deliverables.UpdateReviewDetailsIfPending(ctx, org, rev.ID, `{"v":2}`)
	if err != nil || !updated {
		t.Fatalf("pending update = (%v, %v), want (true, nil)", updated, err)
	}
	got, _ := stores.Deliverables.Get(ctx, org, rev.ID)
	if got.DetailsJSON != `{"v":2}` {
		t.Fatalf("details = %q, want {\"v\":2}", got.DetailsJSON)
	}

	// Resolve the review, then attempt the stale write.
	if won, err := stores.Deliverables.TransitionReviewState(ctx, org, rev.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", ""); err != nil || !won {
		t.Fatalf("resolve: (%v, %v)", won, err)
	}
	updated, err = stores.Deliverables.UpdateReviewDetailsIfPending(ctx, org, rev.ID, `{"v":3}`)
	if err != nil || updated {
		t.Fatalf("post-resolve update = (%v, %v), want (false, nil)", updated, err)
	}
	got, _ = stores.Deliverables.Get(ctx, org, rev.ID)
	if got.DetailsJSON != `{"v":2}` || got.State != domain.DeliverableStateReviewSubmitted {
		t.Fatalf("row after refused write = state %q details %q, want submitted/{\"v\":2} untouched", got.State, got.DetailsJSON)
	}

	// System variant is the same write in SQLite.
	if _, err := stores.Deliverables.UpdateReviewDetailsIfPendingSystem(ctx, org, rev.ID, `{"v":4}`); err != nil {
		t.Fatalf("system variant: %v", err)
	}
}

func newSQLiteForDeliverableTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// seedDeliverableConversation inserts a minimal conversation the deliverables FK
// (conversation_id → conversations(id)) can point at. origin is set non-'blueprint' so the
// conversations_origin_requires_parents CHECK doesn't demand a parent chain; the
// org/team/creator columns default to the local sentinels.
func seedDeliverableConversation(t *testing.T, conn *sql.DB) string {
	t.Helper()
	return seedDeliverableConversationWithID(t, conn, "99999999-9999-9999-9999-999999999999")
}

// seedDeliverableConversationWithID is seedDeliverableConversation parameterized on
// the conversation id, for tests that need more than one conversation
// (CountByConversation batches across conversations).
func seedDeliverableConversationWithID(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
	); err != nil {
		t.Fatalf("seed conversation %s: %v", id, err)
	}
	return id
}

// TestDeliverableStore_SQLite_TransitionReviewStateSystem pins the admin-pool CAS
// the auto-posting review posture claims with: exactly one caller moves the row
// out of pending, and the loser learns it lost rather than silently proceeding
// to a second SubmitReview. Same contract as the app-pool twin the approve
// handler uses — the two are what stop a conversation and a human from both
// publishing one drafted review.
func TestDeliverableStore_SQLite_TransitionReviewStateSystem(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	conversationID := seedDeliverableConversation(t, conn)

	draft, err := stores.Deliverables.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Deliverable{
		ConversationID: conversationID,
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
		Provider:       domain.ProviderGitHub,
		Scope:          "https://github.com",
		Kind:           domain.DeliverableKindReview,
		Target:         "octo/repo#7",
		State:          domain.DeliverableStateReviewPending,
		DedupKey:       domain.ReviewDedupKey("PR_kw7", conversationID),
		DetailsJSON:    `{"review_event":"COMMENT"}`,
	})
	if err != nil {
		t.Fatalf("seed review draft: %v", err)
	}

	won, err := stores.Deliverables.TransitionReviewStateSystem(ctx, runmode.LocalDefaultOrgID, draft.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil || !won {
		t.Fatalf("first claim = %v, %v; want it to win", won, err)
	}
	// The second caller — a human approve racing the conversation's auto-post,
	// or vice versa — must lose, having made no GitHub call on the strength of it.
	lost, err := stores.Deliverables.TransitionReviewStateSystem(ctx, runmode.LocalDefaultOrgID, draft.ID,
		domain.DeliverableStateReviewPending, domain.DeliverableStateReviewSubmitted, "", "", "")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if lost {
		t.Error("second claim won too — the CAS must admit exactly one publisher")
	}

	// The winner's stamp (submitted → submitted) carries the posted review's
	// coordinates onto the claimed row.
	stamped, err := stores.Deliverables.TransitionReviewStateSystem(ctx, runmode.LocalDefaultOrgID, draft.ID,
		domain.DeliverableStateReviewSubmitted, domain.DeliverableStateReviewSubmitted,
		"555", "https://github.com/octo/repo/pull/7#pullrequestreview-555", "")
	if err != nil || !stamped {
		t.Fatalf("stamp = %v, %v; want it to land", stamped, err)
	}
	got, err := stores.Deliverables.Get(ctx, runmode.LocalDefaultOrgID, draft.ID)
	if err != nil || got == nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.ExternalID != "555" || got.URL == "" || got.State != domain.DeliverableStateReviewSubmitted {
		t.Errorf("after stamp: %+v, want submitted with the review id + URL", got)
	}
	// details_json was passed empty — preserve-on-empty leaves it untouched.
	if got.DetailsJSON != `{"review_event":"COMMENT"}` {
		t.Errorf("details_json = %q, want the draft's details preserved", got.DetailsJSON)
	}

	// A release (submitted → pending) puts the draft back in the approval queue.
	released, err := stores.Deliverables.TransitionReviewStateSystem(ctx, runmode.LocalDefaultOrgID, draft.ID,
		domain.DeliverableStateReviewSubmitted, domain.DeliverableStateReviewPending, "", "", "")
	if err != nil || !released {
		t.Fatalf("release = %v, %v; want it to land", released, err)
	}
}
