package delegate

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// handBacks owns this package's hand-back counter. Package-level because a
// metric instrument is process-global by nature, and swapped atomically so a
// test can back it with a manual reader while an engagement may still be
// counting.
var handBacks atomic.Pointer[handBackStats]

func init() {
	handBacks.Store(newHandBackStats(otel.GetMeterProvider()))
}

type handBackStats struct {
	handedBack metric.Int64Counter
}

// newHandBackStats builds the instrument against mp — production passes the
// global provider, which telemetry.Init installs. An instrument-creation error
// can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newHandBackStats(mp metric.MeterProvider) *handBackStats {
	counter, err := mp.Meter("internal/delegate").Int64Counter("conversations.handed_back",
		metric.WithDescription("Claims an engagement or its executor handed back with the conversation still mid-flight, by hand-back outcome."))
	if err != nil {
		dispatchLog.Warn("hand-back counter setup failed", "error", err)
	}
	return &handBackStats{handedBack: counter}
}

// recordHandBack counts n claims of orgID released with outcome, one of the
// db.HandBackPolicies outcomes this package writes. Called after the write
// committed, so a refused or failed hand-back counts nothing.
func recordHandBack(orgID, outcome string, n int) {
	stats := handBacks.Load()
	if stats == nil || stats.handedBack == nil || n <= 0 {
		return
	}
	stats.handedBack.Add(context.Background(), int64(n),
		metric.WithAttributes(attribute.String("outcome", outcome), telemetry.OrgID(orgID)))
}

// upstreamExhaustedNote is the transcript's account of a conversation parked
// because its model provider stayed unavailable through every hand-back the
// upstream budget allows. The model reads it on the next claim and a person
// reads it in place.
const upstreamExhaustedNote = "Paused after repeated attempts: the model provider was unavailable for about four hours. Send a message to try again."

// leaveOnUpstream ends an engagement whose model provider stayed unavailable,
// in either runtime. prior is the claim's Conversation.UpstreamHandBacks. While
// the upstream budget lasts the claim goes back 'requeued_upstream' with the
// schedule's wait for prior, and the next claim continues the conversation
// from where this one stopped: the native loop from its transcript, the SDK
// from sessionID. Once it is spent the conversation parks for a person,
// upstream_unavailable with upstreamExhaustedNote on the transcript, the way a
// stop does. Never a terminal: nothing about the conversation failed, and its
// blueprint keeps running either way.
//
// park is the engagement's ending context; its reason is set here. The caller
// has already established the engagement still holds its claim
// (leaseFenced).
func (s *Spawner) leaveOnUpstream(ctx context.Context, park liveParkContext, sessionID string, prior int, lastErr, creatorUserID string) engagementDisposition {
	park.reason = db.ParkStopped(domain.ParkReasonUpstreamUnavailable, "")
	if prior+1 < s.budgetLimit(db.BudgetUpstream) {
		policy, _ := db.HandBackPolicyFor(db.HandBackUpstream)
		delay := policy.Delay(prior)
		delegateLog.Info("model provider unavailable through the engagement's retries; handing the conversation back to retry later",
			"conversation", park.conversationID, "claim", park.claimID, "upstream_hand_backs", prior, "delay", delay, "error", lastErr)
		return engagementDisposition{fenced: s.handBackOnUpstream(ctx, park, sessionID, delay, lastErr), handedBack: true}
	}
	delegateLog.Warn("model provider unavailable through every retry the upstream budget allows; parking the conversation for a person",
		"conversation", park.conversationID, "claim", park.claimID, "upstream_hand_backs", prior, "error", lastErr)
	if s.insertUpstreamStopNote(ctx, park.orgID, park.conversationID, park.claimID, creatorUserID) {
		return engagementDisposition{fenced: true}
	}
	return engagementDisposition{fenced: s.parkConversationOpen(ctx, park, sessionID)}
}

// insertUpstreamStopNote writes upstreamExhaustedNote as a stop note, through
// the claim fence like every other write of this engagement. fenced is true
// when the fence refused it: a successor owns the conversation, and neither
// the note nor the park that follows is this engagement's to write. Any other
// failure is logged and the park still lands.
func (s *Spawner) insertUpstreamStopNote(ctx context.Context, orgID, conversationID, claimID, creatorUserID string) (fenced bool) {
	if s.conversations == nil {
		return false // test fixture with no DB wired
	}
	msg := &domain.Message{
		ConversationID: conversationID,
		UserID:         creatorUserID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeStopNote,
		Content:        upstreamExhaustedNote,
	}
	id, err := s.conversations.InsertMessageForClaimSystem(context.WithoutCancel(ctx), orgID, claimID, msg)
	if errors.Is(err, db.ErrClaimReleased) {
		delegateLog.Error("claim fence refused the stop note — a successor owns this conversation; recording nothing",
			"conversation", conversationID, "claim_id", claimID, "org_id", orgID)
		return true
	}
	if err != nil {
		delegateLog.Warn("record stop note failed; the park still lands", "conversation", conversationID, "error", err)
		return false
	}
	msg.ID = int(id)
	s.broadcastMessage(orgID, conversationID, msg)
	return false
}

// leaveSDKOnUpstream is the SDK runtime's door to leaveOnUpstream: ok is false,
// and nothing is written, unless result ended on the model provider being
// unavailable (sdkProviderUnavailable). Nothing about the conversation failed
// then, so it is handed back to retry on the upstream schedule, and the next
// claim resumes sessionID with the note composeLaunchTurn picks for it. The
// fence is read first for the reason processCompletion reads it: a result in
// hand is not authority to record anything once the lease has fenced.
func (s *Spawner) leaveSDKOnUpstream(ctx context.Context, park liveParkContext, result *agentproc.Result, sessionID string, prior int, creatorUserID string) (_ engagementDisposition, ok bool) {
	if !sdkProviderUnavailable(result) {
		return engagementDisposition{}, false
	}
	if leaseFenced(ctx) {
		return engagementDisposition{fenced: true}, true
	}
	park.costUSD = result.CostUSD
	return s.leaveOnUpstream(ctx, park, sessionID, prior, sdkUpstreamSummary(result), creatorUserID), true
}

// sdkProviderUnavailable reports whether an SDK result ended on its model
// provider being unavailable after the SDK's own retries: an error result
// whose api_error_status classifies as Transient or RateLimited. The status
// is the SDK's structured report of the provider's answer, so the decision
// never reads the result's prose. A result with no status (0), and every
// other class, is the agent's failure and keeps failing the conversation.
func sdkProviderUnavailable(r *agentproc.Result) bool {
	if r == nil || !r.IsError || r.APIErrorStatus == 0 {
		return false
	}
	switch inference.ClassifyStatus(r.APIErrorStatus) {
	case upstream.Transient, upstream.RateLimited:
		return true
	}
	return false
}

// sdkUpstreamSummary is what an SDK hand-back logs and keeps on
// result_summary: the status the provider answered with. The runtime's own
// text is left out, because it renders the provider's response body, and an
// upstream body reaches neither a log line nor a person's screen.
func sdkUpstreamSummary(r *agentproc.Result) string {
	return fmt.Sprintf("model provider unavailable (HTTP %d)", r.APIErrorStatus)
}

// upstreamSetupFailure reports whether a failure before the agent ran was an
// upstream the engagement fetches from being unreachable, rather than
// something about the conversation or the host: a GitHub API call during
// setup (the pull-request fetch) that failed Transient or RateLimited, or a
// git command whose output names a network failure.
//
// A git command is read by its output alone. Its GitError unwraps to the
// context error when its deadline stopped it, and upstream.ClassOf reads
// context.DeadlineExceeded as a transport timeout, so without this a clone
// that outlasts its bound because the repository is large would spend the
// upstream budget instead of the setup budget.
func upstreamSetupFailure(cause error) bool {
	if worktree.IsTransientGitError(cause) {
		return true
	}
	var gitErr *worktree.GitError
	if errors.As(cause, &gitErr) {
		return false
	}
	class, ok := upstream.ClassOf(cause)
	return ok && (class == upstream.Transient || class == upstream.RateLimited)
}

// upstreamSetupSubject names what an upstreamSetupFailure could not reach, for
// the stop note an exhausted upstream budget parks with. It is a label and
// not the error: git's output names the per-run proxy's address and repeats
// lines the remote sent, the note is a transcript row both the person and
// the resumed model read, and the logs already carry the whole error.
func upstreamSetupSubject(cause error) string {
	if worktree.IsTransientGitError(cause) {
		return "the repository's git host"
	}
	return "a service it needs"
}
