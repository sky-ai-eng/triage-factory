package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

// Publisher is the event sink the tracker emits to. In production it's
// the durable ingestor, which enqueues router-bound github:/jira:
// events (so the router can't drop them under burst) and forwards every
// event to the in-memory bus for cosmetic subscribers.
//
// ctx is the emitting cycle's. It reaches the durable enqueue, which is
// what lets an event's later routing be tied back to the poll that found
// it — a bare *eventbus.Bus no longer satisfies this interface for that
// reason, and a test that only wants the fan-out passes a two-line
// adapter instead.
type Publisher interface {
	Publish(ctx context.Context, evt domain.Event)

	// PublishPreEnqueued forwards an event the caller already committed to
	// the durable outbox — the bus fan-out and the drain-worker nudge
	// without a second enqueue. Every emit that rides a snapshot CAS's
	// transaction takes this path (see emitWithSnapshotCAS).
	PublishPreEnqueued(ctx context.Context, evt domain.Event)
}

const (
	jiraBatchSize = 100 // max issues per JQL id IN (...) / key IN (...) query

	// descriptionStoreMaxRunes caps what we persist on entities.description.
	// Jira descriptions and PR bodies are unbounded (teams regularly paste
	// multi-KB specs, stack traces, etc.); storing them raw would bloat the
	// column for no current benefit — the scorer already truncates at 1500
	// runes for the LLM prompt, so 2000 gives a small buffer while keeping
	// rows compact. If a future UI wants to render the full body it should
	// re-fetch from the source directly rather than relying on this mirror.
	descriptionStoreMaxRunes = 2000
)

// Tracker manages the discover → refresh → diff → emit cycle for both
// GitHub and Jira. In the entity-first model, the tracker:
//   - creates/updates entities (not tasks — that's routing's job)
//   - diffs entity snapshots to produce per-action events
//   - publishes events to the bus (recording is routing's job)
//   - does NOT create or update tasks
type Tracker struct {
	database *sql.DB
	pub      Publisher
	tasks    db.TaskStore       // tracker creates review_requested tasks during discovery + reconciles stale ones
	entities db.EntityStore     // entity lifecycle (find/create, snapshot, title/description, close/reactivate)
	repos    db.RepositoryStore // per-repo conditional-request (ETag) state for GitHub open-PR discovery
	// queue is the durable outbox, held directly rather than reached
	// through pub because the snapshot-paired arms need the ONE write that
	// carries both halves: the snapshot advance and the events that belong
	// to it (EnqueueBatchWithSnapshotCAS) — a diffed cycle's transitions,
	// and a discovery seed's review-request backfill. Every other emit the
	// tracker makes — an unreachable Jira key, poll-complete sentinels —
	// has no snapshot to pair with and goes through pub.
	queue db.EventQueueStore
	// orgID is the tenant this tracker emits events and reads/writes
	// entities for. Set at construction and stable for the Tracker's
	// lifetime; the poller's per-org loop constructs a fresh Tracker
	// per tenant per cycle. Local mode passes runmode.LocalDefaultOrgID;
	// multi mode passes the iterated active org.
	//
	// TODO: a future GitHub-App-credentials change will
	// also bundle the per-org GitHub client + bot username on this
	// struct — today those are method parameters because credentials
	// are process-global.
	orgID string
}

// New creates a Tracker bound to one tenant. The poller's per-org
// loop calls this once per active org per cycle; the resulting
// Tracker handles all event-emission for that org and stamps every
// published event with the tenant via publish() below.
func New(database *sql.DB, pub Publisher, tasks db.TaskStore, entities db.EntityStore, repos db.RepositoryStore, queue db.EventQueueStore, orgID string) *Tracker {
	return &Tracker{database: database, pub: pub, tasks: tasks, entities: entities, repos: repos, queue: queue, orgID: orgID}
}

// publish stamps evt.OrgID with the tracker's configured tenant before
// forwarding to the bus so org-scoped subscribers see a tagged event.
// A pre-set evt.OrgID is left intact so future callers stamping their
// own org (carry-over, backfill in another tenant) override the
// tracker's default.
func (t *Tracker) publish(ctx context.Context, evt domain.Event) {
	if evt.OrgID == "" {
		evt.OrgID = t.orgID
	}
	t.pub.Publish(ctx, evt)
}

// emitWithSnapshotCAS commits one entity's snapshot advance under its
// poll_seq CAS together with the events that snapshot implies, in a single
// transaction. Reports ok=false when the CAS lost, in which case nothing was
// written at all.
//
// The pairing is the point. The stored snapshot is the sole re-emit
// prevention, so a snapshot that advances without its events retires them
// permanently — the next cycle diffs new-against-new and finds nothing.
// Committing both together means a failure before commit leaves the entity
// exactly where the next cycle expects it, and a CAS miss (a straggler
// ex-leader, stale by the time it lands) writes neither half.
//
// Two arms call it: a refreshed entity's diffed transitions, and a
// first-discovery seed carrying the review requests that were already on the
// PR when TF started watching.
//
// evts may be empty: a refreshed entity whose snapshot changed without a
// transition is a pure snapshot advance, and takes this same path rather
// than a second one. A refresh that observed NO change does not come here
// at all — it stamps last_polled_at alone (MarkPolledSystem), because
// advancing poll_seq for nothing would turn every terminating close still
// waiting in the queue stale.
//
// enqueued is how many of evts actually landed on the queue. It is less
// than len(evts) in exactly one case: a close obligation the queue already
// carried for this entity (the enqueue's own unsettled-row check), which is
// then neither recorded nor forwarded to the bus.
//
// Unlike the tracker's other persistence calls this takes the CYCLE's ctx,
// not context.Background(). Those keep Background because a cancellation
// mid-sequence can strand them half-applied; this one cannot — its two
// writes are one transaction, so a cancelled emit commits nothing and the
// next cycle re-diffs from the surviving snapshot. And the enqueue needs a
// real ctx to carry: the producer trace context every queue row it writes
// is stamped with comes from here.
func (t *Tracker) emitWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, evts []domain.Event) (ok bool, enqueued int, err error) {
	if len(evts) == 0 {
		ok, _, err := t.queue.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, snapshotJSON, expectedPollSeq, nil, nil)
		return ok, 0, err
	}

	// One producer span per emitted batch — the trace context every row in
	// it carries, so each event's later routing links back to the cycle
	// that found it. Started around the enqueue, not the whole diff, for
	// the same reason the ingest seam starts one around Enqueue: the link
	// has to answer "which emit was mine", and a poll cycle makes many.
	// The empty-batch path above starts none: nothing is enqueued, so
	// there is nothing to link to it.
	ctx, span := tracer.Start(ctx, "tracker.emit_batch",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(telemetry.EntityID(entityID), telemetry.OrgID(orgID), telemetry.Count(len(evts))))
	defer span.End()

	traceparents := make([]string, len(evts))
	if tp := telemetry.TraceparentFrom(ctx); tp != "" {
		for i := range traceparents {
			traceparents[i] = tp
		}
	}
	// Stamp the tenant these rows commit under — orgID, the argument the
	// enqueue binds, not the tracker's field, and unconditionally rather
	// than publish()'s "leave a pre-set OrgID intact". The two are the same
	// value today; making the copy read from the same place the write does
	// is what keeps them the same value. A bus event naming a tenant its
	// own durable row doesn't belong to is a live update no subscriber
	// could correlate.
	for i := range evts {
		evts[i].OrgID = orgID
	}

	ok, ids, err := t.queue.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, snapshotJSON, expectedPollSeq, evts, traceparents)
	if err != nil {
		span.SetStatus(codes.Error, "enqueue batch")
		return false, 0, err
	}
	if !ok {
		return false, 0, nil
	}

	// Committed. The bus fan-out below is the cosmetic half — the live WS
	// push and the scorer's idempotent nudge — and dying between the commit
	// and it costs only that: the router consumes the queue, not the bus,
	// so these events still route on the drain worker's schedule. An empty
	// id is an event the enqueue declined (an obligation already owed), and
	// nothing is forwarded for what was never recorded.
	for i, evt := range evts {
		if ids[i] == "" {
			continue
		}
		enqueued++
		evt.ID = ids[i] // the id the enqueue minted, so bus and queue agree
		t.pub.PublishPreEnqueued(ctx, evt)
	}
	return true, enqueued, nil
}

// prSnapshotTerminal is the GitHub arm's notion of a finished pull request:
// merged, or in a closed/merged state. Every write that decides an entity's
// state reads it through here so the tracker cannot disagree with itself.
func prSnapshotTerminal(snap domain.PRSnapshot) bool {
	return snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED"
}

// closeOwed decides whether a refresh owes the entity a close obligation —
// see domain.EventSystemEntityCloseOwed — and builds it. Owed when the
// snapshot was terminal on the previous cycle AND still is, on an entity
// Phase 2 listed as active, with no terminating close unsettled for it in
// the queue. The unsettled read is advisory (the enqueue re-checks on its
// own transaction); it is asked here so a cycle that would only re-owe an
// already-owed close appends nothing, and with nothing else to record
// leaves the entity's version where the close in flight was judged at. A
// read failure skips the obligation for this cycle — the next one asks
// again — rather than guessing.
//
// The event is appended to the batch the refresh commits, so it rides the
// same snapshot CAS as a real transition and carries the same version
// stamp.
func (t *Tracker) closeOwed(ctx context.Context, orgID, entityID string, prevTerminal, currTerminal bool) (domain.Event, bool) {
	if !prevTerminal || !currTerminal {
		return domain.Event{}, false
	}
	unsettled, err := t.queue.UnsettledCloseExistsSystem(ctx, orgID, entityID)
	if err != nil {
		trackerLog.ErrorContext(ctx, "close obligation: unsettled-close read failed; deferring the obligation to the next cycle", "entity_id", entityID, "error", err)
		return domain.Event{}, false
	}
	if unsettled {
		return domain.Event{}, false
	}
	return domain.Event{
		EventType: domain.EventSystemEntityCloseOwed,
		EntityID:  &entityID,
		MetadataJSON: mustJSON(events.SystemEntityCloseOwedMetadata{
			Reason: events.SystemEntityCloseOwedReasonTerminalSnapshot,
		}),
		CreatedAt: time.Now(),
	}, true
}

// commitRefresh is the tail every diffed refresh ends in: the snapshot
// advance and the transitions diffed against it, in one transaction — or,
// when the refresh observed no change and diffed nothing, a bare
// last_polled_at stamp with no version bump. The two are told apart on the
// snapshot's canonical bytes, marshalled by this process on both sides so
// jsonb's key order on the stored copy cannot make an unchanged snapshot
// look changed. Returns how many events landed on the queue; ok=false means
// the cycle's view did not commit and the caller suppresses its transitions.
//
// The quiet branch is what keeps a terminating close routable: a queue that
// falls behind the poll by more than one cycle would otherwise find every
// merged / closed / completed row judged at a version a no-op refresh has
// since moved past, and decline them all.
func (t *Tracker) commitRefresh(ctx context.Context, orgID, entityID string, prevJSON, snapJSON string, expectedPollSeq int64, evts []domain.Event) (ok bool, enqueued int, err error) {
	if len(evts) == 0 && snapJSON == prevJSON {
		if err := t.entities.MarkPolledSystem(context.Background(), orgID, entityID); err != nil {
			return false, 0, err
		}
		return true, 0, nil
	}
	return t.emitWithSnapshotCAS(ctx, orgID, entityID, snapJSON, expectedPollSeq, evts)
}

// --- GitHub ---

// RefreshGitHub runs the full tracking cycle for GitHub PRs. resolver
// answers "is this requested reviewer a TF-known identity?" per cycle —
// local mode passes a resolver built from the lone user's login + teams,
// multi mode a store-backed one keyed on the org's GitHub host. username
// remains the user-perspective axis for the merged/closed dashboard
// backfill and the self-authored-PR guard (empty in multi mode).
//
// All entity/task reads and writes are scoped to the Tracker's
// orgID (set at construction). In multi mode the poller's per-org
// loop constructs one Tracker per active org per cycle; in local
// mode there's one Tracker for the single synthetic tenant.
//
// ctx is the poll cycle's context, threaded through every GitHub API call this
// cycle makes — open-PR listing, discovery, and batch refresh.
// IMPORTANT: the root is currently context.Background() (poller.runGitHubCycle),
// which is never cancelled, so an in-flight cycle still runs to completion
// today; close(ghStop) only stops *new* cycles from starting. This threading is
// the plumbing so that when a cancellable root is eventually wired at the poll
// root, shutdown/restart can abort an in-flight cycle mid-fetch without touching
// these call sites — it is not, on its own, live cancellation. The
// entity/task-store writes below deliberately keep context.Background()
// regardless: seeding/closing/reactivating an entity is durable bookkeeping that
// must complete even once the cycle ctx becomes cancellable (a half-seeded
// create→snapshot pair would not be re-seeded, since the next cycle's
// FindOrCreate returns created=false). Threading cancellation into those
// persistence calls is a separate concern, out of scope here. The one
// exceptions are the snapshot+events commits (emitWithSnapshotCAS) — the
// discovery seed and Phase 3's diff — which take the cycle ctx precisely
// because they CAN'T half-apply; see its doc.
// The third return, resumeFrom, is the round-robin resume point — see
// discoverGitHub. It is only ever non-empty alongside a non-nil error
// (the rate-limited discovery-interruption path); every other return path
// (success, or a non-rate-limit failure in Phase 2/3 reached only once Phase
// 1 already covered every entry in repos) reports "" — a full wrap of the
// list passed in, so the poller's cursor resets rather than resuming mid-list.
//
// scope is the org's GitHub host, domain.EntityScope("github", settings): the
// namespace every pull request this cycle discovers is keyed under.
func (t *Tracker) RefreshGitHub(ctx context.Context, scope string, client *ghclient.Client, username string, repos []string, resolver ReviewerResolver) (int, string, error) {
	orgID := t.orgID
	startedAt := time.Now()
	// Phase 1: Discovery — find new PRs and register as entities.
	// quietRepos is the set of "owner/repo" whose open-PR listing returned
	// 304 (unchanged) this cycle; their tracked entities can keep their
	// stored snapshot through the Phase-2 gate without a refresh.
	discovered, quietRepos, resumeFrom, discoveryErr := t.discoverGitHub(ctx, client, username, repos)
	var rateLimited *ghclient.ErrRateLimited
	if discoveryErr != nil {
		if errors.As(discoveryErr, &rateLimited) {
			// The repo fan-out already stopped queuing new repos the moment
			// this surfaced (see discoverGitHub). Deliberately do NOT return
			// here, though: `discovered` still holds every repo that DID
			// complete before the budget ran out, and their pulls-etag was
			// already persisted (recordPullsPoll, inline per-repo) as part of
			// that success. Bailing out before the entity-seeding loop below
			// runs would leave those repos' entities/snapshots un-seeded while
			// their etag has already moved past the very PRs that needed
			// seeding — a silent, hard-to-notice loss (they'd 304 on the next
			// cycle and never get a second chance). So: let this loop process
			// `discovered` as normal, and only skip Phase 2/3 (which share
			// the same client and would just hit the same exhausted budget)
			// afterward. See the check below the entity-seeding loop.
			trackerLog.Warn("github discovery: rate limit budget exhausted, stopping repo fan-out", "resume_at", rateLimited.ResumeAt)
		} else {
			trackerLog.Log(ctx, upstream.LogLevel(discoveryErr, slog.LevelError), "github discovery error", "error", discoveryErr)
		}
	}

	// Build a SourceID-keyed lookup of discovery snapshots so Phase 2 can
	// gate refresh on (updatedAt, headSHA) without a second round-trip.
	// Discovery already returns both fields via prDiscoveryFragment; the
	// only cost here is the map allocation.
	discoveredBySourceID := make(map[string]domain.PRSnapshot, len(discovered))
	for _, d := range discovered {
		discoveredBySourceID[ghSourceID(d.Snapshot.Repo, d.Snapshot.Number)] = d.Snapshot
	}

	for _, d := range discovered {
		// Ensure the NodeID is stored in the snapshot so entity-based refresh
		// can extract it without a separate column.
		snap := d.Snapshot
		snap.NodeID = d.NodeID

		sid := ghSourceID(snap.Repo, snap.Number)
		entity, created, err := t.entities.FindOrCreateSystem(context.Background(), orgID, "github", scope, sid, "", "pr", snap.Title, snap.URL)
		if err != nil {
			trackerLog.Error("create entity failed", "source_id", sid, "error", err)
			continue
		}

		if created {
			terminal := prSnapshotTerminal(snap)
			// Backfill: a per-reviewer review_requested event for every
			// TF-known requested reviewer on a just-discovered open PR.
			// DiffPRSnapshots' "no events on initial load" rule means
			// pr:review_requested would never fire for requests that existed
			// before we started watching — the reviewer would only see them if
			// someone re-requested. Synthesizing here lands existing
			// review-requests in the queue on first connect.
			//
			// Self-authored PRs are skipped: GitHub forbids self-requests, so
			// the only way a match fires here is via a team the user is on
			// (CODEOWNERS auto-assigning them to their own PR). That isn't an
			// ask — surfacing it pollutes the queue. Matches the guard in
			// DiffPRSnapshots.
			var backfilled []domain.Event
			if !terminal && snap.Author != username {
				for _, reviewer := range snap.ReviewRequests {
					login, team, known := resolveReviewer(resolver, reviewer)
					if !known {
						continue
					}
					evt, err := backfillReviewRequestedEvent(entity.ID, snap, login, team)
					if err != nil {
						trackerLog.Error("build backfill review_requested failed", "source_id", sid, "reviewer", reviewer, "error", err)
						continue
					}
					backfilled = append(backfilled, evt)
				}
			}
			// Seed the discovery snapshot and the backfill it implies in ONE
			// transaction, CAS'd against entity.PollSeq (0 for a just-created
			// row). The stored snapshot is the sole re-emit guard, so a seed
			// that commits without its backfill retires those review requests
			// permanently: the next cycle diffs same-against-same and never
			// synthesizes them again.
			//
			// A CAS miss writes nothing and is not re-attempted — the seed
			// CAS's existing contract. What it rests on is that one org polls
			// on one pod at a time (the background-brain lease): the only
			// writer that can advance poll_seq under a seed is another cycle
			// on this same entity, so the loser is a straggler whose read is
			// stale by the time it lands rather than the only cycle that saw
			// these review requests.
			//
			// A PR that is already terminal seeds through the close-with-
			// snapshot write instead: the snapshot and the closed state land
			// in one statement, so it never sits in the active refresh set
			// with a terminal snapshot — not forever, and not for a phase.
			// Nothing is emitted for it (Phase 3 would find prev==curr), and
			// it carried no backfill either.
			snapJSON, _ := json.Marshal(snap)
			if terminal {
				if ok, err := t.entities.CloseWithSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
					trackerLog.Error("seed terminal snapshot failed", "source_id", sid, "error", err)
				} else if !ok {
					trackerLog.Warn("seed terminal snapshot CAS lost race, skipping", "source_id", sid)
				}
			} else if ok, _, err := t.emitWithSnapshotCAS(ctx, orgID, entity.ID, string(snapJSON), entity.PollSeq, backfilled); err != nil {
				trackerLog.Error("seed snapshot+backfill failed", "source_id", sid, "error", err)
			} else if !ok {
				trackerLog.Warn("seed snapshot CAS lost race, skipping", "source_id", sid)
			}
			// Best-effort display/scorer mirror, outside the transaction as in
			// Phase 3: the snapshot is the revision authority and this capped
			// string may lag it.
			if desc := prDescription(snap); desc != "" {
				if _, err := t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, desc); err != nil {
					trackerLog.Error("seed description failed", "source_id", sid, "error", err)
				}
			}
		} else {
			// Update title and description if changed.
			if entity.Title != snap.Title {
				_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, entity.ID, snap.Title)
			}
			if desc := prDescription(snap); snap.BodyHash != "" && entity.Description != desc {
				_, _ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, desc)
			}
			// A previously-closed entity reappearing open (a reopened PR)
			// reactivates with the discovery snapshot in the same statement,
			// under the poll_seq guard. The state flip and the snapshot are
			// one fact: an entity active with its old merged snapshot stored
			// is exactly what a terminating close reads as "close me", and
			// this cycle's Phase 3 already re-diffs from the snapshot written
			// here, so no transition is lost by writing it early.
			if !prSnapshotTerminal(snap) && entity.State == "closed" {
				snapJSON, _ := json.Marshal(snap)
				if reactivated, err := t.entities.ReactivateWithSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
					trackerLog.Error("reactivate entity failed", "source_id", sid, "error", err)
				} else if !reactivated {
					trackerLog.Warn("reactivate entity CAS lost race, skipping", "source_id", sid)
				} else {
					trackerLog.Info("reactivated entity (reopened)", "source_id", sid)
				}
			}
		}
	}

	if rateLimited != nil {
		// Every repo discovery managed to reach before the budget ran out is
		// now seeded above. Phase 2's GraphQL refresh shares the same client,
		// so it would immediately hit the same exhaustion — skip it and
		// propagate the typed error distinctly (errors.As-able) rather than
		// let a less-specific wrapped error surface from Phase 2, or none at
		// all if this cycle happens to have no active entities to refresh.
		// resumeFrom carries the round-robin cursor's resume point; the
		// completion sentinel deliberately does NOT fire on this path (see
		// EmitPollComplete's call sites below, both unreached from here) —
		// TFAC-571's decision to only announce "poll complete" on a full wrap
		// so downstream scoring/profiler triggers don't churn on
		// a cold-start cycle that's still partway through the repo list.
		return 0, resumeFrom, rateLimited
	}
	if discoveryErr != nil {
		// Every listing failed and the connection is why: the cycle fetched
		// nothing, so it must not go on to report a completed poll — Phase 2
		// would fail against the same upstream, and with no active entities
		// it would emit the completion sentinel without having reached
		// GitHub at all.
		return 0, "", discoveryErr
	}

	// Phase 2: Refresh active entities.
	entities, err := t.entities.ListActiveSystem(context.Background(), orgID, "github")
	if err != nil {
		return 0, "", fmt.Errorf("list active github entities: %w", err)
	}

	// Classify by snapshot state (open vs terminal) for query cost tiering.
	// Open entities also pass through the updatedAt-gate using the discovery
	// snapshot we already have in hand — quiet PRs (unchanged updatedAt and
	// SHA, no in-flight CI) skip the refresh entirely. See gate.go for the
	// safety reasoning. Terminal items always refresh because the set is
	// small and the cheap fragment is used; gate doesn't apply.
	type entityWithSnap struct {
		entity domain.Entity
		snap   domain.PRSnapshot
		nodeID string
		// seed marks a snapshot-less entity being enriched this cycle. Phase 3
		// populates it like the discovery create-branch (snapshot + title,
		// close if terminal) WITHOUT diffing, so we don't synthesize events for
		// state that predates our tracking. See resolveStubNodeID.
		//
		// Two things land here. A stub created outside the poller, which never
		// had a snapshot; and an entity whose snapshot an org admin's
		// event-source pause cleared, which is the same situation for the same
		// reason — the state it holds predates the tracking that resumed.
		seed bool
	}
	var openItems, terminalItems []entityWithSnap
	skippedOpen := 0

	for _, e := range entities {
		var snap domain.PRSnapshot
		if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
			_ = json.Unmarshal([]byte(e.SnapshotJSON), &snap)
		}
		if snap.NodeID == "" {
			// No stored snapshot, so no node_id either: a stub created outside
			// the poller (e.g. exec-touch FindOrCreate), or an
			// entity whose snapshot was cleared when an org admin paused this
			// source. Resolve the node_id via a cheap REST read and route it
			// into the refresh batch as a seed; Phase 3 enriches it quietly.
			// Unresolvable this cycle (bad shape / unreachable PR) → skip and
			// retry next cycle (one extra fetch per row until it carries a
			// node_id).
			nodeID, terminal, ok := t.resolveStubNodeID(ctx, client, e)
			if !ok {
				continue
			}
			seedItem := entityWithSnap{entity: e, nodeID: nodeID, seed: true}
			if terminal {
				terminalItems = append(terminalItems, seedItem)
			} else {
				openItems = append(openItems, seedItem)
			}
			continue
		}

		item := entityWithSnap{entity: e, snap: snap, nodeID: snap.NodeID}
		if prSnapshotTerminal(snap) {
			terminalItems = append(terminalItems, item)
			continue
		}
		// Open path: gate against discovery's fresh snapshot if we have one.
		// Entities not in this cycle's discovery (rare — e.g. a PR you've
		// stopped being a reviewer on) fall through to refresh, which is the
		// safe default. age is "time since last full refresh" — nil pointer
		// treated as very stale so first-time skip decisions force a fetch.
		var age time.Duration
		if e.LastPolledAt != nil {
			age = time.Since(*e.LastPolledAt)
		} else {
			age = 24 * time.Hour
		}
		// Gate against discovery's fresh snapshot when we have one. A repo
		// that 304'd this cycle has no fresh snapshot, but a 304 means its
		// open-PR listing (including each PR's updated_at + head sha) is
		// byte-identical to last cycle — so the stored snapshot IS the fresh
		// state for gate purposes. Feed it back as `fresh` so quiet repos
		// keep the skip optimization the REST conditional request earns.
		fresh, ok := discoveredBySourceID[e.SourceID]
		if !ok && quietRepos[snap.Repo] {
			fresh, ok = snap, true
		}
		if ok && shouldSkipRefresh(snap, fresh, age) {
			// Skipped entities won't be diffed, so reconcile stale
			// per-reviewer review_requested tasks here: for each active
			// review_requested task whose keyed reviewer is no longer in the
			// (quiet) snapshot's request list, emit a per-identity
			// review_request_removed so the router can close that one task.
			// Entities proceeding to DiffPRSnapshots emit their own removals.
			if stale, err := t.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, e.ID, domain.EventGitHubPRReviewRequested); err == nil && len(stale) > 0 {
				currReq := toSet(snap.ReviewRequests)
				for _, task := range stale {
					reviewer, ok := reviewerFromDedupKey(task.DedupKey)
					if !ok || currReq[reviewer] {
						continue // legacy/unkeyed task, or reviewer still requested
					}
					login, team := requestedIdentityFields(reviewer)
					meta, _ := json.Marshal(events.GitHubPRReviewRequestRemovedMetadata{
						Author:         snap.Author,
						Repo:           snap.Repo,
						PRNumber:       snap.Number,
						IsDraft:        snap.IsDraft,
						HeadSHA:        snap.HeadSHA,
						Labels:         snap.Labels,
						Title:          snap.Title,
						RequestedLogin: login, RequestedTeam: team,
					})
					eid := e.ID
					t.publish(ctx, domain.Event{
						EventType:    domain.EventGitHubPRReviewRequestRemoved,
						EntityID:     &eid,
						DedupKey:     task.DedupKey,
						MetadataJSON: string(meta),
						OccurredAt:   time.Now().UTC(),
					})
					trackerLog.Info("reconciled: emitting review_request_removed for skipped entity", "dedup_key", task.DedupKey, "entity", e.ID)
				}
			}
			skippedOpen++
			continue
		}
		openItems = append(openItems, item)
	}

	if len(openItems) == 0 && len(terminalItems) == 0 {
		// No-op cycle: every active entity was quiet-skipped (or there were
		// none). Debug, not Info — this is the steady-state case at the
		// default 30s+ poll cadence and carries no actionable signal;
		// liveness is reported independently via /readyz.
		trackerLog.Debug("github refresh: no-op cycle", "discovered", len(discovered), "entities", len(entities), "skipped", skippedOpen)
		if len(entities) > 0 {
			t.EmitPollComplete(ctx, "github", startedAt, len(entities), 0)
		}
		return 0, "", nil
	}

	// Fetch fresh state — open PRs get the full fragment (includes CheckRuns).
	//
	// The two calls below are the most expensive thing a poll cycle does —
	// one GraphQL round trip each regardless of batch size, so the count is
	// what explains a slow one. `full` separates them: the open batch pulls
	// check runs too, so their durations aren't comparable.
	refreshed := make(map[string]domain.PRSnapshot)
	if len(openItems) > 0 {
		nodeIDs := make([]string, len(openItems))
		for i, item := range openItems {
			nodeIDs[i] = item.nodeID
		}
		open, err := t.refreshPRBatch(ctx, client, nodeIDs, true)
		if err != nil {
			return 0, "", fmt.Errorf("refresh open PRs: %w", err)
		}
		for k, v := range open {
			refreshed[k] = v
		}
	}
	if len(terminalItems) > 0 {
		nodeIDs := make([]string, len(terminalItems))
		for i, item := range terminalItems {
			nodeIDs[i] = item.nodeID
		}
		terminal, err := t.refreshPRBatch(ctx, client, nodeIDs, false)
		if err != nil {
			return 0, "", fmt.Errorf("refresh terminal PRs: %w", err)
		}
		for k, v := range terminal {
			refreshed[k] = v
		}
	}

	// Phase 3: Diff + emit events.
	//
	// No network here — snapshot comparison plus the entity writes each
	// transition implies. A cycle slow in this phase and fast in the one
	// before it is a database problem, not a GitHub one.
	ctx, diffSpan := tracer.Start(ctx, "tracker.github.diff_emit")

	allItems := append(openItems, terminalItems...)
	eventsEmitted := 0

	for _, item := range allItems {
		newSnap, ok := refreshed[item.nodeID]
		if !ok {
			continue
		}
		// Preserve NodeID through the refresh (RefreshPRs returns map[nodeID]→snap
		// but doesn't set snap.NodeID).
		newSnap.NodeID = item.nodeID
		bodyFetched := newSnap.BodyHash != ""
		if !bodyFetched {
			newSnap.BodyHash = item.snap.BodyHash
		}

		if item.seed {
			// Quiet-seed a snapshot-less entity: populate it like the
			// discovery create-branch — snapshot + title, close if terminal — and
			// emit NOTHING. DiffPRSnapshots already suppresses non-terminal first-
			// discovery events, but seeding without diffing also keeps a terminal
			// row from emitting a merged/closed event for a PR that closed before
			// we tracked it — or, after an event-source pause, for one that merged
			// while the org had the source turned off. The next cycle diffs
			// against this seed normally.
			// A terminal seed closes the row in the same statement that
			// writes its snapshot, so the entity is never active with a
			// terminal snapshot stored, even between two phases.
			snapJSON, _ := json.Marshal(newSnap)
			var ok bool
			var err error
			if prSnapshotTerminal(newSnap) {
				ok, err = t.entities.CloseWithSnapshotCASSystem(context.Background(), orgID, item.entity.ID, string(snapJSON), item.entity.PollSeq)
			} else {
				ok, err = t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, item.entity.ID, string(snapJSON), item.entity.PollSeq)
			}
			if err != nil {
				trackerLog.ErrorContext(ctx, "seed stub snapshot failed", "source_id", item.entity.SourceID, "error", err)
			} else if !ok {
				trackerLog.WarnContext(ctx, "seed stub snapshot CAS lost race, skipping", "source_id", item.entity.SourceID)
			}
			if item.entity.Title != newSnap.Title {
				_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, item.entity.ID, newSnap.Title)
			}
			if desc := prDescription(newSnap); bodyFetched && item.entity.Description != desc {
				_, _ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, item.entity.ID, desc)
			}
			continue
		}

		// Diff against previous snapshot.
		events := DiffPRSnapshots(item.snap, newSnap, item.entity.ID, username, resolver)

		// The close obligation: this entity is active (Phase 2 lists only
		// active rows) and its snapshot was ALREADY terminal last cycle, so
		// the transition that should have closed it was emitted then and
		// lost somewhere after. The cycle that makes a snapshot terminal
		// emits the real transition above and the router closes from that;
		// only the cycle after a lost close reaches here.
		if owed, ok := t.closeOwed(ctx, orgID, item.entity.ID, prSnapshotTerminal(item.snap), prSnapshotTerminal(newSnap)); ok {
			events = append(events, owed)
		}

		// Commit the snapshot advance and the transitions diffed against it
		// together, CAS'd on item.entity.PollSeq (the value this cycle's
		// diff was read against). Neither half is durable without the
		// other: events written off a snapshot that didn't win would
		// re-derive next cycle under fresh event ids (the event/trigger
		// fence can't collapse them → duplicate tasks/runs), and a snapshot
		// that advanced without its events retires them permanently. A CAS
		// miss is a straggler ex-leader losing to the current one; an error
		// means this cycle's view didn't commit. Either way nothing was
		// written and the winning writer's next cycle re-diffs and emits
		// the transition, so suppression loses nothing. A refresh that
		// observed no change commits nothing but its poll stamp.
		prevJSON, _ := json.Marshal(item.snap)
		snapJSON, _ := json.Marshal(newSnap)
		ok, enqueued, err := t.commitRefresh(ctx, orgID, item.entity.ID, string(prevJSON), string(snapJSON), item.entity.PollSeq, events)
		if err != nil {
			trackerLog.ErrorContext(ctx, "snapshot+events commit failed; suppressing this cycle's transitions (re-diffed next cycle)", "source_id", item.entity.SourceID, "error", err)
			continue
		}
		if !ok {
			trackerLog.WarnContext(ctx, "snapshot CAS lost race (stale poll_seq); suppressing this cycle's transitions", "source_id", item.entity.SourceID)
			continue
		}
		eventsEmitted += enqueued

		// Best-effort, outside the transaction: the title and description
		// are mirrors read outside the diff (display, the scorer), so a
		// failure here costs a stale string until the next cycle, never an
		// event.
		if item.entity.Title != newSnap.Title {
			_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, item.entity.ID, newSnap.Title)
		}
		if desc := prDescription(newSnap); bodyFetched && item.entity.Description != desc {
			_, _ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, item.entity.ID, desc)
		}
	}

	diffSpan.SetAttributes(telemetry.Count(eventsEmitted))
	diffSpan.End()

	// Info only when the cycle actually produced something (an emitted
	// event) — a cycle that fetched fresh state but found no transitions is
	// routine, not noteworthy. See the no-op branch above for the same call.
	if eventsEmitted > 0 {
		trackerLog.Info("github refresh", "discovered", len(discovered), "entities", len(entities), "skipped", skippedOpen, "refreshed", len(refreshed), "events", eventsEmitted)
	} else {
		trackerLog.Debug("github refresh", "discovered", len(discovered), "entities", len(entities), "skipped", skippedOpen, "refreshed", len(refreshed), "events", eventsEmitted)
	}

	if len(entities) > 0 {
		t.EmitPollComplete(ctx, "github", startedAt, len(entities), eventsEmitted)
	}

	return eventsEmitted, "", nil
}

// refreshPRBatch is client.RefreshPRs under a span, wrapped rather than
// instrumented at both call sites so the same three facts — how many PRs,
// which fragment, how long — are stated once.
func (t *Tracker) refreshPRBatch(ctx context.Context, client *ghclient.Client, nodeIDs []string, full bool) (map[string]domain.PRSnapshot, error) {
	ctx, span := tracer.Start(ctx, "tracker.github.refresh_prs",
		trace.WithAttributes(telemetry.Count(len(nodeIDs)), attribute.Bool("full", full)))
	defer span.End()

	snaps, err := client.RefreshPRs(ctx, nodeIDs, full)
	if err != nil {
		span.SetStatus(codes.Error, "refresh prs")
	}
	return snaps, err
}

// resolveStubNodeID resolves the GitHub GraphQL node_id for a snapshot-less stub
// entity so the Phase-2 refresh can fetch its full snapshot. Stubs are created
// outside the poller (exec-touch FindOrCreate) and carry no
// node_id, so they can't ride the entity-based GraphQL refresh until we resolve
// one. The source_id is "owner/repo#N"; a cheap REST read (GetPRBasic) returns
// the node_id plus enough state to route the seed to the open (full fragment) or
// terminal (discovery fragment) batch.
//
// Returns ok=false (with a logged reason) when source_id isn't a PR target or
// the PR can't be read this cycle — the caller skips it and retries next cycle.
// One extra fetch per stub, only until it carries a node_id. Caveat: a PR that
// is permanently unreadable (deleted, or org-private after the App loses access)
// never gets a node_id, so ListActiveSystem keeps returning it and it costs one
// GetPRBasic per cycle indefinitely. Acceptable bound until a stub-staleness /
// dismiss affordance lands (separate ticket) — a 404 is deliberately NOT treated
// as terminal here, since a transient permission blip must not close a live PR.
func (t *Tracker) resolveStubNodeID(ctx context.Context, client *ghclient.Client, e domain.Entity) (nodeID string, terminal, ok bool) {
	owner, repo, number, parsed := domain.ParsePRTarget(e.SourceID)
	if !parsed {
		trackerLog.Warn("stub enrich: unparseable github source_id", "source_id", e.SourceID)
		return "", false, false
	}
	pr, err := client.GetPRBasic(ctx, owner, repo, number)
	if err != nil {
		trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "stub enrich: GetPRBasic failed", "source_id", e.SourceID, "error", err)
		return "", false, false
	}
	if pr == nil || pr.NodeID == "" {
		trackerLog.Warn("stub enrich: PR carries no node_id", "source_id", e.SourceID)
		return "", false, false
	}
	// GetPRBasic is REST, so State is lowercase "open"/"closed"; guard the
	// GraphQL forms too in case the client shape ever changes.
	terminal = pr.Merged || pr.State == "closed" || pr.State == "CLOSED" || pr.State == "MERGED"
	return pr.NodeID, terminal, true
}

// maxSearchQueryLen is GitHub's limit for the q= search parameter.
const maxSearchQueryLen = 256

// discoverGitHub finds open PRs in the configured repo set by enumeration:
// for each repo it lists open PRs via REST (GET /pulls?state=open) with a
// conditional request keyed on the stored ETag. This gives PAT and App
// installation tokens parity of mechanism — they differ only in which repos
// the token can reach — and moves discovery onto the roomier core REST budget
// (conditional 304s are free on the primary rate limit).
//
// Returns the discovered PRs and the set of "owner/repo" that returned 304
// (unchanged open set this cycle). A 304 repo's tracked entities can keep
// their stored snapshot through the Phase-2 gate.
//
// When username is non-empty (the local/PAT perspective — App tokens have no
// "me"), it additionally runs the merged/closed 30-day dashboard backfill via
// GraphQL search to seed recent-history entities the dashboard reads. That
// backfill is inherently user-perspective and stays local/PAT-only;
// multi-mode dashboard history is out of scope.
//
// The fourth return, resumeFrom, is TFAC-571's round-robin resume point: ""
// when the fan-out covered every entry in repos (a full wrap — the poller
// resets its cursor), or the name of the first repo that still needs a
// refresh (never dispatched once the fan-out stopped queuing, or dispatched
// but itself rate-limited) when ErrRateLimited cut the cycle short. It's only
// ever non-empty alongside a non-nil error.
//
// Otherwise the error is non-nil only when every listing sent failed and at
// least one failed because the connection did (see discoveryUnreached): a
// cycle that reached no repo at all reports that rather than an empty
// success. A repo GitHub answers 404 for is skipped and is never such a
// failure. A repo it refuses with a JSON 403 is skipped too, but the refusal
// is an Auth failure, so a cycle whose every repo was refused that way reports
// it: the credential reaches none of what the org tracks (an organization's
// SAML enforcement, say), which is the auth-down state the poller records.
func (t *Tracker) discoverGitHub(ctx context.Context, client *ghclient.Client, username string, repos []string) ([]ghclient.DiscoveredPR, map[string]bool, string, error) {
	seen := map[string]bool{}
	var all []ghclient.DiscoveredPR
	quiet := map[string]bool{}

	// Phase 1a: per-repo conditional open-PR enumeration, fanned out across a
	// bounded worker pool (TFAC-570) sized by repoConcurrency() — default 4,
	// clamped to [1,16] via TF_POLL_REPO_CONCURRENCY, which keeps the burst
	// well under GitHub's secondary (abuse-detection) limits while still
	// letting a large tracked set's cold-start/post-outage sync run wide
	// instead of one round-trip at a time. TF_POLL_REPO_CONCURRENCY=1
	// reproduces the pre-TFAC-570 fully serial sweep exactly.
	//
	// Parallelism is across repos only: each goroutine below does nothing but
	// the REST list + etag lookup/persist for its own repo (recordPullsPoll
	// writes a distinct repo-keyed row — no cross-repo shared state, so it
	// runs inline rather than waiting on the whole fan-out; deferring it to
	// after g.Wait() would let one slow/hanging repo expire a ctx deadline
	// out from under every OTHER repo's already-finished persist). The one
	// piece each goroutine can't touch directly is the shared seen/all/quiet
	// result — that's written to a private, index-owned slot in results (no
	// mutex needed) and merged sequentially, in original repo order, below.
	// Every entity/snapshot mutation — that merge, and all of Phase 2/3 in
	// RefreshGitHub — stays strictly sequential, so per-repo event ordering
	// and the snapshot-diff re-emit invariant are untouched regardless of
	// what order the repo fetches actually complete in.
	ctx, span := tracer.Start(ctx, "tracker.github.discover",
		trace.WithAttributes(telemetry.Count(len(repos))))
	defer span.End()

	results := make([]repoListResult, len(repos))

	var rateLimited atomic.Bool
	var rateLimitErr atomic.Pointer[ghclient.ErrRateLimited]

	g := new(errgroup.Group) // no WithContext: a per-repo failure must never cancel siblings in flight
	g.SetLimit(repoConcurrency())

	// dispatched tracks how many leading entries of repos were handed to
	// g.Go before the fan-out stopped (TFAC-571's resume-cursor needs this).
	// The dispatch loop below is single-threaded — only the per-repo bodies
	// run concurrently — so this count is exact regardless of completion
	// order or repoConcurrency().
	dispatched := 0

	for i, repoFull := range repos {
		if rateLimited.Load() {
			// Budget's known exhausted — stop queuing new repo fetches (no
			// point hammering it further). Goroutines already dispatched (up
			// to the concurrency limit) still run to completion below.
			break
		}
		dispatched = i + 1
		g.Go(func() error {
			// One span per repo, so the fan-out shows as concurrent work
			// and the repo holding the cycle up is identifiable by
			// duration. Which repo it was stays off the span — that's a
			// name, and the concurrency limit is small.
			ctx, span := tracer.Start(ctx, "tracker.github.list_prs")
			defer span.End()

			owner, name := splitOwnerRepo(repoFull)
			if owner == "" || name == "" {
				span.SetAttributes(telemetry.Outcome("unparseable"))
				return nil
			}

			etag := ""
			if t.repos != nil {
				if stored, _, err := t.repos.GetPullsPollStateByRefSystem(ctx, t.orgID, domain.RepoRef{Owner: owner, Repo: name}); err != nil {
					trackerLog.ErrorContext(ctx, "read pulls poll state failed", "repo", repoFull, "error", err)
				} else {
					etag = stored
				}
			}

			prs, newEtag, notModified, err := client.ListOpenPRs(ctx, owner, name, etag)
			if err != nil {
				// A rate-limit budget exhaustion is distinct from an ordinary
				// per-repo failure: it means every remaining fetch would fail
				// the same way, so signal the dispatch loop above to stop
				// queuing more work rather than logging N more failures.
				var rl *ghclient.ErrRateLimited
				if errors.As(err, &rl) {
					rateLimited.Store(true)
					rateLimitErr.Store(rl)
					// This repo's own fetch is what hit the rate limit — it
					// was NOT refreshed, so TFAC-571's cursor must resume
					// here (not at the next repo) next cycle.
					results[i] = repoListResult{rateLimited: true, err: err}
					// Not an error status: exhausting the budget is a
					// handled outcome with a resume cursor behind it.
					span.SetAttributes(telemetry.Outcome("rate_limited"))
					return nil
				}
				results[i] = repoListResult{err: err}
				// A 403/404 that GitHub itself answered (class Auth or
				// Rejected) means the token can't reach this configured repo
				// (a PAT user without access, or an App not installed on it)
				// — skip and log rather than failing the whole sweep. The
				// error stays on the result all the same, so a sweep in which
				// every repo was refused counts as unreached (see the doc
				// above). A 403 from something in front of GitHub (a VPN
				// proxy's HTML page) is Transient: it says nothing about this
				// repo, so it is a failed listing like any other.
				var he *ghclient.HTTPError
				if errors.As(err, &he) && (he.StatusCode == 403 || he.StatusCode == 404) &&
					(he.Class == upstream.Auth || he.Class == upstream.Rejected) {
					span.SetAttributes(telemetry.Outcome("unreachable"))
					trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "discovery: repo unreachable — skipping", "repo", repoFull, "status", he.StatusCode)
					return nil
				}
				span.SetStatus(codes.Error, "list open PRs")
				trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelError), "discovery: list open PRs failed", "repo", repoFull, "error", err)
				return nil
			}

			if notModified {
				span.SetAttributes(telemetry.Outcome("not_modified"))
				t.recordPullsPoll(ctx, repoFull, etag) // advance polled_at, keep etag
			} else {
				span.SetAttributes(telemetry.Outcome("listed"), telemetry.Count(len(prs)))
				t.recordPullsPoll(ctx, repoFull, newEtag)
			}

			results[i] = repoListResult{ok: true, prs: prs, notModified: notModified}
			return nil
		})
	}
	_ = g.Wait() // every goroutine above always returns nil; failures are carried via results/rateLimited instead

	for i, repoFull := range repos {
		r := results[i]
		if !r.ok {
			continue
		}
		if r.notModified {
			quiet[repoFull] = true
			continue
		}

		for _, pr := range r.prs {
			sid := ghSourceID(pr.Snapshot.Repo, pr.Snapshot.Number)
			if !seen[sid] {
				seen[sid] = true
				all = append(all, pr)
			}
		}
	}

	var discoveryErr error
	resumeFrom := ""
	if rl := rateLimitErr.Load(); rl != nil {
		discoveryErr = rl
		// TFAC-571: find the earliest repo (in the caller's list order —
		// already rotated to the org's round-robin cursor by the poller)
		// that still needs a refresh: either it was never dispatched once
		// the fan-out stopped queuing, or it was dispatched but its own
		// fetch is what hit the rate limit. Repos before that point either
		// succeeded or were permanently skipped (403/404) — both are
		// "handled" for cursor purposes and don't need an immediate retry.
		for i := range repos {
			if i >= dispatched || results[i].rateLimited {
				resumeFrom = repos[i]
				break
			}
		}
	} else {
		listings := make([]error, 0, dispatched)
		for _, r := range results[:dispatched] {
			if r.ok || r.err != nil {
				listings = append(listings, r.err)
			}
		}
		discoveryErr = discoveryUnreached("github", listings)
	}

	// Phase 1b: merged/closed dashboard backfill (local/PAT-only). Seeds
	// recent-history entities via user-perspective GraphQL search. App tokens
	// have no "me", so this is skipped when username is empty; multi-mode
	// instead backfills per bound user via Tracker.BackfillDashboardHistory.
	// Query construction is shared with that path (dashboardBackfillQueries) so
	// both search for exactly the same history. Also skipped once Phase 1a hit
	// ErrRateLimited, or reached no repo at all — it shares the same client and
	// the same host, so it would just fail the same way for no benefit.
	if username != "" && discoveryErr == nil {
		for _, q := range dashboardBackfillQueries(username, repos) {
			prs, err := client.DiscoverPRs(ctx, q, 50)
			if err != nil {
				trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelError), "dashboard backfill query failed", "error", err, "query", q)
				continue
			}
			for _, pr := range prs {
				sid := ghSourceID(pr.Snapshot.Repo, pr.Snapshot.Number)
				if !seen[sid] {
					seen[sid] = true
					all = append(all, pr)
				}
			}
		}
	}

	switch {
	case rateLimitErr.Load() != nil:
		// ErrRateLimited is a handled outcome with a resume cursor behind it
		// — an attribute, not an error status, same as the per-repo children
		// above.
		span.SetAttributes(telemetry.Outcome("rate_limited"))
	case discoveryErr != nil:
		span.SetStatus(codes.Error, "every listing failed")
		span.SetAttributes(telemetry.Outcome("failed"))
	}
	return all, quiet, resumeFrom, discoveryErr
}

// discoveryUnreached reports a discovery pass that reached nothing because of
// the connection: errs holds one entry per discovery call sent, nil for a call
// that succeeded. It returns an error when every call failed and at least one
// failure is an upstream Transient or Auth (upstream.ClassOf), wrapping that
// failure so a caller can still read its class; an Auth failure is preferred,
// as the one a person has to fix. It returns nil when any call succeeded, when
// none was sent, and when every failure was an answer about its own request (a
// 404), which says nothing about the connection.
func discoveryUnreached(source string, errs []error) error {
	var cause error
	for _, err := range errs {
		if err == nil {
			return nil
		}
		switch class, _ := upstream.ClassOf(err); class {
		case upstream.Auth:
			if c, _ := upstream.ClassOf(cause); c != upstream.Auth {
				cause = err
			}
		case upstream.Transient:
			if cause == nil {
				cause = err
			}
		}
	}
	if cause == nil {
		return nil
	}
	return fmt.Errorf("%s discovery: all %d calls failed: %w", source, len(errs), cause)
}

// discoveryRateLimited reports a discovery pass that fetched nothing because
// the upstream asked it to wait: no call succeeded and at least one was
// rate-limited. It is checked after discoveryUnreached, which owns the passes
// the connection failed. The returned error wraps the rate limit, so a caller
// reads its class and does not count the pass as a completed poll; the
// connection state is untouched, since a rate limit says nothing about it.
func discoveryRateLimited(source string, errs []error) error {
	var cause error
	for _, err := range errs {
		if err == nil {
			return nil
		}
		if class, _ := upstream.ClassOf(err); class == upstream.RateLimited && cause == nil {
			cause = err
		}
	}
	if cause == nil {
		return nil
	}
	return fmt.Errorf("%s discovery: all %d calls failed: %w", source, len(errs), cause)
}

// repoListResult is one goroutine's outcome from Phase 1a's per-repo
// open-PR listing — everything needed to merge into the shared seen/all/quiet
// result, deferred to the sequential merge so that merge can run in original
// repo order regardless of completion order. Per-repo side effects that
// don't need that ordering (recordPullsPoll's etag persist) happen inline in
// the goroutine instead — see the comment above the fan-out loop. Each index
// in the results slice is owned by exactly one goroutine (index i writes
// only results[i]), so concurrent writers never touch shared memory. ok is
// false for a repo that was skipped (malformed slug, 403/404-unreachable,
// rate-limited, or any other per-repo failure) and whose zero value should
// be ignored by the merge. rateLimited (TFAC-571) narrows that further for
// the resume-cursor computation: true only when THIS repo's own fetch is
// what returned ErrRateLimited — as opposed to a 403/404 (permanently
// unreachable, no retry needed) or a generic per-repo error (retried
// naturally on this repo's next turn in the rotation) — so the cursor
// resumes exactly at the repo that still needs a refresh, not the one after.
type repoListResult struct {
	ok          bool
	prs         []ghclient.DiscoveredPR
	notModified bool
	rateLimited bool
	// err is the listing's failure; nil for a listing that succeeded and for
	// a repo that was never sent (a malformed slug).
	err error
}

// recordPullsPoll persists the conditional-request state for a repo after a
// successful list (200 or 304). Best-effort — a write failure just means the
// next cycle re-lists unconditionally, costing one primary-limit request.
func (t *Tracker) recordPullsPoll(ctx context.Context, repoFull, etag string) {
	if t.repos == nil {
		return
	}
	// Ref-keyed: repoFull is one of the names ListTrackedNamesSystem handed
	// this cycle, the same one that just went into the request path.
	if err := t.repos.SetPullsPollStateByRefSystem(ctx, t.orgID, domain.RepoRefFromSlug(repoFull), etag, time.Now().UTC()); err != nil {
		trackerLog.Error("write pulls poll state failed", "repo", repoFull, "error", err)
	}
}

// splitOwnerRepo splits an "owner/repo" slug at the first slash. Returns
// empty halves for a malformed entry (no slash), which the caller skips.
func splitOwnerRepo(s string) (owner, repo string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// backfillReviewRequestedEvent builds a synthesized pr:review_requested
// event for a PR being discovered for the first time with a TF-known
// identity already in its requested-reviewer list. The caller commits it
// inside the seed's snapshot-CAS transaction; the router then evaluates
// rules off the queue row and fans out to per-team tasks, and the task's
// primary_event_id FK is satisfied by the events row the same transaction
// wrote.
//
// Returning the event rather than publishing it is what lets the seed carry
// it: the stored snapshot is the sole re-emit guard, so an event published
// after the snapshot committed is one the next cycle can no longer derive.
//
// The OccurredAt stamp uses the PR's CreatedAt as a lower bound:
// GitHub doesn't expose per-review-request timestamps, so PR creation
// time is the closest we have — better than "just now" on the card
// for a PR that's been pending your review for weeks. Falls back to
// the zero value (detection time) if the GraphQL timestamp is missing
// or unparseable.
//
// The "is this reviewer TF-known" decision happens upstream at the
// caller's resolveReviewer check, not here; this function just records the
// requested identity (login or "org/slug" team) plus the PR author on the
// metadata, and keys the event by that identity, so the router routes the
// per-reviewer task and the predicate matcher can do its work.
func backfillReviewRequestedEvent(entityID string, snap domain.PRSnapshot, requestedLogin, requestedTeam string) (domain.Event, error) {
	reviewer := requestedLogin
	if reviewer == "" {
		reviewer = requestedTeam
	}
	meta := events.GitHubPRReviewRequestedMetadata{
		Author:         snap.Author,
		Repo:           snap.Repo,
		PRNumber:       snap.Number,
		IsDraft:        snap.IsDraft,
		HeadSHA:        snap.HeadSHA,
		Labels:         snap.Labels,
		Title:          snap.Title,
		RequestedLogin: requestedLogin, RequestedTeam: requestedTeam,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return domain.Event{}, err
	}
	// Parse through the shared external-time parser (handles RFC3339Nano
	// sub-second shapes + Jira offsets) so a fractional-seconds CreatedAt
	// doesn't silently degrade the backfilled task's queue order to "now".
	occurredAt := time.Time{}
	if parsed, ok := domain.ParseExternalTime(snap.CreatedAt); ok {
		occurredAt = parsed
	}
	eid := entityID
	return domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRReviewRequested,
		DedupKey:     reviewerDedupKey(reviewer),
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurredAt,
	}, nil
}

// --- Jira ---

// JiraProjectRules is the tracker-local per-project view of the user's
// Jira status configuration. Mirrors the slice from config.JiraConfig
// but kept independent so the tracker doesn't depend on internal/config
// — call sites in the poller manager convert at the boundary.
type JiraProjectRules struct {
	Key string
	// The status sets discovery queries on. They are refs — id plus the display
	// name captured with it — because the two uses want different halves: the
	// JQL is built from ids, which survive a rename in Jira, while classifying
	// an issue the query returned compares against the name its snapshot
	// records.
	PickupMembers []domain.JiraStatusRef
	DoneMembers   []domain.JiraStatusRef
}

// JiraRules is a slice of per-project rules with lookup helpers.
type JiraRules []JiraProjectRules

// ForKey returns the rules for the given project key, or nil when no
// matching project is configured. Callers should degrade gracefully on
// a nil return — typically by treating the event as "no rules
// configured" (no terminal check, log a warning).
func (r JiraRules) ForKey(key string) *JiraProjectRules {
	for i := range r {
		if r[i].Key == key {
			return &r[i]
		}
	}
	return nil
}

// AllDoneMembers returns the deduplicated union of every project's DoneMembers.
// Useful for subtask classification when the parent and subtasks may live in
// different projects — a subtask arrives inlined in the search response, and it
// carries the same status object the parent does, id included.
func (r JiraRules) AllDoneMembers() []domain.JiraStatusRef {
	seen := map[string]bool{}
	out := make([]domain.JiraStatusRef, 0)
	for _, p := range r {
		for _, ref := range p.DoneMembers {
			key := domain.JiraStatusDedupKey(ref)
			if key != "" && !seen[key] {
				seen[key] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

// doneMembersForKey resolves the Done.Members for an issue key by
// looking up the project. Returns nil when the project isn't in the
// configured rule set — typically because the user removed the project
// from Settings while its entities are still active in the DB (entities
// aren't auto-deleted on settings change). Nil matches jiraCycle.terminal
// (which returns false on unknown project) so discovery's "should I mark
// this entity closed" and the diff layer's "did this transition complete
// it" stay consistent.
//
// An earlier version fell back to the union of every configured
// project's done members, but that misclassifies entities from removed
// projects whose status happens to coincide with another project's
// "done" word (e.g. OLD-1 transitioning to "Resolved" when NEW project
// also uses "Resolved" as Done) — emits a spurious jira:issue:completed
// for an entity whose actual workflow has nothing to do with NEW's.
func (r JiraRules) doneMembersForKey(issueKey string) []domain.JiraStatusRef {
	if rule := r.ForKey(extractProject(issueKey)); rule != nil {
		return rule.DoneMembers
	}
	return nil
}

// jiraCycle is what one RefreshJira pass threads through its phases.
type jiraCycle struct {
	// scope is the org's Jira site, domain.EntityScope("jira", settings): the
	// namespace every issue this cycle reads is keyed under.
	scope string
	// baseURL is what the issues' links are built from.
	baseURL  string
	projects JiraRules
	// merged holds rows a merge folded into another row this cycle. They no
	// longer exist, so the rest of the cycle skips them.
	merged map[string]bool
}

// terminal reports whether snap sits in one of its project's done statuses.
// An issue in a project no rule configures is never terminal.
func (c *jiraCycle) terminal(snap domain.JiraSnapshot) bool {
	rule := c.projects.ForKey(extractProject(snap.Key))
	if rule == nil {
		return false
	}
	return domain.ContainsStatus(rule.DoneMembers, snap.StatusRef())
}

// RefreshJira runs the full tracking cycle for Jira issues: the retirement of
// rows from another site, discovery per configured project, a batched refresh
// of every active Jira entity on the site, the diff, the confirmation of the
// issues the refresh did not answer for, and the poll-complete sentinel.
// projects is the team's full per-project rule set; the tracker dispatches
// discovery JQL per project and looks up terminal-status sets by the issue's
// project_key. Tickets whose project_key has no row degrade silently — no
// terminal check, no pickup discovery.
//
// Actor identity flows through the snapshot (assignee_account_id) and
// predicate matching happens downstream against the assignee_in /
// reporter_in / commenter_in allowlists.
//
// scope is the org's Jira site, domain.EntityScope("jira", settings). An
// issue's entity is matched on its numeric id (external_id) on that site,
// never on its key once the id is known: the key is a display key a project
// move or a project key rename changes, and when a fresh read answers under a
// new one the entity is renamed to it in the same cycle. A row written before
// ids were recorded learns its id from the first response that names it.
// baseURL is what the issues' links are built from.
//
// All entity reads/writes are scoped to the Tracker's orgID (set at
// construction). In multi mode the poller's per-org loop constructs
// one Tracker per active org per cycle; in local mode there's one
// Tracker for the single synthetic tenant.
func (t *Tracker) RefreshJira(ctx context.Context, scope string, client *jiraclient.Client, baseURL string, projects JiraRules) (int, error) {
	orgID := t.orgID
	startedAt := time.Now()
	if scope == "" {
		return 0, errors.New("jira refresh: no site to key issues under")
	}
	c := &jiraCycle{scope: scope, baseURL: baseURL, projects: projects, merged: map[string]bool{}}

	// Phase 0: rows a previous site left active. Retired first, without asking
	// Jira, so nothing below can mistake one for this site's.
	retired := t.RetireJiraOutOfScope(ctx, scope)

	// Phase 1: Discovery
	// A discovery error is returned once the entities it did discover are
	// seeded, and the poller logs it with the org, so it is not logged here.
	discovered, discoveryErr := t.discoverJira(ctx, client, baseURL, projects)
	discoveryEventsEmitted := 0
	for _, state := range discovered {
		discoveryEventsEmitted += t.seedDiscoveredJira(ctx, c, state)
	}

	if discoveryErr != nil {
		// Every discovery query failed, on the connection or a rate limit: the
		// cycle fetched nothing, so it must not go on to report a completed poll.
		// With no active entities, Phase 2 would emit the completion sentinel,
		// which marks Jira ready without Jira having answered once.
		return 0, discoveryErr
	}

	// Phase 2: Refresh
	listed, err := t.entities.ListActiveSystem(context.Background(), orgID, "jira")
	if err != nil {
		return 0, fmt.Errorf("list active jira entities: %w", err)
	}
	entities := make([]domain.Entity, 0, len(listed))
	for _, e := range listed {
		// A row from another site was retired in Phase 0; the router closes it.
		if e.Scope == scope && !c.merged[e.ID] {
			entities = append(entities, e)
		}
	}
	if len(entities) == 0 {
		// No entities to refresh, but still emit poll-complete so carry-over
		// readiness flips true on fresh-setup / empty-project cases.
		eventsEmitted := retired + discoveryEventsEmitted
		t.EmitPollComplete(ctx, "jira", startedAt, 0, eventsEmitted)
		return eventsEmitted, nil
	}

	refreshed, err := t.batchFetchJira(ctx, client, baseURL, entities, projects)
	if err != nil {
		return 0, fmt.Errorf("batch fetch jira: %w", err)
	}

	// Phase 3: Diff + emit events. Network-free, like the GitHub twin.
	ctx, diffSpan := tracer.Start(ctx, "tracker.jira.diff_emit")

	diffEventsEmitted := 0
	staleReads := 0
	for _, e := range entities {
		newState, ok := refreshed[e.ID]
		if !ok || c.merged[e.ID] {
			continue
		}
		emitted, stale := t.applyJiraIssue(ctx, c, e, newState)
		diffEventsEmitted += emitted
		if stale {
			staleReads++
		}
	}

	diffSpan.SetAttributes(telemetry.Count(diffEventsEmitted))
	if staleReads > 0 {
		// A disposition rather than an error status: the cycle worked, it
		// declined to act on part of its input. Without it a cycle that
		// suppressed everything is indistinguishable from a quiet one.
		diffSpan.SetAttributes(telemetry.Disposition("stale_read_suppressed"))
	}
	diffSpan.End()
	eventsEmitted := retired + discoveryEventsEmitted + diffEventsEmitted

	// Phase 4: confirm the long-unanswered issues against the issue endpoint.
	// Emits unreachable events, which the router turns into entity/task
	// closes. Ahead of the cycle log so its event count is the whole cycle's
	// — the same number the poll-complete sentinel carries, rather than a
	// second, quietly smaller one for the same cycle.
	confirmedRetired := t.confirmMissingJiraEntities(ctx, client, c, entities, refreshed, time.Now())
	eventsEmitted += confirmedRetired

	trackerLog.InfoContext(ctx, "jira refresh", "discovered", len(discovered), "entities", len(entities), "refreshed", len(refreshed), "events", eventsEmitted, "retired", retired+confirmedRetired, "stale_reads", staleReads)

	// Always fire the sentinel — it means "a poll cycle completed," not "a
	// poll produced work." Carry-over readiness depends on this firing even
	// on an empty first poll (e.g. projects configured but nothing assigned
	// yet), otherwise the setup step shimmers forever.
	t.EmitPollComplete(ctx, "jira", startedAt, len(entities), eventsEmitted)

	return eventsEmitted, nil
}

// RetireJiraOutOfScope retires every active Jira entity keyed under a site
// other than scope, emitting unreachable with reason scope_changed for each,
// and returns how many it emitted. It runs at the top of every cycle, and on
// its own when the org's Jira is configured but has no project to poll.
//
// Jira is not asked: issue ids and keys repeat across sites, so nothing the
// new site answered would be about these issues. The rows are not moved
// either. They stay where they are, closed with their history, and pointing
// the org at the old site again finds them by id.
func (t *Tracker) RetireJiraOutOfScope(ctx context.Context, scope string) int {
	if scope == "" {
		return 0
	}
	entities, err := t.entities.ListActiveSystem(context.Background(), t.orgID, "jira")
	if err != nil {
		trackerLog.ErrorContext(ctx, "list active jira entities for the site check failed", "error", err)
		return 0
	}
	retired := 0
	for _, e := range entities {
		if e.Scope == scope {
			continue
		}
		t.emitJiraUnreachable(ctx, t.orgID, e, nil, events.JiraUnreachableScopeChanged, "")
		retired++
	}
	return retired
}

// seedDiscoveredJira records one issue discovery found and returns the events
// it enqueued. The issue's entity is the one carrying its id on the site,
// under whatever key it was stored. Only when none does is it looked up by the
// key the issue has now: a row there with no id is this issue's from before
// ids were recorded, and learns it; a row there with another id is another
// issue, and is skipped. Only when neither finds one is an entity created.
func (t *Tracker) seedDiscoveredJira(ctx context.Context, c *jiraCycle, state jiraIssueState) int {
	orgID := t.orgID
	snap := state.Snap

	var entity *domain.Entity
	if snap.ID != "" {
		var err error
		entity, err = t.entities.GetByExternalIDSystem(context.Background(), orgID, "jira", c.scope, snap.ID)
		if err != nil {
			trackerLog.Error("look up jira entity by issue id failed", "source_id", snap.Key, "issue_id", snap.ID, "error", err)
			return 0
		}
	}
	created := false
	if entity == nil {
		var err error
		entity, created, err = t.entities.FindOrCreateSystem(context.Background(), orgID, "jira", c.scope, snap.Key, snap.ID, "issue", snap.Summary, snap.URL)
		if errors.Is(err, db.ErrEntityKeyOccupied) {
			// Another tracked issue still holds the key: a project deleted and
			// recreated under the same key while the old issue's entity is still
			// active. That entity retires once Jira confirms its issue is gone,
			// and the next cycle creates this one.
			trackerLog.Warn("jira key still held by another tracked issue; skipping it this cycle",
				"source_id", snap.Key, "issue_id", snap.ID)
			return 0
		}
		if err != nil {
			trackerLog.Error("create entity failed", "source_id", snap.Key, "error", err)
			return 0
		}
		if !created && snap.ID != "" && entity.ExternalID == "" {
			learned, ok := t.learnJiraIssueID(ctx, c, *entity, snap.ID)
			if !ok {
				return 0
			}
			entity = &learned
		}
	}
	if created {
		return t.seedCreatedJira(ctx, c, *entity, state)
	}

	// A read the search index served stale must not move the entity back onto
	// a key it has already left, nor reopen it.
	if prev, ok := storedJiraSnapshot(*entity); ok {
		if _, _, stale := jiraReadIsStale(prev, snap); stale {
			return 0
		}
	}
	// A known issue under a new key is renamed before anything else is written
	// to it; the refresh below diffs the move and emits it.
	if jiraRenamed(c.scope, *entity, snap) {
		renamed, ok := t.renameJiraEntity(ctx, c, *entity, snap.Key)
		if !ok {
			return 0
		}
		entity = &renamed
	}
	t.mirrorJiraText(orgID, *entity, state)
	// A previously-closed issue reappearing open reactivates with the
	// discovery snapshot in the same statement, under the poll_seq guard —
	// the GitHub arm's reasoning: state and snapshot are one fact, and this
	// cycle's Phase 3 re-diffs from what is written here.
	if !c.terminal(snap) && entity.State == "closed" {
		snapJSON, _ := json.Marshal(jiraReactivationSnapshot(*entity, snap))
		if reactivated, err := t.entities.ReactivateWithSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
			trackerLog.Error("reactivate entity failed", "source_id", snap.Key, "error", err)
		} else if !reactivated {
			trackerLog.Warn("reactivate entity CAS lost race, skipping", "source_id", snap.Key)
		} else {
			trackerLog.Info("reactivated entity (reopened)", "source_id", snap.Key)
		}
	}
	return 0
}

// seedCreatedJira writes the first snapshot of an entity discovery just
// created, and returns the events it enqueued.
func (t *Tracker) seedCreatedJira(ctx context.Context, c *jiraCycle, entity domain.Entity, state jiraIssueState) int {
	orgID := t.orgID
	snap := state.Snap
	emitted := 0
	snapJSON, _ := json.Marshal(snap)
	switch {
	case c.terminal(snap):
		// Already done when first seen: the snapshot and the closed
		// state land in one statement, so the row is never active
		// with a terminal snapshot stored. Nothing is emitted — the
		// issue finished before TF tracked it. Discovery excludes
		// terminal statuses, so this is the rare issue whose done
		// set the query's exclusion did not cover.
		if ok, err := t.entities.CloseWithSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
			trackerLog.Error("seed terminal jira snapshot failed", "source_id", snap.Key, "error", err)
		} else if !ok {
			trackerLog.Warn("seed terminal jira snapshot CAS lost race, skipping", "source_id", snap.Key)
		}
	case state.DiscoveredAssignedToCurrentUser:
		// An issue assigned to someone else is outside both
		// discovery queries, so appearing in the assigned-to-current-user
		// result can itself be the assignment transition. Commit that initial
		// event with the first snapshot; seeding first would make Phase 3
		// diff current-against-current and retire the transition unseen.
		evts := DiffJiraSnapshots(domain.JiraSnapshot{}, snap, entity.ID, c.projects.doneMembersForKey(snap.Key))
		if ok, enqueued, err := t.emitWithSnapshotCAS(ctx, orgID, entity.ID, string(snapJSON), entity.PollSeq, evts); err != nil {
			trackerLog.Error("seed assigned jira snapshot+event failed", "source_id", snap.Key, "error", err)
		} else if !ok {
			trackerLog.Warn("seed assigned jira snapshot CAS lost race, skipping", "source_id", snap.Key)
		} else {
			emitted = enqueued
		}
	default:
		if ok, err := t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
			trackerLog.Error("seed snapshot failed", "source_id", snap.Key, "error", err)
		} else if !ok {
			trackerLog.Warn("seed snapshot CAS lost race, skipping", "source_id", snap.Key)
		}
	}
	if state.Description != "" {
		if _, err := t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, state.Description); err != nil {
			trackerLog.Error("seed description failed", "source_id", snap.Key, "error", err)
		}
	}
	return emitted
}

// jiraReactivationSnapshot is the snapshot a closed entity reactivates with:
// the fresh one, except that when the issue's key has changed since the stored
// snapshot was taken, the stored key and project are kept. The Phase 2 diff
// from it then still sees the change and emits key_changed, in the same commit
// as the fresh snapshot; reactivating with the fresh key would leave that diff
// nothing to compare. An entity with no stored snapshot reactivates with the
// fresh one, as a quiet seed would.
func jiraReactivationSnapshot(e domain.Entity, fresh domain.JiraSnapshot) domain.JiraSnapshot {
	stored, ok := storedJiraSnapshot(e)
	if !ok || stored.Key == "" || stored.Key == fresh.Key {
		return fresh
	}
	fresh.Key, fresh.ProjectID = stored.Key, stored.ProjectID
	return fresh
}

// storedJiraSnapshot parses an entity's stored snapshot. ok=false when it has
// none, or one that does not parse.
func storedJiraSnapshot(e domain.Entity) (domain.JiraSnapshot, bool) {
	var snap domain.JiraSnapshot
	if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
		return snap, false
	}
	if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
		return domain.JiraSnapshot{}, false
	}
	return snap, true
}

// jiraRenamed reports whether an issue answers under a key other than the one
// its entity is stored under: the rename condition, decided on the issue id
// and never on the key alone.
func jiraRenamed(scope string, e domain.Entity, snap domain.JiraSnapshot) bool {
	return len(domain.DetectEntityRenames(
		[]domain.EntityRef{{Source: "jira", Scope: scope, SourceID: e.SourceID, ExternalID: e.ExternalID}},
		[]domain.EntityRef{{Source: "jira", Scope: scope, SourceID: snap.Key, ExternalID: snap.ID}},
	)) > 0
}

// jiraMoved reports whether an issue left the project prev records it in. The
// project id decides when both snapshots carry one, so a project key rename —
// which changes every key's prefix and keeps the project — is not a move; a
// snapshot captured before project ids were recorded falls back to the key's
// prefix.
func jiraMoved(prev, curr domain.JiraSnapshot) bool {
	if prev.ProjectID != "" && curr.ProjectID != "" {
		return prev.ProjectID != curr.ProjectID
	}
	return extractProject(prev.Key) != extractProject(curr.Key)
}

// renameJiraEntity moves an entity onto the key its issue answers under now,
// with the link built from that key, and returns the entity as renamed.
// ok=false means the rename did not land and the caller writes nothing to the
// entity this cycle: an active row still holds the key (it clears once that
// row is renamed or retired), or the write failed.
//
// The returned entity carries the poll_seq the rename bumped to, so the
// caller's snapshot CAS lands on its own rename. When the row already had the
// key, someone else renamed it after this cycle read it — an agent's read of
// the issue, say — and the entity keeps the poll_seq this cycle read, so its
// CAS misses and the next cycle diffs from what is stored.
func (t *Tracker) renameJiraEntity(ctx context.Context, c *jiraCycle, e domain.Entity, key string) (domain.Entity, bool) {
	url := domain.JiraIssueURL(c.baseURL, key)
	out, err := t.entities.RenameSystem(context.Background(), t.orgID, "jira", c.scope, e.ExternalID, key, url)
	if errors.Is(err, db.ErrEntityKeyOccupied) {
		trackerLog.WarnContext(ctx, "jira issue moved onto a key another tracked issue still holds; renaming it next cycle",
			"source_id", e.SourceID, "key", key, "entity_id", e.ID)
		return e, false
	}
	if err != nil {
		trackerLog.ErrorContext(ctx, "rename jira entity failed", "source_id", e.SourceID, "key", key, "entity_id", e.ID, "error", err)
		return e, false
	}
	if out.Renamed {
		trackerLog.InfoContext(ctx, "jira issue answers under a new key; entity renamed",
			"from", out.From, "to", out.To, "entity_id", e.ID)
		e.PollSeq = out.PollSeq
	}
	e.SourceID, e.URL = key, url
	return e, true
}

// learnJiraIssueID writes the issue id a response named onto an entity
// created before ids were recorded, and returns the entity that carries it
// afterwards. ok=false means nothing was learned and the caller leaves the
// entity alone this cycle.
//
// When another row already carries the id, the two rows are one issue — left
// split by a move nothing followed — and they are merged, the older row
// surviving (db.EntityStore.MergeDuplicateEntitiesSystem). The survivor is
// returned, and the merged-away row is skipped by the rest of the cycle.
func (t *Tracker) learnJiraIssueID(ctx context.Context, c *jiraCycle, e domain.Entity, issueID string) (domain.Entity, bool) {
	orgID := t.orgID
	stamped, err := t.entities.StampExternalIDSystem(context.Background(), orgID, e.ID, issueID)
	switch {
	case errors.Is(err, db.ErrEntityIdentityAmbiguous):
		holder, herr := t.entities.GetByExternalIDSystem(context.Background(), orgID, "jira", c.scope, issueID)
		if herr != nil || holder == nil {
			trackerLog.ErrorContext(ctx, "jira issue id is carried by another entity that cannot be read; leaving both",
				"source_id", e.SourceID, "issue_id", issueID, "entity_id", e.ID, "error", herr)
			return e, false
		}
		survivorID, merr := t.entities.MergeDuplicateEntitiesSystem(context.Background(), orgID, e.ID, holder.ID)
		if merr != nil {
			trackerLog.ErrorContext(ctx, "merging two entities for one jira issue failed; leaving both",
				"issue_id", issueID, "entity_id", e.ID, "other_entity_id", holder.ID, "error", merr)
			return e, false
		}
		for _, id := range []string{e.ID, holder.ID} {
			if id != survivorID {
				c.merged[id] = true
			}
		}
		survivor, serr := t.entities.GetSystem(context.Background(), orgID, survivorID)
		if serr != nil || survivor == nil {
			trackerLog.ErrorContext(ctx, "reading the surviving jira entity after a merge failed", "entity_id", survivorID, "error", serr)
			return e, false
		}
		trackerLog.InfoContext(ctx, "two entities were one jira issue; merged into the older",
			"issue_id", issueID, "survivor_entity_id", survivorID, "source_id", survivor.SourceID)
		return *survivor, true
	case err != nil:
		trackerLog.ErrorContext(ctx, "learning a jira entity's issue id failed", "source_id", e.SourceID, "issue_id", issueID, "entity_id", e.ID, "error", err)
		return e, false
	case stamped == nil:
		// The row carries another id: it is another issue, which a key lookup
		// or a response under its key should not have reached.
		trackerLog.WarnContext(ctx, "jira entity already carries another issue id; not relearning it",
			"source_id", e.SourceID, "issue_id", issueID, "entity_id", e.ID)
		return e, false
	}
	return *stamped, true
}

// applyJiraIssue applies one fresh read of an entity's issue and returns the
// events it enqueued, and whether the read was dropped as stale.
//
// An entity created before issue ids were recorded learns its id here first.
// An issue that answers under a new key — moved to another project, or its
// project's key renamed — is the same issue: the entity is renamed, before
// anything decides whether it can still be tracked, and the diff that follows
// emits key_changed ahead of everything else it finds. A move into a project
// no rule configures retires the entity instead: no later diff would report
// the change, so key_changed is published here, ahead of unreachable with
// reason moved, which names the project the issue left. A move between
// configured projects keeps the entity.
//
// key_changed is always the difference from the stored snapshot, so an entity
// with no snapshot (one a source pause cleared) is renamed without one, as it
// is seeded without every other event.
func (t *Tracker) applyJiraIssue(ctx context.Context, c *jiraCycle, e domain.Entity, state jiraIssueState) (emitted int, stale bool) {
	orgID := t.orgID
	newSnap := state.Snap

	if e.ExternalID == "" && newSnap.ID != "" {
		learned, ok := t.learnJiraIssueID(ctx, c, e, newSnap.ID)
		if !ok {
			return 0, false
		}
		e = learned
	}

	var prev *domain.JiraSnapshot
	if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
		var p domain.JiraSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &p); err != nil {
			trackerLog.Warn("corrupt jira snapshot, reseeding", "source_id", e.SourceID, "error", err)
		} else {
			prev = &p
		}
	}

	// Drop a read that predates what we already hold, before it can reach
	// the rename, the diff or the snapshot write. Jira only ever moves
	// `updated` forward, so a backwards read is the search index serving
	// state we have already superseded — never news, and never a reason to
	// move the entity back onto a key it has left. Warn rather than Debug
	// because that claim is the whole justification for suppressing: if a
	// read that WAS news ever gets dropped here, this line is the bug report.
	if prev != nil {
		if storedAt, fetchedAt, isStale := jiraReadIsStale(*prev, newSnap); isStale {
			trackerLog.WarnContext(ctx, "jira read predates stored snapshot; suppressing this cycle's diff and snapshot write",
				"source_id", e.SourceID, "entity_id", e.ID,
				"stored_updated", storedAt.Format(time.RFC3339Nano),
				"fetched_updated", fetchedAt.Format(time.RFC3339Nano))
			return 0, true
		}
	}

	oldKey := e.SourceID
	renamed := false
	if jiraRenamed(c.scope, e, newSnap) {
		next, ok := t.renameJiraEntity(ctx, c, e, newSnap.Key)
		if !ok {
			return 0, false
		}
		e, renamed = next, true
	}

	// Whether the issue left the project it was tracked in comes from the
	// snapshot, not from this call's rename: a cycle that renamed the entity
	// and stopped before retiring it is finished by the next one, and a
	// project key rename keeps the project, so it is not a move. With no
	// snapshot, this call's rename is the only evidence.
	moved := renamed
	leftProject := extractProject(oldKey)
	if prev != nil {
		moved = jiraMoved(*prev, newSnap)
		leftProject = extractProject(prev.Key)
	}
	if moved && c.projects.ForKey(extractProject(newSnap.Key)) == nil {
		retired := 1
		if prev != nil {
			for _, evt := range jiraKeyChangedEvents(*prev, newSnap, e.ID) {
				t.publish(ctx, evt)
				retired++
			}
		}
		t.emitJiraUnreachable(ctx, orgID, e, &newSnap, events.JiraUnreachableMoved, leftProject)
		return retired, false
	}

	if prev == nil {
		// Quiet-seed a snapshot-less row: a stub created
		// outside the poller (exec-touch FindOrCreate), or an entity
		// whose snapshot was cleared when an org admin paused this source.
		// DiffJiraSnapshots' first-discovery branch would synthesize an initial
		// assigned/available/completed event for state that predates our
		// tracking — spuriously minting a task, and after a pause minting
		// one per known issue at once. Seed it like the discovery
		// create-branch (snapshot + title + description, close if terminal) WITHOUT
		// diffing instead. Normal discovery seeds in Phase 1, so this only
		// ever fires for rows that arrived without one.
		// A terminal seed closes the row in the same statement that
		// writes its snapshot, so the entity is never active with a
		// terminal snapshot stored, even between two phases.
		snapJSON, _ := json.Marshal(newSnap)
		var ok bool
		var err error
		if c.terminal(newSnap) {
			ok, err = t.entities.CloseWithSnapshotCASSystem(context.Background(), orgID, e.ID, string(snapJSON), e.PollSeq)
		} else {
			ok, err = t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, e.ID, string(snapJSON), e.PollSeq)
		}
		if err != nil {
			trackerLog.Error("seed jira stub snapshot failed", "source_id", e.SourceID, "error", err)
			return 0, false
		}
		if !ok {
			trackerLog.Warn("seed jira stub snapshot CAS lost race, skipping", "source_id", e.SourceID)
			return 0, false
		}
		t.mirrorJiraText(orgID, e, state)
		return 0, false
	}

	// An omitted description is unknown; retain the last observed body
	// revision so a later response can still detect the next real edit.
	if newSnap.BodyHash == "" {
		newSnap.BodyHash = prev.BodyHash
	}

	// Per-project Done.Members for this entity's project_key. Nil when the
	// entity is in a project that's no longer configured: nothing reads as
	// terminal there.
	evts := DiffJiraSnapshots(*prev, newSnap, e.ID, c.projects.doneMembersForKey(newSnap.Key))

	// The close obligation — the GitHub arm's rule, read against this
	// cycle's per-project done set: the entity is active (Phase 2 lists
	// only active rows) and its snapshot was already terminal last
	// cycle, so the completion that should have closed it was lost.
	if owed, ok := t.closeOwed(ctx, orgID, e.ID, c.terminal(*prev), c.terminal(newSnap)); ok {
		evts = append(evts, owed)
	}

	// Snapshot advance + the transitions diffed against it, one
	// transaction, CAS'd on e.PollSeq (the value this cycle's diff was
	// read against) — the GitHub arm's contract, same reasoning: the
	// snapshot-diff is the sole re-emit prevention, so half of this
	// landing is either a duplicate task (events off a snapshot that
	// didn't win) or a lost one (a snapshot that retired transitions
	// nobody recorded). On a miss or an error nothing was written and
	// the winner's next cycle re-diffs, so suppression loses nothing. A
	// refresh that observed no change commits nothing but its poll stamp.
	prevJSON, _ := json.Marshal(*prev)
	snapJSON, _ := json.Marshal(newSnap)
	ok, enqueued, err := t.commitRefresh(ctx, orgID, e.ID, string(prevJSON), string(snapJSON), e.PollSeq, evts)
	if err != nil {
		trackerLog.Error("jira snapshot+events commit failed; suppressing this cycle's transitions (re-diffed next cycle)", "source_id", e.SourceID, "error", err)
		return 0, false
	}
	if !ok {
		trackerLog.Warn("jira snapshot CAS lost race (stale poll_seq); suppressing this cycle's transitions", "source_id", e.SourceID)
		return 0, false
	}
	t.mirrorJiraText(orgID, e, state)
	return enqueued, false
}

// mirrorJiraText brings the entity's title and description up to the issue's.
// Best effort, outside the snapshot transaction: the event's body hash is the
// revision authority, and these capped strings can lag a committed event. The
// link is not mirrored: it is derived from the key, and only RenameSystem
// writes it, under the row lock and together with the key, so a read that
// predates a rename cannot put the old key's link back.
func (t *Tracker) mirrorJiraText(orgID string, e domain.Entity, state jiraIssueState) {
	if e.Title != state.Snap.Summary {
		_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, e.ID, state.Snap.Summary)
	}
	if state.Snap.BodyHash != "" && e.Description != state.Description {
		_, _ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, e.ID, state.Description)
	}
}

const (
	// jiraUnreachableGrace is how long a tracked issue must go unanswered by
	// the refresh before the tracker spends a request asking Jira about it
	// directly. An entity whose issue id TF has not learned yet is asked
	// about on its first miss instead: the refresh reads it by key, and a key
	// the issue has left never answers again, so waiting would only delay
	// the confirmation that learns its id and renames it.
	//
	// The grace is the whole safety margin. An issue's absence from one search
	// is weak evidence — an index that hasn't caught up, a transient
	// visibility change, or a paging bug all present identically — and the
	// event this pass can emit closes the entity and every task on it. Many
	// consecutive misses across an hour is not proof either, which is why
	// the pass confirms rather than concludes; the grace is only there so
	// the confirmation is spent on issues that look durably unanswered
	// instead of on every blip.
	//
	// Wall-clock rather than a cycle count because the poll interval is the
	// user's to set: an hour is an hour whether that is six cycles or sixty.
	jiraUnreachableGrace = time.Hour

	// jiraUnreachableProbeBudget caps confirmations per cycle. These are one
	// request per issue on top of a cycle that has already done its batch
	// reads, and the population they draw from is unbounded — a whole
	// project's worth of issues can go missing at once when a project is
	// deleted or a credential's visibility narrows.
	//
	// Deferred issues are not dropped. Candidates come off a list ordered
	// oldest-last_polled_at-first, and every confirmation that reaches a
	// verdict advances that column — a 404 by retiring the entity, a 200 by
	// stamping it — so each cycle's budget lands on issues the previous
	// cycles did not reach, and a backlog drains over several cycles rather
	// than arriving as one burst of API calls. The exception is an issue whose
	// confirmation keeps erroring: it stays at the head of the queue and is
	// retried every cycle, which is the right behaviour for a transient
	// fault and self-limiting for a persistent one (nothing behind it could
	// have been confirmed by the same broken endpoint either).
	jiraUnreachableProbeBudget = 20
)

// confirmMissingJiraEntities asks Jira directly about tracked entities the
// refresh has not answered for, and emits jira:issue:unreachable for the
// issues Jira will no longer resolve. Returns the number of events emitted.
// An entity with an id is asked about by id once it has gone unanswered for
// jiraUnreachableGrace; one without is asked about by key on its first miss.
//
// This exists because the refresh cannot retire anything on its own. An issue
// missing from an `id IN (...)` result is skipped by the diff loop, so the
// entity keeps its last snapshot and emits nothing — for as long as it takes
// someone to notice, which for a durable entity is forever. Closing on that
// signal alone would be wrong in the other direction: absence from a search is
// equally consistent with an issue that is merely unindexed, archived, or newly
// invisible to the credential, and closing those would destroy live work.
//
// Asking about the one issue settles it, though not into the answer one might
// want: a 404 says only that this credential cannot resolve it, because Jira
// answers the same way for an issue that was deleted and one it will not admit
// exists. Both make the entity untrackable, which is what the event records and
// all it claims. A 200 is the useful negative. For an entity with no id it
// names the id, which the entity learns, and the key the issue has now, which
// the entity is renamed to if the issue moved; the next refresh reads it by id
// and diffs it. For an entity that already had its id, it means something
// upstream of the diff is failing to return the issue — logged loudly, stamped
// so it stops consuming the budget, and otherwise left alone. Any other error
// is not evidence in either direction.
func (t *Tracker) confirmMissingJiraEntities(ctx context.Context, client *jiraclient.Client, c *jiraCycle, entities []domain.Entity, refreshed map[string]jiraIssueState, now time.Time) int {
	orgID := t.orgID
	var candidates []domain.Entity
	for _, e := range entities {
		if _, answered := refreshed[e.ID]; answered || c.merged[e.ID] {
			continue
		}
		// LastPolledAt advances on every successful refresh write and is
		// stamped at creation, so its age IS the "how long has this issue gone
		// unanswered" clock — no separate miss counter to keep, and nothing
		// to lose across a restart or a change of leader. A nil value predates
		// the column's population and says nothing about recency, so it waits
		// for the next successful refresh to give it a reading.
		if e.ExternalID != "" && (e.LastPolledAt == nil || now.Sub(*e.LastPolledAt) < jiraUnreachableGrace) {
			continue
		}
		candidates = append(candidates, e)
	}
	if len(candidates) == 0 {
		return 0
	}

	ctx, span := tracer.Start(ctx, "tracker.jira.confirm_missing",
		trace.WithAttributes(telemetry.Count(len(candidates))))
	defer span.End()

	emitted := 0
	confirmedWithoutSearch := 0
	repaired := 0
	for i, e := range candidates {
		if i >= jiraUnreachableProbeBudget {
			span.SetAttributes(telemetry.Outcome("partial"))
			trackerLog.InfoContext(ctx, "jira reachability confirmation budget spent; remaining issues re-checked next cycle",
				"budget", jiraUnreachableProbeBudget, "deferred", len(candidates)-i)
			break
		}
		if ctx.Err() != nil {
			return emitted
		}
		if c.merged[e.ID] {
			continue
		}

		idOrKey := e.ExternalID
		if idOrKey == "" {
			idOrKey = e.SourceID
		}
		issue, err := client.GetIssue(ctx, idOrKey)
		switch {
		case err == nil:
			fresh := issueToState(*issue, c.baseURL, nil).Snap
			changed := false
			if e.ExternalID == "" && fresh.ID != "" {
				learned, ok := t.learnJiraIssueID(ctx, c, e, fresh.ID)
				if !ok {
					continue
				}
				e, changed = learned, true
			}
			if jiraRenamed(c.scope, e, fresh) {
				renamed, ok := t.renameJiraEntity(ctx, c, e, fresh.Key)
				if !ok {
					continue
				}
				e, changed = renamed, true
			}
			if changed {
				repaired++
			} else {
				// Confirmed present, and yet the refresh didn't return it. The
				// entity is being skipped every cycle by something other than
				// the issue being unresolvable — an unindexed or archived issue,
				// or one the credential can no longer see through search.
				// Nothing here can repair that, but an entity silently frozen is
				// exactly what this pass exists to stop being invisible.
				confirmedWithoutSearch++
			}
			// Stamp the read. Candidates are selected by how stale this
			// column is and drawn oldest-first against a per-cycle budget,
			// so an entity that will confirm present on every future pass
			// would otherwise sit at the head of that queue forever, consume
			// the budget each cycle, and starve every candidate behind it —
			// including ones that would have confirmed unreachable. Honest as
			// far as it goes: the row *was* just read from the source, which
			// is what the column records; nothing was diffed off it, which
			// is why this is not a snapshot write.
			if err := t.entities.MarkPolledSystem(ctx, orgID, e.ID); err != nil {
				trackerLog.WarnContext(ctx, "stamping a confirmed-present jira entity failed; it stays a confirmation candidate",
					"source_id", e.SourceID, "entity_id", e.ID, "error", err)
			}
		case jiraclient.IsNotFound(err):
			t.emitJiraUnreachable(ctx, orgID, e, nil, events.JiraUnreachableNotFound, "")
			emitted++
		default:
			trackerLog.WarnContext(ctx, "jira reachability confirmation failed; entity left tracked",
				"source_id", e.SourceID, "entity_id", e.ID, "error", err)
		}
	}
	if confirmedWithoutSearch > 0 {
		trackerLog.WarnContext(ctx, "jira issues resolve but no search returned them; entities remain tracked and undiffed",
			"count", confirmedWithoutSearch)
	}
	if repaired > 0 {
		span.SetAttributes(telemetry.Disposition("issue_ids_learned"), telemetry.Attempt(repaired))
	}
	if emitted > 0 {
		// A disposition rather than a second Count — Count is one key, and the
		// count worth keeping on this span is how many issues it examined, not
		// how many it retired. A pass that retires anything is the rare case;
		// this is what makes it findable.
		span.SetAttributes(telemetry.Disposition("entities_retired"), telemetry.Attempt(emitted))
	}
	return emitted
}

// emitJiraUnreachable publishes the terminal event for an entity TF will no
// longer follow, with reason in its metadata. Every field is last-known state
// off the stored snapshot, filled from the fresh read where there is one (an
// issue that moved still answered); a snapshot that is absent or unparseable
// still emits, with those fields blank: the entity has to be retired either
// way, and a corrupt snapshot is not a reason to keep tracking something that
// can no longer be read.
//
// issue_key is the entity's key, which a rename has already brought up to
// date, never one parsed from an older read. project is that key's project,
// unless the caller names another: a move into a project no rule configures
// names the project the issue left, which is the one whose teams tracked it.
//
// Publish, not the snapshot-CAS enqueue: there is no new snapshot to advance,
// and the entity's own close is the router's job (the event terminates it).
func (t *Tracker) emitJiraUnreachable(ctx context.Context, orgID string, e domain.Entity, fresh *domain.JiraSnapshot, reason, project string) {
	var snap domain.JiraSnapshot
	if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			trackerLog.WarnContext(ctx, "corrupt jira snapshot on an unreachable issue; emitting with last-known fields blank",
				"source_id", e.SourceID, "entity_id", e.ID, "error", err)
			snap = domain.JiraSnapshot{}
		}
	}
	if fresh != nil {
		if snap.Assignee == "" && snap.AssigneeAccountID == "" {
			snap.Assignee, snap.AssigneeAccountID = fresh.Assignee, fresh.AssigneeAccountID
		}
		if snap.IssueType == "" {
			snap.IssueType = fresh.IssueType
		}
		if snap.Status == "" {
			snap.Status = fresh.Status
		}
		if snap.Summary == "" {
			snap.Summary = fresh.Summary
		}
	}
	issueID := e.ExternalID
	if issueID == "" {
		issueID = snap.ID
	}
	if project == "" {
		project = extractProject(e.SourceID)
	}
	entityID := e.ID
	trackerLog.InfoContext(ctx, "TF will not follow this jira issue any more; retiring entity",
		"source_id", e.SourceID, "entity_id", e.ID, "reason", reason)
	t.publish(ctx, domain.Event{
		OrgID:     orgID,
		EventType: domain.EventJiraIssueUnreachable,
		EntityID:  &entityID,
		MetadataJSON: mustJSON(events.JiraIssueUnreachableMetadata{
			Assignee:          snap.Assignee,
			AssigneeAccountID: snap.AssigneeAccountID,
			IssueKey:          e.SourceID,
			IssueID:           issueID,
			Project:           project,
			IssueType:         snap.IssueType,
			LastStatus:        snap.Status,
			Summary:           snap.Summary,
			Reason:            reason,
		}),
		// occurred_at is deliberately left zero — Jira reports that an issue does
		// not resolve, never when it stopped, so there is no source time to
		// carry and the nullable contract stores NULL rather than a fabricated
		// one. Consumers fall back to created_at, which is the honest reading:
		// this was observed at detection time.
		//
		// created_at is set even though recordEvent re-stamps it at write
		// time. The durable row is not the only consumer — the bus hands this
		// struct to subscribers as-is, so the websocket push carries whatever
		// is set here, and a zero value would surface as one on the client.
		CreatedAt: time.Now(),
	})
}

// jiraReadIsStale reports whether a freshly fetched snapshot is older than the
// one already stored, handing back both parsed timestamps so the caller can
// name them.
//
// The refresh reads tracked issues out of Jira's search index, which is
// eventually consistent on both deployments — a page can answer with state a
// previous page already superseded. The cost of acting on one is not
// lateness but fabrication: the diff would emit the transition backwards and
// then persist the older read as the baseline, so the next cycle's fresh page
// emits the same transition forwards again. Since the snapshot-diff is the
// sole re-emit prevention, it has no way to recognize its own input
// regressing, and the defence has to sit in front of it. One real status
// change arriving out of order that way mints two tasks, because the new
// status name is the dedup key; one assignment change reaches auto-delegation
// twice.
//
// Strictly older, because Jira's `updated` is millisecond-resolution and two
// edits landing inside one millisecond must still diff. An absent or
// unparseable timestamp on either side is not evidence of anything, so it
// falls through to the diff unchanged — snapshots written before the field
// existed carry none.
func jiraReadIsStale(stored, fetched domain.JiraSnapshot) (storedAt, fetchedAt time.Time, stale bool) {
	storedAt, storedOK := domain.ParseExternalTime(stored.UpdatedAt)
	fetchedAt, fetchedOK := domain.ParseExternalTime(fetched.UpdatedAt)
	if !storedOK || !fetchedOK {
		return storedAt, fetchedAt, false
	}
	return storedAt, fetchedAt, fetchedAt.Before(storedAt)
}

// jiraStatusTerms renders a status set as the inside of a JQL `IN (...)` list,
// or "" when the set contributes nothing.
//
// A numeric id goes in bare, which is how JQL is told to read a term as a
// status id rather than a name — and matching on the id is what keeps a query
// right after someone renames the status in Jira. A ref with no usable id
// falls back to its quoted name, which is all a rule armed before statuses
// were identified has to offer. The all-digits test is the guard on that: an
// id that isn't a number would be read as a name if written bare, silently
// matching nothing, so it takes the name path instead.
func jiraStatusTerms(refs []domain.JiraStatusRef) string {
	terms := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch {
		case isJiraStatusID(ref.ID):
			terms = append(terms, ref.ID)
		case ref.Name != "":
			terms = append(terms, fmt.Sprintf("%q", ref.Name))
		}
	}
	return strings.Join(terms, ", ")
}

func isJiraStatusID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// discoverJira runs JQL queries to find new issues. Each project gets
// its own JQL pair — one Pickup query against the project's
// PickupMembers and one assigned-to-me query that excludes the
// project's DoneMembers. Per-project iteration is required because
// status names rarely overlap across heterogeneous workflows
// ("Backlog/Selected" vs "New/Triage"); a unified `status IN
// (union)` query would surface tickets the user never wants to pick
// up.
//
// Subtask classification uses the union of every project's
// DoneMembers — subtasks can live in projects other than the parent's,
// and the union matches today's "treat any known done status as
// terminal" behavior across heterogeneous projects.
func (t *Tracker) discoverJira(ctx context.Context, client *jiraclient.Client, baseURL string, projects JiraRules) ([]jiraIssueState, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	// Count is projects, not issues: the fan-out below is one JQL search
	// per configured project (sometimes two).
	ctx, span := tracer.Start(ctx, "tracker.jira.discover",
		trace.WithAttributes(telemetry.Count(len(projects))))
	defer span.End()

	// build renders the JQL from a status set, so a query that Jira rejects can
	// be rendered again from a narrower one; members is the set the live jql was
	// built from. The pair is set only where narrowing is SOUND, which is why
	// the assigned-to-me query leaves it nil — see salvageJiraQuery.
	type queryWithDone struct {
		projectKey            string
		jql                   string
		build                 func([]domain.JiraStatusRef) string
		members               []domain.JiraStatusRef
		doneMembers           []domain.JiraStatusRef // for subtask classification on issues returned by this query
		assignedToCurrentUser bool                   // this query's arrival is itself an assignment signal
	}
	var queries []queryWithDone

	allDone := projects.AllDoneMembers()

	for _, p := range projects {
		if p.Key == "" {
			continue
		}

		// An empty pickup set yields no query at all rather than an unfiltered
		// one: "no statuses to pick up from" must never widen into "every
		// unassigned ticket in the project".
		pickupJQL := func(members []domain.JiraStatusRef) string {
			terms := jiraStatusTerms(members)
			if terms == "" {
				return ""
			}
			return fmt.Sprintf(`project = %q AND status IN (%s) AND assignee IS EMPTY`, p.Key, terms)
		}
		if jql := pickupJQL(p.PickupMembers); jql != "" {
			queries = append(queries, queryWithDone{
				projectKey: p.Key, jql: jql, build: pickupJQL, members: p.PickupMembers,
				doneMembers: allDone,
			})
		}

		// Assigned-to-me query, with terminal statuses excluded via the
		// project's Done.Members set. If empty (defensive — Ready()
		// gates the poller on non-empty Done.Members, so we shouldn't
		// hit this in practice), the NOT IN clause is dropped entirely
		// rather than falling back to a hardcoded list that would
		// contradict the user's workflow.
		assignedJQL := func(members []domain.JiraStatusRef) string {
			jql := fmt.Sprintf(`project = %q AND assignee = currentUser()`, p.Key)
			if done := jiraStatusTerms(members); done != "" {
				jql += fmt.Sprintf(` AND status NOT IN (%s)`, done)
			}
			return jql
		}
		// No build/members: this query EXCLUDES its status set, so narrowing it
		// would widen the result rather than shrink it. salvageJiraQuery says
		// why that is unsound even though the members here are just as capable
		// of naming a status Jira has deleted.
		queries = append(queries, queryWithDone{
			projectKey: p.Key, jql: assignedJQL(p.DoneMembers),
			doneMembers: allDone, assignedToCurrentUser: true,
		})
	}

	seen := map[string]bool{}
	var all []jiraIssueState
	fields := jiraIssueFields

	// Live workflows, fetched only when a query has already failed and cached
	// for the rest of the cycle so two failed queries on one project cost one
	// call. Steady state never touches this.
	liveStatuses := map[string][]domain.JiraStatusRef{}

	failed, salvaged := 0, 0
	// One entry per query sent, nil for a query that returned: a pass whose
	// every query failed because of the connection reports that.
	outcomes := make([]error, 0, len(queries))
	for _, q := range queries {
		issues, err := client.SearchIssues(ctx, q.jql, fields, 100)
		// Only a query Jira rejected can be one naming a dead status. One it
		// rate-limited or failed to answer would meet the same failure on the
		// workflow read, which only adds a request to a host already refusing
		// them.
		if class, _ := upstream.ClassOf(err); err != nil && q.build != nil && class == upstream.Rejected {
			if jql, dropped := t.salvageJiraQuery(ctx, client, q.projectKey, q.members, q.build, liveStatuses); jql != "" {
				trackerLog.WarnContext(ctx, "jira discovery query rebuilt without statuses the workflow no longer has",
					"project", q.projectKey, "dropped", domain.JiraStatusNames(dropped), "error", err)
				issues, err = client.SearchIssues(ctx, jql, fields, 100)
				if err == nil {
					salvaged++
				}
			}
		}
		outcomes = append(outcomes, err)
		if err != nil {
			// One project's query failing must not sink the others, so
			// this continues — which means the caller gets a short result
			// with no indication why. The outcome below is that indication.
			failed++
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelError), "jira discovery query failed", "project", q.projectKey, "error", err)
			continue
		}
		for _, issue := range issues {
			// One issue can answer two queries; it is the same issue by id.
			identity := issue.ID
			if identity == "" {
				identity = issue.Key
			}
			if !seen[identity] {
				seen[identity] = true
				state := issueToState(issue, baseURL, q.doneMembers)
				state.DiscoveredAssignedToCurrentUser = q.assignedToCurrentUser
				all = append(all, state)
			}
		}
	}
	if failed > 0 || salvaged > 0 {
		// TODO(TFAC-878): a log line and this span attribute are the only trace.
		// A salvaged query keeps the project producing work, but its rules still
		// name a status Jira does not have, and the settings board is the only
		// place that says so — nobody who is not looking at it ever learns.
		// Surfacing a condition nobody is watching needs the durable
		// notification channel.
		span.SetAttributes(telemetry.Outcome("partial"), telemetry.Attempt(failed+salvaged))
	}
	if err := discoveryUnreached("jira", outcomes); err != nil {
		span.SetStatus(codes.Error, "every query failed")
		span.SetAttributes(telemetry.Outcome("failed"))
		return all, err
	}
	if err := discoveryRateLimited("jira", outcomes); err != nil {
		// Not an error status, for the reason the GitHub fan-out gives: a
		// rate limit is the upstream answering, and a handled outcome.
		span.SetAttributes(telemetry.Outcome("rate_limited"))
		return all, err
	}

	return all, nil
}

// salvageJiraQuery rebuilds a discovery query that Jira rejected, without the
// statuses the project's workflow no longer has.
//
// This is the rare path and it is deliberately not a pre-check: JQL validates
// status terms against Jira's global list, so a status merely retired from
// this project's workflow still makes a valid query, and only one deleted
// outright makes an invalid one. Fetching every project's workflow on every
// cycle to guard against that would be a call per project per poll for a
// condition that almost never holds — so the workflow is read only once a
// query has already failed, and cached in live for the rest of the cycle.
//
// Only an INCLUSION set may be salvaged this way, and callers enforce that by
// passing build only for those. Narrowing a `status IN (…)` set shrinks the
// result; narrowing a `status NOT IN (…)` set widens it — and the filter here
// is deliberately broader than the condition that broke the query. JQL rejects
// a status deleted from the instance, but this drops every status missing from
// the PROJECT's workflow, and an issue can sit in a status a workflow-scheme
// change retired. Dropping one of those from an exclusion set would hand back
// finished tickets as new work; dropping it from an inclusion set only costs
// the tickets that status holds, which is the trade this exists to make.
//
// Returns an empty jql when nothing can be salvaged: the workflow is
// unreachable, every member is still live (so the failure is something else
// entirely and rerunning the same query would only repeat it), or nothing at
// all survives — an inclusion set narrowed to empty is an unfiltered query,
// never a narrower one. The stored rule is never edited: a status that
// vanished upstream is the team's to remove, and the settings board is where
// they are told.
func (t *Tracker) salvageJiraQuery(
	ctx context.Context, client *jiraclient.Client,
	projectKey string, members []domain.JiraStatusRef,
	build func([]domain.JiraStatusRef) string,
	live map[string][]domain.JiraStatusRef,
) (jql string, dropped []domain.JiraStatusRef) {
	known, cached := live[projectKey]
	if !cached {
		statuses, err := client.ProjectStatuses(ctx, projectKey)
		if err != nil {
			trackerLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "jira workflow read failed; cannot tell whether the query names a dead status",
				"project", projectKey, "error", err)
			return "", nil
		}
		known = make([]domain.JiraStatusRef, 0, len(statuses))
		for _, st := range statuses {
			known = append(known, domain.JiraStatusRef{ID: st.ID, Name: st.Name})
		}
		live[projectKey] = known
	}

	surviving := make([]domain.JiraStatusRef, 0, len(members))
	for _, m := range members {
		if domain.ContainsStatus(known, m) {
			surviving = append(surviving, m)
		} else {
			dropped = append(dropped, m)
		}
	}
	if len(dropped) == 0 || len(surviving) == 0 {
		return "", nil
	}
	return build(surviving), dropped
}

// jiraIssueFields is the field list every tracking read of an issue asks for.
// "updated" is required for the diff layer's source-time fallback — without
// it, JiraSnapshot.UpdatedAt is empty and emit() degrades all the way to
// detection time — and "project" for telling a move from a project key
// rename. Spelled out because these reads pass their own list rather than
// relying on DefaultSearchFields.
var jiraIssueFields = []string{"summary", "description", "status", "assignee", "priority", "labels", "issuetype", "project", "parent", "comment", "subtasks", "created", "updated"}

// batchFetchJira reads every entity's issue and returns the answers keyed by
// entity id. An entity with an issue id is read by id (`id IN (...)`) and
// matched back by id, so an issue that moved or whose project's key was renamed
// answers for its entity under its new key. An entity created before ids were
// recorded is read by key (`key IN (...)`) and matched back by key, and learns
// its id from the answer; one whose issue moved before it did answers under
// another key, matches nothing, and is confirmed by the next phase instead.
//
// Descriptions are included so tracked issues keep detecting body edits even
// after reassignment takes them out of the discovery queries.
func (t *Tracker) batchFetchJira(ctx context.Context, client *jiraclient.Client, baseURL string, entities []domain.Entity, projects JiraRules) (map[string]jiraIssueState, error) {
	// Serial, with one iteration per batch of tracked issues — so its cost
	// grows with every issue TF tracks, making it the cycle's most likely
	// creeping regression. The per-request spans underneath are each fast,
	// so nothing else would show it.
	ctx, span := tracer.Start(ctx, "tracker.jira.batch_fetch",
		trace.WithAttributes(telemetry.Count(len(entities))))
	defer span.End()

	results := make(map[string]jiraIssueState, len(entities))
	allDone := projects.AllDoneMembers()

	byID := make(map[string]string, len(entities))  // issue id → entity id
	byKey := make(map[string]string, len(entities)) // issue key → entity id
	var ids, keys []string
	for _, e := range entities {
		if e.ExternalID != "" {
			byID[e.ExternalID] = e.ID
			ids = append(ids, e.ExternalID)
		} else {
			byKey[e.SourceID] = e.ID
			keys = append(keys, e.SourceID)
		}
	}

	fetch := func(field string, values []string, match func(jiraclient.Issue) (string, bool)) error {
		for i := 0; i < len(values); i += jiraBatchSize {
			end := min(i+jiraBatchSize, len(values))
			jql := fmt.Sprintf("%s IN (%s)", field, strings.Join(values[i:end], ", "))
			issues, err := client.SearchIssues(ctx, jql, jiraIssueFields, jiraBatchSize)
			if err != nil {
				span.SetStatus(codes.Error, "batch fetch")
				return fmt.Errorf("batch fetch %s %d-%d: %w", field, i, end, err)
			}
			for _, issue := range issues {
				if entityID, ok := match(issue); ok {
					// Subtask classification uses the union of every project's
					// done members — subtasks can live in projects other than
					// the parent's.
					results[entityID] = issueToState(issue, baseURL, allDone)
				}
			}
		}
		return nil
	}
	if err := fetch("id", ids, func(issue jiraclient.Issue) (string, bool) {
		entityID, ok := byID[issue.ID]
		return entityID, ok
	}); err != nil {
		return nil, err
	}
	if err := fetch("key", keys, func(issue jiraclient.Issue) (string, bool) {
		entityID, ok := byKey[issue.Key]
		return entityID, ok
	}); err != nil {
		return nil, err
	}

	// A tracked issue that comes back in no page — deleted, moved before its
	// entity learned its id, or no longer visible to the service credential —
	// is skipped by the diff loop and left to the confirmation pass. It is
	// said out loud here as well, because a truncated page would masquerade as
	// exactly this: with the gap logged, a paging bug shows up as a log line
	// rather than as entities that quietly stop moving.
	if missing := missingJiraIssues(entities, results); len(missing) > 0 {
		span.SetAttributes(telemetry.Outcome("partial"))
		trackerLog.WarnContext(ctx, "jira batch fetch returned no row for tracked issues",
			"missing", len(missing), "tracked", len(entities),
			"keys", strings.Join(missing[:min(len(missing), jiraMissingKeySample)], ", "))
	}

	return results, nil
}

// jiraMissingKeySample bounds how many absent keys the gap warning names. The
// count is the signal; the keys are there to start an investigation, and a
// hundred of them in one line would bury it.
const jiraMissingKeySample = 10

// missingJiraIssues returns the keys of the entities the batch fetch produced
// no state for, in request order.
func missingJiraIssues(entities []domain.Entity, results map[string]jiraIssueState) []string {
	var missing []string
	for _, e := range entities {
		if _, ok := results[e.ID]; !ok {
			missing = append(missing, e.SourceID)
		}
	}
	return missing
}

// jiraIssueState bundles the diff-scope snapshot with the bulk description
// body. Description is carried alongside rather than inside the snapshot so
// the persisted snapshot_json stays small — diff reads don't drag multi-KB
// issue bodies through every poll.
type jiraIssueState struct {
	Snap                            domain.JiraSnapshot
	Description                     string
	DiscoveredAssignedToCurrentUser bool
}

// issueToState converts a Jira API Issue into the diff-scope snapshot plus
// a flattened description. The description is stored on entities.description
// separately; the snapshot itself only carries fields that DiffJiraSnapshots
// compares. doneStatuses is the user's configured Done.Members set, used
// to decide which subtasks count as "open" when populating OpenSubtaskCount.
func issueToState(issue jiraclient.Issue, baseURL string, doneStatuses []domain.JiraStatusRef) jiraIssueState {
	snap := domain.JiraSnapshot{
		ID:       issue.ID,
		Key:      issue.Key,
		Summary:  issue.Fields.Summary,
		URL:      domain.JiraIssueURL(baseURL, issue.Key),
		BodyHash: domain.JSONBodyHash(issue.Fields.Description),
	}
	if issue.Fields.Project != nil {
		snap.ProjectID = issue.Fields.Project.ID
	}
	if issue.Fields.Status != nil {
		snap.Status = issue.Fields.Status.Name
		snap.StatusID = issue.Fields.Status.ID
	}
	if issue.Fields.Assignee != nil {
		snap.Assignee = issue.Fields.Assignee.DisplayName
		// Derive the stable account id through the shared precedence
		// (accountId → key → name). This MUST agree with the identity stored
		// in user_jira_identities (also via jira.StableUserID, through
		// auth.JiraUser.StableID): assignee-centric routing joins this event's
		// assignee_account_id against that row to resolve the owning team. A
		// name-only fallback here while the identity held the Server/DC key
		// silently broke the join — events landed, no task was created.
		snap.AssigneeAccountID = jiraclient.StableUserID(
			issue.Fields.Assignee.AccountID,
			issue.Fields.Assignee.Key,
			issue.Fields.Assignee.Name,
		)
	}
	if issue.Fields.Priority != nil {
		snap.Priority = issue.Fields.Priority.Name
	}
	if issue.Fields.IssueType != nil {
		snap.IssueType = issue.Fields.IssueType.Name
	}
	if issue.Fields.Parent != nil {
		snap.ParentKey = issue.Fields.Parent.Key
	}
	if issue.Fields.Comment != nil {
		snap.CommentCount = issue.Fields.Comment.Total
	}
	snap.Labels = issue.Fields.Labels
	if issue.Fields.Created != "" {
		snap.CreatedAt = issue.Fields.Created
	}
	if issue.Fields.Updated != "" {
		snap.UpdatedAt = issue.Fields.Updated
	}
	snap.OpenSubtaskCount = countOpenSubtasks(issue, doneStatuses)
	return jiraIssueState{
		Snap:        snap,
		Description: truncateDescription(jiraclient.ExtractDescriptionText(issue.Fields.Description), descriptionStoreMaxRunes),
	}
}

// countOpenSubtasks returns the number of subtasks on this issue whose
// status is NOT in the configured Done.Members set. Missing/unknown status
// is counted as open — conservative default: better to show a parent as
// "has open subtasks" and suppress task creation than to wrongly surface
// it as atomic when we couldn't classify.
func countOpenSubtasks(issue jiraclient.Issue, doneStatuses []domain.JiraStatusRef) int {
	if len(issue.Fields.Subtasks) == 0 {
		return 0
	}
	open := 0
	for _, sub := range issue.Fields.Subtasks {
		var ref domain.JiraStatusRef
		if sub.Fields.Status != nil {
			ref = domain.JiraStatusRef{ID: sub.Fields.Status.ID, Name: sub.Fields.Status.Name}
		}
		if !domain.ContainsStatus(doneStatuses, ref) {
			open++
		}
	}
	return open
}

// prDescription is the stored form of a PR snapshot's body: trimmed and
// capped exactly as the Jira arm caps an issue description, so
// entities.description holds one shape whichever source wrote it.
func prDescription(snap domain.PRSnapshot) string {
	return truncateDescription(strings.TrimSpace(snap.Body), descriptionStoreMaxRunes)
}

// truncateDescription caps the stored description at maxRunes codepoints
// (rune-based so we never persist a string that ends mid-UTF-8-codepoint).
// Strict cap — when truncation happens the returned string contains exactly
// maxRunes runes, with the last rune replaced by an ellipsis so downstream
// readers can distinguish a cut string from a genuinely short one.
func truncateDescription(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

// --- Helpers ---

// EmitPollComplete publishes the system poll-completed sentinel. startedAt
// is the wall-clock time the poll cycle started, carried in metadata so
// subscribers can ignore sentinels emitted by pre-restart poll generations
// (an old RefreshXxx goroutine that finishes after a config-triggered restart).
func (t *Tracker) EmitPollComplete(ctx context.Context, source string, startedAt time.Time, entityCount, eventCount int) {
	t.publish(ctx, domain.Event{
		EventType: domain.EventSystemPollCompleted,
		MetadataJSON: mustJSON(events.SystemPollCompletedMetadata{
			Source:    source,
			StartedAt: startedAt.UnixNano(),
			Entities:  entityCount,
			Events:    eventCount,
		}),
		CreatedAt: time.Now(),
	})
}
