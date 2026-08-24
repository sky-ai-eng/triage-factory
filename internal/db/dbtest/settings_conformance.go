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
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

// SettingsStoresFactory hands the conformance suite a wired bundle of
// settings stores + the IDs (orgID, teamID, userID) to address every
// method against. Per-backend tests own row seeding (orgs / teams /
// users / org_memberships / memberships) — the conformance harness is
// schema-blind.
type SettingsStoresFactory func(t *testing.T) (stores SettingsStores, ids SettingsIDs)

// SettingsStores is the slice of stores the conformance suite exercises, plus
// which dialect they are.
type SettingsStores struct {
	// MultiMode names the dialect in the vocabulary its divergent column
	// DEFAULTs are written in: Postgres serves multi, SQLite serves local. Two
	// settings columns default per dialect — the team default model and the
	// background-jobs model — because each stores what its runtime dispatches,
	// so an assertion about a materialized row has to know which it is reading.
	MultiMode bool

	Orgs             db.OrgsStore
	Teams            db.TeamsStore
	Users            db.UsersStore
	JiraStatusRules  db.JiraStatusRulesStore
	TeamGitHubGroups db.TeamGitHubGroupsStore
	TeamGitHubRepos  db.TeamGitHubReposStore
	OrgEventSources  db.OrgEventSourceStore
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

	t.Run("team_archive_and_restore_return_the_stored_row", func(t *testing.T) {
		// The returned-row standard on the two guarded team transitions. Role
		// is blanked on both sides of the comparison: it is the caller's own
		// membership, which Get resolves separately and no teams column
		// carries, so it is not part of what a write returns.
		stores, ids := factory(t)
		read := func() (*domain.Team, error) {
			got, err := stores.Teams.GetSystem(ctx, ids.OrgID, ids.TeamID)
			if err != nil || got == nil {
				return got, err
			}
			bare := *got
			bare.Role = ""
			return &bare, nil
		}

		archived, err := stores.Teams.Archive(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("Archive: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Teams.Archive", archived, read)
		if archived.DeletedAt == nil {
			t.Error("Archive returned a row with no deleted_at, the column it stamps")
		}
		if _, err := stores.Teams.Archive(ctx, ids.TeamID); !errors.Is(err, db.ErrTeamNotFound) {
			t.Errorf("re-archive: got %v, want db.ErrTeamNotFound", err)
		}

		restored, err := stores.Teams.Restore(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Teams.Restore", restored, read)
		if restored.DeletedAt != nil {
			t.Errorf("Restore returned deleted_at %v, want it cleared", restored.DeletedAt)
		}
		if _, err := stores.Teams.Restore(ctx, ids.TeamID); !errors.Is(err, db.ErrTeamNotFound) {
			t.Errorf("re-restore: got %v, want db.ErrTeamNotFound", err)
		}
	})

	t.Run("team_settings_writes_return_the_stored_row", func(t *testing.T) {
		// The returned-row standard on the two team_settings writes: what the
		// write handed back is what a point read finds. SetDailyCostCapSystem
		// is the case it is for — it touches one column of a row whose other
		// twelve come from schema defaults, so nothing the caller holds
		// describes what landed.
		stores, ids := factory(t)
		read := func() (*domain.TeamSettings, error) {
			set, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
			if err != nil {
				return nil, err
			}
			return &set, nil
		}

		saved, err := stores.Teams.UpdateSettings(ctx, ids.TeamID, domain.TeamSettings{
			JiraProjects: []string{"RET"}, AIReprioritizeThreshold: 4,
			AIPreferenceUpdateInterval: 12, DefaultModel: domain.ModelOpus,
			PermissionAbsentGraceMS: 1000,
			BranchTemplate:          "ret/<ticket-id>",
			ReviewPosture:           domain.ReviewPostureAutoUnlessBlocking,
			BaseBranchPushPolicy:    domain.BaseBranchPushManualOnly,
		})
		if err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Teams.UpdateSettings", saved, read)

		capped, err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 9.5)
		if err != nil {
			t.Fatalf("SetDailyCostCapSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Teams.SetDailyCostCapSystem", capped, read)
		// The cap moved and everything the earlier save wrote is still there —
		// the whole reason a one-column write hands back the whole row.
		if capped.MaxDailyCostUSD != 9.5 || capped.DefaultModel != domain.ModelOpus {
			t.Errorf("SetDailyCostCapSystem returned %+v, want the new cap over the saved settings", capped)
		}

		cleared, err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 0)
		if err != nil {
			t.Fatalf("SetDailyCostCapSystem (clear): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Teams.SetDailyCostCapSystem (clear)", cleared, read)
		if cleared.MaxDailyCostUSD != 0 {
			t.Errorf("clearing the cap returned %v, want 0 (no cap)", cleared.MaxDailyCostUSD)
		}
	})

	t.Run("org_settings_writes_return_the_stored_row", func(t *testing.T) {
		// The returned-row standard on OrgsStore's three settings writes,
		// mirroring the team_settings arm above: what each write hands back is
		// what a point read finds.
		stores, ids := factory(t)
		read := func() (*domain.OrgSettings, error) {
			set, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
			if err != nil {
				return nil, err
			}
			return &set, nil
		}
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}

		saved, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base)
		if err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Orgs.UpdateSettings", saved, read)

		// UpdateSettingsVersioned's update arm — the case it is for: the new
		// version is the one thing a successful caller most needs and cannot
		// compute (expected+1 is a guess), so it rides the returned row rather
		// than a follow-up read.
		versioned := base
		versioned.GitHubBaseURL = "https://versioned-ret.example.com"
		verSaved, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, versioned, saved.Version)
		if err != nil {
			t.Fatalf("UpdateSettingsVersioned: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Orgs.UpdateSettingsVersioned", verSaved, read)
		if verSaved.Version != saved.Version+1 {
			t.Errorf("UpdateSettingsVersioned returned version %d, want %d", verSaved.Version, saved.Version+1)
		}

		// A stale version conflict writes nothing — no row to hand back, so the
		// zero value is correct alongside ErrOrgSettingsVersion.
		zero, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, versioned, saved.Version)
		if !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Fatalf("stale UpdateSettingsVersioned err = %v, want ErrOrgSettingsVersion", err)
		}
		if !reflect.DeepEqual(zero, domain.OrgSettings{}) {
			t.Errorf("refused UpdateSettingsVersioned returned %+v, want the zero value", zero)
		}

		// SetGitHubCredentialClass touches one column of a row whose other
		// twelve may come from schema defaults — the case it is for.
		classed, err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("SetGitHubCredentialClass: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Orgs.SetGitHubCredentialClass", classed, read)
		if classed.GitHubCredentialClass != domain.GitHubCredentialClassBYOApp {
			t.Errorf("SetGitHubCredentialClass returned class %q, want %q", classed.GitHubCredentialClass, domain.GitHubCredentialClassBYOApp)
		}
		// Everything the versioned write landed is still there — the whole
		// reason a one-column write hands back the whole row.
		if classed.GitHubBaseURL != versioned.GitHubBaseURL {
			t.Errorf("SetGitHubCredentialClass returned %+v, want the prior save's base URL preserved", classed)
		}
	})

	// OrgSettings_PerSourceWriteDoesNotShareTheVersionToken pins the
	// concurrency split base_url / poll_interval moving onto org_event_sources
	// left behind: the org_settings.version token guards a settings-page save
	// (UpdateSettingsVersioned, covering that route's own base_url /
	// poll_interval writes too — they land in the same transaction as the
	// guarded org_settings row), but it says nothing about the admin-only
	// per-source route (OrgEventSourceStore.SetDisabled), which is an
	// unguarded, last-writer-wins upsert on a disjoint set of columns on the
	// SAME org_event_sources row.
	//
	// Two admins racing on genuinely different resources (the settings-page
	// save; the per-source disable switch) must not spuriously block or
	// unwind each other, and each must leave the other's columns alone —
	// that's the whole point of the split. This test plays out both
	// directions of that on one shared row.
	t.Run("OrgSettings_PerSourceWriteDoesNotShareTheVersionToken", func(t *testing.T) {
		stores, ids := factory(t)
		if stores.OrgEventSources == nil {
			t.Skip("factory did not wire OrgEventSources")
		}

		// Admin A saves the settings page — this is the version-guarded
		// write, and it lands base_url in the same transaction.
		saved, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, domain.OrgSettings{
			GitHubBaseURL:       "https://a-saved.example.com",
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}, 0)
		if err != nil {
			t.Fatalf("UpdateSettingsVersioned (create): %v", err)
		}

		// Admin B, on the per-source route, pauses github. Unguarded: it
		// doesn't ask for A's version and it must still succeed.
		if _, err := stores.OrgEventSources.SetDisabled(ctx, ids.OrgID, "github", true, ids.UserID); err != nil {
			t.Fatalf("SetDisabled: %v", err)
		}

		// B's write must not have touched org_settings.version, or A's base
		// URL — the two writers own disjoint columns of the org_event_sources
		// row, and B's route doesn't touch org_settings at all.
		afterB, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after SetDisabled: %v", err)
		}
		if afterB.Version != saved.Version {
			t.Errorf("SetDisabled bumped org_settings.version: got %d, want unchanged %d", afterB.Version, saved.Version)
		}
		if afterB.GitHubBaseURL != "https://a-saved.example.com" {
			t.Errorf("SetDisabled clobbered github base_url: got %q", afterB.GitHubBaseURL)
		}

		// A saves again at the version from the FIRST save — B's write never
		// touched org_settings.version, so this is not a stale token and must
		// succeed, not conflict.
		resaved, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, domain.OrgSettings{
			GitHubBaseURL:       "https://a-saved.example.com",
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
			MaxDailyCostUSD:     42,
		}, saved.Version)
		if err != nil {
			t.Fatalf("UpdateSettingsVersioned (A resaves at the version B's write left intact): %v", err)
		}
		if resaved.MaxDailyCostUSD != 42 {
			t.Errorf("A's resave didn't land: MaxDailyCostUSD = %v, want 42", resaved.MaxDailyCostUSD)
		}

		// And the other direction: A's org_settings-scoped save must not have
		// touched B's disabled flag — disjoint columns, same row.
		disabledRow, err := stores.OrgEventSources.Get(ctx, ids.OrgID, "github")
		if err != nil {
			t.Fatalf("OrgEventSources.Get: %v", err)
		}
		if disabledRow == nil || !disabledRow.Disabled {
			t.Errorf("A's settings save cleared github's disabled flag: %+v", disabledRow)
		}
	})

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
			BackgroundJobsModel:   domain.ModelSonnet,
			LLMAuthMethod:         domain.LLMAuthBYOK,
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, want); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, on); err != nil {
			t.Fatalf("UpdateSettings (on): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem (on): %v", err)
		}
		if !got.MarketplaceEnabled {
			t.Errorf("MarketplaceEnabled = false after enabling, want true")
		}
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
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
		if _, err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassBYOApp); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, save); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, save); err != nil {
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
		if _, err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassPAT); err != nil {
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

	// Two settings columns default to a MODEL, and each dialect spells its
	// default in the vocabulary its own runtime dispatches: Postgres carries the
	// native wire id the in-process loop sends, SQLite the Claude Code alias its
	// subprocess resolves. Neither is a stylistic choice — the value is what a
	// deployment materializes when nobody has picked, and every save validator
	// and every dispatch measures a stored model against that deployment's
	// universe, so a default in the other vocabulary seeds an install into a
	// state its own settings page refuses to re-save.
	//
	// Both are read off a MATERIALIZED row rather than a Go constant: each is
	// asserted through a surgical write that names one column and takes the rest
	// from the schema's DEFAULT clauses, which is what a fresh tenant actually
	// takes.
	t.Run("Settings_DialectDefaults_SpeakThisDeploymentsVocabulary", func(t *testing.T) {
		stores, ids := factory(t)

		orgSet, err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassPAT)
		if err != nil {
			t.Fatalf("SetGitHubCredentialClass (materializes org_settings from defaults): %v", err)
		}
		wantJobsModel := domain.LocalBackgroundJobsModel
		if stores.MultiMode {
			// Multi ships no pre-fill: an org there is forced through the setup
			// pick, and until it picks the system jobs skip rather than spend on
			// a model nobody chose.
			wantJobsModel = ""
		}
		if orgSet.BackgroundJobsModel != wantJobsModel {
			t.Errorf("materialized background_jobs_model = %q, want %q", orgSet.BackgroundJobsModel, wantJobsModel)
		}

		teamSet, err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 1)
		if err != nil {
			t.Fatalf("SetDailyCostCapSystem (materializes team_settings from defaults): %v", err)
		}
		wantTeamModel := domain.DefaultModelFor(stores.MultiMode)
		if teamSet.DefaultModel != wantTeamModel {
			t.Errorf("materialized default_model = %q, want %q", teamSet.DefaultModel, wantTeamModel)
		}

		// And what each default names has to be something this deployment can
		// actually offer — the property the two spellings exist to satisfy.
		universe := modelcatalog.UniverseFor(stores.MultiMode)
		for field, model := range map[string]string{
			"background_jobs_model": orgSet.BackgroundJobsModel,
			"default_model":         teamSet.DefaultModel,
		} {
			if model == "" {
				continue
			}
			if !universe.Offers(model) {
				t.Errorf("%s defaults to %q, which this deployment does not offer: %v", field, model, universe.Keys())
			}
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, set); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, set); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in); err != nil {
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

	// The background-jobs model is the one org setting whose column DEFAULT
	// differs per dialect — SQLite pre-fills a local install, Postgres leaves a
	// fresh org to pick during setup — so what has to hold on BOTH is that the
	// value a caller writes is the value it reads back, empty included. Empty is
	// a real intent here (it turns the three system jobs off), and a store that
	// coalesced it to NULL and back to the column default would silently turn
	// them on again.
	t.Run("OrgSettings_BackgroundJobsModel_RoundTripsIncludingEmpty", func(t *testing.T) {
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		for _, want := range []string{domain.ModelOpus, "", domain.ModelHaiku} {
			in := base
			in.BackgroundJobsModel = want
			stored, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in)
			if err != nil {
				t.Fatalf("UpdateSettings(%q): %v", want, err)
			}
			if stored.BackgroundJobsModel != want {
				t.Errorf("write returned BackgroundJobsModel %q, want %q", stored.BackgroundJobsModel, want)
			}
			got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
			if err != nil {
				t.Fatalf("GetSettingsSystem: %v", err)
			}
			if got.BackgroundJobsModel != want {
				t.Errorf("read back BackgroundJobsModel %q, want %q", got.BackgroundJobsModel, want)
			}
		}
	})

	// llm_auth_method is the other org setting whose column DEFAULT differs per
	// dialect — SQLite defaults a local install onto the host's credentials,
	// Postgres onto the org's own, which is multi's only legal value. Both real
	// values must round-trip on both dialects, and neither may store the empty
	// string: a caller that built the struct without naming the field gets the
	// column's own default, because "" is not a third credential source and a
	// read left to guess at one is what this column exists to prevent.
	t.Run("OrgSettings_LLMAuthMethod_RoundTripsAndNeverStoresBlank", func(t *testing.T) {
		stores, ids := factory(t)
		base := domain.OrgSettings{
			GitHubPollInterval:  5 * time.Minute,
			JiraPollInterval:    5 * time.Minute,
			GitHubCloneProtocol: "ssh",
		}
		for _, want := range []string{domain.LLMAuthBYOK, domain.LLMAuthSystem, domain.LLMAuthBYOK} {
			in := base
			in.LLMAuthMethod = want
			stored, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, in)
			if err != nil {
				t.Fatalf("UpdateSettings(%q): %v", want, err)
			}
			if stored.LLMAuthMethod != want {
				t.Errorf("write returned LLMAuthMethod %q, want %q", stored.LLMAuthMethod, want)
			}
			got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
			if err != nil {
				t.Fatalf("GetSettingsSystem: %v", err)
			}
			if got.LLMAuthMethod != want {
				t.Errorf("read back LLMAuthMethod %q, want %q", got.LLMAuthMethod, want)
			}
		}
		blank, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base)
		if err != nil {
			t.Fatalf("UpdateSettings(unset): %v", err)
		}
		if blank.LLMAuthMethod != domain.LLMAuthSystem && blank.LLMAuthMethod != domain.LLMAuthBYOK {
			t.Errorf("an unset LLMAuthMethod stored %q, want this dialect's column default", blank.LLMAuthMethod)
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
			LLMAuthMethod:       domain.LLMAuthBYOK,
			MaxDailyCostUSD:     5,
			MaxConcurrentRuns:   3,
			// Not written by UpdateSettings; the row's default reads back.
			GitHubCredentialClass: domain.GitHubCredentialClassPAT,
		}
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, first); err != nil {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, second); err != nil {
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
		if _, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, first, 0); err != nil {
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
		if _, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, winner, 1); err != nil {
			t.Fatalf("UpdateSettingsVersioned (winner): %v", err)
		}

		// The loser read 1 too — its write is refused, and refused WITHOUT
		// writing: the winner's field is still what a refetch shows.
		loser := base
		loser.GitHubBaseURL = "https://loser.example.com"
		_, err = stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, loser, 1)
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
		_, err = stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, loser, 0)
		if !errors.Is(err, db.ErrOrgSettingsVersion) {
			t.Errorf("asserting version 0 against an existing row = %v; want ErrOrgSettingsVersion", err)
		}

		// Re-reading and re-applying is the loser's whole remedy, so it has to
		// work on the next attempt.
		if _, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, loser, got.Version); err != nil {
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
		_, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, row, 3)
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (first): %v", err)
		}
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings (second): %v", err)
		}
		got, err := stores.Orgs.GetSettingsSystem(ctx, ids.OrgID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		if got.Version != 2 {
			t.Errorf("version after two unguarded saves = %d; want 2", got.Version)
		}
		if _, err := stores.Orgs.UpdateSettingsVersioned(ctx, ids.OrgID, base, 1); !errors.Is(err, db.ErrOrgSettingsVersion) {
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
		if _, err := stores.Orgs.UpdateSettings(ctx, ids.OrgID, base); err != nil {
			t.Fatalf("UpdateSettings: %v", err)
		}
		if _, err := stores.Orgs.SetGitHubCredentialClass(ctx, ids.OrgID, domain.GitHubCredentialClassBYOApp); err != nil {
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
			DefaultModel:                    domain.ModelOpus,
			AutoDelegateEnabled:             true,
			AutoModeEnabled:                 false,
			PermissionAbsentGraceMS:         30000,
			PermissionAbsentAutodenyEnabled: false,
			MaxDailyCostUSD:                 12.50, // TFAC-482 per-team daily cap
			BranchTemplate:                  "team/<ticket-id>-wip",
			ReviewPosture:                   domain.ReviewPostureAutoUnlessBlocking,
			BaseBranchPushPolicy:            domain.BaseBranchPushManualOnly,
			AllowedProviders:                []string{modelcatalog.ProviderAnthropic},
		}
		if _, err := stores.Teams.UpdateSettings(ctx, ids.TeamID, want); err != nil {
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
		if _, err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 42.50); err != nil {
			t.Fatalf("SetDailyCostCapSystem (insert): %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		want := domain.DefaultTeamSettingsFor(stores.MultiMode)
		want.MaxDailyCostUSD = 42.50
		// A materialized row reads its array columns back as empty (non-nil)
		// slices, whereas DefaultTeamSettings leaves them nil — normalize so
		// DeepEqual checks the fields that matter, not the nil-vs-empty
		// distinction.
		want.JiraProjects = []string{}
		want.AllowedProviders = []string{}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after SetDailyCostCapSystem on a fresh team\n got: %+v\nwant: %+v (defaults + cap)", got, want)
		}

		// A read-modify-write team-settings save that changes another field but not
		// the cap must preserve it — the team-admin path can never alter the cap.
		got.DefaultModel = domain.ModelHaiku
		if _, err := stores.Teams.UpdateSettings(ctx, ids.TeamID, got); err != nil {
			t.Fatalf("UpdateSettings (rmw): %v", err)
		}
		after, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after rmw: %v", err)
		}
		if after.MaxDailyCostUSD != 42.50 {
			t.Errorf("UpdateSettings clobbered the cap: got %v, want 42.50 preserved", after.MaxDailyCostUSD)
		}
		if after.DefaultModel != domain.ModelHaiku {
			t.Errorf("UpdateSettings didn't apply the unrelated change: got %q, want %q", after.DefaultModel, domain.ModelHaiku)
		}

		// ≤ 0 clears the cap (stored NULL → read back as 0 / no cap).
		if _, err := stores.Teams.SetDailyCostCapSystem(ctx, ids.TeamID, 0); err != nil {
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

	// SetAllowedProvidersSystem is the org-admin write path for the per-team
	// provider restriction: surgical (touches only that column), creates the row
	// from schema defaults when none exists, and the team-settings
	// read-modify-write path preserves it — a team admin cannot widen their own
	// restriction. An empty list is the unrestricted state.
	t.Run("TeamSettings_AllowedProviders_SetClearAndPreserve", func(t *testing.T) {
		stores, ids := factory(t)

		// Fresh team, no settings row yet → the partial INSERT must materialize the
		// row from defaults + the restriction, touching only allowed_providers.
		if _, err := stores.Teams.SetAllowedProvidersSystem(ctx, ids.TeamID, []string{modelcatalog.ProviderAnthropic}); err != nil {
			t.Fatalf("SetAllowedProvidersSystem (insert): %v", err)
		}
		got, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem: %v", err)
		}
		want := domain.DefaultTeamSettingsFor(stores.MultiMode)
		want.AllowedProviders = []string{modelcatalog.ProviderAnthropic}
		// A materialized row reads its array columns back as empty (non-nil)
		// slices, whereas DefaultTeamSettings leaves them nil — normalize so
		// DeepEqual checks the fields that matter.
		want.JiraProjects = []string{}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after SetAllowedProvidersSystem on a fresh team\n got: %+v\nwant: %+v (defaults + restriction)", got, want)
		}

		// A read-modify-write team-settings save that changes another field must
		// preserve the restriction — the team-admin path can never alter it.
		got.DefaultModel = domain.ModelHaiku
		if _, err := stores.Teams.UpdateSettings(ctx, ids.TeamID, got); err != nil {
			t.Fatalf("UpdateSettings (rmw): %v", err)
		}
		after, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after rmw: %v", err)
		}
		if !reflect.DeepEqual(after.AllowedProviders, []string{modelcatalog.ProviderAnthropic}) {
			t.Errorf("UpdateSettings clobbered the restriction: got %v, want [%s] preserved", after.AllowedProviders, modelcatalog.ProviderAnthropic)
		}
		if after.DefaultModel != domain.ModelHaiku {
			t.Errorf("UpdateSettings didn't apply the unrelated change: got %q, want %q", after.DefaultModel, domain.ModelHaiku)
		}

		// Several providers round-trip in order, and an empty list clears the
		// restriction back to unrestricted.
		both := []string{modelcatalog.ProviderAnthropic, modelcatalog.ProviderBedrock}
		if _, err := stores.Teams.SetAllowedProvidersSystem(ctx, ids.TeamID, both); err != nil {
			t.Fatalf("SetAllowedProvidersSystem (both): %v", err)
		}
		multi, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after both: %v", err)
		}
		if !reflect.DeepEqual(multi.AllowedProviders, both) {
			t.Errorf("AllowedProviders = %v, want %v", multi.AllowedProviders, both)
		}
		if _, err := stores.Teams.SetAllowedProvidersSystem(ctx, ids.TeamID, nil); err != nil {
			t.Fatalf("SetAllowedProvidersSystem (clear): %v", err)
		}
		cleared, err := stores.Teams.GetSettingsSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("GetSettingsSystem after clear: %v", err)
		}
		if len(cleared.AllowedProviders) != 0 {
			t.Errorf("cleared restriction = %v, want empty (unrestricted)", cleared.AllowedProviders)
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
		want := domain.DefaultTeamSettingsFor(stores.MultiMode)
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
			DefaultModel:               domain.ModelSonnet,
		}
		if _, err := stores.Teams.UpdateSettings(ctx, ids.TeamID, in); err != nil {
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
		// SKY maps the optional in-review rule and ENG leaves it empty, so one
		// round-trip covers both stored shapes of it.
		stores, ids := factory(t)
		input := []domain.JiraProjectStatusRules{
			{
				ProjectKey:          "SKY",
				PickupMembers:       jiraRefs("To Do", "Backlog"),
				InProgressMembers:   jiraRefs("In Progress"),
				InProgressCanonical: jiraRef("In Progress"),
				InReviewMembers:     jiraRefs("In Progress", "Code Review"),
				InReviewCanonical:   jiraRef("Code Review"),
				DoneMembers:         jiraRefs("Done"),
				DoneCanonical:       jiraRef("Done"),
			},
			{
				ProjectKey:          "ENG",
				PickupMembers:       jiraRefs("Open"),
				InProgressMembers:   jiraRefs("Doing"),
				InProgressCanonical: jiraRef("Doing"),
				DoneMembers:         jiraRefs("Closed"),
				DoneCanonical:       jiraRef("Closed"),
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
		// compare. An unmapped rule reads back as an EMPTY slice rather than
		// nil — the column holds "[]" and the scan reflects the column — which
		// is why ENG's untouched in-review members need saying here.
		want := make([]domain.JiraProjectStatusRules, len(input))
		copy(want, input)
		for i := range want {
			want[i].TeamID = ids.TeamID
			if want[i].InReviewMembers == nil {
				want[i].InReviewMembers = []domain.JiraStatusRef{}
			}
		}
		sortRulesByKey(got)
		sortRulesByKey(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("after ReplaceForTeam, ListForTeam = %+v; want %+v", got, want)
		}
	})

	t.Run("JiraStatusRules_ReplaceForTeam_ClearsInReviewRule", func(t *testing.T) {
		// The optional rule has to be removable, which is the direction the
		// complete-or-empty CHECK could get wrong: members and canonical must
		// clear together, and the row must survive with the other three intact.
		stores, ids := factory(t)
		mapped := []domain.JiraProjectStatusRules{{
			ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			InReviewMembers: jiraRefs("Code Review"), InReviewCanonical: jiraRef("Code Review"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, mapped); err != nil {
			t.Fatalf("ReplaceForTeam (mapped): %v", err)
		}
		cleared := mapped
		cleared[0].InReviewMembers = nil
		cleared[0].InReviewCanonical = domain.JiraStatusRef{}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, cleared); err != nil {
			t.Fatalf("ReplaceForTeam (cleared): %v", err)
		}
		got, err := stores.JiraStatusRules.ListForTeamSystem(ctx, ids.TeamID)
		if err != nil {
			t.Fatalf("ListForTeamSystem: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("rules = %+v, want the one SKY row", got)
		}
		if len(got[0].InReviewMembers) != 0 || !got[0].InReviewCanonical.IsZero() {
			t.Errorf("in-review rule = %+v / %+v, want both cleared",
				got[0].InReviewMembers, got[0].InReviewCanonical)
		}
		if got[0].InProgressCanonical != jiraRef("In Progress") || got[0].DoneCanonical != jiraRef("Done") {
			t.Errorf("row = %+v, want the other rules untouched by the clear", got[0])
		}
	})

	t.Run("JiraStatusRules_TeamID_And_OrgUnion_RoundTrip", func(t *testing.T) {
		// TeamID populates on every List* read, ListForOrgSystem
		// returns the org-wide union (every team's rows), and
		// TracksProjectSystem answers the router gate.
		stores, ids := factory(t)
		input := []domain.JiraProjectStatusRules{{
			ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
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
				ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
				InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
				DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
			},
			{
				ProjectKey: "ENG", PickupMembers: jiraRefs("Open"),
				InProgressMembers: jiraRefs("Doing"), InProgressCanonical: jiraRef("Doing"),
				DoneMembers: jiraRefs("Closed"), DoneCanonical: jiraRef("Closed"),
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
			ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
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
			ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
			InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
			DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
		}}
		if err := stores.JiraStatusRules.ReplaceForTeam(ctx, ids.TeamID, seed); err != nil {
			t.Fatalf("seed ReplaceForTeam: %v", err)
		}
		bad := []domain.JiraProjectStatusRules{{
			ProjectKey: "", PickupMembers: jiraRefs("x"),
			InProgressMembers: jiraRefs("y"), InProgressCanonical: jiraRef("y"),
			DoneMembers: jiraRefs("z"), DoneCanonical: jiraRef("z"),
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

// jiraRef builds one status ref the way a rule armed through the API carries
// it: an id, which is the identity, plus the display name resolved for it. The
// id is derived from the name so a test can name a status once and get the same
// ref every time.
func jiraRef(name string) domain.JiraStatusRef {
	return domain.JiraStatusRef{ID: "st-" + name, Name: name}
}

func jiraRefs(names ...string) []domain.JiraStatusRef {
	refs := make([]domain.JiraStatusRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, jiraRef(n))
	}
	return refs
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
