package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The dependabotUpdate values GitHub answered on the probe repositories: an
// update that failed (a transitive dependency its parent pins below every
// fixed version), a pull request, and an attempt under way.
const (
	updateFailed  = `{"pullRequest":null,"error":{"errorType":"dependency_file_not_resolvable","title":"Dependabot can't resolve your Python dependency files"}}`
	updatePR      = `{"pullRequest":{"number":7,"url":"https://github.com/octo/api/pull/7"},"error":null}`
	updatePending = `{"pullRequest":null,"error":null}`
)

// clockRig is a rig whose reads of Dependabot updates are scheduled on a clock
// the test moves, starting at start.
func clockRig(t *testing.T, start time.Time, repos ...string) (*vulnRig, *time.Time) {
	t.Helper()
	r := newVulnRig(t, repos...)
	now := start
	r.now = func() time.Time { return now }
	return r, &now
}

var rigStart = time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC)

// updatesReadAt is when repo's Dependabot updates were last read, as stored.
func (r *vulnRig) updatesReadAt(repo string) time.Time {
	r.t.Helper()
	for _, row := range r.rows() {
		if row.Repo == repo {
			a, err := r.stores.DependabotAccess.GetSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID)
			if err != nil || a == nil {
				r.t.Fatalf("read access of %s: %+v %v", repo, a, err)
			}
			return a.UpdatesReadAt
		}
	}
	r.t.Fatalf("no tracked repository %s", repo)
	return time.Time{}
}

// snapshotOf parses e's stored snapshot.
func (r *vulnRig) snapshotOf(e *domain.Entity) domain.VulnerablePackageSnapshot {
	r.t.Helper()
	var snap domain.VulnerablePackageSnapshot
	if e == nil || json.Unmarshal([]byte(e.SnapshotJSON), &snap) != nil {
		r.t.Fatalf("snapshot of %+v does not parse", e)
	}
	return snap
}

// cannotFix decodes every dependabot_cannot_fix among evts.
func cannotFix(t *testing.T, evts []domain.Event) []events.GitHubVulnerabilityDependabotCannotFixMetadata {
	t.Helper()
	var out []events.GitHubVulnerabilityDependabotCannotFixMetadata
	for _, evt := range evts {
		if evt.EventType != domain.EventGitHubVulnerabilityDependabotCannotFix {
			continue
		}
		var m events.GitHubVulnerabilityDependabotCannotFixMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
			t.Fatalf("cannot_fix metadata %q: %v", evt.MetadataJSON, err)
		}
		out = append(out, m)
	}
	return out
}

// TestRefreshVulnerablePackages_AFailedUpdateIsAnnouncedOnce: an alert whose
// Dependabot update reported an error, already so when TF first lists it,
// emits alert_opened and then dependabot_cannot_fix naming the error, the
// snapshot records the update, and the repository the read. Nothing later announces it again:
// the next poll, answered 304, does not read the updates of a package whose
// alerts Dependabot has settled, and a read a day later finds the same error.
func TestRefreshVulnerablePackages_AFailedUpdateIsAnnouncedOnce(t *testing.T) {
	r, now := clockRig(t, rigStart, "api")
	r.gh.set("api", fakeAlert{number: 1, ecosystem: "pip", pkg: "urllib3", severity: "high", updatedAt: rigStart.Add(-time.Hour)})
	r.gh.setUpdate("api", 1, updateFailed)

	evts := r.mustCycle()
	if got := eventTypes(evts); fmt.Sprint(got) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityDependabotCannotFix}) {
		t.Fatalf("the first listing emitted %v, want alert_opened then dependabot_cannot_fix", got)
	}
	e := r.pkg("api", "pip", "urllib3")
	got := cannotFix(t, evts)[0]
	want := events.GitHubVulnerabilityDependabotCannotFixMetadata{
		GitHubVulnerabilityAlert: events.GitHubVulnerabilityAlert{
			Repo: "octo/api", RepositoryID: e.RepositoryID, Ecosystem: "pip", Package: "urllib3",
			AlertNumber: 1, GHSAID: "GHSA-1", Summary: "advisory 1", Severity: "high", ManifestPath: "package.json", Scope: "runtime",
			VulnerableVersionRange: "< 9.9.9", FirstPatchedVersion: "9.9.9", HighestSeverity: "high",
		},
		Reason: domain.DependabotCannotFixUpdateFailed, ErrorType: "dependency_file_not_resolvable", ErrorTitle: "Dependabot can't resolve your Python dependency files",
	}
	if got != want {
		t.Errorf("cannot_fix metadata = %+v, want %+v", got, want)
	}
	if *evts[1].EntityID != e.ID {
		t.Errorf("cannot_fix is on entity %s, want the package's %s", *evts[1].EntityID, e.ID)
	}
	snap := r.snapshotOf(e)
	if u := snap.Alerts[0].DependabotUpdate; u == nil || u.State != domain.DependabotUpdateError || u.ErrorType != "dependency_file_not_resolvable" {
		t.Errorf("stored update = %+v, want the error", u)
	}
	if got := r.updatesReadAt("api"); !got.Equal(rigStart) {
		t.Errorf("updates read at %v, want %v", got, rigStart)
	}

	*now = now.Add(5 * time.Minute)
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("an unchanged poll emitted %v, want nothing", eventTypes(evts))
	}
	if _, notModified := r.gh.counts("api"); notModified != 1 {
		t.Errorf("304s = %d, want the second listing's", notModified)
	}
	if n := r.gh.updateReadsOf("api"); n != 1 {
		t.Errorf("update reads = %d, want only the first listing's", n)
	}

	*now = now.Add(maxDependabotUpdateRecheck)
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("the daily read emitted %v, want nothing for an error already announced", eventTypes(evts))
	}
	if n := r.gh.updateReadsOf("api"); n != 2 {
		t.Errorf("update reads = %d, want a second once a day has passed", n)
	}
}

// TestRefreshVulnerablePackages_DependabotsPullRequestRidesAlertOpened: an
// alert Dependabot already opened a pull request for when TF lists it is
// announced with that pull request, and Dependabot can fix it, so nothing
// says otherwise.
func TestRefreshVulnerablePackages_DependabotsPullRequestRidesAlertOpened(t *testing.T) {
	r, _ := clockRig(t, rigStart, "api")
	r.gh.set("api", lodash(1, "high"), lodash(2, "low"))
	r.gh.setUpdate("api", 1, updatePR)
	r.gh.setUpdate("api", 2, updatePending)

	evts := r.mustCycle()
	if got := eventTypes(evts); fmt.Sprint(got) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityAlertOpened}) {
		t.Fatalf("emitted %v, want two alert_opened", got)
	}
	prs := map[int]string{}
	for _, evt := range evts {
		var m events.GitHubVulnerabilityAlertOpenedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil {
			t.Fatalf("metadata: %v", err)
		}
		prs[m.AlertNumber] = fmt.Sprintf("%d %s", m.DependabotPullRequestNumber, m.DependabotPullRequestURL)
	}
	if want := map[int]string{1: "7 https://github.com/octo/api/pull/7", 2: "0 "}; fmt.Sprint(prs) != fmt.Sprint(want) {
		t.Errorf("Dependabot pull requests by alert = %v, want %v", prs, want)
	}
	snap := r.snapshotOf(r.pkg("api", "npm", "lodash"))
	if u := snap.Alerts[1].DependabotUpdate; u == nil || u.State != domain.DependabotUpdatePending {
		t.Errorf("alert 2's update = %+v, want the attempt under way", u)
	}
}

// TestRefreshVulnerablePackages_AFailureAfterTheAlertOpenedIsReadOnA304:
// Dependabot tries an alert after TF has announced it, and neither the REST
// listing nor any webhook says so. The poll reads the updates of a package
// whose alert Dependabot may still act on at a quarter of the alert's age, on
// polls the listing answers 304, and announces the failure on the read that
// finds it. Once the alert is settled, the polls stop reading.
func TestRefreshVulnerablePackages_AFailureAfterTheAlertOpenedIsReadOnA304(t *testing.T) {
	r, now := clockRig(t, rigStart, "api")
	r.gh.set("api", fakeAlert{number: 1, ecosystem: "pip", pkg: "urllib3", severity: "high", updatedAt: rigStart.Add(-time.Hour)})

	if got := eventTypes(r.mustCycle()); fmt.Sprint(got) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened}) {
		t.Fatalf("the first listing emitted %v, want alert_opened alone", got)
	}
	if u := r.snapshotOf(r.pkg("api", "pip", "urllib3")).Alerts[0].DependabotUpdate; u == nil || u.State != domain.DependabotUpdateNone {
		t.Fatalf("stored update = %+v, want none tried", u)
	}

	// The alert is 65 minutes old, so its updates are read 16 minutes after
	// the last read, and 5 have passed.
	r.gh.setUpdate("api", 1, updateFailed)
	*now = rigStart.Add(5 * time.Minute)
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("a poll before the read is due emitted %v", eventTypes(evts))
	}
	if n := r.gh.updateReadsOf("api"); n != 1 {
		t.Errorf("update reads = %d, want none before it is due", n)
	}

	// 80 minutes old: due 20 minutes after the last read.
	*now = rigStart.Add(20 * time.Minute)
	evts := r.mustCycle()
	if got := cannotFix(t, evts); len(evts) != 1 || len(got) != 1 || got[0].Reason != domain.DependabotCannotFixUpdateFailed {
		t.Fatalf("the due read emitted %v, want one dependabot_cannot_fix", eventTypes(evts))
	}
	if _, notModified := r.gh.counts("api"); notModified != 2 {
		t.Errorf("304s = %d, want both later listings unchanged", notModified)
	}

	*now = rigStart.Add(time.Hour)
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("a poll after the alert settled emitted %v", eventTypes(evts))
	}
	if n := r.gh.updateReadsOf("api"); n != 2 {
		t.Errorf("update reads = %d, want none once the alert is settled", n)
	}
}

// TestRefreshVulnerablePackages_AnAlertWithNoPatchedVersionCannotBeFixed: an
// alert no released version fixes is one Dependabot cannot fix whatever it
// reports, so it is announced with the alert even when the updates cannot be
// read.
func TestRefreshVulnerablePackages_AnAlertWithNoPatchedVersionCannotBeFixed(t *testing.T) {
	r, _ := clockRig(t, rigStart, "api")
	r.gh.set("api", fakeAlert{number: 1, ecosystem: "npm", pkg: "request", severity: "medium", unpatched: true})
	r.gh.mu.Lock()
	r.gh.updateStatus["api"] = http.StatusUnauthorized
	r.gh.mu.Unlock()

	evts := r.mustCycle()
	if got := eventTypes(evts); fmt.Sprint(got) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityDependabotCannotFix}) {
		t.Fatalf("emitted %v, want alert_opened then dependabot_cannot_fix", got)
	}
	m := cannotFix(t, evts)[0]
	if m.Reason != domain.DependabotCannotFixNoPatchedVersion || m.FirstPatchedVersion != "" || m.ErrorType != "" || m.ErrorTitle != "" {
		t.Errorf("cannot_fix = %+v, want no_patched_version with no error", m)
	}
}

// TestRefreshVulnerablePackages_AnErrorThatIsNotAFailureIsNotAnnounced: an
// error saying a pull request already proposes the fix, or that there is
// nothing to fix, is not Dependabot failing to fix the alert.
func TestRefreshVulnerablePackages_AnErrorThatIsNotAFailureIsNotAnnounced(t *testing.T) {
	r, _ := clockRig(t, rigStart, "api")
	r.gh.set("api", lodash(1, "high"), lodash(2, "high"), lodash(3, "high"))
	for n, code := range map[int]string{1: "pull_request_exists_for_latest_version", 2: "pull_request_exists_for_security_update", 3: "security_update_not_needed"} {
		r.gh.setUpdate("api", n, fmt.Sprintf(`{"pullRequest":null,"error":{"errorType":%q,"title":"t"}}`, code))
	}

	evts := r.mustCycle()
	if got := cannotFix(t, evts); len(got) != 0 || len(evts) != 3 {
		t.Errorf("emitted %v, want only the three alert_opened", eventTypes(evts))
	}
}

// TestRefreshVulnerablePackages_AFailedUpdateReadLeavesTheListing: a read of
// the updates that gets no answer does not hold up the listing, whose
// alert_opened and ETag land, and leaves the package with no read recorded,
// so the next poll reads again even though its listing is answered 304. A
// failure that would meet every repository (here a refused credential) stops
// the pass's later reads; the later listings go on.
func TestRefreshVulnerablePackages_AFailedUpdateReadLeavesTheListing(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	r, _ := clockRig(t, rigStart, "a", "b")
	r.gh.set("a", lodash(1, "high"))
	r.gh.set("b", lodash(1, "high"))
	r.gh.setUpdate("a", 1, updateFailed)
	r.gh.setUpdate("b", 1, updateFailed)
	r.gh.mu.Lock()
	r.gh.updateStatus["a"] = http.StatusUnauthorized
	r.gh.mu.Unlock()

	evts := r.mustCycle()
	if got := eventTypes(evts); fmt.Sprint(got) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityAlertOpened}) {
		t.Fatalf("emitted %v, want each repository's alert_opened alone", got)
	}
	if a, b := r.gh.updateReadsOf("a"), r.gh.updateReadsOf("b"); a != 1 || b != 0 {
		t.Errorf("update reads a=%d b=%d, want a's failed read and none after it", a, b)
	}
	if r.etag("a") == "" || r.etag("b") == "" {
		t.Errorf("ETags a=%q b=%q, want both listings stored", r.etag("a"), r.etag("b"))
	}
	if snap := r.snapshotOf(r.pkg("a", "npm", "lodash")); snap.Alerts[0].DependabotUpdate != nil || !r.updatesReadAt("a").IsZero() {
		t.Errorf("a's snapshot = %+v, read at %v; want no read recorded", snap, r.updatesReadAt("a"))
	}

	r.gh.mu.Lock()
	delete(r.gh.updateStatus, "a")
	r.gh.mu.Unlock()
	evts = r.mustCycle()
	if got := cannotFix(t, evts); len(got) != 2 {
		t.Errorf("the next poll emitted %v, want both failures announced", eventTypes(evts))
	}
	if _, notModified := r.gh.counts("a"); notModified != 1 {
		t.Errorf("a's 304s = %d, want the second listing unchanged", notModified)
	}
}

// TestRefreshVulnerablePackages_AnAlertTheAnswerLeavesOutKeepsItsUpdate: an
// answer that does not name an alert the listing holds says nothing about it,
// as GitHub can answer a credential that may not read alerts, and so the
// alert keeps the update TF last read, and the read is counted incomplete.
func TestRefreshVulnerablePackages_AnAlertTheAnswerLeavesOutKeepsItsUpdate(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := stats.Load()
	stats.Store(newInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { stats.Store(prev) })

	r, _ := clockRig(t, rigStart, "api")
	r.gh.set("api", lodash(1, "high"))
	r.gh.setUpdate("api", 1, updateFailed)
	if got := cannotFix(t, r.mustCycle()); len(got) != 1 {
		t.Fatalf("the first listing announced %d failures, want 1", len(got))
	}

	r.gh.set("api", lodash(1, "high"), lodash(2, "high"))
	r.gh.mu.Lock()
	r.gh.omitted["api"] = map[int]bool{1: true}
	r.gh.mu.Unlock()
	evts := r.mustCycle()
	if got := eventTypes(evts); fmt.Sprint(got) != fmt.Sprint([]string{domain.EventGitHubVulnerabilityAlertOpened}) {
		t.Errorf("emitted %v, want alert 2's alert_opened alone", got)
	}
	snap := r.snapshotOf(r.pkg("api", "npm", "lodash"))
	if u := snap.Alerts[0].DependabotUpdate; u == nil || u.State != domain.DependabotUpdateError {
		t.Errorf("alert 1's update = %+v, want the error it had", u)
	}
	if u := snap.Alerts[1].DependabotUpdate; u == nil || u.State != domain.DependabotUpdateNone {
		t.Errorf("alert 2's update = %+v, want none tried", u)
	}
	if got := updateReadsByOutcome(t, reader); got[dependabotUpdateReadRead] != 1 || got[dependabotUpdateReadIncomplete] != 1 {
		t.Errorf("update reads by outcome = %v, want one read and one incomplete", got)
	}
}

// updateReadsByOutcome reads the update-read counter's totals by outcome.
func updateReadsByOutcome(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != dependabotUpdatesMetric {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				got[outcome.AsString()] += dp.Value
			}
		}
	}
	return got
}

// TestDependabotUpdateRecheck pins the read schedule: a quarter of the age of
// the youngest alert Dependabot may still act on, capped at a day, which is
// also the interval for a package with none.
func TestDependabotUpdateRecheck(t *testing.T) {
	now := rigStart
	alert := func(age time.Duration, update *domain.DependabotUpdate, patched string) domain.DependabotAlert {
		return domain.DependabotAlert{Number: int(age.Minutes()) + 1, FirstPatchedVersion: patched, UpdatedAt: now.Add(-age), DependabotUpdate: update}
	}
	none := &domain.DependabotUpdate{State: domain.DependabotUpdateNone}
	failed := &domain.DependabotUpdate{State: domain.DependabotUpdateError, ErrorType: "security_update_not_possible"}
	pr := &domain.DependabotUpdate{State: domain.DependabotUpdatePullRequest, PullRequestNumber: 1}
	for _, tc := range []struct {
		name   string
		alerts []domain.DependabotAlert
		want   time.Duration
	}{
		{"unread, an hour old", []domain.DependabotAlert{alert(time.Hour, nil, "1.0")}, 15 * time.Minute},
		{"untried, the youngest decides", []domain.DependabotAlert{alert(8*time.Hour, none, "1.0"), alert(2*time.Hour, none, "1.0")}, 30 * time.Minute},
		{"a settled young alert does not count", []domain.DependabotAlert{alert(8*time.Hour, none, "1.0"), alert(time.Minute, pr, "1.0")}, 2 * time.Hour},
		{"untried for weeks", []domain.DependabotAlert{alert(21*24*time.Hour, none, "1.0")}, maxDependabotUpdateRecheck},
		{"every alert settled", []domain.DependabotAlert{alert(time.Minute, failed, "1.0"), alert(time.Minute, pr, "1.0")}, maxDependabotUpdateRecheck},
		{"no patched version", []domain.DependabotAlert{alert(time.Minute, none, "")}, maxDependabotUpdateRecheck},
		{"no time from GitHub", []domain.DependabotAlert{{Number: 1, FirstPatchedVersion: "1.0"}}, maxDependabotUpdateRecheck},
		{"changed after the clock", []domain.DependabotAlert{alert(-time.Minute, none, "1.0")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dependabotUpdateRecheck(domain.VulnerablePackageSnapshot{Alerts: tc.alerts}, now); got != tc.want {
				t.Errorf("recheck = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDiffVulnerablePackage_DependabotCannotFix covers the new event in the
// diff alone: it fires on the move to an alert Dependabot cannot fix and on a
// change of reason, and not again while the alert stays one for the same
// reason, nor for an alert that closes.
func TestDiffVulnerablePackage_DependabotCannotFix(t *testing.T) {
	failed := &domain.DependabotUpdate{State: domain.DependabotUpdateError, ErrorType: "security_update_not_possible"}
	pr := &domain.DependabotUpdate{State: domain.DependabotUpdatePullRequest, PullRequestNumber: 3}
	snap := func(alerts ...domain.DependabotAlert) domain.VulnerablePackageSnapshot {
		return domain.VulnerablePackageSnapshot{Repo: "octo/api", Ecosystem: "npm", Package: "lodash", Alerts: append([]domain.DependabotAlert{}, alerts...)}
	}
	a := func(number int, patched string, u *domain.DependabotUpdate) domain.DependabotAlert {
		return domain.DependabotAlert{Number: number, Severity: "low", FirstPatchedVersion: patched, DependabotUpdate: u}
	}
	opened, cannot := domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityDependabotCannotFix
	for _, tc := range []struct {
		name       string
		prev, curr domain.VulnerablePackageSnapshot
		want       []string
	}{
		{"untried to failed", snap(a(1, "1.0", nil)), snap(a(1, "1.0", failed)), []string{cannot}},
		{"still failed", snap(a(1, "1.0", failed)), snap(a(1, "1.0", failed)), nil},
		{"failed to a pull request", snap(a(1, "1.0", failed)), snap(a(1, "1.0", pr)), nil},
		{"a pull request to failed", snap(a(1, "1.0", pr)), snap(a(1, "1.0", failed)), []string{cannot}},
		{"opened failed", snap(), snap(a(1, "1.0", failed)), []string{opened, cannot}},
		{"opened unpatched, then failed", snap(a(1, "", nil)), snap(a(1, "", failed)), nil},
		{"unpatched, then a patch and a failed update", snap(a(1, "", nil)), snap(a(1, "1.0", failed)), []string{cannot}},
		{"failed, then the patch withdrawn", snap(a(1, "1.0", failed)), snap(a(1, "", failed)), []string{cannot}},
		{"a patch released", snap(a(1, "", nil)), snap(a(1, "1.0", nil)), nil},
		{"the failed one closes", snap(a(1, "1.0", failed), a(2, "1.0", nil)), snap(a(2, "1.0", nil)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eventTypes(DiffVulnerablePackage(tc.prev, tc.curr, "e1", "r1"))
			if fmt.Sprint(got) != fmt.Sprint(append([]string{}, tc.want...)) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRefreshVulnerablePackages_AReadOnA304WritesOnlyWhatItChanged: a read a
// poll answered 304 makes is recorded on the repository, and writes only the
// packages whose alerts it changed: nothing when Dependabot did nothing, and
// the one package whose update failed when one did.
func TestRefreshVulnerablePackages_AReadOnA304WritesOnlyWhatItChanged(t *testing.T) {
	r, now := clockRig(t, rigStart, "api")
	r.gh.set("api",
		fakeAlert{number: 1, ecosystem: "npm", pkg: "lodash", severity: "high", updatedAt: rigStart},
		fakeAlert{number: 2, ecosystem: "npm", pkg: "react", severity: "low", updatedAt: rigStart})
	r.gh.setUpdate("api", 2, updatePR)
	r.mustCycle()
	seq := func(name string) int64 { return r.pkg("api", "npm", name).PollSeq }
	lodashSeq, reactSeq := seq("lodash"), seq("react")

	// lodash's alert is untried and 10 minutes old, so a read is due and
	// changes nothing.
	*now = rigStart.Add(10 * time.Minute)
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Fatalf("a read that changed nothing emitted %v", eventTypes(evts))
	}
	if n := r.gh.updateReadsOf("api"); n != 2 {
		t.Fatalf("update reads = %d, want the due one", n)
	}
	if seq("lodash") != lodashSeq || seq("react") != reactSeq {
		t.Errorf("a read that changed nothing wrote the packages: lodash %d→%d, react %d→%d", lodashSeq, seq("lodash"), reactSeq, seq("react"))
	}
	if got := r.updatesReadAt("api"); !got.Equal(*now) {
		t.Errorf("updates read at %v, want %v", got, *now)
	}

	r.gh.setUpdate("api", 1, updateFailed)
	*now = rigStart.Add(15 * time.Minute)
	if got := cannotFix(t, r.mustCycle()); len(got) != 1 || got[0].Package != "lodash" {
		t.Fatalf("the read that found the failure announced %+v, want lodash's", got)
	}
	if seq("lodash") == lodashSeq || seq("react") != reactSeq {
		t.Errorf("after the failure: lodash %d→%d, react %d→%d; want lodash alone written", lodashSeq, seq("lodash"), reactSeq, seq("react"))
	}
}

// TestRefreshVulnerablePackages_ThePassReadsAtMostItsShare: a pass reads the
// updates of at most dependabotUpdateReadsPerPass repositories, and a
// repository it left is due on the next poll, where it is read.
func TestRefreshVulnerablePackages_ThePassReadsAtMostItsShare(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	reader := sdkmetric.NewManualReader()
	prev := stats.Load()
	stats.Store(newInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { stats.Store(prev) })

	names := make([]string, dependabotUpdateReadsPerPass+2)
	for i := range names {
		names[i] = fmt.Sprintf("r%02d", i)
	}
	r, _ := clockRig(t, rigStart, names...)
	for _, name := range names {
		r.gh.set(name, lodash(1, "high"))
		r.gh.setUpdate(name, 1, updateFailed)
	}

	if got := cannotFix(t, r.mustCycle()); len(got) != dependabotUpdateReadsPerPass {
		t.Errorf("the first pass announced %d failures, want one per repository it read, %d", len(got), dependabotUpdateReadsPerPass)
	}
	unread := 0
	for _, name := range names {
		if r.gh.updateReadsOf(name) == 0 {
			unread++
		}
	}
	if unread != 2 {
		t.Errorf("%d repositories unread after the first pass, want 2", unread)
	}
	if got := updateReadsByOutcome(t, reader); got[dependabotUpdateReadDeferred] != 2 {
		t.Errorf("update reads by outcome = %v, want 2 deferred", got)
	}

	if got := cannotFix(t, r.mustCycle()); len(got) != 2 {
		t.Errorf("the next pass announced %d failures, want the 2 it left", len(got))
	}
}

// TestRefreshVulnerablePackages_ASpentBudgetIsNotWaitedFor: once the GraphQL
// budget is spent, a read refuses at once rather than holding the listing
// until the budget resets, and stops the pass's later reads; the listings
// land.
func TestRefreshVulnerablePackages_ASpentBudgetIsNotWaitedFor(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	r, _ := clockRig(t, rigStart, "a", "b", "c")
	for _, name := range []string{"a", "b", "c"} {
		r.gh.set(name, lodash(1, "high"))
	}
	// a's answer says the budget is spent until four minutes from now, within
	// the client's longest wait.
	r.gh.mu.Lock()
	r.gh.spentAfter = "a"
	r.gh.mu.Unlock()

	start := time.Now()
	evts := r.mustCycle()
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the pass took %v, want it not to wait for the budget", elapsed)
	}
	if got := eventTypes(evts); len(got) != 3 {
		t.Errorf("emitted %v, want each repository's alert_opened", got)
	}
	if a, b, c := r.gh.updateReadsOf("a"), r.gh.updateReadsOf("b"), r.gh.updateReadsOf("c"); a != 1 || b != 0 || c != 0 {
		t.Errorf("update reads a=%d b=%d c=%d, want a's alone", a, b, c)
	}
}

// TestRefreshVulnerablePackages_APatchReleasedIsReadAgain: an alert with no
// patched version is announced for that reason. When a patch is released,
// the update Dependabot reported before it is set aside and read again, and a
// failed update to the patch is announced for its own reason.
func TestRefreshVulnerablePackages_APatchReleasedIsReadAgain(t *testing.T) {
	r, now := clockRig(t, rigStart, "api")
	r.gh.set("api", fakeAlert{number: 1, ecosystem: "npm", pkg: "request", severity: "medium", unpatched: true, updatedAt: rigStart.Add(-30 * 24 * time.Hour)})
	r.gh.setUpdate("api", 1, `{"pullRequest":null,"error":{"errorType":"security_update_not_found","title":"Dependabot can't find a non-vulnerable version"}}`)
	if got := cannotFix(t, r.mustCycle()); len(got) != 1 || got[0].Reason != domain.DependabotCannotFixNoPatchedVersion {
		t.Fatalf("the first listing announced %+v, want no_patched_version", got)
	}

	// The patch lands an hour later, well inside the day the old alert's
	// schedule would wait.
	r.gh.set("api", fakeAlert{number: 1, ecosystem: "npm", pkg: "request", severity: "medium", updatedAt: rigStart.Add(-30 * 24 * time.Hour)})
	r.gh.setUpdate("api", 1, updateFailed)
	*now = rigStart.Add(time.Hour)
	got := cannotFix(t, r.mustCycle())
	if len(got) != 1 || got[0].Reason != domain.DependabotCannotFixUpdateFailed || got[0].ErrorType != "dependency_file_not_resolvable" {
		t.Errorf("the listing with the patch announced %+v, want update_failed with the new error", got)
	}
	if n := r.gh.updateReadsOf("api"); n != 2 {
		t.Errorf("update reads = %d, want a second for the patch", n)
	}
}

// TestRefreshVulnerablePackages_AnUnusableAnswerKeepsTheSchedule: GitHub
// answering a read with no repository is taken as an answer naming no alert:
// the alert is recorded as untried and the read time stored, so the next
// read waits for the alert's schedule rather than coming on every poll.
func TestRefreshVulnerablePackages_AnUnusableAnswerKeepsTheSchedule(t *testing.T) {
	r, now := clockRig(t, rigStart, "api")
	r.gh.set("api", fakeAlert{number: 1, ecosystem: "npm", pkg: "lodash", severity: "high", updatedAt: rigStart.Add(-time.Hour)})
	r.gh.mu.Lock()
	r.gh.noRepository["api"] = true
	r.gh.mu.Unlock()

	r.mustCycle()
	if u := r.snapshotOf(r.pkg("api", "npm", "lodash")).Alerts[0].DependabotUpdate; u == nil || u.State != domain.DependabotUpdateNone {
		t.Errorf("stored update = %+v, want none", u)
	}
	if got := r.updatesReadAt("api"); !got.Equal(rigStart) {
		t.Errorf("updates read at %v, want %v", got, rigStart)
	}
	*now = rigStart.Add(5 * time.Minute)
	r.mustCycle()
	if n := r.gh.updateReadsOf("api"); n != 1 {
		t.Errorf("update reads = %d, want none before the alert's schedule", n)
	}
}
