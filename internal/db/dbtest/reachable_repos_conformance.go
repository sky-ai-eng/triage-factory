package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ReachableReposBackend is one wired backend for the grant-mirror suite.
// Installations are staged through the store rather than raw SQL (both dialects
// expose UpsertInstallation), so the only fixture a backend has to supply by
// hand is a tracked repository — team_github_repos has no store method that
// works claims-free on both pools.
type ReachableReposBackend struct {
	Apps   db.GitHubAppsStore
	Mirror db.ReachableReposStore
	// OrgID every call in the suite passes. SQLite pins the local sentinel;
	// Postgres mints a real org.
	OrgID string
	// TrackRepo records that some team in the org tracks owner/repo.
	TrackRepo func(t *testing.T, owner, repo string)
}

// ReachableReposFactory returns a fresh, isolated backend per subtest.
type ReachableReposFactory func(t *testing.T) ReachableReposBackend

// RunReachableReposConformance is the shared assertion suite for the
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
func RunReachableReposConformance(t *testing.T, mk ReachableReposFactory) {
	t.Helper()
	ctx := context.Background()

	// The class almost every case below writes under: an org that brought its own
	// App. The managed cases name their own, and the completeness case walks the
	// whole set.
	const byoApp = domain.GitHubCredentialClassBYOApp

	// install stages a live installation row — the FK parent every mirror row
	// hangs off, and the thing the reads join through.
	install := func(t *testing.T, b ReachableReposBackend, installationID, login, selection string) {
		t.Helper()
		if _, err := b.Apps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID:      installationID,
			OrgID:               b.OrgID,
			AccountType:         "Organization",
			AccountLogin:        login,
			RepositorySelection: selection,
		}); err != nil {
			t.Fatalf("UpsertInstallation(%s): %v", installationID, err)
		}
	}

	entry := func(b ReachableReposBackend, installationID, owner, repo, externalID string) domain.ReachableRepository {
		return domain.ReachableRepository{
			OrgID:          b.OrgID,
			InstallationID: installationID,
			Owner:          owner,
			Repo:           repo,
			ExternalID:     externalID,
		}
	}

	slugs := func(t *testing.T, rows []domain.ReachableRepository) []string {
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
			entry(b, "1", "acme", "web", "11"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		// The account owner narrows the selection: web out, worker in. A mirror
		// that only ever grew would keep reporting reach the App has lost, which
		// is the failure the whole table exists to prevent.
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		bad := entry(b, "1", "acme", "web", "11")
		bad.Source = "gitlab" // not a provider this schema knows
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (1): %v", err)
		}
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "2", []domain.ReachableRepository{
			entry(b, "2", "other", "tool", "20"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (2): %v", err)
		}

		if _, err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}

		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		equal(t, "mirror after one installation was removed", slugs(t, got), []string{"other/tool"})
	})

	t.Run("ClearingWithoutAnInstallationIsRefused", func(t *testing.T) {
		// A write path called only when the grant is KNOWN to be gone. "No rows
		// matched" and "the caller named no installation" must not be the same
		// answer: reporting success for the second would leave every
		// installation's mirror standing while the caller believes one was
		// cleared.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		if err := b.Mirror.ClearForInstallationSystem(ctx, b.OrgID, ""); err == nil {
			t.Error("ClearForInstallationSystem accepted an empty installation id; want an error")
		}
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "", nil); err == nil {
			t.Error("ReplaceForInstallationSystem accepted an empty installation id; want an error")
		}

		got, err := b.Mirror.ListForOrgSystem(ctx, b.OrgID)
		if err != nil {
			t.Fatalf("ListForOrgSystem: %v", err)
		}
		equal(t, "mirror after two refused writes", slugs(t, got), []string{"acme/api"})
	})

	t.Run("MirrorsAreScopedToTheirInstallation", func(t *testing.T) {
		// Two accounts, two grants. Reconciling one must not disturb the other:
		// a per-installation replace that reached across would make a
		// second-account failure look like a first-account revocation.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionAll)
		install(t, b, "2", "other", domain.RepositorySelectionAll)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem (1): %v", err)
		}
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "2", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "secrets", "11"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if _, err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
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
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if drift, err := b.Mirror.ListScopeDriftSystem(ctx, b.OrgID); err != nil {
			t.Fatalf("ListScopeDriftSystem: %v", err)
		} else if len(drift) != 1 {
			t.Fatalf("scope drift = %v before removal; want [acme/legacy]", trackedSlugs(drift))
		}

		if _, err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
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

	// ---------------------------------------------------------------
	// The generalized half: the reads the picker and the write gate run, and
	// the PAT tier that has no installation to hang off.
	// ---------------------------------------------------------------

	patEntry := func(b ReachableReposBackend, owner, repo, language, description string) domain.ReachableRepository {
		return domain.ReachableRepository{
			OrgID:       b.OrgID,
			Owner:       owner,
			Repo:        repo,
			Language:    language,
			Description: description,
		}
	}

	listSlugs := func(t *testing.T, b ReachableReposBackend, class domain.GitHubCredentialClass, q string, opts db.ListOpts) ([]string, int) {
		t.Helper()
		rows, total, err := b.Mirror.ListReachableSystem(ctx, b.OrgID, class, q, opts)
		if err != nil {
			t.Fatalf("ListReachableSystem(%q, %q): %v", class, q, err)
		}
		return slugs(t, rows), total
	}

	t.Run("PATTierNeedsNoInstallation", func(t *testing.T) {
		// The whole point of generalizing the mirror: a PAT org has no
		// installation row to cascade from, so its entries hang off the host.
		// Nothing here stages an installation, and the write must still land.
		b := mk(t)
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "acme", "web", "Go", ""),
			patEntry(b, "acme", "api", "Go", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem: %v", err)
		}
		got, total := listSlugs(t, b, domain.GitHubCredentialClassPAT, "", db.Unwindowed)
		equal(t, "pat list", got, []string{"acme/api", "acme/web"})
		if total != 2 {
			t.Errorf("total = %d; want 2 — a store-backed list can count itself, which a proxy list could not", total)
		}
	})

	t.Run("PATReplaceIsWholeClassIncludingAHostChange", func(t *testing.T) {
		// An org authenticates against one GitHub deployment at a time. Rows left
		// by a previous GitHubBaseURL are the old deployment's repositories, not a
		// second scope to keep — serving them beside the new host's would answer
		// with a reach the org does not have.
		b := mk(t)
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://ghe.internal", []domain.ReachableRepository{
			patEntry(b, "old", "repo", "", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem(old host): %v", err)
		}
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "new", "repo", "", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem(new host): %v", err)
		}
		got, total := listSlugs(t, b, domain.GitHubCredentialClassPAT, "", db.Unwindowed)
		equal(t, "pat list after host change", got, []string{"new/repo"})
		if total != 1 {
			t.Errorf("total = %d after a host change; want 1", total)
		}
	})

	t.Run("ClassesDoNotBleedIntoEachOther", func(t *testing.T) {
		// An org mid-cutover has both answers on file. Every read filters on the
		// org's CURRENT class, so the other tier's rows are inert rather than
		// merged in — the union would be a reach the org does not have.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "granted", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "acme", "personal", "", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem: %v", err)
		}

		app, appTotal := listSlugs(t, b, domain.GitHubCredentialClassBYOApp, "", db.Unwindowed)
		equal(t, "byo_app list", app, []string{"acme/granted"})
		pat, patTotal := listSlugs(t, b, domain.GitHubCredentialClassPAT, "", db.Unwindowed)
		equal(t, "pat list", pat, []string{"acme/personal"})
		if appTotal != 1 || patTotal != 1 {
			t.Errorf("totals = (%d, %d); want (1, 1) — each class counts only its own", appTotal, patTotal)
		}
	})

	t.Run("ListFiltersOnSlugDescriptionAndLanguage", func(t *testing.T) {
		// The three fields the picker used to filter on in the browser. Moving the
		// filter server-side is what lets the surface stop holding the whole set in
		// memory, so all three have to survive the move.
		b := mk(t)
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "acme", "api", "Go", "the billing service"),
			patEntry(b, "acme", "web", "TypeScript", "the storefront"),
			patEntry(b, "other", "tool", "Rust", "a helper"),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem: %v", err)
		}

		bySlug, total := listSlugs(t, b, domain.GitHubCredentialClassPAT, "acme/", db.Unwindowed)
		equal(t, "slug filter", bySlug, []string{"acme/api", "acme/web"})
		if total != 2 {
			t.Errorf("slug-filter total = %d; want 2 — the total is what the filter matches, not the page", total)
		}
		byDesc, _ := listSlugs(t, b, domain.GitHubCredentialClassPAT, "storefront", db.Unwindowed)
		equal(t, "description filter", byDesc, []string{"acme/web"})
		byLang, _ := listSlugs(t, b, domain.GitHubCredentialClassPAT, "rust", db.Unwindowed)
		equal(t, "language filter", byLang, []string{"other/tool"})
		// Case-insensitive on the slug too: GitHub identifiers are.
		byCase, _ := listSlugs(t, b, domain.GitHubCredentialClassPAT, "ACME/API", db.Unwindowed)
		equal(t, "case-insensitive filter", byCase, []string{"acme/api"})
	})

	t.Run("ListFilterTreatsMetacharactersLiterally", func(t *testing.T) {
		// A search box takes whatever is typed. An unescaped % would match every
		// row, which reads as "the filter did nothing" rather than as "no match".
		b := mk(t)
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "acme", "api", "", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem: %v", err)
		}
		got, total := listSlugs(t, b, domain.GitHubCredentialClassPAT, "%", db.Unwindowed)
		if len(got) != 0 || total != 0 {
			t.Errorf("filter %%%% matched %v (total %d); want nothing — the metacharacter is a literal here", got, total)
		}
	})

	t.Run("ListPagesOnATotalOrder", func(t *testing.T) {
		// Offset paging over a set without a total order drops and repeats rows.
		// The folded slug is that order, and the per-class unique index proves no
		// two rows tie on it.
		b := mk(t)
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "acme", "c", "", ""),
			patEntry(b, "acme", "a", "", ""),
			patEntry(b, "acme", "b", "", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem: %v", err)
		}
		first, total := listSlugs(t, b, domain.GitHubCredentialClassPAT, "", db.ListOpts{Limit: 2})
		equal(t, "page 1", first, []string{"acme/a", "acme/b"})
		if total != 3 {
			t.Errorf("page-1 total = %d; want the filtered total 3, not the page length", total)
		}
		second, total := listSlugs(t, b, domain.GitHubCredentialClassPAT, "", db.ListOpts{Limit: 2, Offset: 2})
		equal(t, "page 2", second, []string{"acme/c"})
		if total != 3 {
			t.Errorf("page-2 total = %d; want 3 on every page", total)
		}
	})

	t.Run("StateReportsTheStalestScope", func(t *testing.T) {
		// A cache is only as fresh as its oldest scope. An org with two
		// installations, one refreshed now and one long ago, is stale for the
		// repositories on the second account — and taking the newest would report
		// it fresh and never refresh the half that needs it.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		install(t, b, "2", "beta", domain.RepositorySelectionSelected)

		empty, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("ReachableStateSystem(empty): %v", err)
		}
		if empty.Known() || empty.Count != 0 {
			t.Errorf("state = %+v before any write; want unknown and empty — the readiness gate keys on it", empty)
		}

		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem(1): %v", err)
		}
		afterFirst, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("ReachableStateSystem(after first): %v", err)
		}
		if !afterFirst.Known() || afterFirst.Count != 1 || afterFirst.ObservedAt.IsZero() {
			t.Fatalf("state = %+v after one scope; want known, one row, and an observed_at", afterFirst)
		}

		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "2", []domain.ReachableRepository{
			entry(b, "2", "beta", "web", "20"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem(2): %v", err)
		}
		afterSecond, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("ReachableStateSystem(after second): %v", err)
		}
		if afterSecond.Count != 2 {
			t.Errorf("state count = %d across two scopes; want 2", afterSecond.Count)
		}
		if afterSecond.ObservedAt.After(afterFirst.ObservedAt) {
			t.Errorf("observed_at moved forward to %v when the older scope still reads %v; the state must describe the STALEST scope",
				afterSecond.ObservedAt, afterFirst.ObservedAt)
		}
	})

	t.Run("AnEmptyRefreshIsKnownNotUnlooked", func(t *testing.T) {
		// The distinction row counts cannot express. An installation narrowed to
		// no repositories writes zero entries, exactly like a refresh that never
		// ran — and read as the latter, the picker says "discovering
		// repositories…" forever and asks for a fresh enumeration on every open.
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", nil); err != nil {
			t.Fatalf("ReplaceForInstallationSystem(empty): %v", err)
		}
		state, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("ReachableStateSystem: %v", err)
		}
		if !state.Known() {
			t.Error("a successful refresh that found nothing reads as never-refreshed; the readiness gate cannot tell them apart")
		}
		if state.Count != 0 {
			t.Errorf("count = %d after an empty refresh; want 0", state.Count)
		}
		if state.StaleAt(state.ObservedAt.Add(time.Minute), time.Hour) {
			t.Error("an empty-but-refreshed scope reads as stale a minute later; it would re-enumerate on every open")
		}
	})

	t.Run("RetiringAnInstallationRetiresItsRefresh", func(t *testing.T) {
		// The mirror image: once an installation is gone, the refresh that
		// established its reach must stop vouching for it, or an org whose only
		// installation was removed keeps reading as "looked at, found nothing".
		b := mk(t)
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, byoApp, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if _, err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		state, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, domain.GitHubCredentialClassBYOApp)
		if err != nil {
			t.Fatalf("ReachableStateSystem: %v", err)
		}
		if state.Known() {
			t.Error("the removed installation's refresh still vouches for the org's reach")
		}
	})

	t.Run("SlugSetAnswersTheWriteGate", func(t *testing.T) {
		// The write gate's read: bounded by the caller's selection rather than by
		// org size, keyed lowercase because GitHub identifiers are
		// case-insensitive, and silent about slugs it does not hold — which is what
		// makes an absent slug a rejection rather than a lookup failure.
		b := mk(t)
		if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com", []domain.ReachableRepository{
			patEntry(b, "Acme", "API", "", ""),
		}); err != nil {
			t.Fatalf("ReplaceForPATSystem: %v", err)
		}
		got, err := b.Mirror.ReachableSlugsSystem(ctx, b.OrgID, domain.GitHubCredentialClassPAT,
			[]string{"acme/api", "acme/ghost"})
		if err != nil {
			t.Fatalf("ReachableSlugsSystem: %v", err)
		}
		if _, ok := got["acme/api"]; !ok {
			t.Errorf("reachable set = %v; want it to hold acme/api despite the stored casing", got)
		}
		if _, ok := got["acme/ghost"]; ok {
			t.Errorf("reachable set = %v; want acme/ghost absent", got)
		}

		// The other class holds nothing, so the same selection comes back empty
		// rather than borrowing the PAT tier's answer.
		other, err := b.Mirror.ReachableSlugsSystem(ctx, b.OrgID, domain.GitHubCredentialClassBYOApp,
			[]string{"acme/api"})
		if err != nil {
			t.Fatalf("ReachableSlugsSystem(other class): %v", err)
		}
		if len(other) != 0 {
			t.Errorf("byo_app slug set = %v; want empty — a class answers only for its own entries", other)
		}

		// An empty selection is an empty answer, not an error and not the world.
		if none, err := b.Mirror.ReachableSlugsSystem(ctx, b.OrgID, domain.GitHubCredentialClassPAT, nil); err != nil {
			t.Fatalf("ReachableSlugsSystem(nil): %v", err)
		} else if len(none) != 0 {
			t.Errorf("empty selection returned %v; want nothing", none)
		}
	})

	t.Run("ManagedWorkspaceRoundTrips", func(t *testing.T) {
		// A workspace riding the deployment's shared App keys its reach exactly as
		// one on its own App does: per installation, with a host of nothing. Until
		// the tables were taught the class, every one of these writes was refused
		// by a CHECK — and the picker read that follows returned nothing to serve.
		b := mk(t)
		const managed = domain.GitHubCredentialClassManagedApp
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, managed, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
			entry(b, "1", "acme", "web", "11"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}

		rows, total, err := b.Mirror.ListReachableSystem(ctx, b.OrgID, managed, "", db.Unwindowed)
		if err != nil {
			t.Fatalf("ListReachableSystem: %v", err)
		}
		equal(t, "managed workspace's reachable set", slugs(t, rows), []string{"acme/api", "acme/web"})
		if total != 2 {
			t.Errorf("total_count = %d; want 2 — a real count is the whole reason the picker reads this table", total)
		}
		for _, row := range rows {
			if row.CredentialClass != managed {
				t.Errorf("%s came back keyed %q; want %q", row.Slug(), row.CredentialClass, managed)
			}
			if row.InstallationID != "1" || row.Host != "" {
				t.Errorf("%s carries installation %q / host %q; want the installation scope and no host",
					row.Slug(), row.InstallationID, row.Host)
			}
		}

		state, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, managed)
		if err != nil {
			t.Fatalf("ReachableStateSystem: %v", err)
		}
		if !state.Known() || state.Count != 2 {
			t.Errorf("state = %+v; want known and holding two rows", state)
		}

		// The other App class answers nothing: an org holds one class at a time,
		// and every read filters on the one it holds now.
		byo, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, byoApp)
		if err != nil {
			t.Fatalf("ReachableStateSystem(byo_app): %v", err)
		}
		if byo.Known() || byo.Count != 0 {
			t.Errorf("byo_app state = %+v for a managed workspace; want unknown and empty", byo)
		}
	})

	t.Run("ManagedFoldedIdentityRefusesASecondCasing", func(t *testing.T) {
		// The silent half. Both uniqueness guarantees on this table are PARTIAL
		// indexes, so a class outside their predicates would carry none at all —
		// and the insert's ON CONFLICT DO NOTHING needs an index to have a conflict
		// to detect, degrading to a plain insert without one. GitHub's paginated
		// listings legitimately return the same repository twice when repos are
		// created mid-walk, so what that costs is duplicate rows and a wrong
		// total_count, which is the one question this table exists to answer.
		b := mk(t)
		const managed = domain.GitHubCredentialClassManagedApp
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, managed, "1", []domain.ReachableRepository{
			entry(b, "1", "Acme", "API", "10"),
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		rows, total, err := b.Mirror.ListReachableSystem(ctx, b.OrgID, managed, "", db.Unwindowed)
		if err != nil {
			t.Fatalf("ListReachableSystem: %v", err)
		}
		if len(rows) != 1 || total != 1 {
			t.Fatalf("two casings of one repository landed as %d rows (total_count %d); want 1 — the folded key must refuse the second",
				len(rows), total)
		}
	})

	t.Run("RetiringAManagedInstallationRetiresItsReach", func(t *testing.T) {
		// The soft removal clears the entries and the refresh marker together,
		// under whichever class observed them: an uninstalled installation reaches
		// nothing, and a marker left behind keeps vouching that a reach was
		// established for an installation that has none.
		b := mk(t)
		const managed = domain.GitHubCredentialClassManagedApp
		install(t, b, "1", "acme", domain.RepositorySelectionSelected)
		if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, managed, "1", []domain.ReachableRepository{
			entry(b, "1", "acme", "api", "10"),
		}); err != nil {
			t.Fatalf("ReplaceForInstallationSystem: %v", err)
		}
		if _, err := b.Apps.MarkInstallationRemoved(ctx, b.OrgID, "1"); err != nil {
			t.Fatalf("MarkInstallationRemoved: %v", err)
		}
		state, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, managed)
		if err != nil {
			t.Fatalf("ReachableStateSystem: %v", err)
		}
		if state.Known() || state.Count != 0 {
			t.Errorf("state = %+v after the installation was removed; want its reach and its marker gone", state)
		}
	})

	t.Run("EveryKnownClassIsWritable", func(t *testing.T) {
		// The guard against the bug this suite's managed cases exist to fix. Both
		// reachable tables CHECK-constrain credential_class and predicate their
		// unique indexes on literal values, so a class the domain admits and the
		// schema has not been taught resolves everywhere and stores nowhere.
		// Walking the accepted set is what makes "remember to widen the CHECKs"
		// something the build enforces the next time a class is added.
		b := mk(t)
		for i, class := range domain.AllGitHubCredentialClasses() {
			t.Run(string(class), func(t *testing.T) {
				// A distinct account per class, since an org holds one installation
				// per account: the App classes also share one identity index, so two
				// of them keyed to one installation and one slug are the same row by
				// construction.
				owner, repo := fmt.Sprintf("acct-%d", i), "api"
				switch {
				case class.AppTier():
					installationID := fmt.Sprintf("%d", 900+i)
					install(t, b, installationID, owner, domain.RepositorySelectionSelected)
					if err := b.Mirror.ReplaceForInstallationSystem(ctx, b.OrgID, class, installationID,
						[]domain.ReachableRepository{entry(b, installationID, owner, repo, "")}); err != nil {
						t.Fatalf("ReplaceForInstallationSystem(%s): %v", class, err)
					}
				case class == domain.GitHubCredentialClassPAT:
					if err := b.Mirror.ReplaceForPATSystem(ctx, b.OrgID, "https://github.com",
						[]domain.ReachableRepository{patEntry(b, owner, repo, "", "")}); err != nil {
						t.Fatalf("ReplaceForPATSystem: %v", err)
					}
				default:
					t.Fatalf("no write path stores class %q; a class the domain accepts and nothing can write is one that resolves everywhere and mirrors nowhere", class)
				}

				state, err := b.Mirror.ReachableStateSystem(ctx, b.OrgID, class)
				if err != nil {
					t.Fatalf("ReachableStateSystem(%s): %v", class, err)
				}
				if !state.Known() {
					t.Errorf("%s: the scope marker did not land; reachable_scopes has not been taught this class", class)
				}
				rows, total, err := b.Mirror.ListReachableSystem(ctx, b.OrgID, class, "", db.Unwindowed)
				if err != nil {
					t.Fatalf("ListReachableSystem(%s): %v", class, err)
				}
				equal(t, string(class)+" reachable set", slugs(t, rows), []string{owner + "/" + repo})
				if total != 1 {
					t.Errorf("%s: total_count = %d; want 1", class, total)
				}
			})
		}
	})
}
