package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Projects PATCH records an 'injection:context' delta on every live curator
// conversation of the project for pinned-repos / tracker changes. The
// handler owns the diff; the curator dispatch owns consume/revert; these
// tests pin only the handler half so they don't need agentproc.

// seedProjectWithConversation creates a project plus a live curator
// conversation for the local user — the state a project reaches after its
// first chat message, which is what arms the pending-context producer.
func seedProjectWithConversation(t *testing.T, s *Server) (projectID, convID string) {
	t.Helper()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	created, err := s.projects.Create(t.Context(), org, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID = created.ID
	if err := s.tx.SyntheticClaimsWithTx(t.Context(), org, user, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(t.Context(), org, projectID, user)
		if err != nil {
			return err
		}
		convID = conv.ID
		return nil
	}); err != nil {
		t.Fatalf("mint conversation: %v", err)
	}
	return projectID, convID
}

// listPendingInjections reads the project's undelivered 'injection:context'
// rows straight off the messages table (there is deliberately no listing
// method on the store — the dispatch consumes them, tests peek).
func listPendingInjections(t *testing.T, s *Server, projectID string) []domain.CuratorContextChange {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT m.id, COALESCE(m.metadata, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.project_id = ? AND c.type = 'curator'
		  AND m.subtype = 'injection:context' AND m.delivered = 0
		ORDER BY m.id ASC
	`, projectID)
	if err != nil {
		t.Fatalf("query pending injections: %v", err)
	}
	defer rows.Close()
	var out []domain.CuratorContextChange
	for rows.Next() {
		var (
			id   int64
			meta string
		)
		if err := rows.Scan(&id, &meta); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var payload struct {
			ChangeType    string `json:"change_type"`
			BaselineValue string `json:"baseline_value"`
		}
		if err := json.Unmarshal([]byte(meta), &payload); err != nil {
			t.Fatalf("decode metadata %q: %v", meta, err)
		}
		out = append(out, domain.CuratorContextChange{MessageID: id, ChangeType: payload.ChangeType, BaselineValue: payload.BaselineValue})
	}
	return out
}

func TestProjectPatch_QueuesPinnedRepoChange(t *testing.T) {
	s := newTestServer(t)
	repoID := seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	id, _ := seedProjectWithConversation(t, s)

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repository_ids": []string{repoID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	rows := listPendingInjections(t, s, id)
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

// TestProjectPatch_NoQueueWithoutConversation verifies the no-conversation
// short-circuit: a project nobody has chatted with shouldn't accumulate
// pending rows, since the first conversation's static envelope renders
// fresh values directly.
func TestProjectPatch_NoQueueWithoutConversation(t *testing.T) {
	s := newTestServer(t)
	repoID := seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	created, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	id := created.ID

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repository_ids": []string{repoID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rows := listPendingInjections(t, s, id); len(rows) != 0 {
		t.Errorf("expected 0 pending rows for conversation-less project, got %d (%+v)", len(rows), rows)
	}
}

// TestProjectPatch_NoQueueWhenNothingChanged checks that a no-op
// PATCH (resending the same value) doesn't insert a row. The diff
// comparison runs on the merged value, not on whether the field was
// present in the request, so re-sending an unchanged value should be
// invisible.
func TestProjectPatch_NoQueueWhenNothingChanged(t *testing.T) {
	s := newTestServer(t)
	repoID := seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	id, _ := seedProjectWithConversation(t, s)

	// Seed an initial pinned repo via direct DB write, then PATCH the
	// same value back. The diff should fold to "no change."
	if _, err := s.projects.Update(t.Context(), runmode.LocalDefaultOrgID, domain.Project{
		ID:          id,
		Name:        "P",
		PinnedRepos: []string{"sky-ai-eng/triage-factory"},
	}); err != nil {
		t.Fatalf("seed pinned: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repository_ids": []string{repoID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rows := listPendingInjections(t, s, id); len(rows) != 0 {
		t.Errorf("no-op PATCH queued %d rows: %+v", len(rows), rows)
	}
}

// TestProjectPatch_CoalescesRepeatedPATCHes pins the one-pending-per-type
// invariant: two PATCHes between user messages must not stack into two
// rows — the second REPLACES the first, so the surviving baseline is the
// second PATCH's pre-state.
func TestProjectPatch_CoalescesRepeatedPATCHes(t *testing.T) {
	s := newTestServer(t)
	tfID := seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	anotherID := seedConfiguredRepo(t, s, "sky-ai-eng", "another")
	id, _ := seedProjectWithConversation(t, s)

	// First PATCH: [] → [triage-factory]. Baseline [].
	if rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repository_ids": []string{tfID},
	}); rec.Code != http.StatusOK {
		t.Fatalf("first patch: %d %s", rec.Code, rec.Body.String())
	}

	// Second PATCH: [triage-factory] → [triage-factory, another]. The
	// replacement's baseline is the second PATCH's pre-state.
	if rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repository_ids": []string{tfID, anotherID},
	}); rec.Code != http.StatusOK {
		t.Fatalf("second patch: %d %s", rec.Code, rec.Body.String())
	}

	rows := listPendingInjections(t, s, id)
	if len(rows) != 1 {
		t.Fatalf("expected 1 coalesced row, got %d (%+v)", len(rows), rows)
	}
	if rows[0].BaselineValue != `["sky-ai-eng/triage-factory"]` {
		t.Errorf("baseline = %q, want the replacement's pre-state", rows[0].BaselineValue)
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

// TestProjectPatch_NoQueueOnPureReorder verifies that the pinned set is
// treated as a set on both sides of the diff: a PATCH that only
// reorders the existing list should not queue a row, since the
// curator-side renderer would compute an empty add/remove diff and the
// row would round-trip through consume/render having produced nothing.
func TestProjectPatch_NoQueueOnPureReorder(t *testing.T) {
	s := newTestServer(t)
	tfID := seedConfiguredRepo(t, s, "sky-ai-eng", "triage-factory")
	anotherID := seedConfiguredRepo(t, s, "sky-ai-eng", "another")
	id, _ := seedProjectWithConversation(t, s)

	// Seed a known order via direct DB write so the comparison below
	// is unambiguously a reorder.
	if _, err := s.projects.Update(t.Context(), runmode.LocalDefaultOrgID, domain.Project{
		ID:          id,
		Name:        "P",
		PinnedRepos: []string{"sky-ai-eng/triage-factory", "sky-ai-eng/another"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// PATCH with the same set in reverse order.
	rec := doJSON(t, s, http.MethodPatch, "/api/projects/"+id, map[string]any{
		"pinned_repository_ids": []string{anotherID, tfID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if rows := listPendingInjections(t, s, id); len(rows) != 0 {
		t.Errorf("reorder PATCH queued %d rows (set semantics broken): %+v", len(rows), rows)
	}
}

// TestProjectPatch_QueuesJiraChange exercises the tracker side. Linear
// is rejected by the validator (integration not yet shipped), so we
// only test that path indirectly via clearing — direct setting cannot
// be tested through the handler.
func TestProjectPatch_QueuesJiraChange(t *testing.T) {
	s := newTestServer(t)
	id, _ := seedProjectWithConversation(t, s)

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

	rows := listPendingInjections(t, s, id)
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
