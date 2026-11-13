package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// AlertListing is one repository whose open Dependabot alerts a pass lists:
// its registry row, and its stored answer, which carries the state of the
// repository's previous listing.
type AlertListing struct {
	Repository domain.Repository
	Access     domain.RepositoryDependabotAccess
}

// WithDependabotAccess hands the tracker the store each alert listing is
// recorded in (RefreshVulnerablePackages). Without it nothing is listed.
func (t *Tracker) WithDependabotAccess(store db.RepositoryDependabotAccessStore) *Tracker {
	t.dependabotAccess = store
	return t
}

// RefreshVulnerablePackages lists the open Dependabot alerts of each of repos
// under client and diffs each repository's packages against the vulnerable
// packages TF holds in it, one entity per package:
//
//   - a package with open alerts and no active entity gets one;
//   - each alert open now that the package's stored snapshot did not hold
//     emits github:vulnerability:alert_opened;
//   - a package TF holds that has no open alert left emits
//     github:vulnerability:resolved, whose terminating close closes the
//     entity. A later alert on the package creates a new entity.
//
// A new entity's snapshot starts empty, so the listing that creates it emits
// alert_opened for every alert open on the package, including alerts that
// opened before TF first listed the repository: an open alert needs attention
// however long it has been open, as a check already failing on a pull request
// TF has just found does. A repository's first listing therefore announces
// every open alert, and so does the first listing after the org returns to a
// host whose change retired the repository's packages.
//
// A listing is conditional on the previous one's ETag, so an unchanged
// repository costs a request that does not count against the rate limit. A
// listing some of whose writes did not land is recorded without its ETag, so
// the next cycle lists again in full. A listing that fails changes nothing: no
// package is resolved because a request failed.
//
// A repository GitHub refuses (a 403 or 404: the credential lost the
// permission, or alerts were turned off) is skipped and the pass goes on; the
// access probe records the new answer at its next recheck. Any other failure
// ends the pass and is returned, since it would meet every remaining
// repository too: a rate limit, a refused credential, GitHub failing. Returns
// how many events landed on the queue.
func (t *Tracker) RefreshVulnerablePackages(ctx context.Context, scope string, client *ghclient.Client, repos []AlertListing) (int, error) {
	if t.dependabotAccess == nil || len(repos) == 0 {
		return 0, nil
	}
	held, err := t.heldVulnerablePackages(ctx, scope, repos)
	if err != nil {
		return 0, err
	}
	emitted := 0
	for _, l := range repos {
		if ctx.Err() != nil {
			return emitted, ctx.Err()
		}
		n, err := t.refreshRepositoryAlerts(ctx, scope, client, l, held[l.Repository.ID])
		emitted += n
		if err == nil {
			continue
		}
		if alertsRefused(err) {
			recordDependabotListing(dependabotListingRefused)
			trackerLog.InfoContext(ctx, "dependabot alerts not readable; skipping the repository this cycle", "org", t.orgID, "repo", l.Repository.Slug(), "error", err)
			continue
		}
		var rl *ghclient.ErrRateLimited
		if !errors.As(err, &rl) {
			recordDependabotListing(dependabotListingFailed)
		}
		return emitted, fmt.Errorf("list dependabot alerts of %s: %w", l.Repository.Slug(), err)
	}
	return emitted, nil
}

// alertsRefused reports whether a listing failed because GitHub answered for
// the one repository: its own 403 (the credential lacks the permission, or
// alerts are turned off) or a 404. A 403 that did not come from GitHub, from
// something in front of it, says nothing about the repository and would meet
// every other one too.
func alertsRefused(err error) bool {
	var he *ghclient.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.StatusCode == http.StatusNotFound ||
		(he.StatusCode == http.StatusForbidden && he.Class == upstream.Auth)
}

// heldVulnerablePackages reads the active vulnerable packages on scope in
// repos, by repository id and then key.
func (t *Tracker) heldVulnerablePackages(ctx context.Context, scope string, repos []AlertListing) (map[string]map[string]domain.Entity, error) {
	want := make(map[string]bool, len(repos))
	for _, l := range repos {
		want[l.Repository.ID] = true
	}
	active, err := t.entities.ListActiveSystem(ctx, t.orgID, domain.ProviderGitHub)
	if err != nil {
		return nil, fmt.Errorf("list active github entities: %w", err)
	}
	held := map[string]map[string]domain.Entity{}
	for _, e := range active {
		if e.Kind != domain.EntityKindVulnerablePackage || e.Scope != scope || !want[e.RepositoryID] {
			continue
		}
		if held[e.RepositoryID] == nil {
			held[e.RepositoryID] = map[string]domain.Entity{}
		}
		held[e.RepositoryID][e.SourceID] = e
	}
	return held, nil
}

// refreshRepositoryAlerts lists one repository's open alerts and applies
// them to its packages. held is the repository's active packages by key.
func (t *Tracker) refreshRepositoryAlerts(ctx context.Context, scope string, client *ghclient.Client, l AlertListing, held map[string]domain.Entity) (int, error) {
	repo := l.Repository
	alerts, newETag, notModified, err := client.ListOpenDependabotAlerts(ctx, repo.Owner, repo.Repo, l.Access.AlertsETag)
	if err != nil {
		return 0, err
	}
	emitted := 0
	if notModified {
		recordDependabotListing(dependabotListingNotModified)
		// Nothing opened or closed. A package whose resolution was emitted
		// and whose close has not landed is still owed one.
		for _, key := range slices.Sorted(maps.Keys(held)) {
			if prev, ok := storedVulnerablePackageSnapshot(held[key]); ok && prev.Terminal() {
				n, _ := t.oweVulnerablePackageClose(ctx, held[key])
				emitted += n
			}
		}
		return emitted, nil
	}
	recordDependabotListing(dependabotListingListed)

	packages, updatedAt := groupDependabotAlerts(repo.Slug(), alerts)
	complete := true
	for _, key := range slices.Sorted(maps.Keys(packages)) {
		snap := packages[key]
		e, ok := held[key]
		if !ok {
			created, _, err := t.entities.FindOrCreateVulnerablePackageSystem(ctx, t.orgID, scope, key,
				domain.VulnerablePackageTitle(snap.Ecosystem, snap.Package),
				domain.VulnerablePackageURL(scope, repo.Slug(), snap.Ecosystem, snap.Package))
			if err != nil {
				trackerLog.ErrorContext(ctx, "create vulnerable package entity failed", "org", t.orgID, "source_id", key, "error", err)
				complete = false
				continue
			}
			e = *created
		}
		n, landed := t.applyVulnerablePackage(ctx, e, repo, snap, updatedAt)
		emitted += n
		complete = complete && landed
	}
	for _, key := range slices.Sorted(maps.Keys(held)) {
		if _, open := packages[key]; open {
			continue
		}
		n, landed := t.resolveVulnerablePackage(ctx, held[key], repo)
		emitted += n
		complete = complete && landed
	}

	if !complete {
		// No ETag, so the next listing is answered in full, and the writes
		// that did not land are made again even if the alerts have since gone
		// back to what an earlier ETag described.
		newETag = ""
	}
	if _, err := t.dependabotAccess.RecordAlertListingSystem(ctx, t.orgID, repo.ID, newETag); err != nil {
		trackerLog.ErrorContext(ctx, "record dependabot alert listing failed", "org", t.orgID, "repo", repo.Slug(), "error", err)
	}
	return emitted, nil
}

// groupDependabotAlerts groups a repository's open alerts into one snapshot per
// package, keyed as the package's entity is, with each package's alerts
// ordered by number. updatedAt is each alert's state-change time, by number.
func groupDependabotAlerts(repoSlug string, alerts []ghclient.OpenDependabotAlert) (map[string]domain.VulnerablePackageSnapshot, map[int]time.Time) {
	packages := map[string]domain.VulnerablePackageSnapshot{}
	updatedAt := make(map[int]time.Time, len(alerts))
	for _, a := range alerts {
		key := domain.VulnerablePackageSourceID(repoSlug, a.Ecosystem, a.Package)
		snap, ok := packages[key]
		if !ok {
			snap = domain.VulnerablePackageSnapshot{Repo: repoSlug, Ecosystem: a.Ecosystem, Package: a.Package}
		}
		snap.Alerts = append(snap.Alerts, a.Alert)
		packages[key] = snap
		updatedAt[a.Alert.Number] = a.UpdatedAt
	}
	for key, snap := range packages {
		domain.SortDependabotAlerts(snap.Alerts)
		packages[key] = snap
	}
	return packages, updatedAt
}

// applyVulnerablePackage commits curr, a package's open alerts, as entity e's
// snapshot, with an alert_opened for each alert e's stored snapshot did not
// hold, which is every alert when e has none stored. landed reports whether
// the write went through.
//
// An entity whose stored snapshot is already terminal has had its resolution
// emitted and is waiting on its close: alerts open on the package again belong
// to the entity the next listing creates once this one has closed, so nothing
// is written here, and the close is owed if it was lost.
func (t *Tracker) applyVulnerablePackage(ctx context.Context, e domain.Entity, repo domain.Repository, curr domain.VulnerablePackageSnapshot, updatedAt map[int]time.Time) (emitted int, landed bool) {
	prev, hasPrev := storedVulnerablePackageSnapshot(e)
	if hasPrev && prev.Terminal() {
		n, _ := t.oweVulnerablePackageClose(ctx, e)
		return n, false
	}
	evts := DiffVulnerablePackage(prev, curr, e.ID, repo.ID, updatedAt)
	ok, enqueued, err := t.commitRefresh(ctx, t.orgID, e.ID, canonicalVulnerablePackageJSON(prev, hasPrev), mustJSON(curr), e.PollSeq, evts)
	if err != nil {
		trackerLog.ErrorContext(ctx, "commit vulnerable package snapshot failed", "org", t.orgID, "source_id", e.SourceID, "error", err)
		return 0, false
	}
	if !ok {
		trackerLog.WarnContext(ctx, "vulnerable package snapshot CAS lost race, skipping", "org", t.orgID, "source_id", e.SourceID)
		return 0, false
	}
	return enqueued, true
}

// resolveVulnerablePackage applies a listing that holds no open alert of the
// package entity e is: e's stored alerts close, and resolved is emitted with
// the terminal snapshot. An entity that stored no snapshot never recorded an
// open alert, so it closes with no event; one whose snapshot is already
// terminal is owed its close.
func (t *Tracker) resolveVulnerablePackage(ctx context.Context, e domain.Entity, repo domain.Repository) (emitted int, landed bool) {
	prev, hasPrev := storedVulnerablePackageSnapshot(e)
	if !hasPrev {
		_, ecosystem, name, _ := domain.SplitVulnerablePackageSourceID(e.SourceID)
		terminal := domain.VulnerablePackageSnapshot{Repo: repo.Slug(), Ecosystem: ecosystem, Package: name, Alerts: []domain.DependabotAlert{}}
		ok, err := t.entities.CloseWithSnapshotCASSystem(ctx, t.orgID, e.ID, mustJSON(terminal), e.PollSeq)
		if err != nil {
			trackerLog.ErrorContext(ctx, "close unseeded vulnerable package failed", "org", t.orgID, "source_id", e.SourceID, "error", err)
			return 0, false
		}
		return 0, ok
	}
	if prev.Terminal() {
		return t.oweVulnerablePackageClose(ctx, e)
	}
	curr := prev
	curr.Repo = repo.Slug()
	curr.Alerts = []domain.DependabotAlert{}
	evts := DiffVulnerablePackage(prev, curr, e.ID, repo.ID, nil)
	ok, enqueued, err := t.commitRefresh(ctx, t.orgID, e.ID, mustJSON(prev), mustJSON(curr), e.PollSeq, evts)
	if err != nil {
		trackerLog.ErrorContext(ctx, "commit vulnerable package resolution failed", "org", t.orgID, "source_id", e.SourceID, "error", err)
		return 0, false
	}
	if !ok {
		trackerLog.WarnContext(ctx, "vulnerable package resolution CAS lost race, skipping", "org", t.orgID, "source_id", e.SourceID)
		return 0, false
	}
	return enqueued, true
}

// oweVulnerablePackageClose enqueues the close obligation for an active entity
// whose stored snapshot already says the package resolved, unless a close for
// it is unsettled in the queue. landed reports whether nothing is left owed by
// this call: the obligation committed, or none was needed.
func (t *Tracker) oweVulnerablePackageClose(ctx context.Context, e domain.Entity) (emitted int, landed bool) {
	owed, ok := t.closeOwed(ctx, t.orgID, e.ID, true, true)
	if !ok {
		return 0, true
	}
	committed, enqueued, err := t.commitRefresh(ctx, t.orgID, e.ID, e.SnapshotJSON, e.SnapshotJSON, e.PollSeq, []domain.Event{owed})
	if err == nil && !committed {
		trackerLog.WarnContext(ctx, "vulnerable package close obligation CAS lost race, skipping", "org", t.orgID, "source_id", e.SourceID)
	}
	if err != nil {
		trackerLog.ErrorContext(ctx, "owe vulnerable package close failed", "org", t.orgID, "source_id", e.SourceID, "error", err)
		return 0, false
	}
	return enqueued, committed
}

// DiffVulnerablePackage is the events a package's snapshot moving from prev to
// curr implies: an alert_opened for each alert open in curr whose number prev
// did not hold, and a resolved when prev held open alerts and curr holds none.
// An alert closing while another stays open emits nothing. updatedAt is each
// alert's state-change time by number, which an alert_opened takes as when it
// occurred; nil leaves it unknown.
func DiffVulnerablePackage(prev, curr domain.VulnerablePackageSnapshot, entityID, repositoryID string, updatedAt map[int]time.Time) []domain.Event {
	var evts []domain.Event
	now := time.Now()
	highest := curr.HighestSeverity()
	for _, a := range curr.Alerts {
		if _, wasOpen := prev.Alert(a.Number); wasOpen {
			continue
		}
		evts = append(evts, domain.Event{
			EventType: domain.EventGitHubVulnerabilityAlertOpened,
			EntityID:  &entityID,
			MetadataJSON: mustJSON(events.GitHubVulnerabilityAlertOpenedMetadata{
				Repo:                curr.Repo,
				RepositoryID:        repositoryID,
				Ecosystem:           curr.Ecosystem,
				Package:             curr.Package,
				AlertNumber:         a.Number,
				GHSAID:              a.GHSAID,
				CVEID:               a.CVEID,
				Severity:            a.Severity,
				ManifestPath:        a.ManifestPath,
				Scope:               a.Scope,
				FirstPatchedVersion: a.FirstPatchedVersion,
				HighestSeverity:     highest,
			}),
			OccurredAt: updatedAt[a.Number],
			CreatedAt:  now,
		})
	}
	if !prev.Terminal() && curr.Terminal() {
		numbers := make([]int, 0, len(prev.Alerts))
		for _, a := range prev.Alerts {
			numbers = append(numbers, a.Number)
		}
		evts = append(evts, domain.Event{
			EventType: domain.EventGitHubVulnerabilityResolved,
			EntityID:  &entityID,
			MetadataJSON: mustJSON(events.GitHubVulnerabilityResolvedMetadata{
				Repo:         curr.Repo,
				RepositoryID: repositoryID,
				Ecosystem:    curr.Ecosystem,
				Package:      curr.Package,
				AlertNumbers: numbers,
			}),
			CreatedAt: now,
		})
	}
	return evts
}

// canonicalVulnerablePackageJSON is a stored snapshot as this process
// marshals it, "" for none, which commitRefresh compares the new snapshot
// against: the stored copy's bytes are Postgres's, whose key order differs.
func canonicalVulnerablePackageJSON(snap domain.VulnerablePackageSnapshot, ok bool) string {
	if !ok {
		return ""
	}
	return mustJSON(snap)
}

// storedVulnerablePackageSnapshot parses a vulnerable package's stored
// snapshot. ok=false when it has none, or one that does not parse.
func storedVulnerablePackageSnapshot(e domain.Entity) (domain.VulnerablePackageSnapshot, bool) {
	if e.SnapshotJSON == "" {
		return domain.VulnerablePackageSnapshot{}, false
	}
	var snap domain.VulnerablePackageSnapshot
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil || snap.Package == "" {
		return domain.VulnerablePackageSnapshot{}, false
	}
	return snap, true
}
