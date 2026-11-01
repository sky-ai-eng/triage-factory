package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jira/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"github.com/zalando/go-keyring"
)

// recordingJiraResolver is a jira.Resolver that records the (orgID, userID)
// ForUser was called with and returns a configured client or error.
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
	project := dbseed.JiraIssueProjectID(t, db, runmode.LocalDefaultOrgID, "https://jira.example.com", sourceID)
	if _, err := db.Exec(
		`INSERT INTO entities (id, source, source_id, external_id, kind, state, scope, jira_project_id)
		 VALUES (?, 'jira', ?, ?, 'issue', 'active', 'https://jira.example.com', ?)`,
		entityID, sourceID, dbseed.JiraIssueID(sourceID), project,
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

// TestTaskClaim_JiraTask_ResolvesActingUserCredential pins the positive path:
// a Jira-backed claim resolves the ACTING user's credential — routed by the
// authenticated (orgID, userID), not the org/bot — and the claim lands.
func TestTaskClaim_JiraTask_ResolvesActingUserCredential(t *testing.T) {
	s := newTestServer(t)
	res := &recordingJiraResolver{
		// A non-nil client so the post-switch sync sees jiraUserClient != nil.
		// No status rule is seeded, so the sync logs "no in_progress rule" and
		// never issues an outbound call against it.
		userClient: jira.NewClient(jira.DataCenterPAT("http://127.0.0.1:0", "user-tok")),
	}
	s.jiraResolver = res
	seedQueuedJiraTask(t, s.db, "e_jira_ok", "SKY-2", "00000000-0000-4000-8000-000000000021")

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000021/claim",
		map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// The write actor was resolved as the acting user (org + user from the
	// request), per the provenance routing.
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
		`SELECT claimed_by_user_id FROM tasks WHERE id = '00000000-0000-4000-8000-000000000021'`,
	).Scan(&claim); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	if !claim.Valid || claim.String != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %v; want the acting user", claim)
	}
}

// TestStockQueue_SkipsJiraResolver: the queue route makes no Jira write, so it
// never resolves the user's client and cannot depend on the secret store. The
// queued id names no ticket, which does not matter to whether ForUser ran.
func TestStockQueue_SkipsJiraResolver(t *testing.T) {
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	seedStockConfig(t, s)

	// Queue ignores the client, so the 200 alone would not catch a regression
	// that resolved it up front; the ForUser count does.
	res := &recordingJiraResolver{userErr: jira.ErrNoJiraUserCredential}
	s.jiraResolver = res

	rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/queue", map[string]any{
		"entity_ids": []string{"e_stock_SKY-1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if calls, _, _ := res.snapshot(); calls != 0 {
		t.Errorf("ForUser called %d times for a stock queue; want 0 (queue does no Jira write)", calls)
	}
}

// jiraUserClientFailures are the ways resolving the acting user's Jira client
// fails, and the answer each gets: the user reconnects, the request is retried,
// an admin fixes the org's OAuth app, or the server is at fault.
var jiraUserClientFailures = []struct {
	name       string
	err        error
	status     int
	reason     string
	bodySubstr string
}{
	{"refused_grant", fmt.Errorf("mint: %w", jira.ErrJiraUserCredentialRefused), http.StatusConflict, httpx.ReasonNotConfigured, "Connect your Jira again"},
	{"issuing_app_removed", fmt.Errorf("mint: %w", jira.ErrJiraUserCredentialAppRemoved), http.StatusConflict, httpx.ReasonNotConfigured, "was removed from this organization. Connect your Jira again"},
	{"no_credential", jira.ErrNoJiraUserCredential, http.StatusConflict, httpx.ReasonNotConfigured, "connect your Jira"},
	{"mint_unreachable", fmt.Errorf("mint: %w", &upstream.TransportError{Err: errors.New("dial tcp: connection refused")}), http.StatusBadGateway, httpx.ReasonUpstreamUnavailable, "did not answer"},
	{"mint_unavailable", fmt.Errorf("mint: %w", &jiraoauth.StatusError{Op: "token request", StatusCode: 503, Class: upstream.Transient}), http.StatusBadGateway, httpx.ReasonUpstreamUnavailable, "did not answer"},
	{"mint_rate_limited", fmt.Errorf("mint: %w", &jiraoauth.StatusError{Op: "token request", StatusCode: 429, Class: upstream.RateLimited}), http.StatusTooManyRequests, httpx.ReasonRateLimited, "rate limiting"},
	{"oauth_app_refused", fmt.Errorf("mint: %w", &jiraoauth.StatusError{Op: "token request", StatusCode: 401, Code: "invalid_client", Class: upstream.Auth}), http.StatusBadGateway, httpx.ReasonUpstreamRejected, "org admin"},
	{"secret_store_network", fmt.Errorf("read credential: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), http.StatusInternalServerError, httpx.ReasonInternal, ""},
}

func assertJiraUserClientFailure(t *testing.T, rec *httptest.ResponseRecorder, status int, reason, bodySubstr string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	var body struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Errors) != 1 {
		t.Fatalf("body = %s, want one error item (%v)", rec.Body.String(), err)
	}
	if body.Errors[0].Reason != reason {
		t.Errorf("reason = %q, want %q", body.Errors[0].Reason, reason)
	}
	if !strings.Contains(body.Errors[0].Message, bodySubstr) {
		t.Errorf("message = %q, want it to mention %q", body.Errors[0].Message, bodySubstr)
	}
}

// TestStockClaim_UserClientFailureAnsweredByWhoCanFixIt: a stock claim whose
// acting user's Jira client cannot be resolved is answered with the status and
// reason of whoever can fix the cause.
func TestStockClaim_UserClientFailureAnsweredByWhoCanFixIt(t *testing.T) {
	keyring.MockInit()
	for _, tc := range jiraUserClientFailures {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			seedStockConfig(t, s)
			s.jiraResolver = &recordingJiraResolver{userErr: tc.err}

			rec := doJSON(t, s, http.MethodPost, "/api/jira/stock/claim", map[string]any{
				"entity_ids": []string{"e_stock_SKY-1"},
			})
			assertJiraUserClientFailure(t, rec, tc.status, tc.reason, tc.bodySubstr)
		})
	}
}
