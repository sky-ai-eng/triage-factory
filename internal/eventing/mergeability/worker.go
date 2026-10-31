// Package mergeability is the probe worker: the only reader of a pull
// request's mergeability and the only writer of pull_request_mergeability.
//
// The poll admits probes onto pr_mergeability_probes (a work kind under
// internal/db/workitem, workkinds.PRMergeabilityProbes) when a pull request's
// mergeability can have changed. The worker claims the ripe ones per org,
// reads each repository's batch in one GraphQL query, and records every answer
// in its own transaction, which also enqueues the conflict event the answer's
// transition implies and completes the probe. Nothing in it depends on who
// admitted a probe.
package mergeability

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

var (
	log    = logging.Component("mergeability")
	tracer = otel.Tracer("internal/eventing/mergeability")
)

// Timing. The scan is the worker's only schedule: a probe is never ripe
// sooner than workkinds.PRMergeabilityProbeDelay after its admission, so
// there is nothing for a wake to win. The prune keeps the queue's done rows
// for a day, enough to read back what the worker did; parked rows are kept
// for an operator.
const (
	DefaultScanInterval  = 5 * time.Second
	DefaultPruneInterval = time.Hour
	DefaultPruneAge      = 24 * time.Hour

	// terminalWriteTimeout bounds one probe's completion or requeue, on a
	// context of its own so a read that ran out its deadline can still
	// record what it learned.
	terminalWriteTimeout = 10 * time.Second
)

// probeKind is the kind the worker's timing comes from. The dialect is
// irrelevant to the policy, which is all the worker reads.
var probeKind = workkinds.PRMergeabilityProbes(workitem.SQLite)

// OrgLister lists the orgs the worker claims for.
type OrgLister interface {
	ListActiveSystem(ctx context.Context) ([]string, error)
}

// ClientResolver resolves the GitHub client for one repository.
// ghclient.Resolver satisfies it.
type ClientResolver interface {
	ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error)
}

// Publisher forwards an event the store already committed to the durable
// queue: the bus fan-out and the drain worker's nudge, never a second
// enqueue. The poll's ingestor satisfies it.
type Publisher interface {
	PublishPreEnqueued(ctx context.Context, evt domain.Event)
}

// Worker drains pr_mergeability_probes. Exactly one process runs it at a
// time, which is what makes it a brain component.
type Worker struct {
	store    db.PRMergeabilityStore
	orgs     OrgLister
	resolver ClientResolver
	pub      Publisher
	owner    workitem.Owner
}

// NewWorker builds the worker. owner is the process identity every claim is
// stamped with: provenance, not a fence.
func NewWorker(store db.PRMergeabilityStore, orgs OrgLister, resolver ClientResolver, pub Publisher, owner workitem.Owner) *Worker {
	return &Worker{store: store, orgs: orgs, resolver: resolver, pub: pub, owner: owner}
}

// RunMergeabilityProbes is the worker's loop: a drain at start, a drain on
// every scan tick, and a prune of settled probes on its own tick. Returns
// when ctx is cancelled.
func (w *Worker) RunMergeabilityProbes(ctx context.Context, scanInterval time.Duration) {
	scan := time.NewTicker(scanInterval)
	defer scan.Stop()
	prune := time.NewTicker(DefaultPruneInterval)
	defer prune.Stop()

	drain := func() {
		if err := w.Drain(ctx); err != nil {
			log.WarnContext(ctx, "mergeability probes: drain failed; the next scan retries", "error", err)
		}
	}
	drain()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			drain()
		case <-prune.C:
			if n, err := w.store.PruneSettled(ctx, time.Now().Add(-DefaultPruneAge)); err != nil {
				log.WarnContext(ctx, "mergeability probes: prune failed", "error", err)
			} else if n > 0 {
				log.DebugContext(ctx, "mergeability probes: pruned settled probes", "count", n)
			}
		}
	}
}

// Drain claims and probes every ripe probe of every active org, a batch of up
// to ghclient.MaxMergeabilityBatch at a time per org, until each org's queue
// is drained or ctx is cancelled. It returns the first error that stopped an
// org's drain; the other orgs are still drained.
func (w *Worker) Drain(ctx context.Context) error {
	orgs, err := w.orgs.ListActiveSystem(ctx)
	if err != nil {
		return fmt.Errorf("list active orgs: %w", err)
	}
	var first error
	for _, orgID := range orgs {
		if err := w.drainOrg(ctx, orgID); err != nil && first == nil {
			first = fmt.Errorf("org %s: %w", orgID, err)
		}
	}
	return first
}

func (w *Worker) drainOrg(ctx context.Context, orgID string) error {
	for {
		if ctx.Err() != nil {
			return nil // a clean stop, not a failure
		}
		batch, err := w.store.Claim(ctx, w.owner, orgID, ghclient.MaxMergeabilityBatch)
		if batch.Cancelled+batch.Parked > 0 {
			log.InfoContext(ctx, "mergeability probes: claim settled probes", "org_id", orgID, "cancelled", batch.Cancelled, "parked", batch.Parked)
		}
		// Leases committed before a claim error are real, and their probes
		// are this worker's to dispose of before it reports the failure.
		w.probe(ctx, orgID, batch.Items)
		if err != nil {
			return err
		}
		if len(batch.Items) < ghclient.MaxMergeabilityBatch {
			return nil
		}
	}
}

// repoBatch is the probes of one repository: one GraphQL query.
type repoBatch struct {
	repositoryID, owner, repo string
	items                     []db.ClaimedPRMergeabilityProbe
}

// probe reads one claimed batch. A probe whose pull request is gone, closed
// or has no node id is completed with no read; the rest are grouped by the
// repository registry row their entity references, since a client is
// resolved per repository.
func (w *Worker) probe(ctx context.Context, orgID string, items []db.ClaimedPRMergeabilityProbe) {
	byRepo := map[string]*repoBatch{}
	for _, item := range items {
		if item.Receipt.Reclaimed {
			log.WarnContext(ctx, "mergeability probes: reclaimed a probe whose lease expired without a terminal write",
				"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "previous_owner", item.Receipt.PreviousOwner)
		}
		if !item.Active || item.NodeID == "" || item.RepositoryID == "" || item.Owner == "" || item.Repo == "" {
			w.complete(ctx, item, db.PRMergeabilityAnswer{}, "")
			continue
		}
		b, ok := byRepo[item.RepositoryID]
		if !ok {
			b = &repoBatch{repositoryID: item.RepositoryID, owner: item.Owner, repo: item.Repo}
			byRepo[item.RepositoryID] = b
		}
		b.items = append(b.items, item)
	}
	repos := make([]string, 0, len(byRepo))
	for id := range byRepo {
		repos = append(repos, id)
	}
	sort.Strings(repos)
	for _, id := range repos {
		w.probeRepo(ctx, orgID, byRepo[id])
	}
}

// probeRepo renews the batch's leases, reads the repository's pull requests
// in one query, and records each answer. The read runs under the kind's
// UnitDeadline on ctx with its cancellation dropped: a demoted brain finishes
// the batch it holds rather than abandoning leases the next holder would only
// wait out.
func (w *Worker) probeRepo(ctx context.Context, orgID string, b *repoBatch) {
	ctx, span := tracer.Start(context.WithoutCancel(ctx), "mergeability.probe_batch",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(telemetry.OrgID(orgID), attribute.String("repository_id", b.repositoryID), telemetry.Count(len(b.items))))
	defer span.End()

	// Renewal is the fence check before the read, and where a cancellation
	// request is observed. A batch claimed behind another repository's slow
	// read may otherwise reach GitHub on a lease about to lapse.
	held := b.items[:0]
	for _, item := range b.items {
		r, err := w.store.RenewLease(ctx, item.Receipt)
		switch {
		case err == nil:
			item.Receipt = r
			held = append(held, item)
		case errors.Is(err, workitem.ErrLeaseLost), errors.Is(err, workitem.ErrCancelled):
			log.InfoContext(ctx, "mergeability probes: probe not held at renewal", "probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
		default:
			log.WarnContext(ctx, "mergeability probes: lease renewal failed; the probe is reclaimed after its lease expires",
				"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
		}
	}
	if len(held) == 0 {
		return
	}

	defer func() {
		if rec := recover(); rec != nil {
			// A panic must not kill the single worker goroutine; the probes
			// it held go back with the outcome that says why.
			span.SetStatus(codes.Error, "panic")
			log.ErrorContext(ctx, "mergeability probes: panic probing a repository", "repository_id", b.repositoryID, "panic", rec)
			w.requeueAll(ctx, held, workitem.OutcomePoisonSuspected, fmt.Errorf("panic: %v", rec))
		}
	}()

	client, err := w.resolver.ClientForRepo(ctx, orgID, b.owner, b.repo)
	if err != nil {
		span.SetStatus(codes.Error, "resolve client")
		log.WarnContext(ctx, "mergeability probes: no GitHub client for the repository", "repository_id", b.repositoryID, "error", err)
		w.requeueAll(ctx, held, workitem.OutcomeDependencyDown, err)
		return
	}

	nodeIDs := make([]string, len(held))
	for i, item := range held {
		nodeIDs[i] = item.NodeID
		recordProbe(item.Reason)
	}
	readCtx, cancel := context.WithTimeout(ctx, probeKind.Policy.UnitDeadline)
	reads, err := client.PRMergeability(readCtx, nodeIDs)
	cancel()
	if err != nil {
		outcome := workitem.OutcomeTransient
		var limited *ghclient.ErrRateLimited
		switch {
		case errors.As(err, &limited):
			outcome = workitem.OutcomeDependencyDown
		case errors.Is(err, context.DeadlineExceeded):
			outcome = workitem.OutcomeDeadline
		}
		span.SetStatus(codes.Error, "read")
		log.WarnContext(ctx, "mergeability probes: read failed; the probes retry with backoff", "repository_id", b.repositoryID, "outcome", string(outcome), "error", err)
		w.requeueAll(ctx, held, outcome, err)
		return
	}

	// Every event an answer enqueues is stamped with this batch's span, so
	// its routing links back to the read that found it.
	traceparent := telemetry.TraceparentFrom(ctx)
	for _, item := range held {
		read, found := reads[item.NodeID]
		if found {
			recordAnswer(read.Mergeable)
		}
		w.complete(ctx, item, db.PRMergeabilityAnswer{Found: found, Mergeable: read.Mergeable, HeadSHA: read.HeadSHA}, traceparent)
	}
}

// complete records one answer and forwards its events once they committed.
// The loser contract is every work kind's: ErrLeaseLost logs and stops, since
// nothing landed and the probe's next holder reads again; ErrCancelled logs;
// any other failure returns the probe with backoff.
func (w *Worker) complete(ctx context.Context, item db.ClaimedPRMergeabilityProbe, answer db.PRMergeabilityAnswer, traceparent string) {
	termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
	defer cancel()
	res, err := w.store.CompleteProbe(termCtx, item.Receipt, answer, traceparent)
	switch {
	case err == nil:
	case errors.Is(err, workitem.ErrLeaseLost):
		log.InfoContext(termCtx, "mergeability probes: lease lost at completion; nothing landed and the next holder reads again",
			"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID)
		return
	case errors.Is(err, workitem.ErrCancelled):
		log.InfoContext(termCtx, "mergeability probes: probe settled cancelled at completion", "probe_id", item.Receipt.ItemID, "entity_id", item.EntityID)
		return
	default:
		log.WarnContext(termCtx, "mergeability probes: completion failed; the probe retries with backoff",
			"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
		w.requeue(termCtx, item, workitem.OutcomeTransient, err)
		return
	}

	switch res.Outcome {
	case db.PRMergeabilityGaveUp:
		recordUnknownGiveUp()
		log.InfoContext(termCtx, "mergeability probes: GitHub answered UNKNOWN on every retry; the sweep reads the pull request later",
			"entity_id", item.EntityID)
	case db.PRMergeabilityRecorded:
		if item.Reason == domain.MergeabilityProbeSweep && res.Previous != "" && res.Row != nil && res.Previous != res.Row.Mergeable {
			recordSweepChange()
		}
	}
	for _, evt := range res.Events {
		w.pub.PublishPreEnqueued(termCtx, evt)
	}
}

func (w *Worker) requeueAll(ctx context.Context, items []db.ClaimedPRMergeabilityProbe, outcome workitem.Outcome, cause error) {
	for _, item := range items {
		termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
		w.requeue(termCtx, item, outcome, cause)
		cancel()
	}
}

func (w *Worker) requeue(ctx context.Context, item db.ClaimedPRMergeabilityProbe, outcome workitem.Outcome, cause error) {
	parked, err := w.store.Requeue(ctx, item.Receipt, outcome, cause)
	switch {
	case err == nil && parked:
		// A parked probe keeps its pull request's key, so the poll's later
		// admissions collapse into it until an operator redrives it.
		log.WarnContext(ctx, "mergeability probes: parked a probe; the pull request's mergeability is not read again until an operator redrives it",
			"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "outcome", string(outcome), "cause", cause)
	case err == nil:
	case errors.Is(err, workitem.ErrLeaseLost), errors.Is(err, workitem.ErrCancelled):
		log.InfoContext(ctx, "mergeability probes: probe not held at requeue", "probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
	default:
		log.ErrorContext(ctx, "mergeability probes: requeue failed; the probe is reclaimed after its lease expires",
			"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
	}
}
