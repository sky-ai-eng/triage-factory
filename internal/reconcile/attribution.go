package reconcile

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// Authorship is how the reconciler settles a pending pull request deliverable
// (domain.DeliverableVerificationPending): whether the conversation that
// recorded it opened the pull request. A row it settles verified gets the
// conversation's team stamped on the pull request's entity as its owner
// (owning_team_id, the first tier of the router's owner ladder) and its
// creator as commissioner (commissioned_by_user_id), and becomes actionable
// through TF. A row it settles foreign is closed and never acted on.
type Authorship int

const (
	// TrustRecords takes the record's word. Local mode: the record was
	// written by the process that made the create, so nothing that recorded
	// it could have named a pull request it did not open.
	TrustRecords Authorship = iota
	// SealedAuthor takes GitHub's word: the pull request's author must be
	// the GitHub account sealed into the conversation's engagement that was
	// live when GitHub opened it (the claim's github manifest entry). Multi
	// mode, where a run's records are relayed from its sidecar and nothing
	// that applies them saw the create, so a record can name any pull
	// request. Org GitHub credentials are App-only there, so the account is a
	// bot only TF acts as.
	SealedAuthor
)

// verdict is what the reconciler concluded about who opened a pull request.
type verdict int

const (
	// verdictUnknown: the answer cannot be read — GitHub returned no author
	// or creation time, the conversation is gone, or the engagement live at
	// the creation sealed no account. The row stays pending, and a later pass
	// asks again.
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

// claimSkew is how far GitHub's clock and the database's may disagree about
// when a pull request was opened: an engagement counts as live at a creation
// up to this long before it was claimed and after it was released.
const claimSkew = 2 * time.Minute

// judgeSealedAuthor decides whether claims — one conversation's engagements —
// include the one that opened a pull request created at created by the
// account authorID / authorLogin (GitHub's id and login for it): an engagement
// live at created, within claimSkew, whose sealed GitHub account is that
// account (sealedAuthorIs). When none is and every live engagement sealed an
// account, the answer is foreign, as it is when no engagement was live at all.
// A live engagement that sealed no account (a seal that could not learn one,
// or one sealed before seals recorded it) leaves the answer unknown, unless
// another live engagement's account is the author.
func judgeSealedAuthor(claims []domain.ExecutorClaim, authorID, authorLogin string, created time.Time) (verdict, string) {
	live, unsealed := false, false
	for _, c := range claims {
		if created.Before(c.ClaimedAt.Add(-claimSkew)) {
			continue
		}
		if c.ReleasedAt != nil && created.After(c.ReleasedAt.Add(claimSkew)) {
			continue
		}
		live = true
		e, _ := c.Sources.Entry(eventsource.KindGitHub)
		switch known, is := sealedAuthorIs(e, authorID, authorLogin); {
		case !known:
			unsealed = true
		case is:
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

// sealedAuthorIs compares the account sealed on github entry e with a pull
// request's author: by GitHub's id when both carry one, which a rename leaves
// alone, else by login, case-insensitively as GitHub compares logins. A bot's
// "<slug>[bot]" login is held by its App alone, so it names the account where
// the App's bot id was never stored. known is false when either side has
// neither.
func sealedAuthorIs(e domain.SourceEntry, authorID, authorLogin string) (known, is bool) {
	switch {
	case e.AccountID != "" && authorID != "":
		return true, e.AccountID == authorID
	case e.AccountLogin != "" && authorLogin != "":
		return true, strings.EqualFold(e.AccountLogin, authorLogin)
	}
	return false, false
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
// GitHub's copy of, under the reconciler's Authorship. snap is nil when
// GitHub's copy was not read, which only TrustRecords, needing nothing from it,
// can settle without. A read failure is unknown: nothing is settled on an
// answer that was not read.
func (rc *Reconciler) judge(ctx context.Context, orgID, conversationID string, snap *domain.PRSnapshot, reads *attributionReads) (verdict, string) {
	if rc.authorship == TrustRecords {
		return verdictRun, ""
	}
	if conversationID == "" || snap == nil || (snap.AuthorID == "" && snap.Author == "") {
		return verdictUnknown, ""
	}
	created, err := time.Parse(time.RFC3339, snap.CreatedAt)
	if err != nil {
		return verdictUnknown, ""
	}
	claims, err := reads.conversationClaims(ctx, orgID, conversationID)
	if err != nil {
		reconcileLog.WarnContext(ctx, "read the conversation's engagements failed; leaving the pull request pending this pass",
			"org", orgID, "conversation", conversationID, "error", err)
		return verdictUnknown, ""
	}
	return judgeSealedAuthor(claims, snap.AuthorID, snap.Author, created)
}

// settle settles pending pull request deliverable a, from snap, GitHub's copy
// of it, and returns the row as settled, the verdict, and whether this call
// moved the row. A row is settled once (SettleVerificationSystem), so of two
// settlers racing on it exactly one acts on the verdict.
//
// When the run opened it, the attribution is stamped first, then the row is
// marked verified with the head, base and node id GitHub answered with in
// place of the ones the run recorded. A stamp, or the read behind it, that
// fails leaves the row pending, so the next pass stamps again; each stamp is
// written only if unset, so a repeat changes nothing. When GitHub's answer
// shows the run did not open it, the row is marked foreign, which closes it,
// and counted: a run that opened no pull request could only have recorded one
// through a sidecar that is not behaving as TF built it. Its ledger row stays
// as the record of the attempt.
func (rc *Reconciler) settle(ctx context.Context, orgID string, a domain.Deliverable, snap *domain.PRSnapshot, reads *attributionReads) (domain.Deliverable, verdict, bool) {
	if a.Kind != domain.DeliverableKindPullRequest || a.Verification != domain.DeliverableVerificationPending {
		return a, verdictUnknown, false
	}
	v, reason := rc.judge(ctx, orgID, a.ConversationID, snap, reads)
	switch v {
	case verdictRun:
		if !rc.stampAttribution(ctx, orgID, a, reads) {
			return a, verdictUnknown, false
		}
		details := ""
		if snap != nil {
			details = verifiedDetails(a, *snap)
		}
		moved, err := rc.deliverables.SettleVerificationSystem(ctx, orgID, a.ID, domain.DeliverableVerificationVerified, details)
		if err != nil {
			reconcileLog.WarnContext(ctx, "settle a run's pull request verified failed; leaving it pending this pass",
				"org", orgID, "deliverable", a.ID, "error", err)
			return a, verdictUnknown, false
		}
		if moved {
			a.Verification = domain.DeliverableVerificationVerified
			if details != "" {
				a.DetailsJSON = details
			}
		}
		return a, v, moved
	case verdictForeign:
		moved, err := rc.deliverables.SettleVerificationSystem(ctx, orgID, a.ID, domain.DeliverableVerificationForeign, "")
		if err != nil {
			reconcileLog.WarnContext(ctx, "settle a pull request its run did not open foreign failed; leaving it pending this pass",
				"org", orgID, "deliverable", a.ID, "error", err)
			return a, verdictUnknown, false
		}
		if moved {
			a.Verification, a.State = domain.DeliverableVerificationForeign, domain.DeliverableStatePRClosed
			countForeign(ctx, orgID, reason)
			reconcileLog.ErrorContext(ctx, "a run recorded a pull request it did not open; closed its deliverable, and TF will not act on it",
				"org", orgID, "deliverable", a.ID, "conversation", a.ConversationID, "target", a.Target, "reason", reason)
		}
		return a, v, moved
	}
	return a, v, false
}

// verifiedDetails is a's details with the node id its key names and the head
// and base snap, GitHub's copy of that pull request, answers with. The run's
// own record of them is not what a person's action on the row reads once it
// is verified: rejecting a draft deletes its head branch.
func verifiedDetails(a domain.Deliverable, snap domain.PRSnapshot) string {
	d, _ := domain.ParsePRDeliverableDetails(a.DetailsJSON)
	d.NodeID, d.HeadBranch, d.Base = prNodeID(a), snap.HeadRef, snap.BaseRef
	return domain.MarshalPRDeliverableDetails(d)
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
// never read as a second owner. Each is written only if unset, so an owner
// chosen elsewhere is kept, and an event-triggered run (no creator) stamps
// only the owner. Reports whether every write it owed landed; a deliverable
// linked to no entity has nothing to stamp.
func (rc *Reconciler) stampAttribution(ctx context.Context, orgID string, a domain.Deliverable, reads *attributionReads) bool {
	if a.EntityID == "" {
		return true
	}
	user := ""
	if a.ConversationID != "" {
		var err error
		if user, err = reads.creator(ctx, orgID, a.ConversationID); err != nil {
			reconcileLog.WarnContext(ctx, "read the conversation's creator failed; leaving the pull request pending this pass",
				"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "error", err)
			return false
		}
	}
	stamped, err := rc.entities.StampOwningTeamIfUnsetSystem(ctx, orgID, a.EntityID, a.TeamID)
	if err != nil {
		reconcileLog.WarnContext(ctx, "owning-team stamp failed; leaving the pull request pending this pass",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "error", err)
		return false
	}
	if stamped {
		reconcileLog.InfoContext(ctx, "stamped owning team on a run's pull request",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "team", a.TeamID)
	}
	stamped, err = rc.entities.StampCommissionedByIfUnsetSystem(ctx, orgID, a.EntityID, user)
	if err != nil {
		reconcileLog.WarnContext(ctx, "commissioned-by stamp failed; leaving the pull request pending this pass",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "error", err)
		return false
	}
	if stamped {
		reconcileLog.InfoContext(ctx, "stamped commissioning user on a run's pull request",
			"org", orgID, "conversation", a.ConversationID, "entity", a.EntityID, "target", a.Target, "user", user)
	}
	return true
}

// SettleDeliverable settles pending pull request deliverable a now, for a
// person about to act on it or a teardown about to close it, rather than
// leaving either to wait for the next pass: under SealedAuthor it reads
// GitHub's copy of the pull request itself. Best-effort: when the answer
// cannot be read the row stays pending, and the caller reads the row back to
// learn where it stands. A row in any other state, or on another GitHub host
// than the org's current one, is left as it is. A row settled foreign runs the
// resolved hook when it was a draft, since closing it may have resolved its
// task's last approval.
//
// The writes and the hook run detached from ctx, as Reconcile's do: a row
// settled foreign is terminal at once, so a hook a cancelled caller skipped
// would never run.
func (rc *Reconciler) SettleDeliverable(ctx context.Context, orgID string, a domain.Deliverable) {
	if a.Kind != domain.DeliverableKindPullRequest || a.Verification != domain.DeliverableVerificationPending {
		return
	}
	var snap *domain.PRSnapshot
	if rc.authorship == SealedAuthor {
		s, ok := rc.fetchSnapshot(ctx, orgID, a)
		if !ok {
			return
		}
		snap = &s
	}
	writeCtx := context.WithoutCancel(ctx)
	settled, v, moved := rc.settle(writeCtx, orgID, a, snap, newAttributionReads(rc))
	if !moved || v != verdictForeign {
		return
	}
	rc.broadcast(orgID, settled)
	if rc.prResolved != nil && a.ConversationID != "" && isDraftResolved(a, settled.State) {
		rc.prResolved(writeCtx, orgID, a.ConversationID)
	}
}

// fetchSnapshot reads GitHub's copy of pull request deliverable a's pull
// request, under the owner its repository's registry row names now. ok is
// false when a is not on the org's current host or the read fails.
func (rc *Reconciler) fetchSnapshot(ctx context.Context, orgID string, a domain.Deliverable) (domain.PRSnapshot, bool) {
	_, onHost, err := rc.onCurrentHost(ctx, orgID, []domain.Deliverable{a})
	if err != nil {
		reconcileLog.WarnContext(ctx, "resolve github host to settle a pull request failed", "org", orgID, "deliverable", a.ID, "error", err)
		return domain.PRSnapshot{}, false
	}
	nodeID := prNodeID(a)
	if len(onHost) == 0 || nodeID == "" {
		return domain.PRSnapshot{}, false
	}
	reads := newRegistryReads(rc)
	pr, err := db.ResolveDeliverablePullRequest(ctx, orgID, a, reads.entity, reads.repository)
	if err != nil {
		reconcileLog.WarnContext(ctx, "resolve a pull request to settle it failed", "org", orgID, "deliverable", a.ID, "error", err)
		return domain.PRSnapshot{}, false
	}
	if pr.Retired {
		return domain.PRSnapshot{}, false
	}
	client, err := rc.resolver.ClientFor(ctx, orgID, pr.Owner)
	if err != nil {
		reconcileLog.WarnContext(ctx, "resolve github client to settle a pull request failed", "org", orgID, "deliverable", a.ID, "error", err)
		return domain.PRSnapshot{}, false
	}
	snaps, err := client.RefreshPRs(ctx, []string{nodeID}, false)
	if err != nil {
		reconcileLog.WarnContext(ctx, "read a pull request to settle it failed", "org", orgID, "deliverable", a.ID, "error", err)
		return domain.PRSnapshot{}, false
	}
	snap, ok := snaps[nodeID]
	return snap, ok
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
		metric.WithDescription("Pull request deliverables settled foreign: a run recorded a pull request that GitHub's answer shows it did not open, by reason. A working sidecar never records one."))
	if err != nil {
		reconcileLog.Warn("foreign pull request counter setup failed", "error", err)
	}
	return &foreignCounters{foreign: c}
}

func countForeign(ctx context.Context, orgID, reason string) {
	s := foreignStats.Load()
	if s == nil || s.foreign == nil {
		return
	}
	s.foreign.Add(ctx, 1, metric.WithAttributes(telemetry.OrgID(orgID), telemetry.Reason(reason)))
}
