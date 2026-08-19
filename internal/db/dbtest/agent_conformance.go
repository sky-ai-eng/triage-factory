package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// AgentStoreFactory is what a per-backend test file hands to
// RunAgentStoreConformance. The factory returns the wired store +
// the orgID to pass to every method + a patUserID that's safe to
// write into agents.github_pat_user_id. Postgres backend pre-seeds
// the orgs row + the owner users row (and returns the owner's UUID
// as patUserID so the github_pat_user_id FK satisfies); SQLite has
// no FK and returns any valid UUID string.
type AgentStoreFactory func(t *testing.T) (store db.AgentStore, orgID, patUserID string)

// RunAgentStoreConformance runs the shared assertion suite. What it
// covers:
//
//   - Create inserts the row + returns its id; idempotent on org_id —
//     duplicate Create returns the existing row's id without error.
//   - GetForOrg returns the row's fields verbatim.
//   - GetForOrg returns (nil, nil) when no row exists yet.
//   - Update applies display_name + default_model + autonomy +
//     jira_service_account_id.
//   - SetGitHubPATUser writes the PAT-borrow FK; malformed input is
//     refused before any UPDATE runs.
//   - SetGitHubOrgIdentity stores or clears login + email atomically.
//   - Postgres invalid-UUID guards: Update / SetGitHubPAT return nil
//     instead of bubbling 22P02 parse errors.
//   - DisplayName defaulting: empty input fills "Triage Factory Bot".
func RunAgentStoreConformance(t *testing.T, factory AgentStoreFactory) {
	t.Helper()

	t.Run("Create_FirstCallInsertsAndReturnsID", func(t *testing.T) {
		store, orgID, _ := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{
			DisplayName:  "Custom Bot Name",
			DefaultModel: "claude-opus-4-7",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id == "" {
			t.Fatal("Create returned empty id")
		}
		got, err := store.GetForOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("GetForOrg: %v", err)
		}
		if got == nil {
			t.Fatal("GetForOrg returned nil after Create")
		}
		if got.ID != id {
			t.Errorf("Create returned id=%q but GetForOrg returned id=%q", id, got.ID)
		}
		if got.DisplayName != "Custom Bot Name" {
			t.Errorf("DisplayName=%q want %q", got.DisplayName, "Custom Bot Name")
		}
		if got.DefaultModel != "claude-opus-4-7" {
			t.Errorf("DefaultModel=%q want claude-opus-4-7", got.DefaultModel)
		}
	})

	t.Run("Create_DefaultsDisplayName", func(t *testing.T) {
		store, orgID, _ := factory(t)
		_, err := store.Create(context.Background(), orgID, domain.Agent{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.GetForOrg(context.Background(), orgID)
		if err != nil || got == nil {
			t.Fatalf("GetForOrg: got=%v err=%v", got, err)
		}
		if got.DisplayName != "Triage Factory Bot" {
			t.Errorf("DisplayName=%q; want Triage Factory Bot (default)", got.DisplayName)
		}
	})

	t.Run("Create_IgnoresCallerSuppliedID", func(t *testing.T) {
		// Regression: an earlier version of both impls let a caller-
		// supplied a.ID override BootstrapAgentID(orgID). In SQLite
		// (no UNIQUE(org_id)) that created rows GetForOrg's
		// deterministic lookup couldn't reach AND let a subsequent
		// empty-ID Create insert a second row. In Postgres the custom
		// id was silently dropped on conflict, producing
		// "the id you asked for isn't the id you got" surprises.
		// Both backends now ignore a.ID and use the deterministic
		// derivation; the returned id is the only id the row will
		// ever have. This test pins that contract.
		store, orgID, _ := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{
			ID:          "00000000-1111-2222-3333-444444444444",
			DisplayName: "Custom",
		})
		if err != nil {
			t.Fatalf("Create with caller-supplied ID: %v", err)
		}
		if id == "00000000-1111-2222-3333-444444444444" {
			t.Fatal("Create returned caller's id; impl is honoring caller-supplied ID. Should ignore.")
		}
		// And GetForOrg must reach the row.
		got, err := store.GetForOrg(ctx, orgID)
		if err != nil {
			t.Fatalf("GetForOrg: %v", err)
		}
		if got == nil {
			t.Fatal("GetForOrg returned nil; caller-supplied ID created an unreachable row")
		}
		if got.ID != id {
			t.Errorf("GetForOrg returned id=%q; Create returned id=%q; contract requires they match", got.ID, id)
		}
	})

	t.Run("Create_DuplicateReturnsExistingID", func(t *testing.T) {
		// Idempotency invariant. Bootstrap may run multiple times across
		// boots; the second Create must NOT error and must NOT change
		// the existing row's id (callers persist the id elsewhere —
		// flipping it would invalidate audit traces).
		store, orgID, _ := factory(t)
		ctx := context.Background()
		first, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Original"})
		if err != nil {
			t.Fatalf("first Create: %v", err)
		}
		second, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Different"})
		if err != nil {
			t.Fatalf("duplicate Create: %v", err)
		}
		if first != second {
			t.Errorf("second Create returned new id %q; want existing %q", second, first)
		}
		// And the row should not have been overwritten — ON CONFLICT
		// DO NOTHING preserves the original display_name.
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row vanished after duplicate Create")
		}
		if got.DisplayName != "Original" {
			t.Errorf("DisplayName=%q after duplicate Create; want %q (no overwrite)", got.DisplayName, "Original")
		}
	})

	t.Run("GetForOrg_ReturnsNilWhenAbsent", func(t *testing.T) {
		store, orgID, _ := factory(t)
		got, err := store.GetForOrg(context.Background(), orgID)
		if err != nil {
			t.Fatalf("GetForOrg: %v", err)
		}
		if got != nil {
			t.Fatalf("GetForOrg on fresh store returned %+v; want nil", got)
		}
	})

	t.Run("Update_AppliesFields", func(t *testing.T) {
		store, orgID, _ := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Initial"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		autonomy := 0.85
		if err := store.Update(ctx, orgID, domain.Agent{
			ID:                         id,
			DisplayName:                "Renamed",
			DefaultModel:               "claude-sonnet-4-6",
			DefaultAutonomySuitability: &autonomy,
			JiraServiceAccountID:       "sa-jira-123",
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row vanished after Update")
		}
		if got.DisplayName != "Renamed" {
			t.Errorf("DisplayName=%q want Renamed", got.DisplayName)
		}
		if got.DefaultModel != "claude-sonnet-4-6" {
			t.Errorf("DefaultModel=%q want claude-sonnet-4-6", got.DefaultModel)
		}
		if got.DefaultAutonomySuitability == nil || !nearlyEqual(*got.DefaultAutonomySuitability, 0.85) {
			t.Errorf("DefaultAutonomySuitability=%v want 0.85", got.DefaultAutonomySuitability)
		}
		if got.JiraServiceAccountID != "sa-jira-123" {
			t.Errorf("JiraServiceAccountID=%q want sa-jira-123", got.JiraServiceAccountID)
		}
	})

	t.Run("Update_OnInvalidUUID_IsNoop", func(t *testing.T) {
		// Postgres-only constraint; SQLite TEXT keys accept anything.
		// The conformance asserts the contract (no error returned) for
		// both backends — SQLite's no-op behavior comes from "WHERE
		// id = ?" matching zero rows.
		store, orgID, _ := factory(t)
		ctx := context.Background()
		if _, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Real"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Update(ctx, orgID, domain.Agent{
			ID:          "not-a-uuid",
			DisplayName: "Hijacked",
		}); err != nil {
			t.Errorf("Update with invalid UUID: want nil, got %v", err)
		}
		// Real row untouched.
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil || got.DisplayName != "Real" {
			t.Errorf("real row corrupted by invalid-UUID Update; got=%+v", got)
		}
	})

	t.Run("SetGitHubPATUser_WritesPATUserFK", func(t *testing.T) {
		// PAT-borrow is tier 3 of the credential resolver. Start with
		// a freshly-created agent and write a real users(id) into the
		// FK; the read-back must match. Postgres needs a real
		// users.id (the factory pre-seeds one and hands the UUID back);
		// SQLite accepts any UUID-shaped string but the FK to users(id)
		// exists too.
		store, orgID, patUserID := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Test"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.SetGitHubPATUser(ctx, orgID, id, patUserID); err != nil {
			t.Fatalf("SetGitHubPATUser: %v", err)
		}
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row missing after SetGitHubPATUser")
		}
		if got.GitHubPATUserID != patUserID {
			t.Errorf("PAT user_id=%q want %q", got.GitHubPATUserID, patUserID)
		}
	})

	t.Run("SetGitHubPATUser_RejectsMalformedUserID", func(t *testing.T) {
		// Regression: an earlier version of both impls passed userID
		// through nullUUID() / nullString() unconditionally, so a
		// caller passing a non-UUID non-empty value (e.g. "alice@x.com"
		// from a typo) would silently NULL-out github_pat_user_id and
		// wipe the agent's credentials entirely. Both impls now refuse
		// malformed input loudly. Empty stays the explicit-clear path
		// because that's a legitimate state transition.
		store, orgID, patUserID := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Test"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Seed a valid PAT user first so we can prove the refused call
		// leaves it untouched.
		if err := store.SetGitHubPATUser(ctx, orgID, id, patUserID); err != nil {
			t.Fatalf("seed SetGitHubPATUser: %v", err)
		}
		err = store.SetGitHubPATUser(ctx, orgID, id, "alice@example.com")
		if err == nil {
			t.Fatal("SetGitHubPATUser accepted malformed userID; would silently wipe the credential field")
		}
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil {
			t.Fatal("row missing after refused SetGitHubPATUser")
		}
		if got.GitHubPATUserID != patUserID {
			t.Errorf("PAT user_id corrupted by refused call: got=%q want=%q", got.GitHubPATUserID, patUserID)
		}
	})

	t.Run("SetGitHubPATUser_OnInvalidAgentUUID_IsNoop", func(t *testing.T) {
		store, orgID, _ := factory(t)
		if err := store.SetGitHubPATUser(context.Background(), orgID, "not-a-uuid", uuid.New().String()); err != nil {
			t.Errorf("SetGitHubPATUser invalid agent UUID: want nil, got %v", err)
		}
	})

	t.Run("SetGitHubOrgIdentity_WritesScansAndClears", func(t *testing.T) {
		// The org credential's OWN GitHub login (TFAC-452). Free-form text,
		// NOT a UUID — the App-bot form "acme-bot[bot]" must round-trip
		// unmolested (no shape validation, distinct from the PAT-user FK).
		// Empty clears it back to NULL so an org dropping its PAT path stops
		// stamping a stale identity. Fresh agents default to empty.
		store, orgID, _ := factory(t)
		ctx := context.Background()
		id, err := store.Create(ctx, orgID, domain.Agent{DisplayName: "Test"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, _ := store.GetForOrg(ctx, orgID)
		if got == nil || got.GitHubOrgLogin != "" {
			t.Fatalf("fresh agent GitHubOrgLogin=%v, want empty", got)
		}
		if err := store.SetGitHubOrgIdentity(ctx, orgID, id, "acme-bot[bot]", "bot@example.com"); err != nil {
			t.Fatalf("SetGitHubOrgIdentity: %v", err)
		}
		got, _ = store.GetForOrg(ctx, orgID)
		if got == nil || got.GitHubOrgLogin != "acme-bot[bot]" || got.GitHubOrgEmail != "bot@example.com" {
			t.Errorf("GitHub org identity=%v want acme-bot[bot] <bot@example.com>", got)
		}
		// Setting the org login must NOT disturb the PAT-user FK or other fields.
		if got.DisplayName != "Test" {
			t.Errorf("DisplayName=%q corrupted by SetGitHubOrgIdentity", got.DisplayName)
		}
		if err := store.SetGitHubOrgIdentity(ctx, orgID, id, "", ""); err != nil {
			t.Fatalf("SetGitHubOrgIdentity clear: %v", err)
		}
		got, _ = store.GetForOrg(ctx, orgID)
		if got == nil || got.GitHubOrgLogin != "" || got.GitHubOrgEmail != "" {
			t.Errorf("GitHub org identity=%v after clear, want empty", got)
		}

		partials := []struct {
			name  string
			login string
			email string
		}{
			{name: "login_only", login: "acme-bot[bot]"},
			{name: "email_only", email: "bot@example.com"},
		}
		for _, partial := range partials {
			t.Run(partial.name, func(t *testing.T) {
				if err := store.SetGitHubOrgIdentity(ctx, orgID, id, "acme-bot[bot]", "bot@example.com"); err != nil {
					t.Fatalf("seed full identity: %v", err)
				}
				if err := store.SetGitHubOrgIdentity(ctx, orgID, id, partial.login, partial.email); err != nil {
					t.Fatalf("SetGitHubOrgIdentity partial: %v", err)
				}
				got, err := store.GetForOrg(ctx, orgID)
				if err != nil {
					t.Fatalf("GetForOrg: %v", err)
				}
				if got == nil || got.GitHubOrgLogin != "" || got.GitHubOrgEmail != "" {
					t.Errorf("GitHub org identity=%v after partial write, want both fields empty", got)
				}
			})
		}
	})

	t.Run("SetGitHubOrgIdentity_OnInvalidAgentUUID_IsNoop", func(t *testing.T) {
		store, orgID, _ := factory(t)
		if err := store.SetGitHubOrgIdentity(context.Background(), orgID, "not-a-uuid", "octocat", "octocat@example.com"); err != nil {
			t.Errorf("SetGitHubOrgIdentity invalid agent UUID: want nil, got %v", err)
		}
	})
}
