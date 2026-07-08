package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// dashboardBackfillDays is the trailing window the personal dashboard
// aggregates (it mirrors handleDashboardStats' sinceDays). The backfill seeds
// merged/closed/reviewed entities across this window so the dashboard's merged
// count, reviews, and 14-day sparkline aren't blank for history that predates
// tracking.
const dashboardBackfillDays = 30

// dashboardBackfillQueries builds the user-perspective merged/closed/reviewed
// search queries for login over the trailing dashboardBackfillDays, scoped to
// repos. Shared by the local per-cycle discovery backfill (discoverGitHub) and
// the multi-mode on-bind backfill (BackfillDashboardHistory) so both paths
// search for exactly the same history.
//
// login is an explicit GitHub login (NOT "@me"): the multi-mode path runs on an
// App installation / org-PAT token that has no viewer, and the local path
// passes its resolved "me" login. Either way `author:<login>` /
// `reviewed-by:<login>` is what GitHub search matches.
func dashboardBackfillQueries(login string, repos []string) []string {
	since := time.Now().AddDate(0, 0, -dashboardBackfillDays).Format("2006-01-02")
	bases := []string{
		fmt.Sprintf("is:pr is:merged author:%s merged:>=%s", login, since),
		fmt.Sprintf("is:pr is:merged reviewed-by:%s merged:>=%s", login, since),
		fmt.Sprintf("is:pr is:closed is:unmerged author:%s closed:>=%s", login, since),
		fmt.Sprintf("is:pr is:closed is:unmerged reviewed-by:%s closed:>=%s", login, since),
	}
	var queries []string
	for _, base := range bases {
		queries = append(queries, scopedQueries(base, repos)...)
	}
	return queries
}

// BackfillDashboardHistory runs the merged/closed/reviewed search for login,
// scoped to repos, over client, and seeds the resulting (terminal) PRs as
// entities WITHOUT emitting events — the personal dashboard reads entity
// snapshots, and these are historical records, not asks. Returns the number of
// new entities seeded.
//
// This is the multi-mode counterpart of discoverGitHub's local-only Phase 1b:
// an App installation / org-PAT token has no viewer, so the org poll cycle
// can't run a "me" backfill; instead the server triggers this per bound user
// (on identity bind, and lazily on first dashboard load) with that user's
// explicit login. It is deliberately seed-only — it does not refresh existing
// entities (the poll cycle owns that) and creates no tasks (no retroactive task
// creation, per the append-only-events invariant).
//
// It returns an error only when EVERY search query failed (a transient outage
// the caller should retry); a partial failure seeds what it can and returns
// nil, since organic forward tracking will cover the rest.
//
// ctx is the caller's (the backfill goroutine runs under a bounded timeout):
// the entity-seed writes propagate it, and it's checked between queries so a
// cancelled/timed-out backfill stops issuing further searches and seeds
// promptly rather than running the full query set to completion.
func (t *Tracker) BackfillDashboardHistory(ctx context.Context, client *ghclient.Client, login string, repos []string) (int, error) {
	if client == nil || login == "" {
		return 0, nil
	}
	queries := dashboardBackfillQueries(login, repos)

	seen := map[string]bool{}
	seeded := 0
	failed := 0
	var firstErr error
	for _, q := range queries {
		// Honor cancellation/timeout before each query's search + seeds — the
		// caller is a request-detached goroutine on a bounded deadline.
		if err := ctx.Err(); err != nil {
			return seeded, err
		}
		prs, err := client.DiscoverPRs(ctx, q, 50)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			log.Printf("[tracker] dashboard backfill query failed: %v (query: %s)", err, q)
			continue
		}
		for _, pr := range prs {
			sid := ghSourceID(pr.Snapshot.Repo, pr.Snapshot.Number)
			if seen[sid] {
				continue
			}
			seen[sid] = true
			created, serr := t.seedBackfillEntity(ctx, pr)
			if serr != nil {
				log.Printf("[tracker] dashboard backfill seed %s: %v", sid, serr)
				continue
			}
			if created {
				seeded++
			}
		}
	}
	if len(queries) > 0 && failed == len(queries) {
		return seeded, fmt.Errorf("dashboard backfill: all %d queries failed: %w", failed, firstErr)
	}
	return seeded, nil
}

// seedBackfillEntity creates an entity for a backfilled PR and seeds its
// snapshot, marking it closed when terminal so it doesn't sit in the active
// refresh set. Mirrors the terminal branch of RefreshGitHub's discovery loop.
// Returns true when a NEW entity was created; an already-known entity is left
// untouched (the poll cycle owns refresh) and returns false. Emits no events.
func (t *Tracker) seedBackfillEntity(ctx context.Context, d ghclient.DiscoveredPR) (bool, error) {
	snap := d.Snapshot
	snap.NodeID = d.NodeID
	sid := ghSourceID(snap.Repo, snap.Number)

	entity, created, err := t.entities.FindOrCreateSystem(ctx, t.orgID, "github", sid, "pr", snap.Title, snap.URL)
	if err != nil {
		return false, err
	}
	if !created {
		return false, nil
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return false, err
	}
	// CAS against entity.PollSeq (0 for a just-created row). A miss means a
	// concurrent seed of the same brand-new entity already landed a
	// snapshot — harmless, nothing to retry.
	if _, err := t.entities.UpdateSnapshotCASSystem(ctx, t.orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
		return false, err
	}
	if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
		if err := t.entities.MarkClosedSystem(ctx, t.orgID, entity.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}
