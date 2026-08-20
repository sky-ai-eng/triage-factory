package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

func mustEntity(t *testing.T, database *sql.DB, source, sourceID, kind, title string) *domain.Entity {
	t.Helper()
	e, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, source, sourceID, kind, title, "https://x/"+sourceID)
	if err != nil {
		t.Fatalf("FindOrCreateEntity %s/%s: %v", source, sourceID, err)
	}
	return e
}

// TestBackfillCandidates_ScopesByPinnedReposAndJiraKey verifies the
// per-source filter rules: an entity only appears when its source's
// configured filter (pinned_repos for github, jira_project_key for
// jira) admits it. Empty filter on a source = no filter for that
// source.
func TestBackfillCandidates_ScopesByPinnedReposAndJiraKey(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	seedConfiguredRepo(t, s, "sky-ai-eng", "other-repo")

	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{
		Name:           "Auth",
		PinnedRepos:    []string{"sky-ai-eng/triage-factory"},
		JiraProjectKey: "SKY",
	})
	if err != nil {
		t.Fatal(err)
	}
	pid := created.ID

	// Two GitHub entities, only one in pinned_repos.
	mustEntity(t, s.db, "github", "sky-ai-eng/triage-factory#1", "pr", "in pin")
	mustEntity(t, s.db, "github", "sky-ai-eng/other-repo#9", "pr", "out of pin")
	// Two Jira entities, only one matching SKY.
	mustEntity(t, s.db, "jira", "SKY-100", "issue", "matching jira")
	mustEntity(t, s.db, "jira", "FOO-200", "issue", "non-matching jira")

	candidates := decodeList[backfillCandidate](t, doJSON(t, s, http.MethodPost,
		"/api/projects/"+pid+"/backfill-candidates/list", map[string]any{})).Items

	gotIDs := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		gotIDs[c.SourceID] = true
	}
	if !gotIDs["sky-ai-eng/triage-factory#1"] {
		t.Errorf("missing in-pin github candidate")
	}
	if gotIDs["sky-ai-eng/other-repo#9"] {
		t.Errorf("included out-of-pin github candidate; pinned_repos filter not applied")
	}
	if !gotIDs["SKY-100"] {
		t.Errorf("missing matching jira candidate")
	}
	if gotIDs["FOO-200"] {
		t.Errorf("included non-matching jira candidate; jira_project_key filter not applied")
	}
}

// TestBackfillCandidates_EmptyConfigShowsAll covers the case where
// the project has neither pinned_repos nor a Jira project key —
// every non-terminal entity should appear so the user can claim
// anything from the unconfigured project.
func TestBackfillCandidates_EmptyConfigShowsAll(t *testing.T) {
	s := newTestServer(t)
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "Misc"})
	if err != nil {
		t.Fatal(err)
	}
	pid := created.ID

	mustEntity(t, s.db, "github", "owner/repo#1", "pr", "T1")
	mustEntity(t, s.db, "jira", "ANY-1", "issue", "T2")

	candidates := decodeList[backfillCandidate](t, doJSON(t, s, http.MethodPost,
		"/api/projects/"+pid+"/backfill-candidates/list", map[string]any{})).Items
	if len(candidates) != 2 {
		t.Errorf("expected 2 candidates with empty config, got %d", len(candidates))
	}
}

// TestBackfillCandidates_ExcludesAlreadyInProject — entities already
// in the requested project shouldn't show up; there's nothing to
// backfill for them.
func TestBackfillCandidates_ExcludesAlreadyInProject(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := created.ID
	created2, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "Other", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	other := created2.ID

	already := mustEntity(t, s.db, "github", "owner/repo#1", "pr", "already in")
	if _, err := sqlitestore.New(s.db).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, already.ID, &pid, ""); err != nil {
		t.Fatal(err)
	}
	elsewhere := mustEntity(t, s.db, "github", "owner/repo#2", "pr", "elsewhere")
	if _, err := sqlitestore.New(s.db).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, elsewhere.ID, &other, ""); err != nil {
		t.Fatal(err)
	}
	free := mustEntity(t, s.db, "github", "owner/repo#3", "pr", "unassigned")

	candidates := decodeList[backfillCandidate](t, doJSON(t, s, http.MethodPost,
		"/api/projects/"+pid+"/backfill-candidates/list", map[string]any{})).Items

	got := map[string]string{}
	for _, c := range candidates {
		got[c.ID] = c.CurrentProjectName
	}
	if _, ok := got[already.ID]; ok {
		t.Errorf("entity already in this project should be excluded")
	}
	if got[elsewhere.ID] != "Other" {
		t.Errorf("elsewhere entity current_project_name = %q, want Other", got[elsewhere.ID])
	}
	if _, ok := got[free.ID]; !ok {
		t.Errorf("unassigned entity missing from candidates")
	}
}

// TestBackfill_BulkAssignPartialSuccess covers the happy path plus a
// missing-id row producing a per-row failure rather than aborting
// the whole call.
func TestBackfill_BulkAssignPartialSuccess(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	pid := created.ID
	a := mustEntity(t, s.db, "github", "owner/repo#1", "pr", "A")
	b := mustEntity(t, s.db, "github", "owner/repo#2", "pr", "B")

	body := map[string]any{
		"entity_ids": []string{a.ID, b.ID, "nonexistent-id"},
	}
	rec := doJSON(t, s, http.MethodPost, "/api/projects/"+pid+"/backfill", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied int               `json:"applied"`
		Failed  []backfillFailure `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Real entities applied; bogus id surfaces as a per-row failure
	// rather than being silently counted (relies on
	// EntityStore.AssignProject returning sql.ErrNoRows on 0-row UPDATE).
	if resp.Applied != 2 {
		t.Errorf("applied = %d, want 2 (a + b; bogus id should fail)", resp.Applied)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].EntityID != "nonexistent-id" {
		t.Errorf("failed = %+v, want one entry for nonexistent-id", resp.Failed)
	}
	for _, e := range []domain.Entity{*a, *b} {
		got, _ := sqlitestore.New(s.db).Entities.Get(context.Background(), runmode.LocalDefaultOrgID, e.ID)
		if got == nil || got.ProjectID == nil || *got.ProjectID != pid {
			t.Errorf("entity %s not assigned to %s", e.ID, pid)
			continue
		}
		// Manual claim should stamp the sentinel rationale so the
		// entities panel surfaces "Manually assigned by user" rather
		// than the empty-fallback.
		if got.ClassificationRationale != manualAssignmentMessage {
			t.Errorf("entity %s rationale = %q, want %q", e.ID, got.ClassificationRationale, manualAssignmentMessage)
		}
	}
}

// TestBackfill_BatchAccounting pins the batch-policy invariant: every
// submitted id is accounted for. A request-level fault — an empty list, a
// blank id, a repeated id — fails the whole call rather than being dropped
// mid-loop, and a well-formed batch answers applied + failed = submitted.
func TestBackfill_BatchAccounting(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	pid := created.ID
	a := mustEntity(t, s.db, "github", "owner/repo#1", "pr", "A")

	// Request-level faults, each rejected whole.
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"empty list", []string{}},
		{"blank id", []string{a.ID, "  "}},
		{"duplicate id", []string{a.ID, a.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, "/api/projects/"+pid+"/backfill", map[string]any{"entity_ids": tc.ids})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, httpx.ReasonInvalidField, "entity_ids")
		})
	}

	// A well-formed batch: three submitted, one assignable, two failures.
	closed := mustEntity(t, s.db, "github", "owner/repo#9", "pr", "closed")
	if _, err := s.db.Exec(`UPDATE entities SET state='closed' WHERE id=?`, closed.ID); err != nil {
		t.Fatalf("close entity: %v", err)
	}
	submitted := []string{a.ID, closed.ID, "00000000-0000-4000-8000-0000000009ff"}
	rec := doJSON(t, s, http.MethodPost, "/api/projects/"+pid+"/backfill", map[string]any{"entity_ids": submitted})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied int               `json:"applied"`
		Failed  []backfillFailure `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Applied+len(resp.Failed) != len(submitted) {
		t.Errorf("applied(%d) + failed(%d) = %d, want %d — every submitted id must be accounted for",
			resp.Applied, len(resp.Failed), resp.Applied+len(resp.Failed), len(submitted))
	}
	for _, f := range resp.Failed {
		if len(f.Errors) == 0 || f.Errors[0].Reason == "" {
			t.Errorf("failed row %s carries no structured reason: %+v", f.EntityID, f.Errors)
		}
	}
}

// TestBackfill_RejectsOutOfScopeAndClosed verifies the server-side
// eligibility gate: a stale or tampered request with ids for closed
// entities or entities outside the project's tracker scope must be
// rejected with per-row failures, not silently applied. Without this
// gate, a malicious client could reassign any entity by id and a
// stale UI could re-stamp classified_at on closed work.
func TestBackfill_RejectsOutOfScopeAndClosed(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "in-scope")
	seedConfiguredRepo(t, s, "owner", "out-of-scope")
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{
		Name:           "P",
		PinnedRepos:    []string{"owner/in-scope"},
		JiraProjectKey: "SKY",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	pid := created.ID

	inScope := mustEntity(t, s.db, "github", "owner/in-scope#1", "pr", "ok")
	outScope := mustEntity(t, s.db, "github", "owner/out-of-scope#2", "pr", "wrong repo")
	wrongJira := mustEntity(t, s.db, "jira", "FOO-9", "issue", "wrong project")
	closedEnt := mustEntity(t, s.db, "github", "owner/in-scope#3", "pr", "closed")
	if _, err := sqlitestore.New(s.db).Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, closedEnt.ID); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{
		"entity_ids": []string{inScope.ID, outScope.ID, wrongJira.ID, closedEnt.ID},
	}
	rec := doJSON(t, s, http.MethodPost, "/api/projects/"+pid+"/backfill", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied int               `json:"applied"`
		Failed  []backfillFailure `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Applied != 1 {
		t.Errorf("applied = %d, want 1 (only in-scope active entity)", resp.Applied)
	}
	failedByID := map[string]string{}
	for _, f := range resp.Failed {
		if len(f.Errors) == 0 {
			t.Fatalf("failed row %s carries no errors item", f.EntityID)
		}
		failedByID[f.EntityID] = f.Errors[0].Message
	}
	if msg := failedByID[outScope.ID]; msg == "" || !strings.Contains(msg, "outside") {
		t.Errorf("out-of-scope github entity: failure = %q, want 'outside ... scope'", msg)
	}
	if msg := failedByID[wrongJira.ID]; msg == "" || !strings.Contains(msg, "outside") {
		t.Errorf("wrong-jira-project entity: failure = %q, want 'outside ... scope'", msg)
	}
	if msg := failedByID[closedEnt.ID]; msg == "" || !strings.Contains(msg, "active") {
		t.Errorf("closed entity: failure = %q, want 'not active'", msg)
	}

	// Confirm only the in-scope active entity actually landed.
	got, _ := sqlitestore.New(s.db).Entities.Get(context.Background(), runmode.LocalDefaultOrgID, inScope.ID)
	if got == nil || got.ProjectID == nil || *got.ProjectID != pid {
		t.Errorf("in-scope entity not assigned")
	}
	for _, e := range []*domain.Entity{outScope, wrongJira} {
		got, _ := sqlitestore.New(s.db).Entities.Get(context.Background(), runmode.LocalDefaultOrgID, e.ID)
		if got != nil && got.ProjectID != nil {
			t.Errorf("entity %s should not have been reassigned, got project_id=%q", e.SourceID, *got.ProjectID)
		}
	}
}

// TestBackfill_StampsClassifiedAt — popup-claimed entities must have
// classified_at set so the post-poll auto-classifier doesn't try to
// reassign them.
func TestBackfill_StampsClassifiedAt(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	pid := created.ID
	e := mustEntity(t, s.db, "github", "owner/repo#1", "pr", "T")

	pre, err := sqlitestore.New(s.db).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	wasUnclassified := false
	for _, x := range pre {
		if x.ID == e.ID {
			wasUnclassified = true
			break
		}
	}
	if !wasUnclassified {
		t.Fatalf("test setup: entity should be unclassified before backfill")
	}

	rec := doJSON(t, s, http.MethodPost, "/api/projects/"+pid+"/backfill",
		map[string]any{"entity_ids": []string{e.ID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	post, err := sqlitestore.New(s.db).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range post {
		if x.ID == e.ID {
			t.Errorf("entity still in unclassified queue after backfill — classified_at not stamped")
		}
	}
}
