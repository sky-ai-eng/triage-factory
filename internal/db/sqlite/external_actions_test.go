package sqlite_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestExternalActionStore_SQLite_RoundTrip pins that Record inserts a row whose
// every field reads back intact, that an empty id is generated, and that the
// empty optional columns land as SQL NULL. TFAC-483.
func TestExternalActionStore_SQLite_RoundTrip(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	in := domain.ExternalAction{
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
		Provider:       domain.ArtifactProviderGitHub,
		Action:         domain.ActionPRMarkedReady,
		Target:         "octo/repo#123",
		ExternalID:     "123",
		URL:            "https://github.com/octo/repo/pull/123",
		FromState:      domain.ArtifactStatePRDraft,
		ToState:        domain.ArtifactStatePROpen,
		ConversationID: runID,
		ActorUserID:    runmode.LocalDefaultUserID,
		Credential:     domain.CredentialGitHubApp,
		DedupKey:       "pr_marked_ready:octo/repo#123:once",
		DetailJSON:     `{"k":"v"}`,
	}
	if err := stores.ExternalActions.Record(ctx, runmode.LocalDefaultOrgID, in); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	a := got[0]
	if a.ID == "" || a.OccurredAt.IsZero() {
		t.Errorf("expected generated id + occurred_at, got id=%q occurred=%v", a.ID, a.OccurredAt)
	}
	if a.Provider != "github" || a.Action != domain.ActionPRMarkedReady || a.Target != "octo/repo#123" ||
		a.ExternalID != "123" || a.URL != in.URL || a.FromState != "draft" || a.ToState != "open" ||
		a.ConversationID != runID || a.ActorUserID != runmode.LocalDefaultUserID || a.Credential != "github_app" ||
		a.DedupKey != in.DedupKey || a.DetailJSON != `{"k":"v"}` {
		t.Errorf("round-trip mismatch: %+v", a)
	}

	// A system action: empty optionals (team/actor/from/to/external/url/detail) → NULL.
	if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalAction{
		OrgID:      runmode.LocalDefaultOrgID,
		Provider:   domain.ArtifactProviderJira,
		Action:     domain.ActionIssueTransitioned,
		Target:     "SKY-1",
		Credential: domain.CredentialJiraOrg,
		DedupKey:   "issue_transitioned:SKY-1:once",
	}); err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}
	var team, actor, from, to, ext, url, detail sql.NullString
	if err := conn.QueryRow(
		`SELECT team_id, actor_user_id, from_state, to_state, external_id, url, detail_json
		   FROM external_actions WHERE dedup_key = 'issue_transitioned:SKY-1:once'`,
	).Scan(&team, &actor, &from, &to, &ext, &url, &detail); err != nil {
		t.Fatalf("read back system row: %v", err)
	}
	if team.Valid || actor.Valid || from.Valid || to.Valid || ext.Valid || url.Valid || detail.Valid {
		t.Errorf("empty optionals should be NULL: %v/%v/%v/%v/%v/%v/%v", team, actor, from, to, ext, url, detail)
	}
}

// TestExternalActionStore_SQLite_BranchTwinDedup pins the ONE dedup case: two
// Records with the SAME explicit dedup_key (the git hook+proxy branch twin)
// collapse to one row under ON CONFLICT DO NOTHING — first-write-wins, NOT a
// mutation. A genuinely new push (different sha → different key) is recorded
// distinctly. TFAC-483.
func TestExternalActionStore_SQLite_BranchTwinDedup(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	key := domain.BranchPushDedupKey(runID, "refs/heads/feat", "abc123")
	twin := func(url string) domain.ExternalAction {
		return domain.ExternalAction{
			OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: domain.ArtifactProviderGitHub, Action: domain.ActionBranchPushed,
			Target: "octo/repo", ExternalID: "refs/heads/feat", URL: url, ConversationID: runID,
			ToState: domain.ArtifactStateBranchPushed, Credential: domain.CredentialGitHubApp, DedupKey: key,
		}
	}
	// The hook fires first (the winner), then the proxy backstop with the same key.
	if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, twin("hook")); err != nil {
		t.Fatalf("hook Record: %v", err)
	}
	if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, twin("proxy")); err != nil {
		t.Fatalf("proxy Record (should DO NOTHING, not error): %v", err)
	}

	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM external_actions WHERE dedup_key = ?`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("branch twin did not collapse: %d rows for one dedup_key", count)
	}
	var url string
	if err := conn.QueryRow(`SELECT url FROM external_actions WHERE dedup_key = ?`, key).Scan(&url); err != nil {
		t.Fatalf("read winner: %v", err)
	}
	if url != "hook" {
		t.Errorf("DO NOTHING must keep the first writer, got url=%q (want hook)", url)
	}

	// A new push (different sha) is its own row.
	if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalAction{
		OrgID: runmode.LocalDefaultOrgID, Provider: domain.ArtifactProviderGitHub,
		Action: domain.ActionBranchPushed, Target: "octo/repo", Credential: domain.CredentialGitHubApp,
		DedupKey: domain.BranchPushDedupKey(runID, "refs/heads/feat", "def456"),
	}); err != nil {
		t.Fatalf("new-push Record: %v", err)
	}
	var total int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM external_actions`).Scan(&total); err != nil {
		t.Fatalf("total count: %v", err)
	}
	if total != 2 {
		t.Errorf("want 2 rows (one twin-collapsed + one new push), got %d", total)
	}
}

// TestExternalActionStore_SQLite_EmptyDedupAppendsEveryRow pins that an empty
// DedupKey is filled with a uuid so a REPEATED action is never collapsed: two
// Records with no dedup_key produce two distinct rows. This is what keeps the
// audit log from silently dropping a PR toggled draft↔ready or a comment edited
// twice. TFAC-483.
func TestExternalActionStore_SQLite_EmptyDedupAppendsEveryRow(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	repeat := domain.ExternalAction{
		OrgID: runmode.LocalDefaultOrgID, Provider: domain.ArtifactProviderGitHub,
		Action: domain.ActionPREdited, Target: "octo/repo#1", Credential: domain.CredentialGitHubApp,
		// DedupKey deliberately empty — the store fills a uuid per insert.
	}
	for i := 0; i < 2; i++ {
		if err := stores.ExternalActions.Record(ctx, runmode.LocalDefaultOrgID, repeat); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM external_actions`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("two empty-dedup records should both append, got %d rows", count)
	}
	// And the generated dedup_keys are distinct + non-empty (the NOT NULL column).
	rows, err := conn.Query(`SELECT dedup_key FROM external_actions`)
	if err != nil {
		t.Fatalf("read keys: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if k == "" || seen[k] {
			t.Errorf("generated dedup_key empty or duplicated: %q", k)
		}
		seen[k] = true
	}
}

// TestExternalActionStore_SQLite_ListFiltersAndPaging pins the org + team reads:
// newest-first ordering, the provider/action/actor/time filters, and limit/offset
// paging — over a _time_format=sqlite conn so a bound Since/Until compares against
// occurred_at correctly. TFAC-483.
func TestExternalActionStore_SQLite_ListFiltersAndPaging(t *testing.T) {
	conn := newSQLiteForArtifactTestTimed(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runID := seedArtifactRun(t, conn)

	seeds := []struct {
		provider, action, actor, when string
	}{
		{domain.ArtifactProviderGitHub, domain.ActionPRCreated, "", "2026-06-01 00:00:00"},                             // bot, no actor
		{domain.ArtifactProviderGitHub, domain.ActionPRMarkedReady, runmode.LocalDefaultUserID, "2026-06-02 00:00:00"}, // human-authorized
		{domain.ArtifactProviderJira, domain.ActionIssueTransitioned, "", "2026-06-03 00:00:00"},
		{domain.ArtifactProviderGitHub, domain.ActionCommentPosted, "", "2026-06-04 00:00:00"},
		{domain.ArtifactProviderJira, domain.ActionIssueCommentPosted, "", "2026-06-05 00:00:00"},
	}
	for i, s := range seeds {
		e := domain.ExternalAction{
			OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: s.provider, Action: s.action, Target: "t", ActorUserID: s.actor, ConversationID: runID,
			Credential: domain.CredentialGitHubApp, DedupKey: "k" + string(rune('a'+i)),
		}
		if err := stores.ExternalActions.Record(ctx, runmode.LocalDefaultOrgID, e); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if _, err := conn.Exec(`UPDATE external_actions SET occurred_at = ? WHERE dedup_key = ?`, s.when, e.DedupKey); err != nil {
			t.Fatalf("pin occurred_at: %v", err)
		}
	}

	all, err := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByOrgSystem: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("want 5 rows, got %d", len(all))
	}
	// Newest-first: the 06-05 Jira comment leads, the 06-01 PR-created trails.
	if all[0].Action != domain.ActionIssueCommentPosted || all[4].Action != domain.ActionPRCreated {
		t.Errorf("not newest-first: first=%q last=%q", all[0].Action, all[4].Action)
	}

	gh, _ := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Provider: domain.ArtifactProviderGitHub})
	if len(gh) != 3 {
		t.Errorf("provider=github returned %d, want 3", len(gh))
	}
	act, _ := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{Action: domain.ActionPRMarkedReady})
	if len(act) != 1 || act[0].Action != domain.ActionPRMarkedReady {
		t.Errorf("action filter returned %+v, want exactly pr_marked_ready", act)
	}
	byActor, _ := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{ActorUserID: runmode.LocalDefaultUserID})
	if len(byActor) != 1 || byActor[0].ActorUserID != runmode.LocalDefaultUserID {
		t.Errorf("actor filter returned %+v, want the one human-authorized row", byActor)
	}

	// Time window [06-03, 06-05): the Jira transition (06-03) + the comment (06-04).
	windowed, _ := stores.ExternalActions.ListByOrgSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalActionListOpts{
		Since: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
	})
	if len(windowed) != 2 {
		t.Fatalf("time window returned %d, want 2", len(windowed))
	}
	if windowed[0].Action != domain.ActionCommentPosted || windowed[1].Action != domain.ActionIssueTransitioned {
		t.Errorf("windowed order = [%q,%q], want [comment_posted, issue_transitioned]", windowed[0].Action, windowed[1].Action)
	}

	// Paging over ListByTeam (same shared helper): limit 2, offset 2 → one row left.
	page2, err := stores.ExternalActions.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.ExternalActionListOpts{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListByTeam paging: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("limit 2 offset 2 over 5 rows returned %d, want 2", len(page2))
	}
	last, err := stores.ExternalActions.ListByTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.ExternalActionListOpts{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("ListByTeam last page: %v", err)
	}
	if len(last) != 1 {
		t.Errorf("limit 2 offset 4 over 5 rows returned %d, want 1", len(last))
	}
}

// TestExternalActionStore_SQLite_EgressDenialDedupPerConversation pins the
// flood control on the sandbox egress denial: one conversation hammering a
// blocked host leaves exactly one row, while a SECOND conversation probing the
// same host leaves its own. The dedup key carries the conversation id for
// precisely this reason — the unique index spans (org_id, dedup_key), so a
// host-only key would let the first conversation's row swallow every later
// one, org-wide and forever.
func TestExternalActionStore_SQLite_EgressDenialDedupPerConversation(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runA := seedArtifactRun(t, conn)
	runB := seedArtifactRunWithID(t, conn, "88888888-8888-8888-8888-888888888888")

	const target = "api.github.com:443"
	denial := func(runID string) domain.ExternalAction {
		return domain.ExternalAction{
			OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: domain.ArtifactProviderNetwork, Action: domain.ActionEgressDenied,
			Target: target, ConversationID: runID, Credential: domain.CredentialNone,
			DedupKey: domain.EgressDenialDedupKey(runID, target),
		}
	}

	// The retry loop: ten probes of one host from one conversation.
	for i := 0; i < 10; i++ {
		if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, denial(runA)); err != nil {
			t.Fatalf("probe %d (should DO NOTHING after the first, not error): %v", i, err)
		}
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM external_actions`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("ten probes of one host from one conversation left %d rows, want 1", count)
	}

	// A different conversation probing the same host is its own signal.
	if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, denial(runB)); err != nil {
		t.Fatalf("second conversation Record: %v", err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM external_actions`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("a second conversation's probe of the same host left %d rows total, want 2", count)
	}
}

// TestExternalActionStore_SQLite_ListByRun pins the run-scoped read behind the
// run view's actions list: one conversation's rows, newest first, and nothing
// from a sibling run or from a row that has no run at all (a purged run's
// actions survive in the org feed with conversation_id NULL, and must not
// reappear under whichever run is being read).
func TestExternalActionStore_SQLite_ListByRun(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	runA := seedArtifactRun(t, conn)
	runB := seedArtifactRunWithID(t, conn, "88888888-8888-8888-8888-888888888888")

	// Each row carries a caller-supplied id (the store honors one) so the two
	// UPDATEs below address exactly the row they mean. Keying them on `action`
	// instead would silently span rows the moment this test grew a run that
	// performs the same act twice — which is most runs.
	record := func(id, runID, action string, occurred time.Time) string {
		t.Helper()
		if err := stores.ExternalActions.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.ExternalAction{
			ID: id, OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID,
			Provider: domain.ArtifactProviderGitHub, Action: action, Target: "octo/repo",
			ConversationID: runID, Credential: domain.CredentialGitHubApp,
		}); err != nil {
			t.Fatalf("record %s: %v", action, err)
		}
		// occurred_at is a column DEFAULT, so ordering is pinned by stamping the
		// row after the fact rather than by sleeping between inserts.
		if _, err := conn.Exec(`UPDATE external_actions SET occurred_at = ? WHERE id = ?`, occurred, id); err != nil {
			t.Fatalf("stamp %s: %v", action, err)
		}
		return id
	}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	record("act-1", runA, domain.ActionBranchPushed, base)
	record("act-2", runA, domain.ActionGHChannelWrite, base.Add(time.Minute))
	record("act-3", runB, domain.ActionPRMerged, base.Add(2*time.Minute))
	// A detached row: the run that produced it was purged (FK ON DELETE SET NULL).
	detached := record("act-4", runA, domain.ActionEgressDenied, base.Add(3*time.Minute))
	if _, err := conn.Exec(`UPDATE external_actions SET conversation_id = NULL WHERE id = ?`, detached); err != nil {
		t.Fatalf("detach: %v", err)
	}

	got, err := stores.ExternalActions.ListByRun(ctx, runmode.LocalDefaultOrgID, runA, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var actions []string
	for _, a := range got {
		actions = append(actions, a.Action)
	}
	want := []string{domain.ActionGHChannelWrite, domain.ActionBranchPushed}
	if !slices.Equal(actions, want) {
		t.Errorf("ListByRun(runA) = %v, want %v (newest first, runB's and the detached row excluded)", actions, want)
	}

	// The same opts the handler binds: a cap on how much of a runaway run's
	// history the run view pulls.
	capped, err := stores.ExternalActions.ListByRun(ctx, runmode.LocalDefaultOrgID, runA, domain.ExternalActionListOpts{Limit: 1})
	if err != nil {
		t.Fatalf("ListByRun(limit): %v", err)
	}
	if len(capped) != 1 || capped[0].Action != domain.ActionGHChannelWrite {
		t.Errorf("ListByRun with Limit 1 = %+v, want just the newest row", capped)
	}
}
