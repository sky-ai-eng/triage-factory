package reconcile

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// Authorship is how the reconciler decides whether the conversation a pull
// request deliverable, or a pr_created ledger row, names opened the pull
// request. It decides it before stamping the pull request's entity with the
// conversation's team (owning_team_id, the first tier of the router's owner
// ladder) and creator (commissioned_by_user_id), and before the backstop
// records a deliverable from a ledger row.
type Authorship int

const (
	// TrustRecords takes the record's word. Local mode: the record was
	// written by the process that made the create, so nothing that recorded
	// it could have named a pull request it did not open.
	TrustRecords Authorship = iota
	// SealedAuthor takes GitHub's word: the pull request's author must be
	// the GitHub account sealed into the conversation's engagement that was
	// live when GitHub opened it (domain.SourceEntry.AccountID on the claim's
	// github entry). Multi mode, where a run's records are relayed from its
	// sidecar and nothing that applies them saw the create, so a record can
	// name any pull request. Org GitHub credentials are App-only there, so
	// the account is a bot only TF acts as.
	SealedAuthor
)

// verdict is what the reconciler concluded about who opened a pull request.
type verdict int

const (
	// verdictUnknown: the answer cannot be read — GitHub returned no author
	// id or creation time, the conversation is gone, or the engagement live
	// at the creation sealed no account. Nothing is stamped, deleted or
	// backfilled, and a later pass asks again.
	verdictUnknown verdict = iota
	// verdictRun: the conversation's run opened it.
	verdictRun
	// verdictForeign: GitHub's answer shows the run did not open it.
	verdictForeign
)

// The reasons a verdict is foreign, a closed vocabulary for the counter and
// the log line.
const (
	// foreignAuthor: an engagement was live when the pull request was opened,
	// and its sealed account is not the author.
	foreignAuthor = "author"
	// foreignNoEngagement: no engagement of the conversation was live when
	// the pull request was opened, so no sidecar of it existed to open it.
	foreignNoEngagement = "no_engagement"
)

// What the counter records was done with a foreign pull request.
const (
	// foreignDeleted: the reconciler deleted the deliverable that named it.
	foreignDeleted = "deleted"
	// foreignNotRecorded: the backstop did not record a deliverable from the
	// ledger row that named it.
	foreignNotRecorded = "not_recorded"
)

// claimSkew is how far GitHub's clock and the database's may disagree about
// when a pull request was opened: an engagement counts as live at a creation
// up to this long before it was claimed and after it was released.
const claimSkew = 2 * time.Minute

// judgeSealedAuthor decides whether claims — one conversation's engagements —
// include the one that opened a pull request authorID opened at created: an
// engagement live at created, within claimSkew, whose sealed GitHub account is
// authorID. When none is and every live engagement sealed an account, the
// answer is foreign, as it is when no engagement was live at all. A live
// engagement that sealed no account (a seal that could not learn one, or one
// recorded before seals recorded it) leaves the answer unknown, unless another
// live engagement's account is the author.
func judgeSealedAuthor(claims []domain.ExecutorClaim, authorID string, created time.Time) (verdict, string) {
	live, unsealed := false, false
	for _, c := range claims {
		if created.Before(c.ClaimedAt.Add(-claimSkew)) {
			continue
		}
		if c.ReleasedAt != nil && created.After(c.ReleasedAt.Add(claimSkew)) {
			continue
		}
		live = true
		e, ok := c.Sources.Entry(eventsource.KindGitHub)
		switch {
		case !ok || e.AccountID == "":
			unsealed = true
		case e.AccountID == authorID:
			return verdictRun, ""
		}
	}
	switch {
	case unsealed:
		return verdictUnknown, ""
	case !live:
		return verdictForeign, foreignNoEngagement
	default:
		return verdictForeign, foreignAuthor
	}
}

// attributionReads are the claim and conversation reads one pass makes, each
// conversation's read once however many deliverables name it.
type attributionReads struct {
	rc       *Reconciler
	claims   map[string]claimsRead
	creators map[string]creatorRead
}

type claimsRead struct {
	claims []domain.ExecutorClaim
	err    error
}

type creatorRead struct {
	user string
	err  error
}

func newAttributionReads(rc *Reconciler) *attributionReads {
	return &attributionReads{rc: rc, claims: map[string]claimsRead{}, creators: map[string]creatorRead{}}
}

func (r *attributionReads) conversationClaims(ctx context.Context, orgID, conversationID string) ([]domain.ExecutorClaim, error) {
	read, ok := r.claims[conversationID]
	if !ok {
		read.claims, read.err = r.rc.claims.ClaimsForConversationSystem(ctx, orgID, conversationID)
		r.claims[conversationID] = read
	}
	return read.claims, read.err
}

// creator is the conversation's creator_user_id, "" for an event-triggered
// conversation and for one that is gone.
func (r *attributionReads) creator(ctx context.Context, orgID, conversationID string) (string, error) {
	read, ok := r.creators[conversationID]
	if !ok {
		var conv *domain.Conversation
		conv, read.err = r.rc.conversations.GetSystem(ctx, orgID, conversationID)
		if conv != nil {
			read.user = conv.CreatorUserID
		}
		r.creators[conversationID] = read
	}
	return read.user, read.err
}

// judge decides whether conversationID's run opened the pull request snap is
// GitHub's copy of, under the reconciler's Authorship. A read failure is
// unknown: attribution is best-effort, and nothing is stamped or deleted on
// an answer that was not read.
func (rc *Reconciler) judge(ctx context.Context, orgID, conversationID string, snap domain.PRSnapshot, reads *attributionReads) (verdict, string) {
	if rc.authorship == TrustRecords {
		return verdictRun, ""
	}
	if conversationID == "" || snap.AuthorID == "" {
		return verdictUnknown, ""
	}
	created, err := time.Parse(time.RFC3339, snap.CreatedAt)
	if err != nil {
		return verdictUnknown, ""
	}
	claims, err := reads.conversationClaims(ctx, orgID, conversationID)
	if err != nil {
		reconcileLog.WarnContext(ctx, "read the conversation's engagements failed; leaving the pull request unattributed this pass",
			"org", orgID, "conversation", conversationID, "error", err)
		return verdictUnknown, ""
	}
	return judgeSealedAuthor(claims, snap.AuthorID, created)
}

// attribute settles who opened pull request deliverable a, from snap, GitHub's
// copy of it, while its entity has no owning team. When the deliverable's run
// opened it, the entity is stamped with the deliverable's team as its owner
// and the conversation's creator as its commissioner, each only if unset.
// When GitHub's answer shows the run did not, the deliverable is deleted: it
// offers actions on someone else's pull request, under TF's credential, and
// a run that opened none could only have recorded it through a sidecar that
// is not behaving as TF built it. The ledger row stays as the record of the
// attempt. Reports whether the deliverable was deleted.
//
// An entity with an owner is not judged again, so a deliverable attributed
// once is not deleted by a later pass that reads a manifest a re-seal has
// since replaced. A deliverable linked to no entity has nothing to stamp, and
// is judged only to be deleted if it is foreign.
//
// Best-effort, like the transition beside it: a read or write failure is
// logged and the next pass tries again.
func (rc *Reconciler) attribute(ctx context.Context, orgID string, a domain.Deliverable, snap domain.PRSnapshot, reads *attributionReads) bool {
	if a.Kind != domain.DeliverableKindPullRequest {
		return false
	}
	if a.EntityID != "" {
		owner, err := rc.entities.OwningTeamForEntitySystem(ctx, orgID, a.EntityID)
		if err != nil {
			reconcileLog.WarnContext(ctx, "read the pull request's owner failed; leaving it unattributed this pass",
				"org", orgID, "deliverable", a.ID, "entity", a.EntityID, "error", err)
			return false
		}
		if owner != "" {
			return false
		}
	} else if rc.authorship == TrustRecords {
		return false
	}

	v, reason := rc.judge(ctx, orgID, a.ConversationID, snap, reads)
	switch v {
	case verdictRun:
		if a.EntityID != "" {
			rc.stampAttribution(ctx, orgID, a, reads)
		}
	case verdictForeign:
		return rc.deleteForeign(ctx, orgID, a, reason)
	}
	return false
}

// stampAttribution records on the entity behind pull request deliverable a,
// which its run opened, the two facts only the run knows: the team that
// commissioned it (owning_team_id) and the person who asked for it
// (commissioned_by_user_id). The author of a pull request TF opened is a bot
// that maps to no TF user, so neither the router's owner nor the personal
// list's "mine" can be derived from the pull request itself. Left unstamped,
// its first ci_check_failed finds no owner and mints a task for nobody unless
// the repository is attached to a project, and the person who delegated never
// sees the pull request their ask produced.
//
// The team is the structural owner routing reads and the user is provenance,
// never read as a second owner. Each is written only if unset, independently,
// so an event-triggered run (no creator) stamps only the owner and a failure
// of one write costs the other nothing.
func (rc *Reconciler) stampAttribution(ctx context.Context, orgID string, a domain.Deliverable, reads *attributionReads) {
	stamped, err := rc.entities.StampOwningTeamIfUnsetSystem(ctx, orgID, a.EntityID, a.TeamID)
	if err != nil {
		reconcileLog.WarnContext(ctx, "owning-team stamp failed",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "error", err)
	} else if stamped {
		reconcileLog.InfoContext(ctx, "stamped owning team on a run's pull request",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "team", a.TeamID)
	}
	if a.ConversationID == "" {
		return
	}
	user, err := reads.creator(ctx, orgID, a.ConversationID)
	if err != nil {
		reconcileLog.WarnContext(ctx, "read the conversation's creator failed; leaving the pull request's commissioner unset",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "error", err)
		return
	}
	stamped, err = rc.entities.StampCommissionedByIfUnsetSystem(ctx, orgID, a.EntityID, user)
	if err != nil {
		reconcileLog.WarnContext(ctx, "commissioned-by stamp failed",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "error", err)
		return
	}
	if stamped {
		reconcileLog.InfoContext(ctx, "stamped commissioning user on a run's pull request",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "user", user)
	}
}

// deleteForeign deletes pull request deliverable a, which GitHub's answer
// shows its run did not open, and counts it. Reports whether it was deleted.
func (rc *Reconciler) deleteForeign(ctx context.Context, orgID string, a domain.Deliverable, reason string) bool {
	removed, err := rc.deliverables.DeleteSystem(ctx, orgID, a.ID)
	if err != nil {
		reconcileLog.WarnContext(ctx, "delete a pull request deliverable its run did not open failed",
			"org", orgID, "deliverable", a.ID, "conversation", a.ConversationID, "target", a.Target, "error", err)
		return false
	}
	if !removed {
		return false
	}
	countForeign(ctx, orgID, reason, foreignDeleted)
	reconcileLog.ErrorContext(ctx, "deleted a pull request deliverable its run did not open; the run's sidecar recorded a pull request it did not create",
		"org", orgID, "deliverable", a.ID, "conversation", a.ConversationID, "target", a.Target, "reason", reason)
	rc.publish(orgID, a.ConversationID, map[string]any{"deliverable_id": a.ID})
	return true
}

// foreignStats owns this package's counter of pull requests a run recorded
// and did not open. Package-level because a metric instrument is
// process-global by nature, and swapped atomically so a test can back it with
// a manual reader.
var foreignStats atomic.Pointer[foreignCounters]

func init() {
	foreignStats.Store(newForeignCounters(otel.GetMeterProvider()))
}

type foreignCounters struct {
	foreign metric.Int64Counter
}

// newForeignCounters builds the instrument against mp; production passes the
// global provider, which telemetry.Init installs. A creation error can only be
// an invalid name, and the API hands back a usable no-op beside it, so it is
// logged rather than propagated.
func newForeignCounters(mp metric.MeterProvider) *foreignCounters {
	c, err := mp.Meter("internal/reconcile").Int64Counter("deliverables.foreign_pull_requests",
		metric.WithDescription("Pull requests a run's record named that GitHub's answer shows the run did not open, by reason and disposition. A working sidecar never records one."))
	if err != nil {
		reconcileLog.Warn("foreign pull request counter setup failed", "error", err)
	}
	return &foreignCounters{foreign: c}
}

func countForeign(ctx context.Context, orgID, reason, disposition string) {
	s := foreignStats.Load()
	if s == nil || s.foreign == nil {
		return
	}
	s.foreign.Add(ctx, 1, metric.WithAttributes(
		telemetry.OrgID(orgID), telemetry.Reason(reason), telemetry.Disposition(disposition)))
}
