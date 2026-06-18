package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SKY-224 — projects PATCH inserts curator_pending_context rows for
// pinned-repos / tracker changes whenever the project has an active
// curator_session_id. The handler is responsible for the diff and the
// dispatch is responsible for consume/finalize/revert; these tests
// pin only the handler half so they don't need agentproc.

func seedProjectWithSessionForPatch(t *testing.T, s *Server) (id, sessionID string) {
	t.Helper()
	id, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	sessionID = "session-1"
	if err := db.SetProjectCuratorSessionID(s.db, id, sessionID); err != nil {
		t.Fatalf("set session id: %v", err)
	}
	return id, sessionID
}

func TestProjectPatch_QueuesPinnedRepoChange(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	id, _ := seedProjectWithSessionForPatch(t, s)

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rows, err := s.curatorStore.ListPendingContext(t.Context(), runmode.LocalDefaultOrgID, id)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 pending row, got %d", len(rows))
	}
	if rows[0].ChangeType != domain.ChangeTypePinnedRepos {
		t.Errorf("change_type = %q, want %q", rows[0].ChangeType, domain.ChangeTypePinnedRepos)
	}
	if rows[0].BaselineValue != `[]` {
		t.Errorf("baseline = %q, want [] (project started with no pinned repos)", rows[0].BaselineValue)
	}
}

// TestProjectPatch_NoQueueWithoutSession verifies the no-session
// short-circuit: a project that has never been chatted with shouldn't
// accumulate pending rows, since the next session's static envelope
// renders fresh values directly.
func TestProjectPatch_NoQueueWithoutSession(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	id, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows, _ := s.curatorStore.ListPendingContext(t.Context(), runmode.LocalDefaultOrgID, id)
	if len(rows) != 0 {
		t.Errorf("expected 0 pending rows for session-less project, got %d (%+v)", len(rows), rows)
	}
}

// TestProjectPatch_NoQueueWhenNothingChanged checks that a no-op
// PATCH (resending the same value) doesn't insert a row. The diff
// comparison runs on the merged value, not on whether the field was
// present in the request, so re-sending an unchanged value should be
// invisible.
func TestProjectPatch_NoQueueWhenNothingChanged(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	id, _ := seedProjectWithSessionForPatch(t, s)

	// Seed an initial pinned repo via direct DB write, then PATCH the
	// same value back. The diff should fold to "no change."
	if err := s.projects.Update(t.Context(), runmode.LocalDefaultOrgID, domain.Project{
		ID:               id,
		Name:             "P",
		PinnedRepos:      []string{"sky-ai-eng/triage-factory"},
		CuratorSessionID: "session-1",
	}); err != nil {
		t.Fatalf("seed pinned: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows, _ := s.curatorStore.ListPendingContext(t.Context(), runmode.LocalDefaultOrgID, id)
	if len(rows) != 0 {
		t.Errorf("no-op PATCH queued %d rows: %+v", len(rows), rows)
	}
}

// TestProjectPatch_CoalescesRepeatedPATCHes is the coalescing payoff:
// two PATCHes between user messages must not stack into two rows.
// The first PATCH's pre-state is the truer baseline anchor for diffs
// at consume time, so the unique-on-pending constraint must keep it
// in place.
func TestProjectPatch_CoalescesRepeatedPATCHes(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	seedConfiguredRepo(t, s, "sky-ai-eng", "another")
	id, _ := seedProjectWithSessionForPatch(t, s)

	// First PATCH: [] → [triage-factory]. Baseline should be [].
	if rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{"sky-ai-eng/triage-factory"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("first patch: %d %s", rec.Code, rec.Body.String())
	}

	// Second PATCH: [triage-factory] → [triage-factory, another].
	// Baseline must remain [] (the truer "earliest unconsumed" anchor),
	// not [triage-factory] (which would mask that the user added
	// triage-factory in the first place).
	if rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{"sky-ai-eng/triage-factory", "sky-ai-eng/another"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("second patch: %d %s", rec.Code, rec.Body.String())
	}

	rows, _ := s.curatorStore.ListPendingContext(t.Context(), runmode.LocalDefaultOrgID, id)
	if len(rows) != 1 {
		t.Fatalf("expected 1 coalesced row, got %d (%+v)", len(rows), rows)
	}
	if rows[0].BaselineValue != `[]` {
		t.Errorf("baseline = %q, want [] (oldest snapshot wins)", rows[0].BaselineValue)
	}
}

// TestPinnedReposSetEqual_DedupesBothSides exercises the set-equality
// helper directly. It must treat duplicates as no-ops on both sides
// — ["a","a"] and ["a"] represent the same set, even though their
// lengths differ. The validator currently doesn't dedupe, so any
// length-only short-circuit here would queue spurious pending rows
// for PATCHes that "remove" duplicates.
func TestPinnedReposSetEqual_DedupesBothSides(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"identical", []string{"a", "b"}, []string{"a", "b"}, true},
		{"reorder", []string{"a", "b"}, []string{"b", "a"}, true},
		{"dedup_left", []string{"a", "a"}, []string{"a"}, true},
		{"dedup_right", []string{"a"}, []string{"a", "a"}, true},
		{"dedup_both_diff_counts", []string{"a", "a", "b"}, []string{"a", "b", "b"}, true},
		{"actually_different", []string{"a", "b"}, []string{"a", "c"}, false},
		{"superset", []string{"a", "b"}, []string{"a"}, false},
		{"empty_vs_empty", nil, []string{}, true},
		{"empty_vs_nonempty", nil, []string{"a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedReposSetEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("pinnedReposSetEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestProjectPatch_NoQueueOnPureReorder verifies that pinned_repos is
// treated as a set on both sides of the diff: a PATCH that only
// reorders the existing list should not queue a row, since the
// curator-side renderer would compute an empty add/remove diff and
// the row would round-trip through claim/render/finalize having
// produced nothing. Avoiding the wasted I/O at the queue side keeps
// the audit trail and the consume path quiet.
func TestProjectPatch_NoQueueOnPureReorder(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	seedConfiguredRepo(t, s, "sky-ai-eng", "another")
	id, _ := seedProjectWithSessionForPatch(t, s)

	// Seed a known order via direct DB write so the comparison below
	// is unambiguously a reorder.
	if err := s.projects.Update(t.Context(), runmode.LocalDefaultOrgID, domain.Project{
		ID:               id,
		Name:             "P",
		PinnedRepos:      []string{"sky-ai-eng/triage-factory", "sky-ai-eng/another"},
		CuratorSessionID: "session-1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// PATCH with the same set in reverse order.
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repos": []string{"sky-ai-eng/another", "sky-ai-eng/triage-factory"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rows, _ := s.curatorStore.ListPendingContext(t.Context(), runmode.LocalDefaultOrgID, id)
	if len(rows) != 0 {
		t.Errorf("reorder PATCH queued %d rows (set semantics broken): %+v", len(rows), rows)
	}
}

// TestProjectPatch_QueuesJiraChange exercises the tracker side. Linear
// is rejected by the validator (integration not yet shipped), so we
// only test that path indirectly via clearing — direct setting cannot
// be tested through the handler.
func TestProjectPatch_QueuesJiraChange(t *testing.T) {
	s := newTestServer(t)
	id, _ := seedProjectWithSessionForPatch(t, s)

	// Seed a configured Jira project so validateTrackerKeys accepts
	// the value when the PATCH handler reads the team's rules. The
	// jpsr_*_populated CHECK constraints require fully-populated
	// rules — the test fixture mirrors the handler's strict shape.
	if err := s.jiraRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, []domain.JiraProjectStatusRules{
		validProjectRule("SKY"),
	}); err != nil {
		t.Fatalf("save jira rules: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"jira_project_key": "SKY",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rows, _ := s.curatorStore.ListPendingContext(t.Context(), runmode.LocalDefaultOrgID, id)
	if len(rows) != 1 {
		t.Fatalf("expected 1 pending row, got %d", len(rows))
	}
	if rows[0].ChangeType != domain.ChangeTypeJiraProjectKey {
		t.Errorf("change_type = %q", rows[0].ChangeType)
	}
	if rows[0].BaselineValue != `null` {
		t.Errorf("baseline = %q, want null (was unset)", rows[0].BaselineValue)
	}

	// Read the project back to confirm the PATCH actually applied —
	// otherwise the queued row would be misleading.
	var got domain.Project
	rec2 := doJSON(t, s, http.MethodGet, "/api/projects/"+id, nil)
	_ = json.Unmarshal(rec2.Body.Bytes(), &got)
	if got.JiraProjectKey != "SKY" {
		t.Errorf("jira_project_key = %q, want SKY", got.JiraProjectKey)
	}
}
