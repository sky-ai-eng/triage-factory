package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstallationGrantBackend is one wired backend for the grant-mirror suite.
// Installations are staged through the store rather than raw SQL (both dialects
// expose UpsertInstallation), so the only fixture a backend has to supply by
// hand is a tracked repository — team_github_repos has no store method that
// works claims-free on both pools.
type InstallationGrantBackend struct {
	Apps   db.GitHubAppsStore
	Mirror db.InstallationReposStore
	// OrgID every call in the suite passes. SQLite pins the local sentinel;
	// Postgres mints a real org.
	OrgID string
	// TrackRepo records that some team in the org tracks owner/repo.
	TrackRepo func(t *testing.T, owner, repo string)
}

// InstallationGrantFactory returns a fresh, isolated backend per subtest.
type InstallationGrantFactory func(t *testing.T) InstallationGrantBackend

// RunInstallationGrantConformance is the shared assertion suite for the
// App-installation grant mirror — the record of what each installation can
// actually reach, and the two derived states it exists to make computable.
//
// What it pins, in order of how badly each would hurt:
//
//   - The negative space. A rejected answer leaves the previous one standing:
//     the replace is all-or-nothing, so a mirror never half-empties. This is the
//     most important line in the suite, because an emptied mirror renders "the
//     App can reach code nobody tracks" as a false all-clear — worse than stale.
//   - After a successful replace the mirror IS GitHub's answer, removals
//     included: a repository revoked from a selective grant disappears.
//   - Two casings of one repository cannot both land. The key folds, so the
//     database refuses rather than trusting every writer to remember.
//   - Soft-removing an installation clears its grant. An uninstalled
//     installation reaches nothing; the row survives as history, the reach does
//     not.
//   - 'all' and 'selected' are distinguishable, and a writer that did not look
//     does not erase a width already established.
//   - Both derived states — reach without purpose and scope drift — including
//     the case-folding both comparisons need and the gate that keeps an
//     un-mirrored org from reporting its entire tracked set as drift.
func RunInstallationGrantConformance(t *testing.T, mk InstallationGrantFactory) {
	t.Helper()
	ctx := context.Background()

	// install stages a live installation row — the FK parent every mirror row
	// hangs off, and the thing the reads join through.
	install := func(t *testing.T, b InstallationGrantBackend, installationID, login, selection string) {
		t.Helper()
		if err := b.Apps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID:      installationID,
			OrgID:               b.OrgID,
			AccountType:         "Organization",
			AccountLogin:        login,
			RepositorySelection: selection,
		}); err != nil {
			t.Fatalf("UpsertInstallation(%s): %v", installationID, err)
		}
	}

	entry := func(b InstallationGrantBackend, installationID, owner, repo, externalID string) domain.InstallationRepository {
		return domain.InstallationRepository{
			OrgID:          b.OrgID,
			InstallationID: installationID,
			Owner:          owner,
			Repo:           repo,
			ExternalID:     externalID,
		}
	}

	slugs := func(t *testing.T, rows []domain.InstallationRepository) []string {
		t.Helper()
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Slug())
		}
		return out
	}

	trackedSlugs := func(rows []domain.TeamGitHubRepo) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Owner+"/"+r.Repo)
		}
		return out
	}

	equal := func(t *testing.T, label string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %v; want %v", label, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s = %v; want %v", label, got, want)
			}
		}
	}

	t.Run("ReplaceIsGitHubsAnswerIncludingRemovals", func(t *testing.T) {
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
			entry(b, "1", "acme", "web", "11"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		// The account owner narrows the selection: web out, worker in. A mirror
		// that only ever grew would keep reporting reach the App has lost, which
		// is the failure the whole table exists to prevent.
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
			entry(b, "1", "acme", "worker", "12"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (narrowed): %v", err)
		}

		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		equal(t, "mirror after a narrowed grant", slugs(t, got), []string{"acme/api", "acme/worker"})
	})

	t.Run("ExternalIDRoundTrips", func(t *testing.T) {
		// The id is what matches a grant entry to a registry row when their
		// slugs momentarily disagree mid-rename, so losing it costs the one
		// comparison a rename cannot break.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "424242"),
			entry(b, "1", "acme", "web", ""), // an entry GitHub sent no id for
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListForOrgSystem = %d rows; want 2", len(got))
		}
		if got[0].ExternalID != "424242" {
			t.Errorf("acme/api ExternalID = %q; want %q", got[0].ExternalID, "424242")
		}
		if got[1].ExternalID != "" {
			t.Errorf("acme/web ExternalID = %q; want \"\" — an absent id is not the string zero", got[1].ExternalID)
		}
		if got[0].Source != domain.RepoSourceGitHub {
			t.Errorf("Source = %q; want %q — the join to the registry is keyed on it", got[0].Source, domain.RepoSourceGitHub)
		}
		if got[0].ObservedAt.IsZero() {
			t.Error("ObservedAt is zero; the row cannot say when the grant was read")
		}
	})

	t.Run("TwoCasingsOfOneRepositoryCannotBothLand", func(t *testing.T) {
		// GitHub identifiers are case-insensitive, so "one repository" means one
		// row per case-folded slug. The key folds, which makes a duplicate
		// impossible rather than merely avoided — no writer has to remember.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "Acme", "API", "10"),
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListForOrgSystem = %d rows for two casings of one repository; want 1 (%v)", len(got), slugs(t, got))
		}
	})

	t.Run("ARejectedAnswerLeavesThePreviousOneStanding", func(t *testing.T) {
		// The negative space. A replace that fails must not have emptied
		// anything: the delete and the inserts are one transaction, and an
		// answer the store refuses outright never opens it. Rejection here
		// stands in for the failure the reconcile actually sees (an unreachable
		// GitHub, a truncated listing) — in every case the previous answer is
		// what the mirror must still hold.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		bad := entry(b, "1", "acme", "web", "11")
		bad.Source = "gitlab" // not a provider this schema knows
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "worker", "12"),
			bad,
		}); err == nil {
			t.Fatal("ReplaceForInstallationSystem accepted an unknown source; want an error")
		}

		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		equal(t, "mirror after a rejected replace", slugs(t, got), []string{"acme/api"})
	})

	t.Run("SoftRemovingTheInstallationClearsItsMirror", func(t *testing.T) {
		// An uninstalled installation grants nothing. The row survives as
		// history — that is the registry half — but the reach does not, or the
		// org page reports access the App no longer holds.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		install(t, b, "2", "other", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (1): %v", err)
		}
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "2", []domain.InstallationRepository{
			entry(b, "2", "other", "tool", "20"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (2): %v", err)
		}

		if err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}

		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		equal(t, "mirror after one installation was removed", slugs(t, got), []string{"other/tool"})
	})

	t.Run("MirrorsAreScopedToTheirInstallation", func(t *testing.T) {
		// Two accounts, two grants. Reconciling one must not disturb the other:
		// a per-installation replace that reached across would make a
		// second-account failure look like a first-account revocation.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		install(t, b, "2", "other", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (1): %v", err)
		}
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "2", []domain.InstallationRepository{
			entry(b, "2", "other", "tool", "20"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (2): %v", err)
		}
		if err := b.Mirror.ClearForInstallationSystem(ctx, b.OrgID, "2"); err != nil {
			t.Fatalf("ClearForInstallationSystem: %v", err)
		}

		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		equal(t, "mirror after clearing the other installation", slugs(t, got), []string{"acme/api"})
	})

	t.Run("SelectedAndAllAreDistinguishableAfterReconcile", func(t *testing.T) {
		// Without this the page cannot tell "nothing is drifting" from "drift is
		// impossible here", and those need different copy.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		install(t, b, "2", "other", domain.RepositorySelectionSelected)

		insts, err := b.Apps.ListInstallationsForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		bySelection := map[string]string{}
		for _, inst := range insts {
			bySelection[inst.AccountLogin] = inst.RepositorySelection
		}
		if bySelection["acme"] != domain.RepositorySelectionAll {
			t.Errorf("acme RepositorySelection = %q; want %q", bySelection["acme"], domain.RepositorySelectionAll)
		}
		if bySelection["other"] != domain.RepositorySelectionSelected {
			t.Errorf("other RepositorySelection = %q; want %q", bySelection["other"], domain.RepositorySelectionSelected)
		}
		for _, inst := range insts {
			if inst.AccountLogin == "acme" && !inst.GrantsEveryRepository() {
				t.Error("GrantsEveryRepository() = false for an 'all' installation")
			}
			if inst.AccountLogin == "other" && inst.GrantsEveryRepository() {
				t.Error("GrantsEveryRepository() = true for a 'selected' installation")
			}
		}
	})

	t.Run("AWriterThatDidNotLookDoesNotEraseTheSelection", func(t *testing.T) {
		// The selection takes the account id's fill-in-only rule, not the
		// login's overwrite one: a caller that built the struct from something
		// narrower than the /app/installations listing did not look, and
		// treating that silence as "unknown" would throw away a width already
		// established. Unknown stays unknown until something reports one.
		b := mk(t)
		install(t, b, "1", "acme", "")
		insts, err := b.Apps.ListInstallationsForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if len(insts) != 1 || insts[0].RepositorySelection != "" {
			t.Fatalf("RepositorySelection = %q before anything reported one; want \"\"", insts[0].RepositorySelection)
		}

		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		install(t, b, "1", "acme", "") // a later writer that didn't look

		insts, err = b.Apps.ListInstallationsForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListInstallationsForOrgSystem: %v", err)
		}
		if insts[0].RepositorySelection != domain.RepositorySelectionSelected {
			t.Errorf("RepositorySelection = %q after a writer that omitted it; want the stored %q",
				insts[0].RepositorySelection, domain.RepositorySelectionSelected)
		}
	})

	t.Run("ReachWithoutPurposeIsGrantedMinusTracked", func(t *testing.T) {
		// The finding: TF holds write access to code nobody asked it to touch.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		b.TrackRepo(t, "acme", "api")
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
			entry(b, "1", "acme", "secrets", "11"),
			entry(b, "1", "acme", "payroll", "12"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		got, err := b.Mirror.ListReachWithoutPurposeSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListReachWithoutPurposeSystem: %v", err)
		}
		equal(t, "reach without purpose", slugs(t, got), []string{"acme/payroll", "acme/secrets"})
	})

	t.Run("ReachWithoutPurposeFoldsCase", func(t *testing.T) {
		// A tracked repository reported as untracked because someone
		// capitalized the slug differently is a fabricated security finding,
		// which is worse than a missed one.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		b.TrackRepo(t, "Acme", "API")
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		got, err := b.Mirror.ListReachWithoutPurposeSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListReachWithoutPurposeSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("reach without purpose = %v; want none — the tracked repo differs only in casing", slugs(t, got))
		}
	})

	t.Run("ReachWithoutPurposeIgnoresRemovedInstallations", func(t *testing.T) {
		// Reach the App no longer has is not reach. The read joins live
		// installations for the same reason the removal clears the rows: two
		// independent statements of one rule, since a false finding here reads
		// as a live security problem.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "secrets", "11"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		got, err := b.Mirror.ListReachWithoutPurposeSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListReachWithoutPurposeSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("reach without purpose = %v after the installation was removed; want none", slugs(t, got))
		}
	})

	t.Run("ScopeDriftIsTrackedMinusGranted", func(t *testing.T) {
		// The mirror image: a team tracks a repository the App cannot reach, so
		// it is silently unpolled.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		b.TrackRepo(t, "acme", "api")
		b.TrackRepo(t, "acme", "legacy")
		b.TrackRepo(t, "Acme", "Web") // folds onto the granted acme/web
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
			entry(b, "1", "acme", "web", "11"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		got, err := b.Mirror.ListScopeDriftSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListScopeDriftSystem: %v", err)
		}
		equal(t, "scope drift", trackedSlugs(got), []string{"acme/legacy"})
	})

	t.Run("ScopeDriftReportsOneRepositoryOnce", func(t *testing.T) {
		// Two teams tracking one repository under different casings are tracking
		// one repository. Listing it twice would double-count the finding, which
		// is how a two-repo drift reads as four.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		b.TrackRepo(t, "acme", "legacy")
		b.TrackRepo(t, "Acme", "Legacy")
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		got, err := b.Mirror.ListScopeDriftSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListScopeDriftSystem: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("scope drift = %v; want one entry for one repository", trackedSlugs(got))
		}
	})

	t.Run("ScopeDriftIsEmptyWithoutAMirror", func(t *testing.T) {
		// An org with no App, or one whose first reconcile has not landed, knows
		// nothing about its grant. "Not in the mirror" then describes everything
		// it tracks, and reporting that as drift turns "we have not looked yet"
		// into "your tracking is broken".
		b := mk(t)
		install(t, b, "1", "acme", "")
		b.TrackRepo(t, "acme", "api")
		b.TrackRepo(t, "acme", "web")

		got, err := b.Mirror.ListScopeDriftSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListScopeDriftSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("scope drift = %v with an empty mirror; want none — unknown is not a finding", trackedSlugs(got))
		}
	})

	t.Run("ScopeDriftIgnoresRemovedInstallations", func(t *testing.T) {
		// A grant that no longer exists cannot vouch for a tracked repository.
		// Once the last live installation is gone the org is back to knowing
		// nothing, so the answer is empty rather than "everything drifts".
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		b.TrackRepo(t, "acme", "api")
		b.TrackRepo(t, "acme", "legacy")
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, "1", []domain.InstallationRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if drift, err := b.Mirror.ListScopeDriftSystem(ctx, b.OrgID); err != nil {
			t.Fatalf("ListScopeDriftSystem: %v", err)
		} else if len(drift) != 1 {
			t.Fatalf("scope drift = %v before removal; want [acme/legacy]", trackedSlugs(drift))
		}

		if err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		got, err := b.Mirror.ListScopeDriftSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListScopeDriftSystem: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("scope drift = %v after the installation was removed; want none", trackedSlugs(got))
		}
	})
}
