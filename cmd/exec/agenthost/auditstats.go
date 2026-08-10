package agenthost

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The audit pipeline is fire-and-forget from the moment an external write
// lands: the write already happened on GitHub or Jira, so nothing downstream
// may unwind it, and every step from there to the row is best-effort by
// contract. That is the right posture and it is not changing here. What was
// missing is a number: a lost row surfaced only as a warn line, so the log of
// record could have holes nobody could measure.
//
// A drop is counted at whichever of four stages lost it. They are stages of
// one funnel, not independent failures — a record passes through the ones that
// exist for its placement (a local-mode run has no relay at all, so its only
// reachable stage is the write) and is counted at most once, at the first one
// that drops it.
const (
	// dropStageRelaySend — the sidecar could not hand the notify to the
	// supervision channel. Reported by the sidecar over that same channel, which
	// bounds what this can ever see: see agentproc.NotifyRelayAudit.
	dropStageRelaySend = "relay_send"
	// dropStageRelayDecode — the notify arrived and could not be turned back
	// into a record: undecodable args, or an op this orchestrator has no handler
	// for (a peer newer than this binary, which the single-binary build makes a
	// deployment error rather than routine skew).
	dropStageRelayDecode = "relay_decode"
	// dropStageRelayDispatch — a provider's own notify handler ran and errored.
	// Distinct from the write stage below because the failure is the provider's
	// (its policy op, its store call), not this funnel's.
	dropStageRelayDispatch = "relay_dispatch"
	// dropStageRecord — the record reached the write funnel and the DB write
	// failed. The end of the line in every placement, and the stage a local-mode
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
// recorded, in the vocabulary of the stage that lost it — a relay
// "<namespace>.<op>" pair for the relay stages, the domain kind of the row for
// the write stage — so stage is what tells a reader which vocabulary to read op
// in. Both are closed sets; neither carries a repo, a target, or an id.
func recordAuditDrop(ctx context.Context, stage, op string) {
	if auditDrops == nil || auditDrops.dropped == nil {
		return
	}
	auditDrops.dropped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("stage", stage),
		attribute.String("op", op),
	))
}

// recordDropOp names what the write funnel was recording, for a drop at the
// write stage: the artifact's kind, or — for an audit-only write, which carries
// no artifact — the action's discriminator. Both are closed domain
// vocabularies minted by TF itself, which is what makes either safe as a label;
// the target they were written against is a repo path and stays out, the same
// line the artifact.record span draws.
func recordDropOp(a *domain.Artifact, act *domain.ExternalAction) string {
	switch {
	case a != nil && a.Kind != "":
		return a.Kind
	case act != nil && act.Action != "":
		return act.Action
	default:
		return dropOpOther
	}
}

// coreNotifyOps is the set of core notify ops whose names may be used as a
// label verbatim. Every value is a Go constant from dispatchCoreNotify's
// switch, so the set is the switch.
var coreNotifyOps = map[string]struct{}{
	agentproc.OpRecordDenial:       {},
	agentproc.OpRecordEgressDenial: {},
	agentproc.OpRecordGHWrite:      {},
	agentproc.OpRecordObservation:  {},
	agentproc.OpRecordPush:         {},
	agentproc.OpRecordRelayDrop:    {},
	opRecordExternalWrite:          {},
	opRecordReadTouch:              {},
}

// relayDropOp names a relay-stage drop, in the "<namespace>.<op>" form the
// relay spans already use — but only for a pair this process recognizes.
// Anything else collapses to "other".
//
// Both halves arrive ON THE WIRE, from the one process in the split
// deliberately exposed to hostile text, and what they name becomes a metric
// label. Unclamped, a compromised sidecar could mint unbounded series in the
// orchestrator's exporter simply by naming ops that do not exist. The cost is
// that a provider's own audit op — namespaced, resolved through a registry this
// function does not enumerate — counts as "other"; its stage says so anyway,
// and the warn line accompanying every drop names the pair exactly.
func relayDropOp(namespace, op string) string {
	if namespace != agentproc.RelayNamespaceCore {
		return dropOpOther
	}
	if _, ok := coreNotifyOps[op]; ok {
		return namespace + "." + op
	}
	return dropOpOther
}
