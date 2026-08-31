package grantmirror

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

const testOrg = "org-1"

// fakeApps stands in for the App-installation store. The embedded interface is
// nil on purpose: a method the reconcile is not supposed to call panics rather
// than quietly returning a zero value.
type fakeApps struct {
	db.GitHubAppsStore

	// app is the org's registration. Defaulted to an ACTIVE App by fakeApps —
	// the shape every grant-reading test needs — since an inactive one is
	// deliberately skipped before any grant read.
	app *domain.OrgGitHubApp

	backfillErr   error
	backfillCalls int
	// registrationReads counts reads of the org's own App registration — the row
	// a managed workspace does not have and must not be asked for.
	registrationReads int
	listErr           error
	installs          []domain.OrgGitHubAppInstallation
	// listedAfterBackfill records whether the installation read happened after
	// the existence reconcile, which is the ordering the whole pass depends on.
	listedAfterBackfill bool
}

func (f *fakeApps) GetForOrgSystem(context.Context, string) (*domain.OrgGitHubApp, error) {
	f.registrationReads++
	return f.app, nil
}

func (f *fakeApps) BackfillInstallationsFromAPI(_ context.Context, _ string) error {
	f.backfillCalls++
	return f.backfillErr
}

func (f *fakeApps) ListInstallationsForOrgSystem(_ context.Context, _ string) ([]domain.OrgGitHubAppInstallation, error) {
	f.listedAfterBackfill = f.backfillCalls > 0
	return f.installs, f.listErr
}

// fakeMirror records every write so a test can assert not just what the mirror
// holds but whether it was touched at all — the difference between "left alone"
// and "rewritten with the same content" is the invariant under test.
type fakeMirror struct {
	rows map[string][]string // installation id → slugs
	// classes records the class each scope was written under: the key every read
	// filters on, so a pass that writes the right slugs under the wrong class
	// mirrors into a place nothing looks.
	classes  map[string]domain.GitHubCredentialClass
	replaces int
	cleared  []string
	err      error
}

func newFakeMirror() *fakeMirror {
	return &fakeMirror{rows: map[string][]string{}, classes: map[string]domain.GitHubCredentialClass{}}
}

func (f *fakeMirror) ReplaceForInstallationSystem(_ context.Context, _ string, class domain.GitHubCredentialClass, installationID string, repos []domain.ReachableRepository) error {
	f.replaces++
	if f.err != nil {
		return f.err
	}
	slugs := make([]string, 0, len(repos))
	for _, r := range repos {
		slugs = append(slugs, r.Slug())
	}
	f.rows[installationID] = slugs
	f.classes[installationID] = class
	return nil
}

func (f *fakeMirror) ClearForInstallationSystem(_ context.Context, _, installationID string) error {
	f.cleared = append(f.cleared, installationID)
	delete(f.rows, installationID)
	return nil
}

func (f *fakeMirror) ListForOrgSystem(context.Context, string) ([]domain.ReachableRepository, error) {
	return nil, nil
}

func (f *fakeMirror) ListReachWithoutPurposeSystem(context.Context, string) ([]domain.ReachableRepository, error) {
	return nil, nil
}

func (f *fakeMirror) ListScopeDriftSystem(context.Context, string) ([]domain.TeamGitHubRepo, error) {
	return nil, nil
}

// The PAT-tier half of the store. The reconcile is the App tier's writer and
// never reaches these, so they exist only to satisfy the interface — and they
// panic rather than no-op, so a reconcile that grew a PAT code path would fail
// loudly here instead of writing nothing and passing.
func (f *fakeMirror) ReplaceForPATSystem(context.Context, string, string, []domain.ReachableRepository) error {
	panic("grantmirror must not write pat-tier entries")
}

func (f *fakeMirror) ListReachableSystem(context.Context, string, domain.GitHubCredentialClass, string, db.ListOpts) ([]domain.ReachableRepository, int, error) {
	panic("grantmirror must not read the picker list")
}

func (f *fakeMirror) ReachableStateSystem(context.Context, string, domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	panic("grantmirror must not read cache state")
}

func (f *fakeMirror) ReachableSlugsSystem(context.Context, string, domain.GitHubCredentialClass, []string) (map[string]struct{}, error) {
	panic("grantmirror must not read the write gate's slug set")
}

// fakeGrants answers per account login: a grant, whether the listing was
// complete, and an error.
type fakeGrants struct {
	byLogin    map[string]grantAnswer
	resolveErr map[string]error
}

type grantAnswer struct {
	repos    []github.UserRepo
	complete bool
	err      error
}

func (f fakeGrants) grantListerFor(_ context.Context, _, accountLogin string) (grantLister, error) {
	if err := f.resolveErr[accountLogin]; err != nil {
		return nil, err
	}
	answer, ok := f.byLogin[accountLogin]
	if !ok {
		return nil, errors.New("no client for " + accountLogin)
	}
	return stubLister{answer}, nil
}

type stubLister struct{ answer grantAnswer }

func (s stubLister) ListInstallationReposComplete(context.Context) ([]github.UserRepo, bool, error) {
	return s.answer.repos, s.answer.complete, s.answer.err
}

func repo(fullName string, id int64) github.UserRepo {
	return github.UserRepo{ID: id, FullName: fullName}
}

// activeApp is the registration shape every grant read requires: registered and
// ACTIVE, so tier-1 installation-token minting is switched on.
func activeApp() *domain.OrgGitHubApp {
	return &domain.OrgGitHubApp{OrgID: testOrg, AppID: "1", Active: true}
}

func live(installationID, login string) domain.OrgGitHubAppInstallation {
	return domain.OrgGitHubAppInstallation{
		InstallationID: installationID,
		OrgID:          testOrg,
		AccountType:    "Organization",
		AccountLogin:   login,
	}
}

// fakeClasses stands in for reachcache.ClassResolver: one fixed answer per
// test, since what the resolver decides is settled elsewhere and what matters
// here is which pass that answer selects.
type fakeClasses struct {
	class domain.GitHubCredentialClass
	err   error
}

func (f fakeClasses) For(context.Context, string) (domain.GitHubCredentialClass, error) {
	return f.class, f.err
}

// newReconciler builds the pass for a workspace that owns its App key, which is
// the shape every case below takes bar the managed ones. The staged and PAT
// cases keep this class deliberately: the registration gate they pin runs before
// the class selects anything, so the answer the live resolver would narrow to
// changes nothing about what they assert.
func newReconciler(apps db.GitHubAppsStore, mirror db.ReachableReposStore, grants clientSource) *Reconciler {
	return newReconcilerAs(domain.GitHubCredentialClassBYOApp, apps, mirror, grants)
}

func newReconcilerAs(class domain.GitHubCredentialClass, apps db.GitHubAppsStore, mirror db.ReachableReposStore, grants clientSource) *Reconciler {
	return &Reconciler{apps: apps, mirror: mirror, clients: grants, classes: fakeClasses{class: class}}
}

func TestRunOrg_MirrorsEachInstallationsGrant(t *testing.T) {
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme"), live("2", "other")}}
	mirror := newFakeMirror()
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme":  {repos: []github.UserRepo{repo("acme/api", 10), repo("acme/web", 11)}, complete: true},
		"other": {repos: []github.UserRepo{repo("other/tool", 20)}, complete: true},
	}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if !apps.listedAfterBackfill {
		t.Error("installations were listed before the existence reconcile ran; a stale set is what the pass exists to fix")
	}
	if got := mirror.rows["1"]; len(got) != 2 || got[0] != "acme/api" || got[1] != "acme/web" {
		t.Errorf("installation 1 mirror = %v; want [acme/api acme/web]", got)
	}
	if got := mirror.rows["2"]; len(got) != 1 || got[0] != "other/tool" {
		t.Errorf("installation 2 mirror = %v; want [other/tool]", got)
	}
}

func TestRunOrg_FetchFailureLeavesTheMirrorAlone(t *testing.T) {
	// The most important line in the ticket: a reconcile that cannot reach
	// GitHub leaves the previous answer in place. An emptied mirror renders
	// "the App can reach code nobody tracks" as a false all-clear.
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	mirror := newFakeMirror()
	mirror.rows["1"] = []string{"acme/api"} // what a previous pass established
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme": {err: errors.New("502 bad gateway")},
	}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err == nil {
		t.Fatal("RunOrg returned nil after the grant fetch failed; the failure must be visible")
	}
	if mirror.replaces != 0 {
		t.Errorf("mirror was written %d times after a failed fetch; want 0", mirror.replaces)
	}
	if got := mirror.rows["1"]; len(got) != 1 || got[0] != "acme/api" {
		t.Errorf("mirror = %v after a failed fetch; want the previous answer [acme/api]", got)
	}
}

func TestRunOrg_ClientResolutionFailureLeavesTheMirrorAlone(t *testing.T) {
	// Same invariant one step earlier: no usable credential is not evidence of
	// a narrowed grant either.
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	mirror := newFakeMirror()
	mirror.rows["1"] = []string{"acme/api"}
	grants := fakeGrants{resolveErr: map[string]error{"acme": errors.New("no credentials")}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err == nil {
		t.Fatal("RunOrg returned nil after client resolution failed")
	}
	if mirror.replaces != 0 {
		t.Errorf("mirror was written %d times without a client; want 0", mirror.replaces)
	}
}

func TestRunOrg_TruncatedListingIsRefused(t *testing.T) {
	// A partial answer written as a whole one makes every repository past the
	// cut look revoked — a manufactured finding rather than a missing one.
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	mirror := newFakeMirror()
	mirror.rows["1"] = []string{"acme/api", "acme/web"}
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme": {repos: []github.UserRepo{repo("acme/api", 10)}, complete: false},
	}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err) // not an error — nothing went wrong, we just declined to write
	}
	if mirror.replaces != 0 {
		t.Errorf("mirror was written %d times from a truncated listing; want 0", mirror.replaces)
	}
	if got := mirror.rows["1"]; len(got) != 2 {
		t.Errorf("mirror = %v after a truncated listing; want the previous answer intact", got)
	}
}

func TestRunOrg_SuspendedInstallationIsNotReconciledToEmpty(t *testing.T) {
	// A suspension refuses every token minted from the installation, but the
	// grant itself is untouched and resumes intact on unsuspend. Turning those
	// 403s into "this installation now reaches nothing" is the false all-clear
	// the lifecycle table forbids.
	suspended := live("1", "acme")
	suspended.SuspendedAt = time.Now().UTC()
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{suspended}}
	mirror := newFakeMirror()
	mirror.rows["1"] = []string{"acme/api"}
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme": {err: errors.New("403 installation suspended")},
	}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err) // a suspension is a state, not a failure
	}
	if mirror.replaces != 0 {
		t.Errorf("mirror was written %d times for a suspended installation; want 0", mirror.replaces)
	}
	if got := mirror.rows["1"]; len(got) != 1 || got[0] != "acme/api" {
		t.Errorf("mirror = %v for a suspended installation; want the previous answer [acme/api]", got)
	}
}

func TestRunOrg_OneInstallationsFailureDoesNotStopAnother(t *testing.T) {
	// Per-installation isolation: one account's expired credential must not
	// starve another account's refresh, exactly as the poll cycle isolates its
	// per-installation mints.
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme"), live("2", "other")}}
	mirror := newFakeMirror()
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme":  {err: errors.New("502 bad gateway")},
		"other": {repos: []github.UserRepo{repo("other/tool", 20)}, complete: true},
	}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err == nil {
		t.Fatal("RunOrg returned nil though one installation failed")
	}
	if got := mirror.rows["2"]; len(got) != 1 || got[0] != "other/tool" {
		t.Errorf("second installation mirror = %v; want [other/tool] — its refresh must survive the first's failure", got)
	}
}

func TestRunOrg_ExistenceFailureStopsThePass(t *testing.T) {
	// Without a trustworthy installation set, "GitHub no longer reports this"
	// and "we could not ask" are indistinguishable, and acting on the second as
	// though it were the first is how a mirror empties itself.
	apps := &fakeApps{app: activeApp(), backfillErr: errors.New("app pem unreadable")}
	mirror := newFakeMirror()
	mirror.rows["1"] = []string{"acme/api"}

	if err := newReconciler(apps, mirror, fakeGrants{}).RunOrg(context.Background(), testOrg); err == nil {
		t.Fatal("RunOrg returned nil though the existence reconcile failed")
	}
	if apps.listedAfterBackfill {
		t.Error("installations were listed after the existence reconcile failed; the pass must stop")
	}
	if mirror.replaces != 0 || len(mirror.cleared) != 0 {
		t.Errorf("mirror was touched (%d replaces, %d clears) after the existence reconcile failed; want none",
			mirror.replaces, len(mirror.cleared))
	}
}

func TestRunOrg_NoInstallationsIsANoOp(t *testing.T) {
	// A PAT-only org, or one whose App was just torn down. Nothing to mirror,
	// and in particular nothing to clear: the clearing happens on removal, not
	// on absence.
	apps := &fakeApps{app: activeApp()}
	mirror := newFakeMirror()
	if err := newReconciler(apps, mirror, fakeGrants{}).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if mirror.replaces != 0 || len(mirror.cleared) != 0 {
		t.Errorf("mirror was touched for an org with no installations; want untouched")
	}
}

func TestRunOrg_CarriesRepositoryIDs(t *testing.T) {
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	mirror := newFakeMirror()
	var captured []domain.ReachableRepository
	capture := &capturingMirror{fakeMirror: mirror, onReplace: func(rows []domain.ReachableRepository) {
		captured = rows
	}}
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme": {complete: true, repos: []github.UserRepo{
			repo("acme/api", 10),
			repo("acme/web", 0), // GitHub sent no id
		}},
	}}

	if err := newReconciler(apps, capture, grants).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured %d entries; want 2", len(captured))
	}
	if captured[0].Owner != "acme" || captured[0].Repo != "api" || captured[0].ExternalID != "10" {
		t.Errorf("first entry = %+v; want acme/api with external id 10", captured[0])
	}
	if captured[1].ExternalID != "" {
		t.Errorf("entry with no GitHub id = %q; want \"\" rather than the string zero", captured[1].ExternalID)
	}
	if captured[0].Source != domain.RepoSourceGitHub {
		t.Errorf("Source = %q; want %q", captured[0].Source, domain.RepoSourceGitHub)
	}
}

func TestRunOrg_AnUnnameableEntryRefusesTheWholeAnswer(t *testing.T) {
	// An entry TF cannot name is one it cannot store, and an answer missing an
	// entry is not the whole grant — which is the mirror's entire contract.
	// Writing the rest would persist a grant NARROWER than the real one,
	// understating reach and possibly hiding the very finding the table exists
	// for. Same posture as a truncated listing: keep the previous answer.
	for _, unusable := range []string{"no-slash", "a/b/c", "/api", "acme/", ""} {
		t.Run(unusable, func(t *testing.T) {
			apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
			mirror := newFakeMirror()
			mirror.rows["1"] = []string{"acme/api", "acme/web"}
			grants := fakeGrants{byLogin: map[string]grantAnswer{
				"acme": {complete: true, repos: []github.UserRepo{
					repo("acme/api", 10),
					repo(unusable, 11),
				}},
			}}

			if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err == nil {
				t.Fatal("RunOrg returned nil though an entry could not be named; a partial grant must be visible")
			}
			if mirror.replaces != 0 {
				t.Errorf("mirror was written %d times from an answer with an unnameable entry; want 0", mirror.replaces)
			}
			if got := mirror.rows["1"]; len(got) != 2 {
				t.Errorf("mirror = %v; want the previous answer intact", got)
			}
		})
	}
}

// capturingMirror wraps fakeMirror to expose the exact rows handed to the
// replace, which is what pins the mapping from GitHub's payload onto the row.
type capturingMirror struct {
	*fakeMirror
	onReplace func([]domain.ReachableRepository)
}

func (c *capturingMirror) ReplaceForInstallationSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, installationID string, repos []domain.ReachableRepository) error {
	c.onReplace(repos)
	return c.fakeMirror.ReplaceForInstallationSystem(ctx, orgID, class, installationID, repos)
}

func TestSplitFullName(t *testing.T) {
	// The slug becomes half of a unique key, so a guess here keys a grant entry
	// the tracked set can never match. Two non-empty segments or nothing.
	cases := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"acme/api", "acme", "api", true},
		{"Acme/API", "Acme", "API", true}, // casing is preserved; the key folds, not the value
		{"acme", "", "", false},
		{"/api", "", "", false},
		{"acme/", "", "", false},
		{"a/b/c", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := splitFullName(c.in)
		if owner != c.owner || repo != c.repo || ok != c.ok {
			t.Errorf("splitFullName(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, owner, repo, ok, c.owner, c.repo, c.ok)
		}
	}
}

func TestRunOrg_StagedAppReconcilesExistenceButNotTheGrant(t *testing.T) {
	// A registered-but-inactive App is one staged mid PAT→App switch. Its
	// installations must still be discovered — the cutover preflight verifies
	// them before the bit flips — but its tier-1 mint is switched off, so every
	// grant read would resolve to the org PAT and take a guaranteed 403 from an
	// endpoint that accepts installation tokens only.
	apps := &fakeApps{
		app:      &domain.OrgGitHubApp{OrgID: testOrg, AppID: "1", Active: false},
		installs: []domain.OrgGitHubAppInstallation{live("1", "acme")},
	}
	mirror := newFakeMirror()
	mirror.rows["1"] = []string{"acme/api"}
	// A grant source that fails the test if it is consulted at all.
	grants := fakeGrants{resolveErr: map[string]error{"acme": errors.New("tier-1 mint is off for a staged app")}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err) // a staged App is a state, not a failure
	}
	if apps.backfillCalls != 1 {
		t.Errorf("existence reconcile ran %d times for a staged App; want 1", apps.backfillCalls)
	}
	if mirror.replaces != 0 {
		t.Errorf("mirror was written %d times for a staged App; want 0", mirror.replaces)
	}
	if got := mirror.rows["1"]; len(got) != 1 {
		t.Errorf("mirror = %v for a staged App; want the previous answer intact", got)
	}
}

func TestRunOrg_PATOrgReconcilesNothingAndFailsNothing(t *testing.T) {
	// No App registration at all. The existence reconcile is a no-op store-side,
	// and there is no grant to read — the whole pass has to be silent rather
	// than an error the poll cycle logs every tick.
	apps := &fakeApps{installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	apps.app = nil
	mirror := newFakeMirror()

	if err := newReconciler(apps, mirror, fakeGrants{}).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if mirror.replaces != 0 || len(mirror.cleared) != 0 {
		t.Error("mirror was touched for an org with no App registration; want untouched")
	}
}

func TestRunOrg_BYOOrgKeysItsEntriesUnderItsOwnClass(t *testing.T) {
	// The class is what every read filters on, so the right slugs under the wrong
	// class are entries nothing serves. A workspace on its own App keys them
	// 'byo_app'.
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	mirror := newFakeMirror()
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme": {repos: []github.UserRepo{repo("acme/api", 10)}, complete: true},
	}}

	if err := newReconciler(apps, mirror, grants).RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if got := mirror.classes["1"]; got != domain.GitHubCredentialClassBYOApp {
		t.Errorf("installation 1 keyed under %q; want %q", got, domain.GitHubCredentialClassBYOApp)
	}
}

func TestRunOrg_ManagedWorkspaceRefreshesWhatItBoundAndDiscoversNothing(t *testing.T) {
	// A workspace on the deployment's shared App holds no registration row and
	// can hold none, so the two reads the BYO preamble makes are not skipped for
	// speed: the registration read has no row to find, and the existence
	// reconcile asks a shared key a question that answers with every tenant's
	// installations. What the org bound is the whole of what it reaches.
	apps := &fakeApps{installs: []domain.OrgGitHubAppInstallation{live("7", "acme")}}
	mirror := newFakeMirror()
	grants := fakeGrants{byLogin: map[string]grantAnswer{
		"acme": {repos: []github.UserRepo{repo("acme/api", 10)}, complete: true},
	}}

	reconciler := newReconcilerAs(domain.GitHubCredentialClassManagedApp, apps, mirror, grants)
	if err := reconciler.RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if apps.backfillCalls != 0 {
		t.Errorf("the existence reconcile ran %d times for a managed workspace; want 0 — it discovers, and only the bind may", apps.backfillCalls)
	}
	if apps.registrationReads != 0 {
		t.Errorf("the org's own App registration was read %d times for a managed workspace; want 0 — there is no row", apps.registrationReads)
	}
	if got := mirror.rows["7"]; len(got) != 1 || got[0] != "acme/api" {
		t.Errorf("bound installation's mirror = %v; want [acme/api]", got)
	}
	if got := mirror.classes["7"]; got != domain.GitHubCredentialClassManagedApp {
		t.Errorf("installation 7 keyed under %q; want %q", got, domain.GitHubCredentialClassManagedApp)
	}
	if len(mirror.rows) != 1 {
		t.Errorf("mirror holds %d scopes; want 1 — a refresh may update what the org bound and create nothing else", len(mirror.rows))
	}
}

func TestRunOrg_ManagedWorkspaceWithNothingBoundWritesNothing(t *testing.T) {
	// An installed-but-never-bound workspace is an ordinary state, not an
	// anomaly. Its refresh has nothing to refresh, and inventing an installation
	// for it is exactly the discovery the shared key cannot do safely.
	apps := &fakeApps{}
	mirror := newFakeMirror()
	// A grant source that fails the test if it is consulted at all.
	grants := fakeGrants{resolveErr: map[string]error{"acme": errors.New("nothing is bound")}}

	reconciler := newReconcilerAs(domain.GitHubCredentialClassManagedApp, apps, mirror, grants)
	if err := reconciler.RunOrg(context.Background(), testOrg); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}
	if mirror.replaces != 0 || len(mirror.cleared) != 0 {
		t.Error("mirror was touched for a managed workspace with no bound installation; want untouched")
	}
}

func TestRunOrg_UnresolvableClassStopsThePass(t *testing.T) {
	// The class is the key the entries are written under. Without it the pass
	// could only guess, and a guess writes a whole org's reach where nothing
	// reads it — so it stops before the first upstream call.
	apps := &fakeApps{app: activeApp(), installs: []domain.OrgGitHubAppInstallation{live("1", "acme")}}
	mirror := newFakeMirror()
	reconciler := &Reconciler{
		apps: apps, mirror: mirror, clients: fakeGrants{},
		classes: fakeClasses{err: errors.New("settings unreadable")},
	}

	if err := reconciler.RunOrg(context.Background(), testOrg); err == nil {
		t.Fatal("RunOrg returned nil with an unresolvable credential class; want the failure surfaced")
	}
	if apps.backfillCalls != 0 || mirror.replaces != 0 {
		t.Error("the pass proceeded past an unresolvable class; want nothing asked and nothing written")
	}
}
