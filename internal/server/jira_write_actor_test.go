package server

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// recordingJiraResolver is a test double for jira.Resolver that records the
// (orgID, userID) ForUser was routed with and returns a configurable
// client/error. It pins SKY-463's contract: user-initiated Jira writes resolve
// the ACTING user's credential, and the handler surfaces ErrNoJiraUserCredential
// rather than falling back to the org/bot client.
type recordingJiraResolver struct {
	userClient *jira.Client
	userErr    error

	mu           sync.Mutex
	forUserCalls int
	gotOrgID     string
	gotUserID    string
}

func (r *recordingJiraResolver) ForSystem(_ context.Context, _ string) (*jira.Client, error) {
	return nil, nil
}

func (r *recordingJiraResolver) ForUser(_ context.Context, orgID, userID string) (*jira.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forUserCalls++
	r.gotOrgID, r.gotUserID = orgID, userID
	return r.userClient, r.userErr
}

func (r *recordingJiraResolver) snapshot() (calls int, orgID, userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forUserCalls, r.gotOrgID, r.gotUserID
}

// seedQueuedJiraTask installs the FK chain for an unclaimed, queued Jira task.
func seedQueuedJiraTask(t *testing.T, db *sql.DB, entityID, sourceID, taskID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES (?, 'jira', ?, 'issue', 'active')`,
		entityID, sourceID,
	); err != nil {
		t.Fatalf("seed jira entity: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES (?, ?, ?, '')`,
		"ev_"+taskID, entityID, domain.EventJiraIssueAssigned,
	); err != nil {
		t.Fatalf("seed jira event: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES (?, ?, ?, ?, 'queued')`,
		taskID, entityID, domain.EventJiraIssueAssigned, "ev_"+taskID,
	); err != nil {
		t.Fatalf("seed jira task: %v", err)
	}
}

// TestSwipeClaim_JiraTask_NoUserCredential_Returns409 pins the defense-in-depth
// boundary: claiming a Jira-backed task when the acting user has no connected
// Jira is refused with 409 BEFORE the claim lands. Acting as the org/bot here
// would mis-assign the ticket to the service account — the exact bug SKY-463
// closes — so the handler must refuse rather than fall back.
func TestSwipeClaim_JiraTask_NoUserCredential_Returns409(t *testing.T) {
	s := newTestServer(t)
	s.jiraResolver = &recordingJiraResolver{userErr: jira.ErrNoJiraUserCredential}
	seedQueuedJiraTask(t, s.db, "e_jira_nocred", "SKY-1", "t_jira_nocred")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_jira_nocred/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connect your Jira") {
		t.Errorf("body = %s; want a 'connect your Jira' prompt", rec.Body.String())
	}

	// Refused before mutation: no claim stamped, no audit row.
	var claim sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = 't_jira_nocred'`,
	).Scan(&claim); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if claim.Valid && claim.String != "" {
		t.Errorf("claimed_by_user_id = %q; want unclaimed (refusal must not mutate)", claim.String)
	}
	var swipeCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't_jira_nocred'`,
	).Scan(&swipeCount); err != nil {
		t.Fatalf("scan swipe_events: %v", err)
	}
	if swipeCount != 0 {
		t.Errorf("swipe_events = %d, want 0 (refused claim must leave no audit)", swipeCount)
	}
}

// TestSwipeClaim_JiraTask_ResolvesActingUserCredential pins the positive path:
// a Jira-backed claim resolves the ACTING user's credential — routed by the
// authenticated (orgID, userID), not the org/bot — and the claim lands.
func TestSwipeClaim_JiraTask_ResolvesActingUserCredential(t *testing.T) {
	s := newTestServer(t)
	res := &recordingJiraResolver{
		// A non-nil client so the post-switch sync sees jiraUserClient != nil.
		// No status rule is seeded, so the sync logs "no in_progress rule" and
		// never issues an outbound call against it.
		userClient: jira.NewClient(jira.DataCenterPAT("http://127.0.0.1:0", "user-tok")),
	}
	s.jiraResolver = res
	seedQueuedJiraTask(t, s.db, "e_jira_ok", "SKY-2", "t_jira_ok")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_jira_ok/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The write actor was resolved as the acting user (org + user from the
	// request), the provenance routing SKY-463 introduces.
	calls, gotOrg, gotUser := res.snapshot()
	if calls != 1 {
		t.Errorf("ForUser called %d times, want 1", calls)
	}
	if gotOrg != runmode.LocalDefaultOrgID || gotUser != runmode.LocalDefaultUserID {
		t.Errorf("ForUser routed (org=%q user=%q); want (org=%q user=%q)",
			gotOrg, gotUser, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	}

	// The claim landed on the TF side.
	var claim sql.NullString
	if err := s.db.QueryRow(
		`SELECT claimed_by_user_id FROM tasks WHERE id = 't_jira_ok'`,
	).Scan(&claim); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claim.Valid || claim.String != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %v; want the acting user", claim)
	}
}

// TestSwipeClaim_GitHubTask_SkipsJiraResolver pins that the resolver is only
// consulted for Jira-backed claims — a GitHub task claim must not touch the
// Jira write path at all (no spurious ForUser, no 409 on a GitHub claim from a
// user without Jira).
func TestSwipeClaim_GitHubTask_SkipsJiraResolver(t *testing.T) {
	s := newTestServer(t)
	res := &recordingJiraResolver{userErr: jira.ErrNoJiraUserCredential}
	s.jiraResolver = res

	const eventType = "github:pr:opened"
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state)
		 VALUES ('e_gh', 'github', 'sky/repo#1', 'pr', 'active')`,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key)
		 VALUES ('ev_gh', 'e_gh', ?, '')`, eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES ('t_gh', 'e_gh', ?, 'ev_gh', 'queued')`, eventType,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/t_gh/swipe",
		map[string]any{"action": "claim", "hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (GitHub claim unaffected by Jira cred); body=%s", rec.Code, rec.Body.String())
	}
	if calls, _, _ := res.snapshot(); calls != 0 {
		t.Errorf("ForUser called %d times for a GitHub task; want 0", calls)
	}
}
