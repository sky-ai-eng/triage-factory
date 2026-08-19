package dbtest

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SettingsStoresFactory hands the conformance suite a wired bundle of
// settings stores + the IDs (orgID, teamID, userID) to address every
// method against. Per-backend tests own row seeding (orgs / teams /
// users / org_memberships / memberships) — the conformance harness is
// schema-blind.
type SettingsStoresFactory func(t *testing.T) (stores SettingsStores, ids SettingsIDs)

// SettingsStores is the slice of stores the conformance suite exercises.
type SettingsStores struct {
	Orgs             db.OrgsStore
	Teams            db.TeamsStore
	Users            db.UsersStore
	JiraStatusRules  db.JiraStatusRulesStore
	TeamGitHubGroups db.TeamGitHubGroupsStore
	TeamGitHubRepos  db.TeamGitHubReposStore
}

// SettingsIDs are the tenancy keys the factory pre-seeded.
type SettingsIDs struct {
	OrgID  string
	TeamID string
	UserID string
}

// RunSettingsStoresConformance is the shared assertion suite. It covers:
//   - Empty-row reads return zero-value structs (and nil errors), so
//     callers can treat "no row yet" identically to "default values".
//   - Round-trip every field on OrgSettings/TeamSettings via the
//     `...System` reader (admin pool — bypasses RLS, isolates the
//     conformance from RLS test coverage which lives in the
//     per-backend test files).
//   - GitHubCloneProtocol defaulting: "" upserts as "ssh" (the column
//     CHECK rejects empty strings on both backends).
//   - JiraStatusRulesStore.ReplaceForTeam bulk-replace semantics:
//     upsert wins on conflict, missing project keys get pruned.
//   - UserSettings round-trip is a no-op probe (v1 has no user fields)
//     that still upserts a row so future tests can assert presence.
func RunSettingsStoresConformance(t *testing.T, factory SettingsStoresFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("OrgSettings_RoundTripsEveryField", func(t *testing.T) {
		stores, ids := factory(t)
		want := domain.OrgSettings{
			GitHubBaseURL:         "https://ghe.example.com",
			GitHubPollInterval:    7 * time.Minute,
			GitHubCloneProtocol:   "https",
			JiraBaseURL:           "https://acme.atlassian.net",
			JiraPollInterval:      3 * time.Minute,
			AnthropicAPIKeyRef:    "vault://orgs/A/anthropic",
			BedrockCredentialsRef: "vault://orgs/A/bedrock",
			MaxLLMModelTier:       "sonnet",
			MaxDailyCostUSD:       12.50,
			MaxConcurrentRuns:     8,
			MarketplaceEnabled:    true,
			// Read-only through this struct: UpdateSettings doesn't own
			// github_credential_class, so the row keeps its schema default and
			// the read hands it back. Stated as the expected value rather than
			// left zero, because "" is not something the store can return.
			GitHubCredentialClass: domain.GitHubCredentialClassPAT,
			// Read-only through this struct too, and set by the write rather
			// than carried into it: the first save materializes the row at
			// version 1.
			Version: 1,
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, want); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("OrgSettings round-trip mismatch\n got: %+v\nwant: %+v", got, want)
		}
	})

	// TFAC-535: marketplace_enabled is a plain NOT NULL DEFAULT false column
	// (no NULL-round-trip subtlety like the nullable fields above) — pin the
	// explicit true→false toggle since RoundTripsEveryField only exercises
	// true once.
	t.Run("OrgSettings_MarketplaceEnabled_Toggles", func(t *testing.T) {
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		on := base
		on.MarketplaceEnabled = true
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, on); err != nil {
			t.Fatalf("UpdateSettings (on): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (on): %v", err)
		}
		if !got.MarketplaceEnabled {
			t.Errorf("MarketplaceEnabled = false after enabling, want true")
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (off): %v", err)
		}
		got, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (off): %v", err)
		}
		if got.MarketplaceEnabled {
			t.Errorf("MarketplaceEnabled = true after disabling, want false")
		}
	})

	// github_credential_class names which credential system an org's GitHub
	// access belongs to. It is owned by the credential transitions, not by the
	// settings writer, and this case pins both halves of that: the dedicated
	// writer round-trips, and a bulk settings save leaves what it wrote alone.
	//
	// The second half is the one that matters. Adding the column to
	// UpdateSettings' upsert lists looks like tidiness and would instead reset
	// the class to the struct's zero value on every settings save — silently
	// converting a BYO-App org to PAT, with no error and nothing in the log.
	// That is the specific regression this case exists to catch, on both
	// backends.
	t.Run("OrgSettings_GitHubCredentialClass_OwnedByTransitionsNotSettingsSave", func(t *testing.T) {
		stores, ids := factory(t)

		// A fresh org is in the PAT system — the column's schema default, which
		// GetSettings must surface rather than an empty string.
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (fresh): %v", err)
		}
		if got.GitHubCredentialClass != domain.GitHubCredentialClassPAT {
			t.Errorf("fresh org class = %q; want %q (the column default)", got.GitHubCredentialClass, domain.GitHubCredentialClassPAT)
		}

		// The dedicated writer round-trips, materializing the row from schema
		// defaults if the org has none yet.
		if err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassBYOApp); err != nil {
			t.Fatalf("SetGitHubCredentialClass: %v", err)
		}
		got, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (after set): %v", err)
		}
		if got.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
			t.Fatalf("after SetGitHubCredentialClass, class = %q; want %q", got.GitHubCredentialClass, domain.GitHubCredentialClassBYOApp)
		}

		// THE NEGATIVE SPACE. A bulk settings save — the shape a
		// POST /api/settings/org handler produces, read-modify-write over the
		// whole struct — changes an unrelated field and must leave the class
		// exactly as the transition wrote it.
		save := got
		save.GitHubBaseURL = "https://ghe.example.com"
		save.MaxConcurrentRuns = 4
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, save); err != nil {
			t.Fatalf("UpdateSettings (bulk save): %v", err)
		}
		after, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (after bulk save): %v", err)
		}
		if after.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
			t.Errorf("a settings save reset the credential class to %q; want %q preserved — the column belongs to the credential transitions, not to UpdateSettings",
				after.GitHubCredentialClass, domain.GitHubCredentialClassBYOApp)
		}
		if after.GitHubBaseURL != "https://ghe.example.com" || after.MaxConcurrentRuns != 4 {
			t.Errorf("the settings save didn't apply its own fields: base=%q concurrent=%d", after.GitHubBaseURL, after.MaxConcurrentRuns)
		}

		// A save that explicitly carries a DIFFERENT class in the struct is
		// still ignored — the field is read-only through this writer, so a
		// caller cannot move an org between credential systems by saving
		// settings. This is the same assertion from the attacker's side.
		save = after
		save.GitHubCredentialClass = domain.GitHubCredentialClassPAT
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, save); err != nil {
			t.Fatalf("UpdateSettings (class in struct): %v", err)
		}
		after, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (after class in struct): %v", err)
		}
		if after.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
			t.Errorf("UpdateSettings honoured the struct's class (%q); want %q — the column must be unreachable through the settings writer",
				after.GitHubCredentialClass, domain.GitHubCredentialClassBYOApp)
		}

		// And the transition can move it back, so the column isn't write-once.
		if err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassPAT); err != nil {
			t.Fatalf("SetGitHubCredentialClass (back to pat): %v", err)
		}
		after, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (after switch back): %v", err)
		}
		if after.GitHubCredentialClass != domain.GitHubCredentialClassPAT {
			t.Errorf("class after switch-back = %q; want %q", after.GitHubCredentialClass, domain.GitHubCredentialClassPAT)
		}
	})

	t.Run("OrgSettings_EmptyRow_ReturnsDefaults", func(t *testing.T) {
		// Provisioning seeds org_settings at org-create time; tests
		// that build raw DBs without going through provisioning hit
		// the GetSettings* fallback, which materializes
		// domain.DefaultOrgSettings() (matching the schema DEFAULT
		// clauses). Pins that contract so the wire shape doesn't
		// regress to "0s" poll intervals on a missing-row read.
		stores, ids := factory(t)
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem on empty row: %v", err)
		}
		want := domain.DefaultOrgSettings()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GetSettingsSystem on empty row = %+v; want %+v", got, want)
		}
	})

	t.Run("OrgSettings_EmptyCloneProtocol_DefaultsToSSH", func(t *testing.T) {
		// The github_clone_protocol column CHECK rejects empty string —
		// UpdateSettings substitutes "ssh" so a fresh-mutate caller
		// doesn't have to know the column constraint.
		stores, ids := factory(t)
		in := domain.OrgSettings{
			GitHubPollInterval: 5 * time.Minute,
			JiraPollInterval:   5 * time.Minute,
			// GitHubCloneProtocol intentionally empty.
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.GitHubCloneProtocol != "ssh" {
			t.Errorf("GitHubCloneProtocol=%q; want \"ssh\" (default substitution)", got.GitHubCloneProtocol)
		}
	})

	t.Run("OrgSettings_NullableFields_RoundTripEmpty", func(t *testing.T) {
		// AnthropicAPIKeyRef / BedrockCredentialsRef / MaxLLMModelTier /
		// GitHubBaseURL / JiraBaseURL: empty input writes NULL, scans
		// back as "". MaxDailyCostUSD: 0 input writes NULL, scans back as 0
		// (TFAC-477's "no cap" round-trip). MaxConcurrentRuns: 0 input writes
		// NULL, scans back as 0 ("unlimited"). Pins the ""/0 ↔ NULL contract.
		stores, ids := factory(t)
		in := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.GitHubBaseURL != "" || got.JiraBaseURL != "" ||
			got.AnthropicAPIKeyRef != "" || got.BedrockCredentialsRef != "" ||
			got.MaxLLMModelTier != "" || got.MaxDailyCostUSD != 0 ||
			got.MaxConcurrentRuns != 0 {
			t.Errorf("nullable empties did not round-trip: %+v", got)
		}
	})

	t.Run("OrgSettings_DailyCostCap_SetThenClear", func(t *testing.T) {
		// TFAC-477: a positive daily cap round-trips, and writing 0 clears it
		// back to "no cap" (0 ↔ NULL). The set/clear cycle on the same row
		// proves the column isn't write-once.
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		set := base
		set.MaxDailyCostUSD = 25
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, set); err != nil {
			t.Fatalf("UpdateSettings (set cap): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after set: %v", err)
		}
		if got.MaxDailyCostUSD != 25 {
			t.Errorf("after set, MaxDailyCostUSD = %v; want 25", got.MaxDailyCostUSD)
		}
		// Clear: 0 writes NULL, reads back 0.
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (clear cap): %v", err)
		}
		got, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after clear: %v", err)
		}
		if got.MaxDailyCostUSD != 0 {
			t.Errorf("after clear, MaxDailyCostUSD = %v; want 0 (no cap)", got.MaxDailyCostUSD)
		}
	})

	t.Run("OrgSettings_ConcurrentRunsLimit_SetThenClear", func(t *testing.T) {
		// A positive concurrent-run limit round-trips, and writing 0 clears it
		// back to "unlimited" (0 ↔ NULL). The set/clear cycle on the same row
		// proves the column isn't write-once — the integer sibling of the daily
		// cost-cap test above.
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		set := base
		set.MaxConcurrentRuns = 12
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, set); err != nil {
			t.Fatalf("UpdateSettings (set limit): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after set: %v", err)
		}
		if got.MaxConcurrentRuns != 12 {
			t.Errorf("after set, MaxConcurrentRuns = %v; want 12", got.MaxConcurrentRuns)
		}
		// Clear: 0 writes NULL, reads back 0.
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (clear limit): %v", err)
		}
		got, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after clear: %v", err)
		}
		if got.MaxConcurrentRuns != 0 {
			t.Errorf("after clear, MaxConcurrentRuns = %v; want 0 (unlimited)", got.MaxConcurrentRuns)
		}
	})

	t.Run("OrgSettings_ConcurrentRunsLimit_NegativeScansAsUnlimited", func(t *testing.T) {
		// No DB CHECK guards max_concurrent_runs, so a negative could reach the
		// column (a migration, a manual write). Everything downstream reads
		// <= 0 as unlimited; the store read must agree, surfacing a stored
		// negative as 0 rather than handing the settings UI a value it rejects.
		stores, ids := factory(t)
		in := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
			MaxConcurrentRuns:   -7,
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
			t.Fatalf("UpdateSettings (negative): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.MaxConcurrentRuns != 0 {
			t.Errorf("stored negative MaxConcurrentRuns scanned as %v; want 0 (clamped to unlimited)", got.MaxConcurrentRuns)
		}
	})

	t.Run("OrgSettings_Update_Overwrites", func(t *testing.T) {
		stores, ids := factory(t)
		first := domain.OrgSettings{
			GitHubBaseURL:       "https://first.example.com",
			GitHubPollInterval:  5 * time.Minute,
			GitHubCloneProtocol: "ssh",
			JiraPollInterval:    5 * time.Minute,
			MaxLLMModelTier:     "haiku",
			MaxDailyCostUSD:     5,
			MaxConcurrentRuns:   3,
			// Not written by UpdateSettings; the row's default reads back.
			GitHubCredentialClass: domain.GitHubCredentialClassPAT,
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, first); err != nil {
			t.Fatalf("first UpdateSettings: %v", err)
		}
		second := first
		second.GitHubBaseURL = "https://second.example.com"
		second.MaxLLMModelTier = "opus"
		second.MaxDailyCostUSD = 10
		second.MaxConcurrentRuns = 20
		// Two saves, so the row's concurrency token has been bumped twice. The
		// struct's own Version is ignored on the way in — stating it here
		// describes what the read must hand back.
		second.Version = 2
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, second); err != nil {
			t.Fatalf("second UpdateSettings: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if !reflect.DeepEqual(got, second) {
			t.Errorf("after re-Update, GetSettings = %+v; want %+v", got, second)
		}
	})

	// The optimistic-concurrency contract behind PATCH /api/orgs/{id}/settings.
	// The settings save is a read-modify-write over the whole row, so two
	// admins editing different sections of one page would otherwise silently
	// overwrite each other. The guard has to live in the statement: both
	// dialects run READ COMMITTED, so a re-read inside the loser's own
	// transaction cannot see the winner's commit and a Go-side comparison
	// would pass for both.
	t.Run("OrgSettings_Versioned_GuardsTheLoser", func(t *testing.T) {
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}

		// No row yet: version 0 is the "materialize it" assertion, and it
		// lands at 1.
		first := base
		first.GitHubBaseURL = "https://first.example.com"
		if err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, first, 0); err != nil {
			t.Fatalf("UpdateSettingsVersioned (insert): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.Version != 1 {
			t.Fatalf("version after insert = %d; want 1", got.Version)
		}

		// The winner reads 1, writes at 1, lands at 2.
		winner := base
		winner.GitHubBaseURL = "https://winner.example.com"
		if err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, winner, 1); err != nil {
			t.Fatalf("UpdateSettingsVersioned (winner): %v", err)
		}

		// The loser read 1 too — its write is refused, and refused WITHOUT
		// writing: the winner's field is still what a refetch shows.
		loser := base
		loser.GitHubBaseURL = "https://loser.example.com"
		err = stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, loser, 1)
		if !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Fatalf("stale UpdateSettingsVersioned err = %v; want ErrOrgSettingsVersion", err)
		}
		got, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (after loser): %v", err)
		}
		if got.GitHubBaseURL != "https://winner.example.com" {
			t.Errorf("after the refused write, base URL = %q; want the winner's value preserved", got.GitHubBaseURL)
		}
		if got.Version != 2 {
			t.Errorf("version after one winner = %d; want 2 (the refused write must not bump it)", got.Version)
		}

		// A version assertion must never CREATE. Asserting version 0 against a
		// row that now exists is the create-vs-create race, and it loses.
		err = stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, loser, 0)
		if !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Errorf("asserting version 0 against an existing row = %v; want ErrOrgSettingsVersion", err)
		}

		// Re-reading and re-applying is the loser's whole remedy, so it has to
		// work on the next attempt.
		if err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, loser, got.Version); err != nil {
			t.Fatalf("UpdateSettingsVersioned (loser retry): %v", err)
		}
		got, err = stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (after retry): %v", err)
		}
		if got.GitHubBaseURL != "https://loser.example.com" || got.Version != 3 {
			t.Errorf("after retry: base=%q version=%d; want the loser's value at version 3", got.GitHubBaseURL, got.Version)
		}
	})

	// A version assertion is an assertion about a row that EXISTS, and it must
	// never quietly become a create. The guard can only ride an upsert's
	// conflict arm, so a single guarded upsert lets a caller asserting a stale
	// non-zero version against a since-deleted row fall through to the INSERT
	// and materialize it at version 1 — a create reported as a successful
	// update, with the caller's whole form written over a row it never read.
	t.Run("OrgSettings_Versioned_NonZeroExpectedNeverCreates", func(t *testing.T) {
		stores, ids := factory(t)
		row := domain.OrgSettings{
			GitHubBaseURL:       "https://ghost.example.com",
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}

		// No row yet, and the caller claims to have read one at version 3.
		err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, row, 3)
		if !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Fatalf("asserting version 3 against a missing row = %v; want ErrOrgSettingsVersion", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.Version != 0 || got.GitHubBaseURL != "" {
			t.Errorf("the refused assertion created a row: %+v", got)
		}
	})

	// The unguarded writer is the credential transitions' path and stays
	// last-writer-wins — but it must still move the token, or an admin's stale
	// settings edit would land on top of a credential change it never saw.
	t.Run("OrgSettings_UnversionedWrite_StillBumpsTheToken", func(t *testing.T) {
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (first): %v", err)
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (second): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.Version != 2 {
			t.Errorf("version after two unguarded saves = %d; want 2", got.Version)
		}
		if err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, base, 1); !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Errorf("a settings write at the pre-transition version = %v; want ErrOrgSettingsVersion", err)
		}
	})

	// SetGitHubCredentialClass is the counter-example: a surgical single-column
	// write that does NOT move the token, so a credential transition can't
	// invalidate an admin's in-flight settings edit.
	t.Run("OrgSettings_CredentialClassWrite_LeavesTheTokenAlone", func(t *testing.T) {
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		if err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		if err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassBYOApp); err != nil {
			t.Fatalf("SetGitHubCredentialClass: %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.Version != 1 {
			t.Errorf("version after a class write = %d; want 1 (unchanged)", got.Version)
		}
	})

	t.Run("TeamSettings_RoundTripsEveryField", func(t *testing.T) {
		stores, ids := factory(t)
		want := domain.TeamSettings{
			JiraProjects:                    []string{"SKY", "ENG", "OPS"},
			AIReprioritizeThreshold:         7,
			AIPreferenceUpdateInterval:      30,
			DefaultModel:                    "opus",
			AutoDelegateEnabled:             true,
			AutoModeEnabled:                 false,
			PermissionAbsentGraceMS:         30000,
			PermissionAbsentAutodenyEnabled: false,
			MaxDailyCostUSD:                 12.50, // TFAC-482 per-team daily cap
			BranchTemplate:                  "team/<ticket-id>-wip",
			ReviewPosture:                   domain.ReviewPostureAutoUnlessBlocking,
			BaseBranchPushPolicy:            domain.BaseBranchPushManualOnly,
		}
		if err := stores.Teams.UpdateSettings(ctx, ids.TeamID, want); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("TeamSettings round-trip mismatch\n got: %+v\nwant: %+v", got, want)
		}
	})

	// SetDailyCostCapSystem is the org-admin write path for the per-team daily
	// spend cap (TFAC-482): surgical (touches only the cap), creates the row from
	// schema defaults when none exists, and the team-settings read-modify-write
	// path preserves it (a team admin can't change the cap). ≤0 clears it (NULL).
	t.Run("TeamSettings_DailyCostCap_SetClearAndPreserve", func(t *testing.T) {
		stores, ids := factory(t)

		// Fresh team, no settings row yet → the partial INSERT must materialize the
		// row from defaults + the cap, touching only max_daily_cost_usd.
		if err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 42.50); err != nil {
			t.Fatalf("SetDailyCostCapSystem (insert): %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		want := domain.DefaultTeamSettings()
		want.MaxDailyCostUSD = 42.50
		// A materialized row reads jira_projects back as an empty (non-nil) slice,
		// whereas DefaultTeamSettings leaves it nil — normalize so DeepEqual checks
		// the fields that matter, not the nil-vs-empty distinction.
		want.JiraProjects = []string{}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after SetDailyCostCapSystem on a fresh team\n got: %+v\nwant: %+v (defaults + cap)", got, want)
		}

		// A read-modify-write team-settings save that changes another field but not
		// the cap must preserve it — the team-admin path can never alter the cap.
		got.DefaultModel = "haiku"
		if err := stores.Teams.UpdateSettings(ctx, ids.TeamID, got); err != nil {
			t.Fatalf("UpdateSettings (rmw): %v", err)
		}
		after, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after rmw: %v", err)
		}
		if after.MaxDailyCostUSD != 42.50 {
			t.Errorf("UpdateSettings clobbered the cap: got %v, want 42.50 preserved", after.MaxDailyCostUSD)
		}
		if after.DefaultModel != "haiku" {
			t.Errorf("UpdateSettings didn't apply the unrelated change: got %q, want haiku", after.DefaultModel)
		}

		// ≤ 0 clears the cap (stored NULL → read back as 0 / no cap).
		if err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 0); err != nil {
			t.Fatalf("SetDailyCostCapSystem (clear): %v", err)
		}
		cleared, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after clear: %v", err)
		}
		if cleared.MaxDailyCostUSD != 0 {
			t.Errorf("cleared cap = %v, want 0 (NULL)", cleared.MaxDailyCostUSD)
		}
	})

	t.Run("TeamSettings_EmptyRow_ReturnsDefaults", func(t *testing.T) {
		// Same fallback contract as OrgSettings — provisioning seeds
		// the row in production, but tests that bypass it should
		// still see populated defaults (sonnet model, 5/20 thresholds,
		// auto_delegate=true) rather than the Go zero value.
		stores, ids := factory(t)
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem on empty row: %v", err)
		}
		want := domain.DefaultTeamSettings()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GetSettingsSystem on empty row = %+v; want %+v", got, want)
		}
	})

	t.Run("TeamSettings_NilProjects_RoundTripsEmpty", func(t *testing.T) {
		// JiraProjects=nil writes []; reads back as []. Keeps "no
		// projects configured" stable for downstream callers.
		stores, ids := factory(t)
		in := domain.TeamSettings{
			AIReprioritizeThreshold:    5,
			AIPreferenceUpdateInterval: 20,
			DefaultModel:               "sonnet",
		}
		if err := stores.Teams.UpdateSettings(ctx, ids.TeamID, in); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if len(got.JiraProjects) != 0 {
			t.Errorf("JiraProjects=%v; want empty slice", got.JiraProjects)
		}
	})

	t.Run("UserSettings_TouchAndRead", func(t *testing.T) {
		// v1 user_settings has no fields — call exercises the upsert
		// path + read-back contract so future per-user prefs land on
		// established wiring.
		stores, ids := factory(t)
		if err := stores.Users.UpdateSettings(ctx, ids.UserID, domain.UserSettings{}); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		got, err := stores.Users.GetSettings(ctx, ids.UserID)
		if err != nil {
			t.Fatalf("GetSettings: %v", err)
		}
		var zero domain.UserSettings
		if !reflect.DeepEqual(got, zero) {
			t.Errorf("UserSettings = %+v; want zero value", got)
		}
	})

	t.Run("UserSettings_AbsentRow_ReturnsZeroValue", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.Users.GetSettings(ctx, ids.UserID)
		if err != nil {
			t.Fatalf("GetSettings on absent row: %v", err)
		}
		var zero domain.UserSettings
		if !reflect.DeepEqual(got, zero) {
			t.Errorf("GetSettings on absent row = %+v; want zero value", got)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_UpsertsRows", func(t *testing.T) {
		stores, ids := factory(t)
		input := []domain.JiraProjectStatusRules{
			{
				ProjectKey:          "SKY",
				PickupMembers:       []string{"To Do", "Backlog"},
				InProgressMembers:   []string{"In Progress"},
				InProgressCanonical: "In Progress",
				DoneMembers:         []string{"Done"},
				DoneCanonical:       "Done",
			},
			{
				ProjectKey:          "ENG",
				PickupMembers:       []string{"Open"},
				InProgressMembers:   []string{"Doing"},
				InProgressCanonical: "Doing",
				DoneMembers:         []string{"Closed"},
				DoneCanonical:       "Closed",
			},
		}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, input); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		// List* reads populate TeamID (the PK's first column);
		// ReplaceForTeam takes the team as a parameter and ignores the
		// struct field, so set the expected TeamID before the round-trip
		// compare.
		want := make([]domain.JiraProjectStatusRules, len(input))
		copy(want, input)
		for i := range want {
			want[i].TeamID = ids.TeamID
		}
		sortRulesByKey(got)
		sortRulesByKey(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after ReplaceForTeam, ListForTeam = %+v; want %+v", got, want)
		}
	})

	t.Run("JiraStatusRules_TeamID_And_OrgUnion_RoundTrip", func(t *testing.T) {
		// TeamID populates on every List* read, ListForOrgSystem
		// returns the org-wide union (every team's rows), and
		// TracksProjectSystem answers the router gate.
		stores, ids := factory(t)
		input := []domain.JiraProjectStatusRules{{
			ProjectKey: "SKY", PickupMembers: []string{"To Do"},
			InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
			DoneMembers: []string{"Done"}, DoneCanonical: "Done",
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, input); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}

		// ListForTeam[System] populate TeamID.
		for _, got := range [][]domain.JiraProjectStatusRules{
			mustListJira(t, func() ([]domain.JiraProjectStatusRules, error) {
				return stores.JiraStatusRules.ListForTeam(ctx, ids.TeamID)
			}),
			mustListJira(t, func() ([]domain.JiraProjectStatusRules, error) {
				return stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
			}),
		} {
			if len(got) != 1 || got[0].TeamID != ids.TeamID {
				t.Errorf("List* TeamID = %+v; want one row with TeamID=%q", got, ids.TeamID)
			}
		}

		// ListForOrgSystem returns the union (here, the single team's row)
		// with TeamID set.
		union, err := stores.JiraStatusRules.ListForOrgSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		if len(union) != 1 || union[0].ProjectKey != "SKY" || union[0].TeamID != ids.TeamID {
			t.Errorf("ListForOrgSystem = %+v; want one SKY row with TeamID=%q", union, ids.TeamID)
		}

		// TracksProjectSystem: tracked vs untracked.
		for _, c := range []struct {
			project string
			want    bool
		}{{"SKY", true}, {"OPS", false}} {
			got, err := stores.JiraStatusRules.TracksProjectSystem(ctx, ids.TeamID, c.project)
			if err != nil {
				t.Fatalf("TracksProjectSystem(%s): %v", c.project, err)
			}
			if got != c.want {
				t.Errorf("TracksProjectSystem(%s) = %v; want %v", c.project, got, c.want)
			}
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_PrunesMissingKeys", func(t *testing.T) {
		// First insert two rules, then re-call with just one — the
		// missing project_key row must be deleted.
		stores, ids := factory(t)
		two := []domain.JiraProjectStatusRules{
			{
				ProjectKey: "SKY", PickupMembers: []string{"To Do"},
				InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
				DoneMembers: []string{"Done"}, DoneCanonical: "Done",
			},
			{
				ProjectKey: "ENG", PickupMembers: []string{"Open"},
				InProgressMembers: []string{"Doing"}, InProgressCanonical: "Doing",
				DoneMembers: []string{"Closed"}, DoneCanonical: "Closed",
			},
		}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, two); err != nil {
			t.Fatalf("seed ReplaceForTeam: %v", err)
		}
		one := two[:1]
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, one); err != nil {
			t.Fatalf("prune ReplaceForTeam: %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].ProjectKey != "SKY" {
			t.Errorf("after prune ReplaceForTeam, got=%+v; want one row keyed SKY", got)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_EmptyClearsAll", func(t *testing.T) {
		stores, ids := factory(t)
		seed := []domain.JiraProjectStatusRules{{
			ProjectKey: "SKY", PickupMembers: []string{"To Do"},
			InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
			DoneMembers: []string{"Done"}, DoneCanonical: "Done",
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, seed); err != nil {
			t.Fatalf("seed ReplaceForTeam: %v", err)
		}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, nil); err != nil {
			t.Fatalf("empty ReplaceForTeam: %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("after empty ReplaceForTeam, got=%+v; want empty slice", got)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_EmptyProjectKeyRefused", func(t *testing.T) {
		// Regression guard: a rules slice carrying an empty ProjectKey
		// must be refused outright. Silently skipping the row in the
		// upsert loop AND filtering it out of the prune list would
		// turn an all-empty input into a stealth clear-all, sharply
		// different behavior from the "nil/empty clears" contract
		// callers actually mean. Pre-validation also short-circuits
		// before any tx work, so partial writes can't happen.
		stores, ids := factory(t)
		// Seed a baseline rule so we can prove it survives the
		// refused call.
		seed := []domain.JiraProjectStatusRules{{
			ProjectKey: "SKY", PickupMembers: []string{"To Do"},
			InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
			DoneMembers: []string{"Done"}, DoneCanonical: "Done",
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, seed); err != nil {
			t.Fatalf("seed ReplaceForTeam: %v", err)
		}
		bad := []domain.JiraProjectStatusRules{{
			ProjectKey: "", PickupMembers: []string{"x"},
			InProgressMembers: []string{"y"}, InProgressCanonical: "y",
			DoneMembers: []string{"z"}, DoneCanonical: "z",
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, bad); err == nil {
			t.Error("ReplaceForTeam accepted empty ProjectKey; expected error")
		}
		// Original row survives — no partial writes / stealth clear.
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].ProjectKey != "SKY" {
			t.Errorf("after refused call, rules=%+v; want one SKY row preserved", got)
		}
	})

	t.Run("JiraStatusRules_EmptyTeam_ReturnsEmptySlice", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ListForTeamSystem on empty team = %+v; want empty slice", got)
		}
	})

	t.Run("TeamGitHubGroups_SetForTeam_RoundTrips", func(t *testing.T) {
		stores, ids := factory(t)
		input := []domain.TeamGitHubGroup{
			{OrgLogin: "acme", TeamSlug: "backend"},
			{OrgLogin: "acme", TeamSlug: "frontend"},
		}
		if err := stores.TeamGitHubGroups.SetForTeam(ctx, ids.TeamID, input); err != nil {
			t.Fatalf("SetForTeam: %v", err)
		}
		got, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		sortGroups(got)
		sortGroups(input)
		if !reflect.DeepEqual(got, input) {
			t.Errorf("after SetForTeam, ListForTeamSystem = %+v; want %+v", got, input)
		}
	})

	t.Run("TeamGitHubGroups_SetForTeam_NormalizesAndDedups", func(t *testing.T) {
		// Mixed case + duplicate + surrounding whitespace all collapse to
		// one lowercase row, so routing lookups match regardless of input.
		stores, ids := factory(t)
		input := []domain.TeamGitHubGroup{
			{OrgLogin: "Acme", TeamSlug: "Backend"},
			{OrgLogin: " acme ", TeamSlug: " backend "},
		}
		if err := stores.TeamGitHubGroups.SetForTeam(ctx, ids.TeamID, input); err != nil {
			t.Fatalf("SetForTeam: %v", err)
		}
		got, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		want := []domain.TeamGitHubGroup{{OrgLogin: "acme", TeamSlug: "backend"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("normalized groups = %+v; want %+v", got, want)
		}
		// Case-insensitive routing lookup resolves the team.
		teams, err := stores.TeamGitHubGroups.TeamsForGroupSystem(ctx, ids.OrgID, "ACME", "BACKEND")
		if err != nil {
			t.Fatalf("TeamsForGroupSystem: %v", err)
		}
		if len(teams) != 1 || teams[0] != ids.TeamID {
			t.Errorf("TeamsForGroupSystem = %v; want [%s]", teams, ids.TeamID)
		}
	})

	t.Run("TeamGitHubGroups_SetForTeam_ReplaceSetPrunes", func(t *testing.T) {
		stores, ids := factory(t)
		two := []domain.TeamGitHubGroup{
			{OrgLogin: "acme", TeamSlug: "backend"},
			{OrgLogin: "acme", TeamSlug: "frontend"},
		}
		if err := stores.TeamGitHubGroups.SetForTeam(ctx, ids.TeamID, two); err != nil {
			t.Fatalf("seed SetForTeam: %v", err)
		}
		if err := stores.TeamGitHubGroups.SetForTeam(ctx, ids.TeamID, two[:1]); err != nil {
			t.Fatalf("replace SetForTeam: %v", err)
		}
		got, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].TeamSlug != "backend" {
			t.Errorf("after replace-set, got=%+v; want one row slug=backend", got)
		}
		// Empty clears all.
		if err := stores.TeamGitHubGroups.SetForTeam(ctx, ids.TeamID, nil); err != nil {
			t.Fatalf("clear SetForTeam: %v", err)
		}
		got, err = stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("after clear, got=%+v; want empty", got)
		}
	})

	t.Run("TeamGitHubGroups_PruneMissingSystem_DropsDeletedSlugs", func(t *testing.T) {
		stores, ids := factory(t)
		seed := []domain.TeamGitHubGroup{
			{OrgLogin: "acme", TeamSlug: "backend"},
			{OrgLogin: "acme", TeamSlug: "frontend"},
			{OrgLogin: "beta", TeamSlug: "platform"},
		}
		if err := stores.TeamGitHubGroups.SetForTeam(ctx, ids.TeamID, seed); err != nil {
			t.Fatalf("seed SetForTeam: %v", err)
		}
		// "frontend" was deleted on GitHub — only "backend" remains under
		// acme. The prune is scoped to the acme login, so beta/platform
		// is untouched.
		n, err := stores.TeamGitHubGroups.PruneMissingSystem(ctx, ids.OrgID, "acme", []string{"backend"})
		if err != nil {
			t.Fatalf("PruneMissingSystem: %v", err)
		}
		if n != 1 {
			t.Errorf("PruneMissingSystem removed %d rows; want 1", n)
		}
		got, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		want := []domain.TeamGitHubGroup{
			{OrgLogin: "acme", TeamSlug: "backend"},
			{OrgLogin: "beta", TeamSlug: "platform"},
		}
		sortGroups(got)
		sortGroups(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after prune, groups=%+v; want %+v", got, want)
		}
		// Empty present-set clears every acme mapping (org has no acme
		// teams left); beta survives.
		if _, err := stores.TeamGitHubGroups.PruneMissingSystem(ctx, ids.OrgID, "acme", nil); err != nil {
			t.Fatalf("PruneMissingSystem (clear): %v", err)
		}
		got, err = stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].OrgLogin != "beta" {
			t.Errorf("after clear-acme prune, groups=%+v; want only beta/platform", got)
		}
	})

	t.Run("TeamGitHubGroups_EmptyTeam_ReturnsEmptySlice", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ListForTeamSystem on empty team = %+v; want empty slice", got)
		}
	})

	t.Run("TeamGitHubRepos_ReplaceForTeam_RoundTripsAndPrunes", func(t *testing.T) {
		stores, ids := factory(t)
		input := []domain.TeamGitHubRepo{
			{Owner: "acme", Repo: "api"},
			{Owner: "acme", Repo: "web"},
		}
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, ids.OrgID, ids.TeamID, input); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		got, err := stores.TeamGitHubRepos.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		sortRepos(got)
		sortRepos(input)
		if !reflect.DeepEqual(got, input) {
			t.Errorf("after ReplaceForTeam, got=%+v; want %+v", got, input)
		}

		// Replace-set prunes the missing one.
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, ids.OrgID, ids.TeamID, input[:1]); err != nil {
			t.Fatalf("replace ReplaceForTeam: %v", err)
		}
		got, err = stores.TeamGitHubRepos.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 || got[0].Repo != "api" {
			t.Errorf("after replace-set, got=%+v; want one row repo=api", got)
		}

		// Empty clears all.
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, ids.OrgID, ids.TeamID, nil); err != nil {
			t.Fatalf("clear ReplaceForTeam: %v", err)
		}
		got, err = stores.TeamGitHubRepos.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("after clear, got=%+v; want empty", got)
		}
	})

	t.Run("TeamGitHubRepos_TracksRepoSystem_OwnerCaseInsensitive", func(t *testing.T) {
		stores, ids := factory(t)
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, ids.OrgID, ids.TeamID, []domain.TeamGitHubRepo{
			{Owner: "Acme", Repo: "api"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		for _, c := range []struct {
			owner, repo string
			want        bool
		}{
			{"Acme", "api", true},
			{"acme", "api", true},
			{"acme", "web", false},
			{"other", "api", false},
		} {
			got, err := stores.TeamGitHubRepos.TracksRepoSystem(ctx, ids.TeamID, c.owner, c.repo)
			if err != nil {
				t.Fatalf("TracksRepoSystem(%s/%s): %v", c.owner, c.repo, err)
			}
			if got != c.want {
				t.Errorf("TracksRepoSystem(%s/%s) = %v; want %v", c.owner, c.repo, got, c.want)
			}
		}
	})

	t.Run("TeamGitHubRepos_ViewerAdminScoped_NeverBroaderThanViewerScoped", func(t *testing.T) {
		// The write gate must be a subset of the read gate: every repo a
		// caller may *mutate* is one they may *see*. Nothing else about
		// these two is dialect-agnostic — Postgres answers from live RLS
		// plus the caller's team-admin role, SQLite answers true for both
		// because N=1 has no team boundary — so the exact values are
		// pinned in the per-dialect tests and the containment is pinned
		// here, where a regression on either backend fails immediately.
		//
		// The failure this catches: an admin-scoped implementation that
		// drops or mis-joins the team-admin predicate and ends up matching
		// rows the membership-scoped read doesn't.
		stores, ids := factory(t)
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, ids.OrgID, ids.TeamID, []domain.TeamGitHubRepo{
			{Owner: "Acme", Repo: "api"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		for _, c := range []struct{ owner, repo string }{
			{"Acme", "api"},  // tracked, stored casing
			{"acme", "API"},  // tracked, mismatched casing
			{"acme", "web"},  // same owner, untracked repo
			{"other", "api"}, // untracked owner
		} {
			scoped, err := stores.TeamGitHubRepos.TracksRepoViewerScoped(ctx, ids.OrgID, c.owner, c.repo)
			if err != nil {
				t.Fatalf("TracksRepoViewerScoped(%s/%s): %v", c.owner, c.repo, err)
			}
			adminScoped, err := stores.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, ids.OrgID, c.owner, c.repo)
			if err != nil {
				t.Fatalf("TracksRepoViewerAdminScoped(%s/%s): %v", c.owner, c.repo, err)
			}
			if adminScoped && !scoped {
				t.Errorf("%s/%s: admin-scoped=true but viewer-scoped=false; the write gate must never be broader than the read gate", c.owner, c.repo)
			}
		}
	})

	t.Run("TeamGitHubRepos_ListForOrgSystem_Union", func(t *testing.T) {
		stores, ids := factory(t)
		if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, ids.OrgID, ids.TeamID, []domain.TeamGitHubRepo{
			{Owner: "acme", Repo: "api"},
			{Owner: "acme", Repo: "web"},
		}); err != nil {
			t.Fatalf("ReplaceForTeam: %v", err)
		}
		union, err := stores.TeamGitHubRepos.ListForOrgSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		sortRepos(union)
		want := []domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}, {Owner: "acme", Repo: "web"}}
		if !reflect.DeepEqual(union, want) {
			t.Errorf("ListForOrgSystem = %+v; want %+v", union, want)
		}
	})

	t.Run("TeamGitHubRepos_EmptyTeam_ReturnsEmptySlice", func(t *testing.T) {
		stores, ids := factory(t)
		got, err := stores.TeamGitHubRepos.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("ListForTeamSystem on empty team = %+v; want empty slice", got)
		}
	})
}

func sortRepos(repos []domain.TeamGitHubRepo) {
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].Owner != repos[j].Owner {
			return repos[i].Owner < repos[j].Owner
		}
		return repos[i].Repo < repos[j].Repo
	})
}

func sortGroups(groups []domain.TeamGitHubGroup) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].OrgLogin != groups[j].OrgLogin {
			return groups[i].OrgLogin < groups[j].OrgLogin
		}
		return groups[i].TeamSlug < groups[j].TeamSlug
	})
}

func sortRulesByKey(rules []domain.JiraProjectStatusRules) {
	sort.Slice(rules, func(i, j int) bool { return rules[i].ProjectKey < rules[j].ProjectKey })
}

func mustListJira(t *testing.T, fn func() ([]domain.JiraProjectStatusRules, error)) []domain.JiraProjectStatusRules {
	t.Helper()
	got, err := fn()
	if err != nil {
		t.Fatalf("list jira rules: %v", err)
	}
	return got
}
