package tracker

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// RetireGitHubOutOfScope retires every active GitHub entity keyed under a host
// other than scope, emitting github:pr:unreachable with reason scope_changed for
// each, and returns how many it emitted. The poller runs it once per org cycle,
// before discovery, and also when the org tracks nothing on its current host —
// which is every org that has just moved, since the tracked set does not carry
// over.
//
// GitHub is not asked: repository ids are per-deployment sequences and slugs
// repeat across deployments, so nothing the new host answered would be about
// these pull requests. Asking would also hand the new host the old one's node
// ids, and its stub lookups the old one's owner/repo#N. The rows are not moved:
// they stay where they are, closed with their history, and the old host's
// discovery finds them again by key if the org moves back. For that discovery
// to see them, the conditional-request cursor of each retired pull request's
// repository on the old host is dropped here (forgetPullsCursor).
//
// The entity is closed by the router, which this event terminates it through;
// an entity whose close has not landed by the next cycle is emitted for again,
// and the duplicate settles against the closed row.
func (t *Tracker) RetireGitHubOutOfScope(ctx context.Context, scope string) int {
	if scope == "" {
		return 0
	}
	entities, err := t.entities.ListActiveSystem(context.Background(), t.orgID, "github")
	if err != nil {
		trackerLog.ErrorContext(ctx, "list active github entities for the host check failed", "error", err)
		return 0
	}
	retired := 0
	forgotten := map[domain.RepoRef]bool{}
	for _, e := range entities {
		if e.Scope == scope {
			continue
		}
		t.forgetPullsCursor(ctx, e, forgotten)
		t.emitGitHubUnreachable(ctx, e, events.GitHubUnreachableScopeChanged)
		retired++
	}
	return retired
}

// forgetPullsCursor clears the stored ETag of the open-PR listing of e's
// repository on e's host, keeping its poll time, so the next listing there is
// unconditional. Without it, an org that returns to the host would replay the
// ETag stored before it left; a listing nothing changed in since answers 304,
// discovery sees none of its pull requests, and the ones retired in the
// meantime stay closed until something in the repository changes. done holds
// the repositories already handled this pass. Best-effort: a cursor left in
// place costs that delay, never a wrong event.
func (t *Tracker) forgetPullsCursor(ctx context.Context, e domain.Entity, done map[domain.RepoRef]bool) {
	if t.repos == nil || e.Scope == "" {
		return
	}
	owner, repo, _ := domain.SplitGitHubEntitySourceID(e.SourceID)
	if owner == "" || repo == "" {
		return
	}
	ref := domain.RepoRef{Host: e.Scope, Owner: strings.ToLower(owner), Repo: strings.ToLower(repo)}
	if done[ref] {
		return
	}
	done[ref] = true
	etag, polledAt, err := t.repos.GetPullsPollStateByRefSystem(ctx, t.orgID, ref)
	if err != nil {
		trackerLog.WarnContext(ctx, "read pulls poll state of a retired pull request's repository failed", "host", e.Scope, "repo", owner+"/"+repo, "error", err)
		return
	}
	if etag == "" || polledAt == nil {
		return
	}
	if err := t.repos.SetPullsPollStateByRefSystem(ctx, t.orgID, ref, "", *polledAt); err != nil {
		trackerLog.WarnContext(ctx, "clear pulls poll state of a retired pull request's repository failed", "host", e.Scope, "repo", owner+"/"+repo, "error", err)
	}
}

// emitGitHubUnreachable publishes github:pr:unreachable for e, from its stored
// snapshot. A stub with no snapshot carries its repo and number parsed from its
// key, which is what it has. Publish, not the snapshot-CAS enqueue: there is no
// new snapshot to advance, and the entity's close is the router's job.
func (t *Tracker) emitGitHubUnreachable(ctx context.Context, e domain.Entity, reason string) {
	var snap domain.PRSnapshot
	if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			trackerLog.WarnContext(ctx, "corrupt pr snapshot on an unreachable pull request; emitting with last-known fields blank",
				"source_id", e.SourceID, "entity_id", e.ID, "error", err)
			snap = domain.PRSnapshot{}
		}
	}
	if snap.Repo == "" || snap.Number == 0 {
		owner, repo, number := domain.SplitGitHubEntitySourceID(e.SourceID)
		if owner != "" && repo != "" {
			snap.Repo, snap.Number = owner+"/"+repo, number
		}
	}
	entityID := e.ID
	trackerLog.InfoContext(ctx, "TF will not follow this pull request any more; retiring entity",
		"source_id", e.SourceID, "entity_id", e.ID, "host", e.Scope, "reason", reason)
	t.publish(ctx, domain.Event{
		OrgID:     t.orgID,
		EventType: domain.EventGitHubPRUnreachable,
		EntityID:  &entityID,
		MetadataJSON: mustJSON(events.GitHubPRUnreachableMetadata{
			Author:   snap.Author,
			Repo:     snap.Repo,
			PRNumber: snap.Number,
			IsDraft:  snap.IsDraft,
			HeadSHA:  snap.HeadSHA,
			Labels:   snap.Labels,
			Title:    snap.Title,
			Host:     e.Scope,
			Reason:   reason,
		}),
		// occurred_at is left zero: nothing at GitHub happened. TF stopped
		// following the pull request when the org's host changed, which is
		// observed here; consumers fall back to created_at. created_at is set
		// for the bus, which hands this struct to subscribers as-is.
		CreatedAt: time.Now(),
	})
}
