package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
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
	// without a second enqueue. The tracker's diffed transitions take this
	// path because they ride the snapshot CAS's transaction (see
	// emitWithSnapshotCAS).
	PublishPreEnqueued(ctx context.Context, evt domain.Event)
}

const (
	jiraBatchSize = 100 // max issues per JQL key IN (...) query

	// descriptionStoreMaxRunes caps what we persist on entities.description.
	// Jira descriptions are unbounded (teams regularly paste multi-KB specs,
	// stack traces, etc.); storing them raw would bloat the column for no
	// current benefit — the scorer already truncates at 1500 runes for the
	// LLM prompt, so 2000 gives a small buffer while keeping rows compact.
	// If a future UI wants to render the full body it should re-fetch from
	// Jira directly rather than relying on this mirror.
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
	// through pub because the diff arms need the ONE write that carries
	// both halves of a cycle's result: the snapshot advance and the
	// transitions diffed against it (EnqueueBatchWithSnapshotCAS). Every
	// other emit the tracker makes — discovery backfills, poll-complete
	// sentinels — has no snapshot to pair with and goes through pub.
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

// emitWithSnapshotCAS commits a diffed cycle's result for one entity: the
// snapshot advance under its poll_seq CAS, and the transitions diffed
// against that snapshot, in a single transaction. Reports ok=false when the
// CAS lost, in which case nothing was written at all.
//
// The pairing is the point. The snapshot-diff is the sole re-emit
// prevention, so a snapshot that advances without its transitions retires
// them permanently — the next cycle diffs new-against-new and finds
// nothing. Committing both together means a failure before commit leaves
// the entity exactly where the next cycle expects it, and a CAS miss (a
// straggler ex-leader, stale by the time it lands) writes neither half.
//
// evts may be empty: a refreshed entity with no transitions is a pure
// snapshot advance, and takes this same path rather than a second one.
//
// Unlike the tracker's other persistence calls this takes the CYCLE's ctx,
// not context.Background(). Those keep Background because a cancellation
// mid-sequence can strand them half-applied; this one cannot — its two
// writes are one transaction, so a cancelled emit commits nothing and the
// next cycle re-diffs from the surviving snapshot. And the enqueue needs a
// real ctx to carry: the producer trace context every queue row it writes
// is stamped with comes from here.
func (t *Tracker) emitWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, evts []domain.Event) (bool, error) {
	if len(evts) == 0 {
		ok, _, err := t.queue.EnqueueBatchWithSnapshotCAS(ctx, orgID, entityID, snapshotJSON, expectedPollSeq, nil, nil)
		return ok, err
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
		return false, err
	}
	if !ok {
		return false, nil
	}

	// Committed. The bus fan-out below is the cosmetic half — the live WS
	// push and the scorer's idempotent nudge — and dying between the commit
	// and it costs only that: the router consumes the queue, not the bus,
	// so these events still route on the drain worker's schedule.
	for i, evt := range evts {
		evt.ID = ids[i] // the id the enqueue minted, so bus and queue agree
		t.pub.PublishPreEnqueued(ctx, evt)
	}
	return true, nil
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
// exception is Phase 3's snapshot+events commit (emitWithSnapshotCAS),
// which takes the cycle ctx precisely because it CAN'T half-apply — see
// its doc.
// The third return, resumeFrom, is the round-robin resume point — see
// discoverGitHub. It is only ever non-empty alongside a non-nil error
// (the rate-limited discovery-interruption path); every other return path
// (success, or a non-rate-limit failure in Phase 2/3 reached only once Phase
// 1 already covered every entry in repos) reports "" — a full wrap of the
// list passed in, so the poller's cursor resets rather than resuming mid-list.
func (t *Tracker) RefreshGitHub(ctx context.Context, client *ghclient.Client, username string, repos []string, resolver ReviewerResolver) (int, string, error) {
	orgID := t.orgID
	startedAt := time.Now()
	// Phase 1: Discovery — find new PRs and register as entities.
	// quietRepos is the set of "owner/repo" whose open-PR listing returned
	// 304 (unchanged) this cycle; their tracked entities can keep their
	// stored snapshot through the Phase-2 gate without a refresh.
	discovered, quietRepos, resumeFrom, err := t.discoverGitHub(ctx, client, username, repos)
	var rateLimited *ghclient.ErrRateLimited
	if err != nil {
		if errors.As(err, &rateLimited) {
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
			trackerLog.Error("github discovery error", "error", err)
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
		entity, created, err := t.entities.FindOrCreateSystem(context.Background(), orgID, "github", sid, "pr", snap.Title, snap.URL)
		if err != nil {
			trackerLog.Error("create entity failed", "source_id", sid, "error", err)
			continue
		}

		if created {
			// Seed the discovery snapshot. CAS against entity.PollSeq (0 for
			// a just-created row); a miss means a concurrent seed already won.
			snapJSON, _ := json.Marshal(snap)
			if ok, err := t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
				trackerLog.Error("seed snapshot failed", "source_id", sid, "error", err)
			} else if !ok {
				trackerLog.Warn("seed snapshot CAS lost race, skipping", "source_id", sid)
			}
			// If the PR is already terminal, mark the entity closed immediately
			// so it doesn't sit in the active refresh set forever (Phase 3
			// won't emit a merged/closed event because prev==curr).
			if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
				if _, err := t.entities.MarkClosedSystem(context.Background(), orgID, entity.ID); err != nil {
					trackerLog.Error("mark entity closed on discovery failed", "source_id", sid, "error", err)
				}
			} else if snap.Author != username {
				// Backfill: emit a per-reviewer review_requested event for
				// every TF-known requested reviewer on a just-discovered open
				// PR. DiffPRSnapshots' "no events on initial load"
				// rule means pr:review_requested would never fire for requests
				// that existed before we started watching — the reviewer would
				// only see them if someone re-requested. Synthesizing here lands
				// existing review-requests in the queue on first connect.
				// Mirrors the Jira carry-over queue path in handleJiraStockQueue.
				//
				// Self-authored PRs are skipped: GitHub forbids self-requests,
				// so the only way a match fires here is via a team the user is
				// on (CODEOWNERS auto-assigning them to their own PR). That isn't
				// an ask — surfacing it pollutes the queue. Matches the guard in
				// DiffPRSnapshots.
				for _, reviewer := range snap.ReviewRequests {
					login, team, known := resolveReviewer(resolver, reviewer)
					if !known {
						continue
					}
					if err := t.backfillReviewRequested(ctx, entity.ID, snap, login, team); err != nil {
						trackerLog.Error("backfill review_requested failed", "source_id", sid, "reviewer", reviewer, "error", err)
					}
				}
			}
		} else {
			// Update title if changed.
			if entity.Title != snap.Title {
				_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, entity.ID, snap.Title)
			}
			// Reactivate if a previously-closed entity reappears as open
			// (e.g., reopened PR).
			if !snap.Merged && snap.State != "CLOSED" && snap.State != "MERGED" && entity.State == "closed" {
				if reactivated, err := t.entities.ReactivateSystem(context.Background(), orgID, entity.ID); err != nil {
					trackerLog.Error("reactivate entity failed", "source_id", sid, "error", err)
				} else if reactivated {
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
		if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
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

		if item.seed {
			// Quiet-seed a snapshot-less entity: populate it like the
			// discovery create-branch — snapshot + title, close if terminal — and
			// emit NOTHING. DiffPRSnapshots already suppresses non-terminal first-
			// discovery events, but seeding without diffing also keeps a terminal
			// row from emitting a merged/closed event for a PR that closed before
			// we tracked it — or, after an event-source pause, for one that merged
			// while the org had the source turned off. The next cycle diffs
			// against this seed normally.
			snapJSON, _ := json.Marshal(newSnap)
			if ok, err := t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, item.entity.ID, string(snapJSON), item.entity.PollSeq); err != nil {
				trackerLog.ErrorContext(ctx, "seed stub snapshot failed", "source_id", item.entity.SourceID, "error", err)
			} else if !ok {
				trackerLog.WarnContext(ctx, "seed stub snapshot CAS lost race, skipping", "source_id", item.entity.SourceID)
			}
			if item.entity.Title != newSnap.Title {
				_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, item.entity.ID, newSnap.Title)
			}
			if newSnap.Merged || newSnap.State == "CLOSED" || newSnap.State == "MERGED" {
				if _, err := t.entities.MarkClosedSystem(context.Background(), orgID, item.entity.ID); err != nil {
					trackerLog.ErrorContext(ctx, "mark stub closed failed", "source_id", item.entity.SourceID, "error", err)
				}
			}
			continue
		}

		// Diff against previous snapshot.
		events := DiffPRSnapshots(item.snap, newSnap, item.entity.ID, username, resolver)

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
		// the transition, so suppression loses nothing.
		snapJSON, _ := json.Marshal(newSnap)
		ok, err := t.emitWithSnapshotCAS(ctx, orgID, item.entity.ID, string(snapJSON), item.entity.PollSeq, events)
		if err != nil {
			trackerLog.ErrorContext(ctx, "snapshot+events commit failed; suppressing this cycle's transitions (re-diffed next cycle)", "source_id", item.entity.SourceID, "error", err)
			continue
		}
		if !ok {
			trackerLog.WarnContext(ctx, "snapshot CAS lost race (stale poll_seq); suppressing this cycle's transitions", "source_id", item.entity.SourceID)
			continue
		}
		eventsEmitted += len(events)

		// Best-effort, outside the transaction: the title is display-only
		// mirroring, so a failure here costs a stale string until the next
		// cycle, never an event.
		if item.entity.Title != newSnap.Title {
			_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, item.entity.ID, newSnap.Title)
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
		trackerLog.Warn("stub enrich: GetPRBasic failed", "source_id", e.SourceID, "error", err)
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
					results[i] = repoListResult{rateLimited: true}
					// Not an error status: exhausting the budget is a
					// handled outcome with a resume cursor behind it.
					span.SetAttributes(telemetry.Outcome("rate_limited"))
					return nil
				}
				// 403/404 means the token can't reach this configured repo (a
				// PAT user without access, or an App not installed on it) —
				// skip and log rather than failing the whole sweep.
				var he *ghclient.HTTPError
				if errors.As(err, &he) && (he.StatusCode == 403 || he.StatusCode == 404) {
					span.SetAttributes(telemetry.Outcome("unreachable"))
					trackerLog.WarnContext(ctx, "discovery: repo unreachable — skipping", "repo", repoFull, "status", he.StatusCode)
					return nil
				}
				span.SetStatus(codes.Error, "list open PRs")
				trackerLog.ErrorContext(ctx, "discovery: list open PRs failed", "repo", repoFull, "error", err)
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
	}

	// Phase 1b: merged/closed dashboard backfill (local/PAT-only). Seeds
	// recent-history entities via user-perspective GraphQL search. App tokens
	// have no "me", so this is skipped when username is empty; multi-mode
	// instead backfills per bound user via Tracker.BackfillDashboardHistory.
	// Query construction is shared with that path (dashboardBackfillQueries) so
	// both search for exactly the same history. Also skipped once Phase 1a hit
	// ErrRateLimited — it shares the same client budget, so it would just fail
	// the same way for no benefit.
	if username != "" && discoveryErr == nil {
		for _, q := range dashboardBackfillQueries(username, repos) {
			prs, err := client.DiscoverPRs(ctx, q, 50)
			if err != nil {
				trackerLog.Error("dashboard backfill query failed", "error", err, "query", q)
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

	if discoveryErr != nil {
		// The only error this returns is ErrRateLimited, which is a
		// handled outcome with a resume cursor behind it — an attribute,
		// not an error status, same as the per-repo children above.
		span.SetAttributes(telemetry.Outcome("rate_limited"))
	}
	return all, quiet, resumeFrom, discoveryErr
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

// backfillReviewRequested publishes a synthesized pr:review_requested
// event for a PR being discovered for the first time with the session
// user already in its requested-reviewer list. The router subscribes
// to the bus, evaluates rules, and fans out to per-team tasks.
// The task's primary_event_id FK is satisfied when the router records
// the event in its HandleEvent step 1.
//
// Previously the tracker bypassed the bus and called
// tasks.FindOrCreateAt directly, which sidestepped rule evaluation —
// every backfilled task ended up assigned to "the oldest team in the
// org" regardless of which team's rule actually matched. Routing
// through the bus gives backfill the same team-aware fanout every
// other tracker-detected event already gets.
//
// The OccurredAt stamp uses the PR's CreatedAt as a lower bound:
// GitHub doesn't expose per-review-request timestamps, so PR creation
// time is the closest we have — better than "just now" on the card
// for a PR that's been pending your review for weeks. Falls back to
// time.Now() if the GraphQL timestamp is missing or unparseable.
//
// The "is this reviewer TF-known" decision happens upstream at the
// caller's resolveReviewer check, not here; this function just records the
// requested identity (login or "org/slug" team) plus the PR author on the
// metadata, and keys the event by that identity, so the router routes the
// per-reviewer task and the predicate matcher can do its work.
func (t *Tracker) backfillReviewRequested(ctx context.Context, entityID string, snap domain.PRSnapshot, requestedLogin, requestedTeam string) error {
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
		return err
	}
	// Parse through the shared external-time parser (handles RFC3339Nano
	// sub-second shapes + Jira offsets) so a fractional-seconds CreatedAt
	// doesn't silently degrade the backfilled task's queue order to "now".
	occurredAt := time.Time{}
	if parsed, ok := domain.ParseExternalTime(snap.CreatedAt); ok {
		occurredAt = parsed
	}
	eid := entityID
	t.publish(ctx, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRReviewRequested,
		DedupKey:     reviewerDedupKey(reviewer),
		MetadataJSON: string(metaJSON),
		OccurredAt:   occurredAt,
	})
	return nil
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
// aren't auto-deleted on settings change). Nil matches the closure in
// RefreshJira's terminal() (which returns false on unknown project)
// so discovery's "should I mark this entity closed" and the diff
// layer's "did this transition complete it" stay consistent.
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

// RefreshJira runs the full tracking cycle for Jira issues. projects is
// the team's full per-project rule set; the tracker dispatches discovery
// JQL per project and looks up terminal-status sets by the issue's
// project_key. Tickets whose project_key has no row degrade silently
// — no terminal check, no pickup discovery.
//
// The username parameter was dropped: actor identity now flows through
// the snapshot (assignee_account_id) and predicate matching happens
// downstream against the assignee_in / reporter_in / commenter_in
// allowlists.
//
// All entity reads/writes are scoped to the Tracker's orgID (set at
// construction). In multi mode the poller's per-org loop constructs
// one Tracker per active org per cycle; in local mode there's one
// Tracker for the single synthetic tenant.
func (t *Tracker) RefreshJira(ctx context.Context, client *jiraclient.Client, baseURL string, projects JiraRules) (int, error) {
	orgID := t.orgID
	startedAt := time.Now()
	discoveryEventsEmitted := 0
	terminal := func(snap domain.JiraSnapshot) bool {
		rule := projects.ForKey(extractProject(snap.Key))
		if rule == nil {
			return false
		}
		return domain.ContainsStatus(rule.DoneMembers, snap.StatusRef())
	}
	// Phase 1: Discovery
	discovered, err := t.discoverJira(ctx, client, baseURL, projects)
	if err != nil {
		trackerLog.Error("jira discovery error", "error", err)
	}

	for _, state := range discovered {
		snap := state.Snap
		entity, created, err := t.entities.FindOrCreateSystem(context.Background(), orgID, "jira", snap.Key, "issue", snap.Summary, snap.URL)
		if err != nil {
			trackerLog.Error("create entity failed", "source_id", snap.Key, "error", err)
			continue
		}
		if created {
			snapJSON, _ := json.Marshal(snap)
			if state.DiscoveredAssignedToCurrentUser {
				// An issue assigned to someone else is outside both
				// discovery queries, so appearing in the assigned-to-current-user
				// result can itself be the assignment transition. Commit that initial
				// event with the first snapshot; seeding first would make Phase 3
				// diff current-against-current and retire the transition unseen.
				events := DiffJiraSnapshots(domain.JiraSnapshot{}, snap, entity.ID, projects.doneMembersForKey(snap.Key))
				if ok, err := t.emitWithSnapshotCAS(ctx, orgID, entity.ID, string(snapJSON), entity.PollSeq, events); err != nil {
					trackerLog.Error("seed assigned jira snapshot+event failed", "source_id", snap.Key, "error", err)
				} else if !ok {
					trackerLog.Warn("seed assigned jira snapshot CAS lost race, skipping", "source_id", snap.Key)
				} else {
					discoveryEventsEmitted += len(events)
				}
			} else if ok, err := t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, entity.ID, string(snapJSON), entity.PollSeq); err != nil {
				trackerLog.Error("seed snapshot failed", "source_id", snap.Key, "error", err)
			} else if !ok {
				trackerLog.Warn("seed snapshot CAS lost race, skipping", "source_id", snap.Key)
			}
			if state.Description != "" {
				if _, err := t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, state.Description); err != nil {
					trackerLog.Error("seed description failed", "source_id", snap.Key, "error", err)
				}
			}
			if terminal(snap) {
				if _, err := t.entities.MarkClosedSystem(context.Background(), orgID, entity.ID); err != nil {
					trackerLog.Error("mark entity closed on discovery failed", "source_id", snap.Key, "error", err)
				}
			}
		} else {
			if entity.Title != snap.Summary {
				_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, entity.ID, snap.Summary)
			}
			if entity.Description != state.Description {
				_, _ = t.entities.UpdateDescriptionSystem(context.Background(), orgID, entity.ID, state.Description)
			}
			// Reactivate if a previously-closed issue reappears as open.
			if !terminal(snap) && entity.State == "closed" {
				if reactivated, err := t.entities.ReactivateSystem(context.Background(), orgID, entity.ID); err != nil {
					trackerLog.Error("reactivate entity failed", "source_id", snap.Key, "error", err)
				} else if reactivated {
					trackerLog.Info("reactivated entity (reopened)", "source_id", snap.Key)
				}
			}
		}
	}

	// Phase 2: Refresh
	entities, err := t.entities.ListActiveSystem(context.Background(), orgID, "jira")
	if err != nil {
		return 0, fmt.Errorf("list active jira entities: %w", err)
	}
	if len(entities) == 0 {
		// No entities to refresh, but still emit poll-complete so carry-over
		// readiness flips true on fresh-setup / empty-project cases.
		t.EmitPollComplete(ctx, "jira", startedAt, 0, 0)
		return 0, nil
	}

	keys := make([]string, len(entities))
	for i, e := range entities {
		keys[i] = e.SourceID
	}

	refreshed, err := t.batchFetchJira(ctx, client, baseURL, keys, projects)
	if err != nil {
		return 0, fmt.Errorf("batch fetch jira: %w", err)
	}

	// Phase 3: Diff + emit events. Network-free, like the GitHub twin.
	ctx, diffSpan := tracer.Start(ctx, "tracker.jira.diff_emit")

	diffEventsEmitted := 0
	staleReads := 0
	for _, e := range entities {
		newState, ok := refreshed[e.SourceID]
		if !ok {
			continue
		}
		newSnap := newState.Snap

		if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
			// Quiet-seed a snapshot-less row: a stub created
			// outside the poller (exec-touch FindOrCreate), or an entity
			// whose snapshot was cleared when an org admin paused this source.
			// DiffJiraSnapshots' prev.Key=="" branch would synthesize an initial
			// assigned/available/completed event for state that predates our
			// tracking — spuriously minting a task, and after a pause minting
			// one per known issue at once. Seed it like the discovery
			// create-branch (snapshot + title, close if terminal) WITHOUT
			// diffing instead. Normal discovery seeds in Phase 1, so this only
			// ever fires for rows that arrived without one. Description isn't carried by batchFetchJira;
			// a rediscovered stub picks it up from Phase 1's else-branch,
			// otherwise it fills in on a later discovery pass.
			snapJSON, _ := json.Marshal(newSnap)
			if ok, err := t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, e.ID, string(snapJSON), e.PollSeq); err != nil {
				trackerLog.Error("seed jira stub snapshot failed", "source_id", e.SourceID, "error", err)
			} else if !ok {
				trackerLog.Warn("seed jira stub snapshot CAS lost race, skipping", "source_id", e.SourceID)
			}
			if e.Title != newSnap.Summary {
				_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, e.ID, newSnap.Summary)
			}
			if terminal(newSnap) {
				if _, err := t.entities.MarkClosedSystem(context.Background(), orgID, e.ID); err != nil {
					trackerLog.Error("mark jira stub closed failed", "source_id", e.SourceID, "error", err)
				}
			}
			continue
		}

		var prevSnap domain.JiraSnapshot
		if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
			if err := json.Unmarshal([]byte(e.SnapshotJSON), &prevSnap); err != nil {
				trackerLog.Warn("corrupt jira snapshot, reseeding", "source_id", e.SourceID, "error", err)
				snapJSON, _ := json.Marshal(newSnap)
				_, _ = t.entities.UpdateSnapshotCASSystem(context.Background(), orgID, e.ID, string(snapJSON), e.PollSeq)
				continue
			}
		}

		// Drop a read that predates what we already hold, before it can reach
		// either the diff or the snapshot write. Jira only ever moves
		// `updated` forward, so a backwards read is the search index serving
		// state we have already superseded — never news. Warn rather than
		// Debug because that claim is the whole justification for
		// suppressing: if a read that WAS news ever gets dropped here, this
		// line is the bug report.
		if storedAt, fetchedAt, stale := jiraReadIsStale(prevSnap, newSnap); stale {
			staleReads++
			trackerLog.WarnContext(ctx, "jira read predates stored snapshot; suppressing this cycle's diff and snapshot write",
				"source_id", e.SourceID, "entity_id", e.ID,
				"stored_updated", storedAt.Format(time.RFC3339Nano),
				"fetched_updated", fetchedAt.Format(time.RFC3339Nano))
			continue
		}

		// Per-project Done.Members for this entity's project_key. Falls
		// back to the union across all projects when the entity is in
		// a project that's no longer configured (defensive — terminal
		// detection still works for previously-known done statuses).
		events := DiffJiraSnapshots(prevSnap, newSnap, e.ID, projects.doneMembersForKey(newSnap.Key))

		// Snapshot advance + the transitions diffed against it, one
		// transaction, CAS'd on e.PollSeq (the value this cycle's diff was
		// read against) — the GitHub arm's contract, same reasoning: the
		// snapshot-diff is the sole re-emit prevention, so half of this
		// landing is either a duplicate task (events off a snapshot that
		// didn't win) or a lost one (a snapshot that retired transitions
		// nobody recorded). On a miss or an error nothing was written and
		// the winner's next cycle re-diffs, so suppression loses nothing.
		snapJSON, _ := json.Marshal(newSnap)
		ok, err := t.emitWithSnapshotCAS(ctx, orgID, e.ID, string(snapJSON), e.PollSeq, events)
		if err != nil {
			trackerLog.Error("jira snapshot+events commit failed; suppressing this cycle's transitions (re-diffed next cycle)", "source_id", e.SourceID, "error", err)
			continue
		}
		if !ok {
			trackerLog.Warn("jira snapshot CAS lost race (stale poll_seq); suppressing this cycle's transitions", "source_id", e.SourceID)
			continue
		}
		diffEventsEmitted += len(events)

		// Best-effort, outside the transaction: display-only mirroring, so
		// a failure costs a stale title until the next cycle, never an event.
		if e.Title != newSnap.Summary {
			_, _ = t.entities.UpdateTitleSystem(context.Background(), orgID, e.ID, newSnap.Summary)
		}
		// Description intentionally not updated here — batchFetchJira
		// excludes the description field to save bandwidth, so newState's
		// description would be the empty-string parse result of an absent
		// field and writing it back would wipe the stored value. Description
		// is seeded and refreshed by phase 1 (discoverJira), which is the
		// only place that actually carries the field in the response.
	}

	diffSpan.SetAttributes(telemetry.Count(diffEventsEmitted))
	if staleReads > 0 {
		// A disposition rather than an error status: the cycle worked, it
		// declined to act on part of its input. Without it a cycle that
		// suppressed everything is indistinguishable from a quiet one.
		diffSpan.SetAttributes(telemetry.Disposition("stale_read_suppressed"))
	}
	diffSpan.End()
	eventsEmitted := discoveryEventsEmitted + diffEventsEmitted

	// Phase 4: confirm the long-unanswered keys against the issue endpoint.
	// Emits unreachable events, which the router turns into entity/task
	// closes. Ahead of the cycle log so its event count is the whole cycle's
	// — the same number the poll-complete sentinel carries, rather than a
	// second, quietly smaller one for the same cycle.
	retired := t.confirmMissingJiraEntities(ctx, client, orgID, entities, refreshed, time.Now())
	eventsEmitted += retired

	trackerLog.InfoContext(ctx, "jira refresh", "discovered", len(discovered), "entities", len(entities), "refreshed", len(refreshed), "events", eventsEmitted, "retired", retired, "stale_reads", staleReads)

	// Always fire the sentinel — it means "a poll cycle completed," not "a
	// poll produced work." Carry-over readiness depends on this firing even
	// on an empty first poll (e.g. projects configured but nothing assigned
	// yet), otherwise the setup step shimmers forever.
	t.EmitPollComplete(ctx, "jira", startedAt, len(entities), eventsEmitted)

	return eventsEmitted, nil
}

const (
	// jiraUnreachableGrace is how long a tracked key must go unanswered by
	// the refresh before the tracker spends a request asking Jira about it
	// directly.
	//
	// The grace is the whole safety margin. A key's absence from one search
	// is weak evidence — an index that hasn't caught up, a transient
	// visibility change, or a paging bug all present identically — and the
	// event this pass can emit closes the entity and every task on it. Many
	// consecutive misses across an hour is not proof either, which is why
	// the pass confirms rather than concludes; the grace is only there so
	// the confirmation is spent on keys that look durably unanswered instead
	// of on every blip.
	//
	// Wall-clock rather than a cycle count because the poll interval is the
	// user's to set: an hour is an hour whether that is six cycles or sixty.
	jiraUnreachableGrace = time.Hour

	// jiraUnreachableProbeBudget caps confirmations per cycle. These are one
	// request per key on top of a cycle that has already done its batch
	// reads, and the population they draw from is unbounded — a whole
	// project's worth of keys can go missing at once when a project is
	// deleted or a credential's visibility narrows.
	//
	// Deferred keys are not dropped. Candidates come off a list ordered
	// oldest-last_polled_at-first, and every confirmation that reaches a
	// verdict advances that column — a 404 by retiring the entity, a 200 by
	// stamping it — so each cycle's budget lands on keys the previous
	// cycles did not reach, and a backlog drains over several cycles rather
	// than arriving as one burst of API calls. The exception is a key whose
	// confirmation keeps erroring: it stays at the head of the queue and is
	// retried every cycle, which is the right behaviour for a transient
	// fault and self-limiting for a persistent one (nothing behind it could
	// have been confirmed by the same broken endpoint either).
	jiraUnreachableProbeBudget = 20
)

// confirmMissingJiraEntities asks Jira directly about tracked entities the
// refresh has not answered for in a while, and emits jira:issue:unreachable for
// the keys Jira will no longer resolve. Returns the number of events emitted.
//
// This exists because the refresh cannot retire anything on its own. A key
// missing from a `key IN (...)` result is skipped by the diff loop, so the
// entity keeps its last snapshot and emits nothing — for as long as it takes
// someone to notice, which for a durable entity is forever. Closing on that
// signal alone would be wrong in the other direction: absence from a search is
// equally consistent with an issue that is merely unindexed, archived, or newly
// invisible to the credential, and closing those would destroy live work.
//
// Asking about the one key settles it, though not into the answer one might
// want: a 404 says only that this credential cannot resolve this key, because
// Jira answers the same way for an issue that was deleted and one it will not
// admit exists. Both make the entity untrackable, which is what the event
// records and all it claims. A 200 is the useful negative — the issue resolves,
// so something upstream of the diff is failing to return it — logged loudly,
// stamped so it stops consuming the budget, and otherwise left alone. Any other
// error is not evidence in either direction.
func (t *Tracker) confirmMissingJiraEntities(ctx context.Context, client *jiraclient.Client, orgID string, entities []domain.Entity, refreshed map[string]jiraIssueState, now time.Time) int {
	var candidates []domain.Entity
	for _, e := range entities {
		if _, answered := refreshed[e.SourceID]; answered {
			continue
		}
		// LastPolledAt advances on every successful refresh write and is
		// stamped at creation, so its age IS the "how long has this key gone
		// unanswered" clock — no separate miss counter to keep, and nothing
		// to lose across a restart or a change of leader. A nil value predates
		// the column's population and says nothing about recency, so it waits
		// for the next successful refresh to give it a reading.
		if e.LastPolledAt == nil || now.Sub(*e.LastPolledAt) < jiraUnreachableGrace {
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
	rekeyed := 0
	merged := 0
	for i, e := range candidates {
		if i >= jiraUnreachableProbeBudget {
			span.SetAttributes(telemetry.Outcome("partial"))
			trackerLog.InfoContext(ctx, "jira reachability confirmation budget spent; remaining keys re-checked next cycle",
				"budget", jiraUnreachableProbeBudget, "deferred", len(candidates)-i)
			break
		}
		if ctx.Err() != nil {
			return emitted
		}

		issue, err := client.GetIssue(ctx, e.SourceID)
		switch {
		case err == nil:
			if issue.Key != "" && issue.Key != e.SourceID {
				survivorID, wasMerged, rekeyErr := t.entities.RekeyOrMergeSystem(ctx, orgID, e.ID, issue.Key)
				if rekeyErr != nil {
					trackerLog.WarnContext(ctx, "repairing moved jira issue key failed; entity stays a confirmation candidate",
						"old_source_id", e.SourceID, "new_source_id", issue.Key, "entity_id", e.ID, "error", rekeyErr)
					continue
				}
				if wasMerged {
					merged++
				} else {
					rekeyed++
				}
				// Deliberately silent in the event stream: Jira changed the
				// issue's address, not its work state. A plain re-key also clears
				// project classification in the store, since a cross-project move
				// invalidates the old classification. We follow the current key
				// even when its destination project is not configured; entities
				// are durable and leaving the discovery set is not retirement.
				trackerLog.InfoContext(ctx, "followed moved jira issue to its current key",
					"old_source_id", e.SourceID, "new_source_id", issue.Key,
					"entity_id", e.ID, "survivor_entity_id", survivorID, "merged", wasMerged)
				continue
			}
			// Confirmed present, and yet the refresh didn't return it. The
			// entity is being skipped every cycle by something other than
			// the key being unresolvable — an unindexed or archived issue, or one
			// the credential can no longer see through search. Nothing here
			// can repair that, but an entity silently frozen is exactly what
			// this pass exists to stop being invisible.
			confirmedWithoutSearch++
			// Stamp the read. Candidates are selected by how stale this
			// column is and drawn oldest-first against a per-cycle budget,
			// so an entity that will confirm present on every future pass
			// would otherwise sit at the head of that queue forever, consume
			// the budget each cycle, and starve every candidate behind it —
			// including ones that would have confirmed unreachable. Honest as
			// as necessary: the row *was* just read from the source, which
			// is what the column records; nothing was diffed off it, which
			// is why this is not a snapshot write.
			if err := t.entities.MarkPolledSystem(ctx, orgID, e.ID); err != nil {
				trackerLog.WarnContext(ctx, "stamping a confirmed-present jira entity failed; it stays a confirmation candidate",
					"source_id", e.SourceID, "entity_id", e.ID, "error", err)
			}
		case jiraclient.IsNotFound(err):
			t.emitJiraUnreachable(ctx, orgID, e)
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
	if rekeyed+merged > 0 {
		span.SetAttributes(telemetry.Disposition("issue_keys_repaired"), telemetry.Attempt(rekeyed+merged))
	}
	if emitted > 0 {
		// A disposition rather than a second Count — Count is one key, and the
		// count worth keeping on this span is how many keys it examined, not
		// how many it retired. A pass that retires anything is the rare case;
		// this is what makes it findable.
		span.SetAttributes(telemetry.Disposition("entities_retired"), telemetry.Attempt(emitted))
	}
	return emitted
}

// emitJiraUnreachable publishes the terminal event for an entity whose key Jira
// will no longer resolve. Every metadata field is last-known state off the
// stored snapshot — the source has nothing left to read — and a snapshot that
// is absent or unparseable still emits, with blank fields: the entity has to be
// retired either way, and a corrupt snapshot is not a reason to keep tracking
// something that can no longer be read.
//
// Publish, not the snapshot-CAS enqueue: there is no new snapshot to advance,
// and the entity's own close is the router's job (the event terminates it).
func (t *Tracker) emitJiraUnreachable(ctx context.Context, orgID string, e domain.Entity) {
	var snap domain.JiraSnapshot
	if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			trackerLog.WarnContext(ctx, "corrupt jira snapshot on an unreachable issue; emitting with last-known fields blank",
				"source_id", e.SourceID, "entity_id", e.ID, "error", err)
		}
	}
	entityID := e.ID
	trackerLog.InfoContext(ctx, "jira will not resolve this key (deleted, or no longer visible to the credential); retiring entity",
		"source_id", e.SourceID, "entity_id", e.ID)
	t.publish(ctx, domain.Event{
		OrgID:     orgID,
		EventType: domain.EventJiraIssueUnreachable,
		EntityID:  &entityID,
		MetadataJSON: mustJSON(events.JiraIssueUnreachableMetadata{
			Assignee:          snap.Assignee,
			AssigneeAccountID: snap.AssigneeAccountID,
			IssueKey:          e.SourceID,
			Project:           extractProject(e.SourceID),
			IssueType:         snap.IssueType,
			LastStatus:        snap.Status,
			Summary:           snap.Summary,
		}),
		// occurred_at is deliberately left zero — Jira reports that a key does
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

	// "updated" is required for the diff layer's source-time fallback —
	// without it, JiraSnapshot.UpdatedAt is empty and emit() degrades all
	// the way to detection time. Added explicitly here because this
	// callsite passes a custom field list rather than relying on
	// DefaultSearchFields.
	fields := []string{"summary", "description", "status", "assignee", "priority", "labels", "issuetype", "parent", "comment", "subtasks", "created", "updated"}

	// Live workflows, fetched only when a query has already failed and cached
	// for the rest of the cycle so two failed queries on one project cost one
	// call. Steady state never touches this.
	liveStatuses := map[string][]domain.JiraStatusRef{}

	failed, salvaged := 0, 0
	for _, q := range queries {
		issues, err := client.SearchIssues(ctx, q.jql, fields, 100)
		if err != nil && q.build != nil {
			if jql, dropped := t.salvageJiraQuery(ctx, client, q.projectKey, q.members, q.build, liveStatuses); jql != "" {
				trackerLog.WarnContext(ctx, "jira discovery query rebuilt without statuses the workflow no longer has",
					"project", q.projectKey, "dropped", domain.JiraStatusNames(dropped), "error", err)
				issues, err = client.SearchIssues(ctx, jql, fields, 100)
				if err == nil {
					salvaged++
				}
			}
		}
		if err != nil {
			// One project's query failing must not sink the others, so
			// this continues — which means the caller gets a short result
			// with no indication why. The outcome below is that indication.
			failed++
			trackerLog.ErrorContext(ctx, "jira discovery query failed", "project", q.projectKey, "error", err)
			continue
		}
		for _, issue := range issues {
			if !seen[issue.Key] {
				seen[issue.Key] = true
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
			trackerLog.WarnContext(ctx, "jira workflow read failed; cannot tell whether the query names a dead status",
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

// batchFetchJira fetches current state for tracked Jira issues. Description
// is deliberately excluded from the field list — it's seeded on discovery
// and only relevant to the scorer, which reads from the stored column rather
// than the API response. Skipping the multi-KB body on every poll saves
// bandwidth and latency; the tradeoff is that descriptions for entities
// that stop matching discovery's JQL (e.g. reassigned to someone else) stay
// pinned at their last-captured value. Acceptable — description relevance
// drops fast once a ticket is off the user's plate.
func (t *Tracker) batchFetchJira(ctx context.Context, client *jiraclient.Client, baseURL string, keys []string, projects JiraRules) (map[string]jiraIssueState, error) {
	// Serial, with one iteration per batch of tracked issues — so its cost
	// grows with every issue TF tracks, making it the cycle's most likely
	// creeping regression. The per-request spans underneath are each fast,
	// so nothing else would show it.
	ctx, span := tracer.Start(ctx, "tracker.jira.batch_fetch",
		trace.WithAttributes(telemetry.Count(len(keys))))
	defer span.End()

	results := make(map[string]jiraIssueState, len(keys))
	// "updated" is required for the diff layer's source-time fallback.
	// See the comment on the discovery field list for context.
	fields := []string{"summary", "status", "assignee", "priority", "labels", "issuetype", "parent", "comment", "subtasks", "created", "updated"}

	allDone := projects.AllDoneMembers()

	for i := 0; i < len(keys); i += jiraBatchSize {
		end := i + jiraBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]

		jql := fmt.Sprintf("key IN (%s)", strings.Join(batch, ", "))
		issues, err := client.SearchIssues(ctx, jql, fields, jiraBatchSize)
		if err != nil {
			span.SetStatus(codes.Error, "batch fetch")
			return nil, fmt.Errorf("batch fetch keys %d-%d: %w", i, end, err)
		}

		for _, issue := range issues {
			// Subtask classification uses the union of every project's
			// done members — subtasks can live in projects other than
			// the parent's.
			results[issue.Key] = issueToState(issue, baseURL, allDone)
		}
	}

	// A tracked key that comes back in no page — deleted, moved to another
	// key, or no longer visible to the service credential — is skipped by the
	// diff loop, so the entity holds its last snapshot and emits nothing,
	// indefinitely and silently. Nothing here retires it (a durable entity is
	// the user's to dismiss, not a poller's to purge), but it is said out loud,
	// because a truncated page would masquerade as exactly this: with the gap
	// logged, a paging bug shows up as a log line rather than as entities that
	// quietly stop moving.
	if missing := missingJiraKeys(keys, results); len(missing) > 0 {
		span.SetAttributes(telemetry.Outcome("partial"))
		trackerLog.WarnContext(ctx, "jira batch fetch returned no row for tracked keys",
			"missing", len(missing), "tracked", len(keys),
			"keys", strings.Join(missing[:min(len(missing), jiraMissingKeySample)], ", "))
	}

	return results, nil
}

// jiraMissingKeySample bounds how many absent keys the gap warning names. The
// count is the signal; the keys are there to start an investigation, and a
// hundred of them in one line would bury it.
const jiraMissingKeySample = 10

// missingJiraKeys returns the requested keys that the batch fetch produced no
// state for, in request order. A moved issue answers under its new key, so it
// shows up here as absent rather than as a silent substitution.
func missingJiraKeys(keys []string, results map[string]jiraIssueState) []string {
	var missing []string
	for _, k := range keys {
		if _, ok := results[k]; !ok {
			missing = append(missing, k)
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
		Key:     issue.Key,
		Summary: issue.Fields.Summary,
		URL:     fmt.Sprintf("%s/browse/%s", strings.TrimRight(baseURL, "/"), issue.Key),
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
