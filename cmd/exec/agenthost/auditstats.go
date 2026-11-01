package agenthost

import (
	"context"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The audit pipeline never unwinds the act it records: the write already
// happened on GitHub or Jira, so nothing downstream may fail it, and the agent
// has moved on by the time the row is written. What the pipeline does instead
// is try hard to land the row — the write stage retries a retryable failure
// (record_retry.go), and in a sandboxed run the sidecar holds each record until
// the orchestrator acknowledges it (agentproc.RecordSender) — and count every
// record it still loses, so the log of record has no hole nobody can measure.
//
// A drop is counted at whichever of four stages lost it. They are stages of
// one funnel, not independent failures — a record passes through the ones that
// exist for its placement (a local-mode run has no relay at all, so its only
// reachable stage is the write) and is counted at the stage that finally lost
// it, never at two. The write stage counts what it lost rather than the record
// that carried it: a record whose deliverable and then its action-alone fallback
// both fail counts twice there, once under each.
const (
	// dropStageRelaySend — the sidecar gave the record up without the
	// orchestrator ever applying it: its args did not marshal, its queue was
	// full, or it was still unacknowledged when the run ended. Reported by the
	// sidecar's end-of-run drain (CountUndeliveredRecords). A sidecar that dies
	// before the drain takes its queue with it uncounted, since the channel the
	// count would cross dies with it.
	dropStageRelaySend = "relay_send"
	// dropStageRelayDecode — the record arrived and could not be turned back
	// into a write: undecodable args, no valid key, or an op this orchestrator
	// has no handler for (a peer newer than this binary, which the
	// single-binary build makes a deployment error rather than routine skew).
	// Not retried: the same bytes would fail the same way.
	dropStageRelayDecode = "relay_decode"
	// dropStageRelayDispatch — a record's handler ran and failed: a provider's
	// own handler returned an error, or any handler panicked. Distinct from the
	// write stage below because the failure is the handler's (its policy op,
	// its store call, its bug), not this funnel's.
	dropStageRelayDispatch = "relay_dispatch"
	// dropStageRecord — the record reached the write funnel and the DB write
	// failed for good: on an error a retry cannot get past, or on a retryable
	// one once the write stage's retries ran out. A relayed record's retryable
	// failure is not a drop yet — the sidecar sends the record again — so it is
	// counted here only for a local-mode run, which has no one to resend it.
	// The end of the line in every placement, and the stage a local-mode
	// deployment can reach without a sidecar existing.
	dropStageRecord = "record"
)

// dropOpOther is the op label for a drop whose op name this process cannot
// vouch for — see relayDropOp.
const dropOpOther = "other"

// auditDrops owns this package's drop instrument. Package-level (like tracer)
// because the funnel it measures is a free function reached from four call
// sites across two placements, and because a metric instrument is
// process-global by nature; tests swap it for one backed by a manual reader.
var auditDrops = newAuditDropStats(otel.GetMeterProvider())

// auditDropStats is the instrument set behind recordAuditDrop. Nil-safe on
// every method, matching the other optional collaborators in this package.
type auditDropStats struct {
	dropped metric.Int64Counter
}

// newAuditDropStats builds the instrument against mp — production passes the
// global provider, tests pass one wired to a ManualReader. An
// instrument-creation error can only be a programmer error (an invalid name),
// and the API contract hands back a usable no-op alongside it, so it is logged
// rather than propagated.
func newAuditDropStats(mp metric.MeterProvider) *auditDropStats {
	dropped, err := mp.Meter("cmd/exec/agenthost").Int64Counter("audit.records.dropped",
		metric.WithDescription("External-write audit records that never reached the log of record, by the funnel stage that lost them."))
	if err != nil {
		agenthostLog.Warn("audit drop counter setup failed", "error", err)
	}
	return &auditDropStats{dropped: dropped}
}

// recordAuditDrop counts one lost audit record. op names what was being
// recorded, in the vocabulary of the stage that lost it — a registered relay
// "<namespace>.<op>" pair for the relay stages, the domain kind of the row for
// the write stage, and "other" at either when the name is one this binary
// cannot vouch for — so stage is what tells a reader which vocabulary to read
// op in. Every value is a closed set; none carries a repo, a target, or an id.
func recordAuditDrop(ctx context.Context, stage, op string) {
	recordAuditDrops(ctx, stage, op, 1)
}

// recordAuditDrops counts n lost audit records of one op at one stage — the
// form a batch report, like the sidecar's end-of-run drain, arrives in.
func recordAuditDrops(ctx context.Context, stage, op string, n int64) {
	if auditDrops == nil || auditDrops.dropped == nil {
		return
	}
	auditDrops.dropped.Add(ctx, n, metric.WithAttributes(
		attribute.String("stage", stage),
		attribute.String("op", op),
	))
}

// recordDropOp names what the write funnel was recording, for a drop at the
// write stage: the deliverable's kind, or — for an audit-only write, which carries
// no deliverable — the action's discriminator. Both are closed domain
// vocabularies minted by TF itself, which is what makes either safe as a label;
// the target they were written against is a repo path and stays out, the same
// line the artifact.record span draws.
func recordDropOp(a *domain.Deliverable, act *domain.ExternalAction) string {
	switch {
	case a != nil && a.Kind != "":
		return a.Kind
	case act != nil && act.Action != "":
		return act.Action
	default:
		return dropOpOther
	}
}

// coreRecordOps is the set of core record ops whose names may be used as a
// label verbatim. Every value is a Go constant from dispatchCoreRecord's
// switch, so the set is the switch.
var coreRecordOps = map[string]struct{}{
	agentproc.OpRecordDenial:       {},
	agentproc.OpRecordEgressDenial: {},
	agentproc.OpRecordGHWrite:      {},
	agentproc.OpRecordPush:         {},
	opRecordExternalWrite:          {},
	opRecordBranchPush:             {},
	opRecordReadTouch:              {},
}

// relayDropOp names a relay-stage drop, in the "<namespace>.<op>" form the
// relay spans already use — but only for a pair this process can name.
// Anything else collapses to "other".
//
// Both halves arrive ON THE WIRE, from the one process in the split
// deliberately exposed to hostile text, and what they name becomes a metric
// label. Unclamped, a compromised sidecar could mint unbounded series in the
// orchestrator's exporter simply by naming ops that do not exist. Clamping to
// what is REGISTERED (rather than to the core catalog alone) is what keeps a
// provider's own audit op nameable: sidecar and executor are one binary, so a
// namespace that dispatches here is one this process also has in its registry.
// An op nothing registered is the only thing left over, and "other" is the
// honest name for it.
func relayDropOp(namespace, op string) string {
	if !isRegisteredRelayRecordOp(namespace, op) {
		return dropOpOther
	}
	return namespace + "." + op
}

// isRegisteredRelayRecordOp reports whether (namespace, op) is a pair this
// binary serves: a core op from dispatchCoreRecord's switch, or a provider op
// some ee package registered at init. Both sets are fixed at startup and
// bounded by construction — the core one by its switch, the provider one by a
// registry that panics on a duplicate registration.
func isRegisteredRelayRecordOp(namespace, op string) bool {
	if namespace == agentproc.RelayNamespaceCore {
		_, ok := coreRecordOps[op]
		return ok
	}
	// Read without a lock, like dispatchProviderOp itself: the registry is
	// written only by init-time registration, never once ops are being served.
	ops, ok := providerOps[namespace]
	if !ok {
		return false
	}
	_, ok = ops[op]
	return ok
}

// auditUnnamedComments counts posted-comment rows recorded naming no comment:
// the provider accepted the comment but its response carried no comment id,
// or the issue it was posted on could not be read back to key it under. The
// row still lands, so the drop counter never sees it, and a row naming no
// object is left out of the run's artifacts.
var auditUnnamedComments = newUnnamedCommentCounter(otel.GetMeterProvider())

// newUnnamedCommentCounter builds the instrument against mp, on the same terms
// as newAuditDropStats.
func newUnnamedCommentCounter(mp metric.MeterProvider) metric.Int64Counter {
	c, err := mp.Meter("cmd/exec/agenthost").Int64Counter("audit.comments.unnamed",
		metric.WithDescription("Posted-comment audit rows recorded naming no comment, by provider and action."))
	if err != nil {
		agenthostLog.Warn("unnamed comment counter setup failed", "error", err)
	}
	return c
}

// countUnnamedComment counts act when it is a posted comment that landed
// naming no object. A relayed row's provider is the sidecar's to name, so a
// provider outside domain.Providers is labeled "other", as relayDropOp
// clamps a relayed op; the action is one of two constants. The target stays
// out, as it does from the drop counter.
func countUnnamedComment(ctx context.Context, act *domain.ExternalAction) {
	if act == nil || act.ObjectKind != "" || auditUnnamedComments == nil {
		return
	}
	if act.Action != domain.ActionCommentPosted && act.Action != domain.ActionIssueCommentPosted {
		return
	}
	provider := act.Provider
	if !slices.Contains(domain.Providers(), provider) {
		provider = dropOpOther
	}
	auditUnnamedComments.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", provider),
		attribute.String("action", act.Action),
	))
}
