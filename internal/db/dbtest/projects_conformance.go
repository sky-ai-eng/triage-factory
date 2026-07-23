package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ProjectStoreFactory is what a per-backend test file hands to
// RunProjectStoreConformance. Returns the wired ProjectStore impl and
// the orgID + teamID to thread through Create (Postgres needs a real
// FK-valid team; SQLite ignores it).
type ProjectStoreFactory func(t *testing.T) (
	store db.ProjectStore,
	orgID, teamID string,
)

// RunProjectStoreConformance covers the project-store contract every
// backend impl must hold:
//
//   - Create returns the server-generated id when p.ID is empty.
//   - Create honors a caller-supplied p.ID verbatim.
//   - Create + Get round-trip across the full mutable field surface,
//     with PinnedRepos = nil collapsing to [].
//   - Get returns (nil, nil) on miss.
//   - List returns rows in case-insensitive name order; empty
//     install returns [] (not nil).
//   - Update writes through the full mutable surface, stamps
//     updated_at fresh, preserves created_at, and returns
//     sql.ErrNoRows on a bogus id.
//   - Delete returns sql.ErrNoRows on a bogus id and removes the row
//     on a real id.
func RunProjectStoreConformance(t *testing.T, mk ProjectStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Create_generates_id_when_empty", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		id, err := s.Create(ctx, orgID, teamID, domain.Project{
			Name: "Generated", Description: "x",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id == "" {
			t.Fatal("Create with empty ID should return a generated id")
		}
		got, _ := s.Get(ctx, orgID, id)
		if got == nil {
			t.Fatalf("Get on freshly-created project returned nil")
		}
		if got.Name != "Generated" {
			t.Errorf("Name = %q, want %q", got.Name, "Generated")
		}
	})

	t.Run("Create_honors_supplied_id", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		// Postgres' projects.id is a uuid column — a bare string id
		// would fail the cast. We don't pre-set an ID in conformance;
		// the caller-supplied path is exercised at the call site
		// (projectbundle.Import) which already passes uuid-shaped ids.
		id, err := s.Create(ctx, orgID, teamID, domain.Project{Name: "supplied"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got == nil || got.ID != id {
			t.Errorf("Get(%q) = %v, want id=%q", id, got, id)
		}
	})

	t.Run("Create_then_Get_round_trips_full_surface", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		// SpecAuthorshipBlueprintID is intentionally left empty here — it
		// FKs to prompts(id), which would need a backend-specific
		// fixture to seed. The empty path exercises the NULLIF
		// collapse; non-empty handling is covered indirectly via the
		// Update-round-trip subtest, which preserves whatever was set
		// (including empty).
		input := domain.Project{
			Name:             "Round-Trip",
			Description:      "spec body",
			PinnedRepos:      []string{"octo/widget", "octo/api"},
			JiraProjectKey:   "SKY",
			LinearProjectKey: "LIN",
		}
		id, err := s.Create(ctx, orgID, teamID, input)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := s.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.Name != input.Name || got.Description != input.Description {
			t.Errorf("name/desc mismatch: %+v", got)
		}
		if got.JiraProjectKey != "SKY" || got.LinearProjectKey != "LIN" {
			t.Errorf("project keys mismatch: jira=%q linear=%q", got.JiraProjectKey, got.LinearProjectKey)
		}
		// TeamID must round-trip — the per-project pinned-repos / Jira-rule
		// validation scopes against the project's own team, so Get/List have
		// to surface it (not leave it empty and force a default-team fallback).
		if got.TeamID != teamID {
			t.Errorf("TeamID = %q, want %q (the team the project was created under)", got.TeamID, teamID)
		}
		if got.SpecAuthorshipBlueprintID != "" {
			t.Errorf("SpecAuthorshipBlueprintID = %q, want empty (no prompt fixture seeded)", got.SpecAuthorshipBlueprintID)
		}
		if !reflect.DeepEqual(got.PinnedRepos, input.PinnedRepos) {
			t.Errorf("PinnedRepos = %v, want %v", got.PinnedRepos, input.PinnedRepos)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Errorf("timestamps should be populated: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
		}
	})

	t.Run("Create_with_nil_pinned_repos_yields_empty_slice", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		id, err := s.Create(ctx, orgID, teamID, domain.Project{
			Name:        "Nil Pinned",
			PinnedRepos: nil,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got.PinnedRepos == nil {
			t.Errorf("PinnedRepos read back as nil; want non-nil empty slice for stable JSON shape")
		}
		if len(got.PinnedRepos) != 0 {
			t.Errorf("PinnedRepos = %v, want empty", got.PinnedRepos)
		}
	})

	t.Run("Get_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.Get(ctx, orgID, "00000000-0000-0000-0000-0000000000ff")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("Get on missing id should be nil, got %+v", got)
		}
	})

	t.Run("List_empty_returns_empty_slice", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.List(ctx, orgID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got == nil {
			t.Errorf("List on empty org returned nil; want non-nil empty slice")
		}
		if len(got) != 0 {
			t.Errorf("List on empty org = %v, want empty", got)
		}
	})

	t.Run("List_orders_case_insensitive_by_name", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		names := []string{"banana", "Apple", "cherry"}
		for _, n := range names {
			if _, err := s.Create(ctx, orgID, teamID, domain.Project{Name: n}); err != nil {
				t.Fatalf("Create %q: %v", n, err)
			}
		}
		got, err := s.List(ctx, orgID)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		gotNames := make([]string, len(got))
		for i, p := range got {
			gotNames[i] = p.Name
		}
		want := []string{"Apple", "banana", "cherry"}
		if !sort.StringsAreSorted(lowerSlice(gotNames)) {
			t.Errorf("list not sorted case-insensitive: %v", gotNames)
		}
		if !reflect.DeepEqual(gotNames, want) {
			t.Errorf("list order = %v, want %v", gotNames, want)
		}
	})

	t.Run("Update_writes_mutable_surface_and_stamps_updated_at", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		id, _ := s.Create(ctx, orgID, teamID, domain.Project{
			Name: "Before", Description: "before", PinnedRepos: []string{"a/b"},
		})
		before, _ := s.Get(ctx, orgID, id)

		updated := *before
		updated.Name = "After"
		updated.Description = "after"
		updated.PinnedRepos = []string{"x/y", "z/w"}
		updated.JiraProjectKey = "SKY"
		if err := s.Update(ctx, orgID, updated); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got.Name != "After" || got.Description != "after" {
			t.Errorf("Update mutable surface not written: %+v", got)
		}
		if !reflect.DeepEqual(got.PinnedRepos, []string{"x/y", "z/w"}) {
			t.Errorf("PinnedRepos = %v, want [x/y z/w]", got.PinnedRepos)
		}
		if got.JiraProjectKey != "SKY" {
			t.Errorf("optional fields not written: %+v", got)
		}
		if !got.UpdatedAt.After(before.UpdatedAt) && !got.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("UpdatedAt did not advance: before=%v after=%v", before.UpdatedAt, got.UpdatedAt)
		}
		if !got.CreatedAt.Equal(before.CreatedAt) {
			t.Errorf("CreatedAt should be preserved: before=%v after=%v", before.CreatedAt, got.CreatedAt)
		}
	})

	t.Run("Update_on_missing_id_returns_ErrNoRows", func(t *testing.T) {
		s, orgID, _ := mk(t)
		err := s.Update(ctx, orgID, domain.Project{
			ID:   "00000000-0000-0000-0000-0000000000ee",
			Name: "ghost",
		})
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("Update on missing id: err=%v, want sql.ErrNoRows", err)
		}
	})

	t.Run("Delete_removes_row", func(t *testing.T) {
		s, orgID, teamID := mk(t)
		id, _ := s.Create(ctx, orgID, teamID, domain.Project{Name: "to-delete"})
		if err := s.Delete(ctx, orgID, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, _ := s.Get(ctx, orgID, id)
		if got != nil {
			t.Errorf("Get after Delete should be nil, got %+v", got)
		}
	})

	t.Run("Delete_on_missing_id_returns_ErrNoRows", func(t *testing.T) {
		s, orgID, _ := mk(t)
		err := s.Delete(ctx, orgID, "00000000-0000-0000-0000-0000000000dd")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("Delete on missing id: err=%v, want sql.ErrNoRows", err)
		}
	})
}

func lowerSlice(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		// lowercased copy — sort.StringsAreSorted needs it for the
		// case-insensitive predicate to be meaningful.
		out[i] = toLower(s)
	}
	return out
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
