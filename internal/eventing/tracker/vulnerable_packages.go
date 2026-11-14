package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

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
	// Untracked is a repository no team tracks, listed only for the packages
	// TF already holds in it: the listing updates and resolves those, and a
	// package it holds no entity for gets none, as a pull request opened in
	// an untracked repository is not discovered.
	Untracked bool
	// Marked is a repository a refresh mark names: a webhook delivery said
	// its alerts changed. Its listing sends no condition: after a change a
	// conditional request is answered in full anyway, and without one a 304
	// can never stand in for the change the delivery announced. A delivery
	// that announced nothing new, a duplicate or a replay, costs one listing
	// that diffs to nothing. The stored ETag is still cleared before the
	// listing's writes and replaced after them.
	Marked bool
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
//   - each open alert that becomes one Dependabot cannot fix emits
//     github:vulnerability:dependabot_cannot_fix;
//   - a package TF holds that has no open alert left emits
//     github:vulnerability:resolved, whose terminating close closes the
//     entity. A later alert on the package creates a new entity.
//
// What Dependabot did about each alert comes from a second read, of GitHub's
// GraphQL alert objects (ghclient.ListDependabotUpdates), which the REST
// listing does not carry and whose change no ETag or webhook reports. It is
// made when the listing finds an alert opening and otherwise on a schedule
// that thins out as the alerts age (dependabotUpdateRecheck), whether or not
// the listing was answered 304.
//
// A new entity's snapshot starts empty, so the listing that creates it emits
// alert_opened for every alert open on the package, and dependabot_cannot_fix
// for every one of them Dependabot cannot fix, including alerts that opened
// before TF first listed the repository: an open alert needs attention
// however long it has been open, as a check already failing on a pull request
// TF has just found does. A repository's first listing therefore announces
// every open alert, and so does the first listing after the org returns to a
// host whose change retired the repository's packages.
//
// A listing is conditional on the previous one's ETag, so an unchanged
// repository costs a request that does not count against the rate limit. A
// listing that changed clears the stored ETag before any of its writes, and
// stores its own only once every write landed, so a listing interrupted
// between the two leaves the next cycle to list again in full. A listing that
// fails changes nothing: no package is resolved because a request failed.
//
// A listing with Untracked set is of a repository no team tracks, listed only
// for the packages TF holds in it (AlertListing.Untracked), and one with
// Marked set sends no condition (AlertListing.Marked).
//
// A listing of a tracked repository that no mark named, and that announced an
// alert opening or a package resolving against an earlier listing of the
// repository, counts a missed hint: a change no webhook delivery announced.
//
// The repositories are listed concurrently, as many at once as the pull
// request listing (repoConcurrency). Each repository's listing and writes
// touch only its own packages, and held is read once before any of them.
//
// A repository GitHub refuses (a 403 or 404: the credential lost the
// permission, or alerts were turned off) is skipped and the pass goes on; the
// access probe records the new answer at its next recheck. So is one whose
// listing GitHub answered in a form TF cannot read in full
// (ghclient.ErrDependabotListingUnusable), which would fail the same way on
// every cycle. Any other failure ends the pass, since it would meet every
// remaining repository too: a rate limit, a refused credential, GitHub
// failing. No listing starts after it, the listings already in flight finish
// with their writes, and the first such failure is returned.
//
// Returns how many events landed on the queue, and finished: the listings of
// repos the pass is done with, each listed or skipped, in the order of repos.
// A listing that ended the pass, and one the pass never started, is left
// out. A tracker without the store lists nothing and finishes none.
func (t *Tracker) RefreshVulnerablePackages(ctx context.Context, scope string, client *ghclient.Client, repos []AlertListing) (emitted int, finished []AlertListing, err error) {
	if t.dependabotAccess == nil || len(repos) == 0 {
		return 0, nil, nil
	}
	held, err := t.heldVulnerablePackages(ctx, scope, repos)
	if err != nil {
		return 0, nil, err
	}
	var events atomic.Int64
	var stopped atomic.Bool
	reads := newUpdateReads()
	// done is written by each listing's goroutine at its own index, and read
	// once Wait has returned.
	done := make([]bool, len(repos))
	g := new(errgroup.Group) // no WithContext: a failure ends the pass by stopping new listings, not by cancelling the ones in flight
	g.SetLimit(repoConcurrency())
	for i, l := range repos {
		if stopped.Load() {
			break
		}
		g.Go(func() error {
			// Asked again here: Go waits for a free slot, and the listing
			// that freed it can be the one that ended the pass.
			if stopped.Load() {
				return nil
			}
			if err := ctx.Err(); err != nil {
				stopped.Store(true)
				return err
			}
			n, announced, err := t.refreshRepositoryAlerts(ctx, scope, client, l, held[l.Repository.ID], reads)
			events.Add(int64(n))
			if err == nil && announced > 0 && missedAlertHint(l, held[l.Repository.ID]) {
				recordMissedHint(t.orgID, missedHintDependabotListing)
			}
			if err = t.alertListingFailure(ctx, l.Repository, err); err != nil {
				stopped.Store(true)
				return err
			}
			done[i] = true
			return nil
		})
	}
	err = g.Wait()
	for i, l := range repos {
		if done[i] {
			finished = append(finished, l)
		}
	}
	return int(events.Load()), finished, err
}

// missedAlertHint reports whether a listing that announced a change is a
// missed hint: of a tracked repository no mark named, which held packages
// from an earlier listing or carried that listing's ETag. A repository's first
// listing, or the first since a host change or lost access retired its
// packages and cleared its ETag, announces what was already open, which no
// delivery could have named. Receivers mark no untracked repository, so its
// listing would count every change.
func missedAlertHint(l AlertListing, held map[string]domain.Entity) bool {
	return !l.Marked && !l.Untracked && (l.Access.AlertsETag != "" || len(held) > 0)
}

// alertListingFailure is what a repository's listing error does to its pass:
// nil when there is none or the pass skips the repository, which is counted
// and logged here, and otherwise the failure that ends the pass.
func (t *Tracker) alertListingFailure(ctx context.Context, repo domain.Repository, err error) error {
	if err == nil {
		return nil
	}
	if alertsRefused(err) {
		recordDependabotListing(dependabotListingRefused)
		trackerLog.InfoContext(ctx, "dependabot alerts not readable; skipping the repository this cycle", "org", t.orgID, "repo", repo.Slug(), "error", err)
		return nil
	}
	if errors.Is(err, ghclient.ErrDependabotListingUnusable) {
		recordDependabotListing(dependabotListingFailed)
		trackerLog.WarnContext(ctx, "dependabot alert listing unusable; skipping the repository this cycle", "org", t.orgID, "repo", repo.Slug(), "error", err)
		return nil
	}
	var rl *ghclient.ErrRateLimited
	if !errors.As(err, &rl) {
		recordDependabotListing(dependabotListingFailed)
	}
	return fmt.Errorf("list dependabot alerts of %s: %w", repo.Slug(), err)
}

// RetireUnreadableVulnerablePackages retires the packages TF holds in each of
// repos whose stored answer says their alerts can no longer be read:
// github:vulnerability:unreachable for each, with the answer as its reason,
// whose terminating close closes the entity and its tasks. Nothing fixed the
// alerts, but TF can no longer tell when anything does. The repository's ETag
// is cleared, so once its alerts are readable again the next listing is
// answered in full and every alert open then is announced on a new entity. A
// repository whose answer is readable is left alone. Like the retirement on a
// host change, it publishes again on a later call while a close has not
// landed. Returns how many packages it retired.
func (t *Tracker) RetireUnreadableVulnerablePackages(ctx context.Context, scope string, repos []AlertListing) int {
	var unreadable []AlertListing
	for _, l := range repos {
		if unreadableReason(l.Access.Access) != "" {
			unreadable = append(unreadable, l)
		}
	}
	if len(unreadable) == 0 {
		return 0
	}
	held, err := t.heldVulnerablePackages(ctx, scope, unreadable)
	if err != nil {
		trackerLog.ErrorContext(ctx, "read the vulnerable packages of repositories whose alerts became unreadable failed", "org", t.orgID, "error", err)
		return 0
	}
	retired := 0
	for _, l := range unreadable {
		repo := l.Repository
		packages := held[repo.ID]
		for _, key := range slices.Sorted(maps.Keys(packages)) {
			t.emitVulnerablePackageUnreachable(ctx, packages[key], &repo, unreadableReason(l.Access.Access))
			retired++
		}
		if l.Access.AlertsETag != "" && t.dependabotAccess != nil {
			if _, err := t.dependabotAccess.RecordAlertListingSystem(ctx, t.orgID, repo.ID, ""); err != nil {
				trackerLog.ErrorContext(ctx, "clear the ETag of a repository whose alerts became unreadable failed", "org", t.orgID, "repo", repo.Slug(), "error", err)
			}
		}
	}
	return retired
}

// unreadableReason is the unreachable reason an access answer gives, "" for
// a readable one.
func unreadableReason(access domain.DependabotAlertsAccess) string {
	switch access {
	case domain.DependabotAlertsNoPermission:
		return events.GitHubUnreachableNoPermission
	case domain.DependabotAlertsDisabled:
		return events.GitHubUnreachableAlertsDisabled
	}
	return ""
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
	active, err := t.entities.ListActiveKindSystem(ctx, t.orgID, domain.ProviderGitHub, scope, domain.EntityKindVulnerablePackage)
	if err != nil {
		return nil, fmt.Errorf("list active vulnerable packages: %w", err)
	}
	held := map[string]map[string]domain.Entity{}
	for _, e := range active {
		if !want[e.RepositoryID] {
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
// them to its packages, reading what Dependabot did about them when that is
// due (readDependabotUpdates). held is the repository's active packages by
// key. emitted counts every event that landed on the queue, and announced the
// alert_opened and resolved events the listing's diff produced, which leaves
// out the closes owed to packages whose resolution landed on an earlier
// listing and every dependabot_cannot_fix, which no webhook delivery
// announces.
//
// A listing answered 304 opened and closed nothing, but Dependabot may have
// acted on an alert since: the listing does not carry what it did, and GitHub
// sends no webhook when an update fails. So a 304 still reads the updates of
// the packages TF holds when they are due, and writes only the packages that
// read changed.
func (t *Tracker) refreshRepositoryAlerts(ctx context.Context, scope string, client *ghclient.Client, l AlertListing, held map[string]domain.Entity, reads *updateReads) (emitted, announced int, err error) {
	repo := l.Repository
	condition := l.Access.AlertsETag
	if l.Marked {
		condition = ""
	}
	alerts, newETag, notModified, err := client.ListOpenDependabotAlerts(ctx, repo.Owner, repo.Repo, condition)
	if err != nil {
		return 0, 0, err
	}
	if notModified {
		recordDependabotListing(dependabotListingNotModified)
		var refreshes []packageRefresh
		for _, key := range slices.Sorted(maps.Keys(held)) {
			e := held[key]
			prev, ok := storedVulnerablePackageSnapshot(e)
			if !ok {
				continue
			}
			if prev.Terminal() {
				// Its resolution was emitted and its close has not landed,
				// so it is still owed one.
				n, _ := t.oweVulnerablePackageClose(ctx, e)
				emitted += n
				continue
			}
			refreshes = append(refreshes, packageRefresh{entity: e, prev: prev, hasPrev: true, curr: prev})
		}
		readAt, read := t.readDependabotUpdates(ctx, client, l, refreshes, reads)
		if !read {
			return emitted, 0, nil
		}
		landed := true
		for _, p := range refreshes {
			if mustJSON(p.curr) == mustJSON(p.prev) {
				continue
			}
			n, _, ok := t.applyVulnerablePackage(ctx, p, repo)
			emitted += n
			landed = landed && ok
		}
		if landed {
			t.recordUpdatesRead(ctx, repo, readAt)
		}
		return emitted, 0, nil
	}
	recordDependabotListing(dependabotListingListed)
	if l.Access.AlertsETag != "" {
		// The stored ETag describes the listing before this one. Were it left
		// while the snapshots move past it, a later listing that went back to
		// those alerts would be answered 304 and never diffed.
		if _, err := t.dependabotAccess.RecordAlertListingSystem(ctx, t.orgID, repo.ID, ""); err != nil {
			trackerLog.ErrorContext(ctx, "clear dependabot alert listing ETag failed; skipping the repository this cycle", "org", t.orgID, "repo", repo.Slug(), "error", err)
			return 0, 0, nil
		}
	}

	packages := groupDependabotAlerts(repo.Slug(), alerts)
	complete := true
	var refreshes []packageRefresh
	for _, key := range slices.Sorted(maps.Keys(packages)) {
		snap := packages[key]
		e, ok := held[key]
		if !ok && l.Untracked {
			// Left out, and the listing's ETag with it, so a team tracking
			// the repository again gets the package from a full listing.
			complete = false
			continue
		}
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
		if awaitingClose(e) {
			// Its resolution was emitted and its close has not landed. The
			// alerts open on the package again belong to the entity the next
			// listing creates once this one has closed, so nothing is written
			// here, and the close is owed if it was lost.
			n, _ := t.oweVulnerablePackageClose(ctx, e)
			emitted += n
			complete = false
			continue
		}
		prev, hasPrev := storedVulnerablePackageSnapshot(e)
		refreshes = append(refreshes, packageRefresh{entity: e, prev: prev, hasPrev: hasPrev, curr: carryDependabotUpdates(prev, snap)})
	}
	readAt, read := t.readDependabotUpdates(ctx, client, l, refreshes, reads)
	updatesLanded := true
	for _, p := range refreshes {
		n, a, landed := t.applyVulnerablePackage(ctx, p, repo)
		emitted += n
		announced += a
		complete = complete && landed
		updatesLanded = updatesLanded && landed
	}
	if read && updatesLanded {
		t.recordUpdatesRead(ctx, repo, readAt)
	}
	for _, key := range slices.Sorted(maps.Keys(held)) {
		if _, open := packages[key]; open {
			continue
		}
		if awaitingClose(held[key]) {
			n, landed := t.oweVulnerablePackageClose(ctx, held[key])
			emitted += n
			complete = complete && landed
			continue
		}
		n, landed := t.resolveVulnerablePackage(ctx, held[key], repo)
		emitted += n
		announced += n
		complete = complete && landed
	}

	if !complete || newETag == "" {
		// The ETag is left cleared, so the next listing is answered in full
		// and the writes that did not land are made again.
		return emitted, announced, nil
	}
	if _, err := t.dependabotAccess.RecordAlertListingSystem(ctx, t.orgID, repo.ID, newETag); err != nil {
		trackerLog.ErrorContext(ctx, "record dependabot alert listing failed", "org", t.orgID, "repo", repo.Slug(), "error", err)
	}
	return emitted, announced, nil
}

// packageRefresh is one package a listing applies: its entity, the snapshot
// stored on it (hasPrev false when there is none), and the snapshot it moves
// to.
type packageRefresh struct {
	entity  domain.Entity
	prev    domain.VulnerablePackageSnapshot
	hasPrev bool
	curr    domain.VulnerablePackageSnapshot
}

// dependabotUpdateReadsPerPass caps the reads of Dependabot updates one
// credential pass makes, so that a burst of young alerts across a large
// tracked set (one advisory reaching hundreds of repositories at once) spreads
// its reads over several polls rather than spending the GraphQL budget the
// pull request refresh draws on too. A repository past the cap stays due and
// is read on a later poll.
const dependabotUpdateReadsPerPass = 25

// dependabotUpdateReadTimeout bounds one repository's read. A read is never
// worth waiting for, since a repository it misses is read on the next poll,
// and the deadline is what has the client refuse a request it would otherwise
// hold until a spent GraphQL budget resets, with the repository's listing
// waiting behind it.
const dependabotUpdateReadTimeout = 30 * time.Second

// updateReads is one pass's reads of what Dependabot did about its
// repositories' alerts: how many it may still make
// (dependabotUpdateReadsPerPass), and whether a read failed for a reason that
// would meet every other repository too (a rate limit, a refused credential,
// GitHub failing), which stops the pass's later reads. The listings go on
// without them, and the next poll reads again.
type updateReads struct {
	left    atomic.Int64
	stopped atomic.Bool
}

func newUpdateReads() *updateReads {
	r := &updateReads{}
	r.left.Store(dependabotUpdateReadsPerPass)
	return r
}

// maxDependabotUpdateRecheck is the longest a repository's Dependabot updates
// go unread while it has open alerts, however settled they are: Dependabot
// tries again when someone asks it to on the alert or turns security updates
// on, and neither sends TF anything.
const maxDependabotUpdateRecheck = 24 * time.Hour

// dependabotUpdateRecheck is how long after the last read a package's
// Dependabot updates are read again: a quarter of the age of its youngest
// alert Dependabot may still act on (DependabotAlert.AwaitsDependabot), the
// age being since GitHub last changed the alert, and at most
// maxDependabotUpdateRecheck, which is also the interval for a package with no
// such alert. Dependabot tries an alert soon after it opens, so a young alert
// is read on nearly every poll, and the reads thin out as it ages: an alert
// Dependabot never tries, on a repository with security updates off, costs
// about one read a day once it is a few days old.
func dependabotUpdateRecheck(s domain.VulnerablePackageSnapshot, now time.Time) time.Duration {
	interval := maxDependabotUpdateRecheck
	for _, a := range s.Alerts {
		if a.AwaitsDependabot() && !a.UpdatedAt.IsZero() {
			interval = min(interval, max(now.Sub(a.UpdatedAt)/4, 0))
		}
	}
	return interval
}

// updatesDue reports whether the Dependabot updates of the packages in
// refreshes, one repository's, are due a read at now, the last read being at
// readAt: an alert has none recorded, because it is new, a read failed after
// it opened, or a patch was released since its update was read, or the last
// read is older than the shortest of the packages' recheck intervals
// (dependabotUpdateRecheck).
func updatesDue(refreshes []packageRefresh, readAt, now time.Time) bool {
	if len(refreshes) == 0 {
		return false
	}
	interval := maxDependabotUpdateRecheck
	for _, p := range refreshes {
		for _, a := range p.curr.Alerts {
			if a.DependabotUpdate == nil {
				return true
			}
		}
		interval = min(interval, dependabotUpdateRecheck(p.curr, now))
	}
	return now.Sub(readAt) >= interval
}

// readDependabotUpdates reads what Dependabot did about the alerts of l's
// repository when the packages in refreshes are due it (updatesDue) and the
// pass may still read (updateReads), and moves each package's curr to what
// the read says (withDependabotUpdates). read reports whether it read, and
// readAt is when, which the caller records once the packages the read
// changed are written (recordUpdatesRead).
//
// A read GitHub answered with something TF cannot use for the repository
// (ghclient.ErrDependabotListingUnusable) is taken as an answer that names no
// alert, as GitHub answers a credential that may not read the repository's
// alerts, so the repository's next read keeps to its schedule. Any other
// failure changes nothing, and stops the pass's later reads.
func (t *Tracker) readDependabotUpdates(ctx context.Context, client *ghclient.Client, l AlertListing, refreshes []packageRefresh, reads *updateReads) (readAt time.Time, read bool) {
	repo := l.Repository
	now := t.now().UTC()
	if reads.stopped.Load() || !updatesDue(refreshes, l.Access.UpdatesReadAt, now) {
		return time.Time{}, false
	}
	if reads.left.Add(-1) < 0 {
		recordDependabotUpdateRead(dependabotUpdateReadDeferred)
		return time.Time{}, false
	}
	readCtx, cancel := context.WithTimeout(ctx, dependabotUpdateReadTimeout)
	updates, err := client.ListDependabotUpdates(readCtx, repo.Owner, repo.Repo)
	cancel()
	outcome := dependabotUpdateReadRead
	switch {
	case errors.Is(err, ghclient.ErrDependabotListingUnusable):
		trackerLog.WarnContext(ctx, "dependabot updates unusable; reading the repository's alerts as unanswered", "org", t.orgID, "repo", repo.Slug(), "error", err)
		updates, outcome = nil, dependabotUpdateReadFailed
	case err != nil:
		recordDependabotUpdateRead(dependabotUpdateReadFailed)
		reads.stopped.Store(true)
		var rl *ghclient.ErrRateLimited
		if !errors.As(err, &rl) {
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "read dependabot updates failed; the pass's other repositories are read next cycle", "org", t.orgID, "repo", repo.Slug(), "error", err)
		}
		return time.Time{}, false
	}
	for i := range refreshes {
		if !withDependabotUpdates(&refreshes[i].curr, updates) && outcome == dependabotUpdateReadRead {
			outcome = dependabotUpdateReadIncomplete
		}
	}
	recordDependabotUpdateRead(outcome)
	return now, true
}

// recordUpdatesRead stores readAt as when repo's Dependabot updates were last
// read. A write that fails leaves the earlier time, so the repository is read
// again sooner than its schedule says, and nothing is lost.
func (t *Tracker) recordUpdatesRead(ctx context.Context, repo domain.Repository, readAt time.Time) {
	if _, err := t.dependabotAccess.RecordUpdatesReadSystem(ctx, t.orgID, repo.ID, readAt); err != nil {
		trackerLog.ErrorContext(ctx, "record dependabot updates read failed", "org", t.orgID, "repo", repo.Slug(), "error", err)
	}
}

// withDependabotUpdates sets each of s's alerts that updates names to the
// update it names. An alert it does not name keeps the update s has, or, with
// none, gets DependabotUpdateNone: GitHub reported nothing for it, which is
// what GitHub reports for an alert Dependabot has not tried. complete reports
// whether updates named every alert. s's alerts are copied first, so a
// snapshot that shares them with a stored one leaves the stored one as it
// was.
func withDependabotUpdates(s *domain.VulnerablePackageSnapshot, updates map[int]domain.DependabotUpdate) (complete bool) {
	complete = true
	s.Alerts = slices.Clone(s.Alerts)
	for i, a := range s.Alerts {
		u, ok := updates[a.Number]
		if !ok {
			complete = false
			if a.DependabotUpdate != nil {
				continue
			}
			u = domain.DependabotUpdate{State: domain.DependabotUpdateNone}
		}
		s.Alerts[i].DependabotUpdate = &u
	}
	return complete
}

// carryDependabotUpdates is curr, a package as the listing gives it, with
// what TF last read of Dependabot's updates carried over from prev, its
// stored snapshot: each alert prev holds keeps its update, unless the alert's
// patched version has changed since, as when a fix is released for an alert
// that had none. Dependabot's answer then predates the version it would
// upgrade to, so the alert is left with none and read again. The listing does
// not carry updates.
func carryDependabotUpdates(prev, curr domain.VulnerablePackageSnapshot) domain.VulnerablePackageSnapshot {
	for i, a := range curr.Alerts {
		if p, ok := prev.Alert(a.Number); ok && p.FirstPatchedVersion == a.FirstPatchedVersion {
			curr.Alerts[i].DependabotUpdate = p.DependabotUpdate
		}
	}
	return curr
}

// awaitingClose reports whether e's stored snapshot already says the package
// resolved: its resolution was emitted, and its close has not landed.
func awaitingClose(e domain.Entity) bool {
	prev, ok := storedVulnerablePackageSnapshot(e)
	return ok && prev.Terminal()
}

// groupDependabotAlerts groups a repository's open alerts into one snapshot per
// package, keyed as the package's entity is, with each package's alerts
// ordered by number.
func groupDependabotAlerts(repoSlug string, alerts []ghclient.OpenDependabotAlert) map[string]domain.VulnerablePackageSnapshot {
	packages := map[string]domain.VulnerablePackageSnapshot{}
	for _, a := range alerts {
		key := domain.VulnerablePackageSourceID(repoSlug, a.Ecosystem, a.Package)
		snap, ok := packages[key]
		if !ok {
			snap = domain.VulnerablePackageSnapshot{Repo: repoSlug, Ecosystem: a.Ecosystem, Package: a.Package}
		}
		snap.Alerts = append(snap.Alerts, a.Alert)
		packages[key] = snap
	}
	for key, snap := range packages {
		domain.SortDependabotAlerts(snap.Alerts)
		packages[key] = snap
	}
	return packages
}

// applyVulnerablePackage commits p.curr, a package's open alerts, as the
// snapshot of p.entity, with the events DiffVulnerablePackage finds against
// p.prev, which hold every alert when the entity has no snapshot stored.
// announced counts the alert_opened events among those that landed, and
// landed reports whether the write went through. The entity's stored
// snapshot is not terminal (awaitingClose).
func (t *Tracker) applyVulnerablePackage(ctx context.Context, p packageRefresh, repo domain.Repository) (emitted, announced int, landed bool) {
	e := p.entity
	evts := DiffVulnerablePackage(p.prev, p.curr, e.ID, repo.ID)
	ok, enqueued, err := t.commitRefresh(ctx, t.orgID, e.ID, canonicalVulnerablePackageJSON(p.prev, p.hasPrev), mustJSON(p.curr), e.PollSeq, evts)
	if err != nil {
		trackerLog.ErrorContext(ctx, "commit vulnerable package snapshot failed", "org", t.orgID, "source_id", e.SourceID, "error", err)
		return 0, 0, false
	}
	if !ok {
		trackerLog.WarnContext(ctx, "vulnerable package snapshot CAS lost race, skipping", "org", t.orgID, "source_id", e.SourceID)
		return 0, 0, false
	}
	for _, evt := range evts {
		if evt.EventType == domain.EventGitHubVulnerabilityAlertOpened {
			announced++
		}
	}
	return enqueued, announced, true
}

// resolveVulnerablePackage applies a listing that holds no open alert of the
// package entity e is: e's stored alerts close, and resolved is emitted with
// the terminal snapshot. An entity that stored no snapshot never recorded an
// open alert, so it closes with no event. e's stored snapshot is not terminal
// (awaitingClose).
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
	curr := prev
	curr.Repo = repo.Slug()
	curr.Alerts = []domain.DependabotAlert{}
	evts := DiffVulnerablePackage(prev, curr, e.ID, repo.ID)
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
// curr implies, in this order for each alert open in curr:
//
//   - an alert_opened when prev did not hold its number, at the alert's
//     UpdatedAt, naming the pull request Dependabot opened for it when curr
//     records one;
//   - a dependabot_cannot_fix when Dependabot cannot fix it
//     (DependabotAlert.DependabotCannotFix) for a reason it did not have in
//     prev: every such alert prev did not hold, one Dependabot could fix or
//     might yet in prev, and one whose reason moved (no patched version, then
//     a patch released and Dependabot's update for it failed), so a handler on
//     one reason sees every alert that comes to have it.
//
// Then a resolved when prev held open alerts and curr holds none. An alert
// closing while another stays open emits nothing, and neither does any other
// change to an alert: its update, its severity, its patched version.
func DiffVulnerablePackage(prev, curr domain.VulnerablePackageSnapshot, entityID, repositoryID string) []domain.Event {
	var evts []domain.Event
	now := time.Now()
	highest := curr.HighestSeverity()
	for _, a := range curr.Alerts {
		was, wasOpen := prev.Alert(a.Number)
		alert := events.GitHubVulnerabilityAlert{
			Repo:                   curr.Repo,
			RepositoryID:           repositoryID,
			Ecosystem:              curr.Ecosystem,
			Package:                curr.Package,
			AlertNumber:            a.Number,
			GHSAID:                 a.GHSAID,
			CVEID:                  a.CVEID,
			Summary:                a.Summary,
			Severity:               a.Severity,
			ManifestPath:           a.ManifestPath,
			Scope:                  a.Scope,
			VulnerableVersionRange: a.VulnerableVersionRange,
			FirstPatchedVersion:    a.FirstPatchedVersion,
			HighestSeverity:        highest,
		}
		u := a.DependabotUpdate
		if !wasOpen {
			m := events.GitHubVulnerabilityAlertOpenedMetadata{GitHubVulnerabilityAlert: alert}
			if u != nil && u.State == domain.DependabotUpdatePullRequest {
				m.DependabotPullRequestNumber = u.PullRequestNumber
				m.DependabotPullRequestURL = u.PullRequestURL
			}
			evts = append(evts, domain.Event{
				EventType:    domain.EventGitHubVulnerabilityAlertOpened,
				EntityID:     &entityID,
				MetadataJSON: mustJSON(m),
				OccurredAt:   a.UpdatedAt,
				CreatedAt:    now,
			})
		}
		reason := a.DependabotCannotFix()
		if reason == "" || (wasOpen && was.DependabotCannotFix() == reason) {
			continue
		}
		m := events.GitHubVulnerabilityDependabotCannotFixMetadata{GitHubVulnerabilityAlert: alert, Reason: reason}
		if u != nil && u.State == domain.DependabotUpdateError {
			m.ErrorType = u.ErrorType
			m.ErrorTitle = u.ErrorTitle
		}
		evts = append(evts, domain.Event{
			EventType:    domain.EventGitHubVulnerabilityDependabotCannotFix,
			EntityID:     &entityID,
			MetadataJSON: mustJSON(m),
			CreatedAt:    now,
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
	return domain.ParseVulnerablePackageSnapshot(e.SnapshotJSON)
}
