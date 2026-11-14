package tracker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const vulnHost = "https://github.com"

// fakeAlert is one open alert the fake serves.
type fakeAlert struct {
	number    int
	ecosystem string
	pkg       string
	severity  string
}

// alertsFake is a GitHub serving each repository's open Dependabot alerts,
// with an ETag over the body that it honors, and a status a test can set per
// repository instead.
type alertsFake struct {
	mu          sync.Mutex
	alerts      map[string][]fakeAlert // by repository name, under octo/
	status      map[string]int
	requests    map[string]int
	notModified map[string]int
	malformed   map[string]bool
	// limited is the repositories whose listing GitHub refuses for a
	// secondary rate limit, with a Retry-After the client does not wait out.
	// The answer carries no budget reading, so the client sends the next
	// request rather than refusing it itself.
	limited map[string]bool
	// hold, when set, is called with the repository before its request is
	// served, outside mu, so a test can keep listings in flight.
	hold func(repo string)
}

func newAlertsFake(t *testing.T) (*alertsFake, *ghclient.Client) {
	t.Helper()
	f := &alertsFake{alerts: map[string][]fakeAlert{}, status: map[string]int{}, requests: map[string]int{}, notModified: map[string]int{}, malformed: map[string]bool{}, limited: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, ghclient.NewClient(srv.URL, "tok")
}

func (f *alertsFake) serve(w http.ResponseWriter, r *http.Request) {
	_, rest, _ := strings.Cut(r.URL.Path, "/repos/octo/")
	repo, suffix, _ := strings.Cut(rest, "/")
	if suffix != "dependabot/alerts" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		hold(repo)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[repo]++
	if f.limited[repo] {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
		return
	}
	if f.malformed[repo] {
		_, _ = w.Write([]byte(`{"message":"not a list"}`))
		return
	}
	if status := f.status[repo]; status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		return
	}
	var objs []string
	for _, a := range f.alerts[repo] {
		objs = append(objs, fmt.Sprintf(`{"number":%d,"state":"open","updated_at":"2026-11-0%dT00:00:00Z",
			"dependency":{"package":{"ecosystem":%q,"name":%q},"manifest_path":"package.json","scope":"runtime"},
			"security_advisory":{"ghsa_id":"GHSA-%d","cve_id":null,"summary":"advisory %d","severity":%q},
			"security_vulnerability":{"package":{"ecosystem":%q,"name":%q},"severity":%q,"vulnerable_version_range":"< 9.9.9","first_patched_version":{"identifier":"9.9.9"}}}`,
			a.number, 1+a.number%9, a.ecosystem, a.pkg, a.number, a.number, a.severity, a.ecosystem, a.pkg, a.severity))
	}
	body := "[" + strings.Join(objs, ",") + "]"
	sum := sha256.Sum256([]byte(body))
	etag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		f.notModified[repo]++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	_, _ = w.Write([]byte(body))
}

func (f *alertsFake) set(repo string, alerts ...fakeAlert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts[repo] = alerts
}

func (f *alertsFake) fail(repo string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[repo] = status
}

func (f *alertsFake) counts(repo string) (requests, notModified int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[repo], f.notModified[repo]
}

// vulnRig is a tracked set whose alerts the org's credential can read.
type vulnRig struct {
	t        *testing.T
	gh       *alertsFake
	client   *ghclient.Client
	database *sql.DB
	stores   db.Stores
	pub      *recordingPublisher
	repos    []string
	// access, when set, is the store the tracker records listings in
	// instead of the rig's own.
	access db.RepositoryDependabotAccessStore
}

func newVulnRig(t *testing.T, repos ...string) *vulnRig {
	t.Helper()
	gh, client := newAlertsFake(t)
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, "octo/"+r)
	}
	trackRepos(t, stores, runmode.LocalDefaultOrgID, names)
	r := &vulnRig{t: t, gh: gh, client: client, database: database, stores: stores, pub: &recordingPublisher{}, repos: repos}
	for _, row := range r.rows() {
		if _, err := stores.DependabotAccess.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID, domain.DependabotAlertsReadable); err != nil {
			t.Fatalf("record access for %s: %v", row.Slug(), err)
		}
	}
	return r
}

func (r *vulnRig) rows() []domain.Repository {
	r.t.Helper()
	rows, _, err := r.stores.Repos.ListTrackedSystem(context.Background(), runmode.LocalDefaultOrgID, vulnHost, db.Unwindowed)
	if err != nil {
		r.t.Fatalf("list tracked: %v", err)
	}
	return rows
}

func (r *vulnRig) tracker() *Tracker {
	s := r.stores
	access := s.DependabotAccess
	if r.access != nil {
		access = r.access
	}
	return New(r.database, r.pub, s.Tasks, s.Entities, s.Repos, s.JiraProjects, s.Deliverables, s.ExternalActions, s.EventQueue, s.PRMergeability, runmode.LocalDefaultOrgID).
		WithDependabotAccess(access)
}

// listings is every tracked repository with its listing state as stored.
func (r *vulnRig) listings() []AlertListing {
	r.t.Helper()
	var listings []AlertListing
	for _, row := range r.rows() {
		a, err := r.stores.DependabotAccess.GetSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID)
		if err != nil || a == nil {
			r.t.Fatalf("read access of %s: %+v %v", row.Slug(), a, err)
		}
		listings = append(listings, AlertListing{Repository: row, Access: *a})
	}
	return listings
}

// cycle lists every tracked repository's alerts with the listing state as
// stored, and returns the events the cycle enqueued and the pass's error.
func (r *vulnRig) cycle() ([]domain.Event, error) {
	r.t.Helper()
	listings := r.listings()
	before := r.pub.count()
	_, err := r.tracker().RefreshVulnerablePackages(context.Background(), vulnHost, r.client, listings)
	return r.pub.since(before), err
}

// etag is the ETag stored for repo's alert listing.
func (r *vulnRig) etag(repo string) string {
	r.t.Helper()
	for _, row := range r.rows() {
		if row.Repo != repo {
			continue
		}
		a, err := r.stores.DependabotAccess.GetSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID)
		if err != nil || a == nil {
			r.t.Fatalf("read access of %s: %+v %v", row.Slug(), a, err)
		}
		return a.AlertsETag
	}
	r.t.Fatalf("no tracked repository %s", repo)
	return ""
}

// mustCycle is cycle for a pass that must succeed.
func (r *vulnRig) mustCycle() []domain.Event {
	r.t.Helper()
	evts, err := r.cycle()
	if err != nil {
		r.t.Fatalf("RefreshVulnerablePackages: %v", err)
	}
	return evts
}

// pkg reads the entity under the package's key: the active row, else the most
// recently closed.
func (r *vulnRig) pkg(repo, ecosystem, name string) *domain.Entity {
	r.t.Helper()
	e, err := r.stores.Entities.GetBySourceSystem(context.Background(), runmode.LocalDefaultOrgID, "github", vulnHost, domain.VulnerablePackageSourceID("octo/"+repo, ecosystem, name))
	if err != nil {
		r.t.Fatalf("read package %s: %v", name, err)
	}
	return e
}

func (r *vulnRig) alertNumbers(e *domain.Entity) []int {
	r.t.Helper()
	var snap domain.VulnerablePackageSnapshot
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
		r.t.Fatalf("parse snapshot %q: %v", e.SnapshotJSON, err)
	}
	out := []int{}
	for _, a := range snap.Alerts {
		out = append(out, a.Number)
	}
	return out
}

// closeAsRouted closes e the way the router's terminating close does.
func (r *vulnRig) closeAsRouted(e *domain.Entity) {
	r.t.Helper()
	res, err := r.stores.Entities.CloseTerminalSystem(context.Background(), runmode.LocalDefaultOrgID, e.ID, nil,
		[]string{domain.EventGitHubVulnerabilityAlertOpened}, "entity_closed", domain.EventGitHubVulnerabilityResolved, "")
	if err != nil || !res.Closed {
		r.t.Fatalf("close %s: %+v %v", e.SourceID, res, err)
	}
}

func lodash(number int, severity string) fakeAlert {
	return fakeAlert{number: number, ecosystem: "npm", pkg: "lodash", severity: severity}
}

// TestRefreshVulnerablePackages_TheFirstListingAnnouncesEveryOpenAlert: a
// repository's first listing creates an entity per package, holding its open
// alerts, and emits an alert_opened for each alert, on its package's entity.
func TestRefreshVulnerablePackages_TheFirstListingAnnouncesEveryOpenAlert(t *testing.T) {
	r := newVulnRig(t, "api")
	r.gh.set("api", lodash(1, "high"), lodash(2, "low"), fakeAlert{number: 3, ecosystem: "pip", pkg: "django", severity: "medium"})

	evts := r.mustCycle()
	l := r.pkg("api", "npm", "lodash")
	if l == nil || l.State != "active" || l.Kind != domain.EntityKindVulnerablePackage || l.Title != "lodash (npm)" {
		t.Fatalf("lodash entity = %+v, want an active vulnerable package", l)
	}
	if got := r.alertNumbers(l); fmt.Sprint(got) != "[1 2]" {
		t.Errorf("lodash alerts = %v, want [1 2]", got)
	}
	wantURL := "https://github.com/octo/api/security/dependabot?q=is%3Aopen+ecosystem%3Anpm+package%3Alodash"
	if l.URL != wantURL {
		t.Errorf("url = %q, want %q", l.URL, wantURL)
	}
	d := r.pkg("api", "pip", "django")
	if d == nil || fmt.Sprint(r.alertNumbers(d)) != "[3]" {
		t.Fatalf("django entity = %+v, want one holding alert 3", d)
	}
	got := map[int]string{}
	for _, evt := range evts {
		var meta events.GitHubVulnerabilityAlertOpenedMetadata
		if evt.EventType != domain.EventGitHubVulnerabilityAlertOpened || json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
			t.Fatalf("emitted %v, want only alert_opened", eventTypes(evts))
		}
		got[meta.AlertNumber] = *evt.EntityID
	}
	if want := map[int]string{1: l.ID, 2: l.ID, 3: d.ID}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("alert_opened by alert = %v, want %v", got, want)
	}
	a, err := r.stores.DependabotAccess.GetSystem(context.Background(), runmode.LocalDefaultOrgID, l.RepositoryID)
	if err != nil || a == nil || a.AlertsETag == "" {
		t.Errorf("listing = %+v %v, want the ETag stored", a, err)
	}
}

// TestRefreshVulnerablePackages_AlertsOpenOnOneEntity: in a repository listed
// with no open alert, a new alert creates the package's entity and emits one
// alert_opened; a second
// alert on the package emits a second on the same entity; a cycle with
// nothing changed is a 304 and emits nothing.
func TestRefreshVulnerablePackages_AlertsOpenOnOneEntity(t *testing.T) {
	r := newVulnRig(t, "api")
	r.mustCycle()

	r.gh.set("api", lodash(5, "medium"))
	evts := r.mustCycle()
	if got := eventTypes(evts); fmt.Sprint(got) != "["+domain.EventGitHubVulnerabilityAlertOpened+"]" {
		t.Fatalf("first alert emitted %v, want one alert_opened", got)
	}
	e := r.pkg("api", "npm", "lodash")
	if e == nil || *evts[0].EntityID != e.ID {
		t.Fatalf("event entity = %v, want the package's entity %+v", *evts[0].EntityID, e)
	}
	var meta events.GitHubVulnerabilityAlertOpenedMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	want := events.GitHubVulnerabilityAlertOpenedMetadata{
		Repo: "octo/api", RepositoryID: e.RepositoryID, Ecosystem: "npm", Package: "lodash",
		AlertNumber: 5, GHSAID: "GHSA-5", Summary: "advisory 5", Severity: "medium", ManifestPath: "package.json",
		Scope: "runtime", VulnerableVersionRange: "< 9.9.9", FirstPatchedVersion: "9.9.9", HighestSeverity: "medium",
	}
	if meta != want {
		t.Errorf("metadata = %+v, want %+v", meta, want)
	}
	if evts[0].DedupKey != "" || evts[0].OccurredAt.IsZero() {
		t.Errorf("event = %+v, want an empty dedup key and the alert's time", evts[0])
	}

	r.gh.set("api", lodash(5, "medium"), lodash(6, "critical"))
	evts = r.mustCycle()
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityAlertOpened || *evts[0].EntityID != e.ID {
		t.Fatalf("second alert emitted %v, want one alert_opened on the same entity", eventTypes(evts))
	}
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil || meta.AlertNumber != 6 || meta.HighestSeverity != "critical" {
		t.Errorf("metadata = %+v %v, want alert 6 with the package's highest severity critical", meta, err)
	}

	_, before := r.gh.counts("api")
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("an unchanged cycle emitted %v", eventTypes(evts))
	}
	if _, after := r.gh.counts("api"); after != before+1 {
		t.Errorf("the unchanged cycle's listing was not answered 304 (%d -> %d)", before, after)
	}
}

// TestRefreshVulnerablePackages_TheLastAlertClosingResolves: an alert closing
// while another stays open emits nothing and updates the snapshot; the last
// one closing emits one resolved with the terminal snapshot, and once the
// entity has closed, a later alert on the package creates a new entity.
func TestRefreshVulnerablePackages_TheLastAlertClosingResolves(t *testing.T) {
	r := newVulnRig(t, "api")
	r.gh.set("api", lodash(1, "high"), lodash(2, "low"))
	r.mustCycle()
	first := r.pkg("api", "npm", "lodash")

	r.gh.set("api", lodash(2, "low"))
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("one alert closing emitted %v, want nothing", eventTypes(evts))
	}
	if got := r.alertNumbers(r.pkg("api", "npm", "lodash")); fmt.Sprint(got) != "[2]" {
		t.Errorf("alerts = %v, want [2]", got)
	}

	r.gh.set("api")
	evts := r.mustCycle()
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityResolved || *evts[0].EntityID != first.ID {
		t.Fatalf("the last alert closing emitted %v, want one resolved on the entity", eventTypes(evts))
	}
	var meta events.GitHubVulnerabilityResolvedMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil || fmt.Sprint(meta.AlertNumbers) != "[2]" || meta.Package != "lodash" {
		t.Errorf("metadata = %+v %v, want lodash with alert 2", meta, err)
	}
	resolved := r.pkg("api", "npm", "lodash")
	if got := r.alertNumbers(resolved); len(got) != 0 {
		t.Errorf("alerts after resolution = %v, want none", got)
	}

	// Until the router closes it, the entity stays as it is and a new alert
	// waits for it; nothing more is emitted, since the resolution is queued.
	r.gh.set("api", lodash(3, "critical"))
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("an alert before the close landed emitted %v", eventTypes(evts))
	}
	r.closeAsRouted(resolved)

	evts = r.mustCycle()
	next := r.pkg("api", "npm", "lodash")
	if next == nil || next.ID == first.ID || next.State != "active" {
		t.Fatalf("after the close the package is %+v, want a new active entity", next)
	}
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityAlertOpened || *evts[0].EntityID != next.ID {
		t.Errorf("the later alert emitted %v, want one alert_opened on the new entity", eventTypes(evts))
	}
}

// TestRefreshVulnerablePackages_ALostCloseIsOwed: a resolution whose close
// never landed is owed one on the next cycle, unchanged listing or not.
func TestRefreshVulnerablePackages_ALostCloseIsOwed(t *testing.T) {
	r := newVulnRig(t, "api")
	r.gh.set("api", lodash(1, "high"))
	r.mustCycle()
	r.gh.set("api")
	r.mustCycle()
	// The resolution was consumed without closing the entity.
	if _, err := r.database.Exec(`UPDATE event_queue SET status = 'done'`); err != nil {
		t.Fatalf("settle queue: %v", err)
	}

	evts := r.mustCycle()
	if len(evts) != 1 || evts[0].EventType != domain.EventSystemEntityCloseOwed {
		t.Errorf("the cycle after a lost close emitted %v, want the close obligation", eventTypes(evts))
	}
	if evts := r.mustCycle(); len(evts) != 0 {
		t.Errorf("an obligation already owed was owed again: %v", eventTypes(evts))
	}
}

// TestRefreshVulnerablePackages_AFailedListingChangesNothing: a repository
// GitHub refuses is skipped and the next one is listed; any other failure
// ends the pass. Neither resolves a package.
func TestRefreshVulnerablePackages_AFailedListingChangesNothing(t *testing.T) {
	// One at a time, so web starts only after api's refusal is handled.
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	r := newVulnRig(t, "api", "web")
	r.gh.set("api", lodash(1, "high"))
	r.gh.set("web", fakeAlert{number: 1, ecosystem: "npm", pkg: "react", severity: "low"})
	r.mustCycle()

	r.gh.fail("api", http.StatusForbidden)
	r.gh.set("web", fakeAlert{number: 1, ecosystem: "npm", pkg: "react", severity: "low"}, fakeAlert{number: 2, ecosystem: "npm", pkg: "react", severity: "low"})
	evts, err := r.cycle()
	if err != nil {
		t.Fatalf("a refused repository failed the pass: %v", err)
	}
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityAlertOpened {
		t.Errorf("emitted %v, want only web's new alert", eventTypes(evts))
	}
	if e := r.pkg("api", "npm", "lodash"); e.State != "active" || fmt.Sprint(r.alertNumbers(e)) != "[1]" {
		t.Errorf("the refused repository's package = %+v, want it untouched", e)
	}

	r.gh.fail("api", http.StatusUnauthorized)
	if _, err := r.cycle(); err == nil {
		t.Error("a refused credential did not end the pass")
	}
	if e := r.pkg("api", "npm", "lodash"); e.State != "active" || fmt.Sprint(r.alertNumbers(e)) != "[1]" {
		t.Errorf("after a failed listing the package = %+v, want it untouched", e)
	}
}

// TestRefreshVulnerablePackages_NoStoreListsNothing: a tracker without the
// access store makes no request.
func TestRefreshVulnerablePackages_NoStoreListsNothing(t *testing.T) {
	r := newVulnRig(t, "api")
	s := r.stores
	tr := New(r.database, r.pub, s.Tasks, s.Entities, s.Repos, s.JiraProjects, s.Deliverables, s.ExternalActions, s.EventQueue, s.PRMergeability, runmode.LocalDefaultOrgID)
	if _, err := tr.RefreshVulnerablePackages(context.Background(), vulnHost, r.client, []AlertListing{{Repository: r.rows()[0]}}); err != nil {
		t.Fatalf("RefreshVulnerablePackages: %v", err)
	}
	if n, _ := r.gh.counts("api"); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

// TestDiffVulnerablePackage covers the diff alone: each newly open alert
// emits, a closing one does not, and resolved fires only on the move to no
// open alert.
func TestDiffVulnerablePackage(t *testing.T) {
	snap := func(numbers ...int) domain.VulnerablePackageSnapshot {
		s := domain.VulnerablePackageSnapshot{Repo: "octo/api", Ecosystem: "npm", Package: "lodash", Alerts: []domain.DependabotAlert{}}
		for _, n := range numbers {
			s.Alerts = append(s.Alerts, domain.DependabotAlert{Number: n, Severity: "low"})
		}
		return s
	}
	for _, tc := range []struct {
		name       string
		prev, curr domain.VulnerablePackageSnapshot
		want       []string
	}{
		{"nothing changed", snap(1, 2), snap(1, 2), nil},
		{"two new alerts", snap(1), snap(1, 2, 3), []string{domain.EventGitHubVulnerabilityAlertOpened, domain.EventGitHubVulnerabilityAlertOpened}},
		{"one closes, one stays", snap(1, 2), snap(2), nil},
		{"one closes, another opens", snap(1), snap(2), []string{domain.EventGitHubVulnerabilityAlertOpened}},
		{"the last closes", snap(1, 2), snap(), []string{domain.EventGitHubVulnerabilityResolved}},
		{"still none", snap(), snap(), nil},
		{"from no snapshot", domain.VulnerablePackageSnapshot{}, snap(4), []string{domain.EventGitHubVulnerabilityAlertOpened}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eventTypes(DiffVulnerablePackage(tc.prev, tc.curr, "e1", "r1", nil))
			if fmt.Sprint(got) != fmt.Sprint(append([]string{}, tc.want...)) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRefreshVulnerablePackages_AHostRoundTripAnnouncesWhatItClosed: a
// package retired by a host change closed with its tasks while its alerts were
// still open. When the org moves back, the package gets a new entity that
// announces every alert open on it again.
func TestRefreshVulnerablePackages_AHostRoundTripAnnouncesWhatItClosed(t *testing.T) {
	r := newVulnRig(t, "api")
	ctx := context.Background()
	r.gh.set("api", lodash(1, "high"), lodash(2, "low"))
	if evts := r.mustCycle(); len(evts) != 2 {
		t.Fatalf("setup: the first listing emitted %v", eventTypes(evts))
	}
	first := r.pkg("api", "npm", "lodash")

	// The org moves to another host: the package is retired and the router
	// closes it, and the cycle there forgets the repository's ETag.
	r.closeAsRouted(first)
	if _, err := r.stores.DependabotAccess.ClearUntrackedAlertListingsSystem(ctx, runmode.LocalDefaultOrgID, "https://ghe.example.com"); err != nil {
		t.Fatalf("clear on the other host: %v", err)
	}

	// Back on the host with nothing changed: the listing is answered in full
	// and both alerts, whose tasks the retirement closed, are announced again.
	evts := r.mustCycle()
	next := r.pkg("api", "npm", "lodash")
	if next == nil || next.ID == first.ID || next.State != "active" {
		t.Fatalf("after the return the package is %+v, want a new active entity", next)
	}
	if len(evts) != 2 {
		t.Fatalf("the return emitted %v, want two alert_opened", eventTypes(evts))
	}
	for _, evt := range evts {
		if evt.EventType != domain.EventGitHubVulnerabilityAlertOpened || *evt.EntityID != next.ID {
			t.Errorf("the return emitted %s on %s, want alert_opened on the new entity", evt.EventType, *evt.EntityID)
		}
	}

	// An alert opened after the return is announced as usual.
	r.gh.set("api", lodash(1, "high"), lodash(2, "low"), lodash(3, "medium"))
	if evts := r.mustCycle(); len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityAlertOpened {
		t.Errorf("a later alert emitted %v, want one alert_opened", eventTypes(evts))
	}
}

// TestRefreshVulnerablePackages_AnUnusableListingSkipsOnlyItsRepository: a
// repository whose listing TF cannot read is skipped every cycle, its packages
// untouched, and the repositories after it are still listed.
func TestRefreshVulnerablePackages_AnUnusableListingSkipsOnlyItsRepository(t *testing.T) {
	// One at a time, so web starts only after api's unusable listing is
	// handled.
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")
	r := newVulnRig(t, "api", "web")
	r.gh.set("api", lodash(1, "high"))
	r.mustCycle()

	r.gh.mu.Lock()
	r.gh.malformed["api"] = true
	r.gh.mu.Unlock()
	r.gh.set("web", fakeAlert{number: 1, ecosystem: "npm", pkg: "react", severity: "low"})
	evts, err := r.cycle()
	if err != nil {
		t.Fatalf("an unusable listing failed the pass: %v", err)
	}
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityAlertOpened {
		t.Errorf("emitted %v, want only web's alert", eventTypes(evts))
	}
	if e := r.pkg("api", "npm", "lodash"); e.State != "active" || fmt.Sprint(r.alertNumbers(e)) != "[1]" {
		t.Errorf("the unusable repository's package = %+v, want it untouched", e)
	}
}

// TestRefreshVulnerablePackages_ListsRepositoriesConcurrently: a pass has as
// many listings in flight at once as the bound allows, and no more, and each
// repository's alerts land once its listing answers.
func TestRefreshVulnerablePackages_ListsRepositoriesConcurrently(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "4")
	names := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	r := newVulnRig(t, names...)
	for _, name := range names {
		r.gh.set(name, lodash(1, "high"))
	}

	// Every listing is held until four are in flight together, which a pass
	// listing one repository after another never reaches.
	var mu sync.Mutex
	inFlight, most := 0, 0
	arrived := make(chan struct{}, len(names))
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	r.gh.mu.Lock()
	r.gh.hold = func(string) {
		mu.Lock()
		inFlight++
		most = max(most, inFlight)
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	r.gh.mu.Unlock()

	listings := r.listings()
	tr := r.tracker()
	var passErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, passErr = tr.RefreshVulnerablePackages(context.Background(), vulnHost, r.client, listings)
	}()
	// Runs before the rig's cleanups, so a failed wait below does not close
	// the database under a pass still writing to it.
	t.Cleanup(func() {
		unblock()
		<-finished
	})
	for i := range 4 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d listings in flight together, want 4", i)
		}
	}
	unblock()
	<-finished
	if passErr != nil {
		t.Fatalf("RefreshVulnerablePackages: %v", passErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if most != 4 {
		t.Errorf("at most %d listings in flight, want 4", most)
	}
	for _, name := range names {
		if e := r.pkg(name, "npm", "lodash"); e == nil || fmt.Sprint(r.alertNumbers(e)) != "[1]" {
			t.Errorf("%s's package = %+v, want one holding alert 1", name, e)
		}
		if r.etag(name) == "" {
			t.Errorf("%s's listing stored no ETag", name)
		}
	}
	if evts := r.pub.since(0); len(evts) != len(names) {
		t.Errorf("emitted %v, want one alert_opened per repository", eventTypes(evts))
	}
}

// TestRefreshVulnerablePackages_ARateLimitStopsTheListingsNotStarted: once a
// listing meets a rate limit no listing starts, the listings already made keep
// their writes, and the pass returns the rate limit.
func TestRefreshVulnerablePackages_ARateLimitStopsTheListingsNotStarted(t *testing.T) {
	// Two at once: r0 and r1 start before r2 and r3, and r4 can start only
	// once three listings have finished, at least one of them r2 or r3, so
	// the rate limit is met before r4 or r5 could start whatever order the
	// listings finish in.
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "2")
	names := []string{"r0", "r1", "r2", "r3", "r4", "r5"}
	r := newVulnRig(t, names...)
	for _, name := range names {
		r.gh.set(name, lodash(1, "high"))
	}
	r.gh.mu.Lock()
	r.gh.limited["r2"] = true
	r.gh.limited["r3"] = true
	r.gh.mu.Unlock()

	evts, err := r.cycle()
	var rl *ghclient.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("the pass returned %v, want the rate limit", err)
	}
	for _, name := range []string{"r4", "r5"} {
		if n, _ := r.gh.counts(name); n != 0 {
			t.Errorf("%s was listed %d times after the rate limit, want never", name, n)
		}
		if e := r.pkg(name, "npm", "lodash"); e != nil {
			t.Errorf("%s's package = %+v, want none", name, e)
		}
	}
	for _, name := range []string{"r0", "r1"} {
		if e := r.pkg(name, "npm", "lodash"); e == nil || fmt.Sprint(r.alertNumbers(e)) != "[1]" {
			t.Errorf("%s's package = %+v, want one holding alert 1", name, e)
		}
		if r.etag(name) == "" {
			t.Errorf("%s's listing stored no ETag", name)
		}
	}
	if len(evts) != 2 {
		t.Errorf("emitted %v, want r0's and r1's alert_opened", eventTypes(evts))
	}
}

// etagWriteFailing is an access store whose writes of a non-empty ETag fail.
type etagWriteFailing struct {
	db.RepositoryDependabotAccessStore
}

func (s etagWriteFailing) RecordAlertListingSystem(ctx context.Context, orgID, repositoryID, etag string) (domain.RepositoryDependabotAccess, error) {
	if etag != "" {
		return domain.RepositoryDependabotAccess{}, fmt.Errorf("injected failure")
	}
	return s.RepositoryDependabotAccessStore.RecordAlertListingSystem(ctx, orgID, repositoryID, etag)
}

// TestRefreshVulnerablePackages_ALostETagWriteLeavesNoStaleCondition: a
// listing whose snapshot writes landed and whose ETag write did not leaves no
// ETag, rather than the previous listing's. When the alerts go back to what
// that previous listing held, the next listing is answered in full and the
// package resolves, where a 304 would have left it open.
func TestRefreshVulnerablePackages_ALostETagWriteLeavesNoStaleCondition(t *testing.T) {
	r := newVulnRig(t, "api")
	r.mustCycle()
	row := r.rows()[0]
	before, err := r.stores.DependabotAccess.GetSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID)
	if err != nil || before == nil || before.AlertsETag == "" {
		t.Fatalf("setup: the empty listing's ETag = %+v, %v", before, err)
	}

	r.access = etagWriteFailing{r.stores.DependabotAccess}
	r.gh.set("api", lodash(2, "high"))
	if evts := r.mustCycle(); len(evts) != 1 {
		t.Fatalf("the alert emitted %v", eventTypes(evts))
	}
	after, err := r.stores.DependabotAccess.GetSystem(context.Background(), runmode.LocalDefaultOrgID, row.ID)
	if err != nil || after == nil || after.AlertsETag != "" {
		t.Fatalf("after the lost ETag write the stored ETag = %+v, %v; want none", after, err)
	}

	r.access = nil
	r.gh.set("api")
	evts := r.mustCycle()
	if len(evts) != 1 || evts[0].EventType != domain.EventGitHubVulnerabilityResolved {
		t.Errorf("the alert closing emitted %v, want the package resolved", eventTypes(evts))
	}
}

// TestRetireUnreadableVulnerablePackages: a repository whose answer says its
// alerts are turned off has its packages retired with that reason and loses
// its ETag; one whose alerts are readable is left alone.
func TestRetireUnreadableVulnerablePackages(t *testing.T) {
	r := newVulnRig(t, "api", "web")
	ctx := context.Background()
	r.gh.set("api", lodash(1, "high"))
	r.gh.set("web", fakeAlert{number: 1, ecosystem: "npm", pkg: "react", severity: "low"})
	r.mustCycle()
	api := r.pkg("api", "npm", "lodash")
	if _, err := r.stores.DependabotAccess.RecordSystem(ctx, runmode.LocalDefaultOrgID, api.RepositoryID, domain.DependabotAlertsDisabled); err != nil {
		t.Fatalf("record: %v", err)
	}
	var listings []AlertListing
	for _, row := range r.rows() {
		a, err := r.stores.DependabotAccess.GetSystem(ctx, runmode.LocalDefaultOrgID, row.ID)
		if err != nil || a == nil {
			t.Fatalf("read %s: %+v %v", row.Slug(), a, err)
		}
		listings = append(listings, AlertListing{Repository: row, Access: *a})
	}

	before := r.pub.count()
	if n := r.tracker().RetireUnreadableVulnerablePackages(ctx, vulnHost, listings); n != 1 {
		t.Fatalf("retired %d packages, want api's one", n)
	}
	evts := r.pub.since(before)
	var meta events.GitHubVulnerabilityUnreachableMetadata
	if len(evts) != 1 || *evts[0].EntityID != api.ID || json.Unmarshal([]byte(evts[0].MetadataJSON), &meta) != nil || meta.Reason != events.GitHubUnreachableAlertsDisabled || fmt.Sprint(meta.AlertNumbers) != "[1]" {
		t.Errorf("emitted %v (%+v), want one unreachable on api's package with reason %s", eventTypes(evts), meta, events.GitHubUnreachableAlertsDisabled)
	}
	a, err := r.stores.DependabotAccess.GetSystem(ctx, runmode.LocalDefaultOrgID, api.RepositoryID)
	if err != nil || a == nil || a.AlertsETag != "" {
		t.Errorf("api's answer = %+v, %v; want its ETag cleared", a, err)
	}
	web := r.pkg("web", "npm", "react")
	if w, err := r.stores.DependabotAccess.GetSystem(ctx, runmode.LocalDefaultOrgID, web.RepositoryID); err != nil || w == nil || w.AlertsETag == "" {
		t.Errorf("web's answer = %+v, %v; want it untouched", w, err)
	}
}
