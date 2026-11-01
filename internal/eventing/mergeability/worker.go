// Package mergeability is the probe worker: the only reader of a pull
// request's mergeability and the only writer of the answer columns of
// pull_request_mergeability.
//
// The poll admits probes onto pr_mergeability_probes (a work kind under
// internal/db/workitem, workkinds.PRMergeabilityProbes) when a pull request's
// mergeability can have changed. The worker claims the ripe ones per org,
// reads the ones that share a GitHub credential in one GraphQL query, and
// records every answer in its own transaction, which also enqueues the
// conflict event the answer's transition implies and completes the probe.
// Nothing in it depends on who admitted a probe.
package mergeability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// time, which is what makes it a brain component, and one goroutine drives
// it: Drain is not safe for concurrent use.
type Worker struct {
	store    db.PRMergeabilityStore
	orgs     OrgLister
	resolver ClientResolver
	pub      Publisher
	owner    workitem.Owner
	// limited is when each credential GitHub rate-limited may be read again.
	limited map[limitKey]time.Time
}

// NewWorker builds the worker. owner is the process identity every claim is
// stamped with: provenance, not a fence.
func NewWorker(store db.PRMergeabilityStore, orgs OrgLister, resolver ClientResolver, pub Publisher, owner workitem.Owner) *Worker {
	return &Worker{store: store, orgs: orgs, resolver: resolver, pub: pub, owner: owner, limited: map[limitKey]time.Time{}}
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

// repoProbes is the probes of one repository, which share the client
// resolved for it.
type repoProbes struct {
	repositoryID, owner, repo string
	items                     []db.ClaimedPRMergeabilityProbe
}

// credentialBatch is the probes whose repositories resolved to one
// credential: one GraphQL query, read through the first of their clients.
type credentialBatch struct {
	credential string
	client     *ghclient.Client
	items      []db.ClaimedPRMergeabilityProbe
}

// errResolvePanic marks a client resolution that panicked, which returns its
// probes as poison rather than as a dependency being down.
var errResolvePanic = errors.New("panic resolving a GitHub client")

// probe reads one claimed batch. A probe whose pull request is gone, closed
// or has no node id is completed with no read. The rest are resolved to a
// client a repository at a time, since an App installation's grant decides
// per repository whether the installation may read it, and are then read a
// credential at a time: nodes(ids:) takes ids from any repositories the
// credential can read, so the probes that share one cost one query whatever
// repositories they are in. A claimed batch is at most
// ghclient.MaxMergeabilityBatch probes, so no credential's share of it needs
// more than one query.
func (w *Worker) probe(ctx context.Context, orgID string, items []db.ClaimedPRMergeabilityProbe) {
	byRepo := map[string]*repoProbes{}
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
			b = &repoProbes{repositoryID: item.RepositoryID, owner: item.Owner, repo: item.Repo}
			byRepo[item.RepositoryID] = b
		}
		b.items = append(b.items, item)
	}

	byCredential := map[string]*credentialBatch{}
	for _, id := range sortedKeys(byRepo) {
		b := byRepo[id]
		client, err := w.resolve(ctx, orgID, b)
		switch {
		case err == nil:
		case errors.Is(err, errResolvePanic):
			log.WarnContext(ctx, "mergeability probes: no GitHub client for the repository", "repository_id", b.repositoryID, "error", err)
			w.requeueAll(ctx, b.items, workitem.OutcomePoisonSuspected, err)
			continue
		case errors.Is(err, ghclient.ErrNoGitHubCredentials):
			// No credential the org holds reaches the repository: a
			// repository outside the App's grant, say, which the org's
			// GitHub access settings report as scope drift. That holds until
			// someone changes the configuration, so retrying cannot help;
			// the probes drop now, and the poll admits them again each cycle
			// until it is fixed.
			log.InfoContext(ctx, "mergeability probes: no credential reaches the repository; its probes are dropped",
				"repository_id", b.repositoryID, "probes", len(b.items), "error", err)
			for _, item := range b.items {
				termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
				w.drop(termCtx, item, dropNoCredential, err)
				cancel()
			}
			continue
		default:
			log.WarnContext(ctx, "mergeability probes: no GitHub client for the repository", "repository_id", b.repositoryID, "error", err)
			w.failReads(ctx, b.items, workitem.OutcomeDependencyDown, err)
			continue
		}
		g, ok := byCredential[client.Credential()]
		if !ok {
			g = &credentialBatch{credential: client.Credential(), client: client}
			byCredential[g.credential] = g
		}
		g.items = append(g.items, b.items...)
	}
	for _, credential := range sortedKeys(byCredential) {
		w.read(ctx, orgID, byCredential[credential])
	}
}

// resolve is ClientForRepo for one repository's probes, with a panic turned
// into errResolvePanic so it cannot kill the single worker goroutine.
func (w *Worker) resolve(ctx context.Context, orgID string, b *repoProbes) (client *ghclient.Client, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			client, err = nil, fmt.Errorf("%w: %v", errResolvePanic, rec)
		}
	}()
	return w.resolver.ClientForRepo(ctx, orgID, b.owner, b.repo)
}

// read renews one credential's probes, reads them in one query, and records
// each answer. The read runs under the kind's UnitDeadline on ctx with its
// cancellation dropped: a demoted brain finishes the batch it holds rather
// than abandoning leases the next holder would only wait out.
//
// A credential GitHub rate-limited is not read again until the limit resets:
// the read that was refused and every later one on that credential defer
// their probes to the reset, the later ones without a request, and a
// deferral spends none of a probe's attempts.
func (w *Worker) read(ctx context.Context, orgID string, g *credentialBatch) {
	ctx, span := tracer.Start(context.WithoutCancel(ctx), "mergeability.probe_batch",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(telemetry.OrgID(orgID), attribute.String("credential", g.credential), telemetry.Count(len(g.items))))
	defer span.End()

	key := limitKey{orgID: orgID, credential: g.credential}
	if until, ok := w.limited[key]; ok {
		if time.Now().Before(until) {
			span.SetAttributes(telemetry.Outcome("rate_limited"))
			w.deferAll(ctx, g.items, until)
			return
		}
		delete(w.limited, key)
	}

	// Renewal is the fence check before the read, and where a cancellation
	// request is observed. A batch claimed behind another credential's slow
	// read may otherwise reach GitHub on a lease about to lapse.
	held := g.items[:0]
	for _, item := range g.items {
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
			// A panic must not kill the single worker goroutine. Each
			// completion recovers its own, so one here is in the read (or the
			// loop around the completions), and every probe it held goes back
			// with the outcome that says why; one already completed answers
			// the requeue with a lost lease.
			span.SetStatus(codes.Error, "panic")
			log.ErrorContext(ctx, "mergeability probes: panic reading a credential's probes", "credential", g.credential, "panic", rec)
			w.requeueAll(ctx, held, workitem.OutcomePoisonSuspected, fmt.Errorf("panic: %v", rec))
		}
	}()

	nodeIDs := make([]string, len(held))
	for i, item := range held {
		nodeIDs[i] = item.NodeID
		recordProbe(item.Reason)
	}
	readCtx, cancel := context.WithTimeout(ctx, probeKind.Policy.UnitDeadline)
	reads, err := g.client.PRMergeability(readCtx, nodeIDs)
	cancel()
	if err != nil {
		if until, ok := rateLimitedUntil(err); ok {
			w.limited[key] = until
			span.SetAttributes(telemetry.Outcome("rate_limited"))
			log.WarnContext(ctx, "mergeability probes: GitHub rate-limited the read; the credential's probes wait for the reset",
				"credential", g.credential, "probes", len(held), "resume_at", until, "error", err)
			w.deferAll(ctx, held, until)
			return
		}
		outcome := workitem.OutcomeTransient
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = workitem.OutcomeDeadline
		}
		span.SetStatus(codes.Error, "read")
		log.WarnContext(ctx, "mergeability probes: read failed; the probes retry with backoff", "credential", g.credential, "outcome", string(outcome), "error", err)
		w.failReads(ctx, held, outcome, err)
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
		w.complete(ctx, item, db.PRMergeabilityAnswer{Found: found, Mergeable: read.Mergeable, HeadSHA: read.HeadSHA, BaseRef: read.BaseRef}, traceparent)
	}
}

// limitKey names one credential's rate-limit budget: GitHub meters an App
// installation or a token on its own, and an org's credentials are its own.
type limitKey struct {
	orgID, credential string
}

// minRateLimitWait is the shortest wait a rate limit defers probes for, so
// a reset GitHub gave as now, or a refusal that named no reset, is not read
// again at once.
const minRateLimitWait = time.Minute

// rateLimitedUntil reports when a read that failed with err may be sent again
// on its credential, when the failure was a rate limit. The client returns
// ErrRateLimited for every rate limit it does not wait out, a wait that would
// outlast the read's deadline included, so that is the only error to look
// for.
func rateLimitedUntil(err error) (time.Time, bool) {
	var limited *ghclient.ErrRateLimited
	if !errors.As(err, &limited) {
		return time.Time{}, false
	}
	until := limited.ResumeAt
	if floor := time.Now().Add(minRateLimitWait); until.Before(floor) {
		until = floor
	}
	return until, true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// complete records one answer and forwards its events once they committed.
// The loser contract is every work kind's: ErrLeaseLost logs and stops, since
// nothing landed and the probe's next holder reads again; ErrCancelled logs;
// any other failure returns the probe with backoff. A panic returns this
// probe alone as poison, so the probes completed after it in the same batch
// are not charged for it.
func (w *Worker) complete(ctx context.Context, item db.ClaimedPRMergeabilityProbe, answer db.PRMergeabilityAnswer, traceparent string) {
	termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			log.ErrorContext(termCtx, "mergeability probes: panic completing a probe", "probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "panic", rec)
			w.requeue(termCtx, item, workitem.OutcomePoisonSuspected, fmt.Errorf("panic: %v", rec))
		}
	}()
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
		log.InfoContext(termCtx, "mergeability probes: GitHub answered UNKNOWN on every retry; the poll starts a fresh round of reads later",
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

// deferAll returns probes to ready until a rate limit resets, with the
// attempt each claim charged refunded.
func (w *Worker) deferAll(ctx context.Context, items []db.ClaimedPRMergeabilityProbe, until time.Time) {
	for _, item := range items {
		termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
		err := w.store.DeferRateLimited(termCtx, item.Receipt, until)
		cancel()
		switch {
		case err == nil:
		case errors.Is(err, workitem.ErrLeaseLost), errors.Is(err, workitem.ErrCancelled):
			log.InfoContext(ctx, "mergeability probes: probe not held at deferral", "probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
		default:
			log.ErrorContext(ctx, "mergeability probes: deferral failed; the probe is reclaimed after its lease expires",
				"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
		}
	}
}

// failReads disposes of probes whose read failed, or could not be sent, for
// a reason outside them: the client could not be resolved, GitHub failed the
// query, or it ran out its deadline. A probe below
// workkinds.PRMergeabilityMaxFailedReads attempts retries with backoff. One
// at it, or past it after a lost lease, is completed with nothing written
// instead of parked, so its pull request's key is free and the poll's next
// cycle admits it again: every admission reason but the UNKNOWN retry is derived
// from the stored row on every cycle, so a parked probe would hold nothing an
// admin could act on that the next cycle does not redo, and an outage would
// stop the pull request's conflict detection for good rather than delay it.
// A failure in the worker itself (a panic, or a completion that would not
// write) still parks, through requeue, since retrying it would only repeat
// it.
func (w *Worker) failReads(ctx context.Context, items []db.ClaimedPRMergeabilityProbe, outcome workitem.Outcome, cause error) {
	for _, item := range items {
		termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
		if item.Receipt.Attempt >= workkinds.PRMergeabilityMaxFailedReads {
			w.drop(termCtx, item, string(outcome), cause)
		} else {
			w.requeue(termCtx, item, outcome, cause)
		}
		cancel()
	}
}

// dropNoCredential is the drop outcome of a probe whose repository no
// credential the org holds can read. The others are the read's
// workitem.Outcome.
const dropNoCredential = "no_credential"

// drop completes a probe that will not be read, writing nothing: the store's
// completion with no answer. outcome is why, as the dropped counter labels
// it.
func (w *Worker) drop(ctx context.Context, item db.ClaimedPRMergeabilityProbe, outcome string, cause error) {
	_, err := w.store.CompleteProbe(ctx, item.Receipt, db.PRMergeabilityAnswer{}, "")
	switch {
	case err == nil:
		recordDrop(item.Receipt.OrgID, outcome)
		// A drop for no credential was logged once for its repository; a
		// drop after failed reads ends an outage the probe could not outlast.
		level := slog.LevelWarn
		if outcome == dropNoCredential {
			level = slog.LevelDebug
		}
		log.Log(ctx, level, "mergeability probes: dropped a probe; the poll queues the pull request again",
			"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "outcome", outcome, "cause", cause)
	case errors.Is(err, workitem.ErrLeaseLost), errors.Is(err, workitem.ErrCancelled):
		log.InfoContext(ctx, "mergeability probes: probe not held at drop", "probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
	default:
		log.ErrorContext(ctx, "mergeability probes: drop failed; the probe is reclaimed after its lease expires",
			"probe_id", item.Receipt.ItemID, "entity_id", item.EntityID, "error", err)
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
