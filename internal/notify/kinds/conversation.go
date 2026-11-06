package kinds

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
)

// The kinds below tell the person who delegated a conversation what happened
// to it while they were not watching. A conversation a trigger fired has
// nobody who asked for it; the board shows how it went.

// FailedConversation is a conversation a person delegated that failed.
type FailedConversation struct {
	ConversationID string `json:"conversation_id"`
	// UserID is the person who delegated it.
	UserID string `json:"user_id"`
	// Subject is the key of the entity its task is about, e.g.
	// owner/repo#18. Display only; empty when the task names no entity.
	Subject     string                         `json:"subject"`
	FailureKind domain.ConversationFailureKind `json:"failure_kind"`
	// Summary is why it failed, as the failure recorded it. Display only.
	Summary string `json:"summary"`
}

// summaryLimit is how many characters of a failure's own text a body carries.
// The whole of it is on the run's transcript, which the row links to.
const summaryLimit = 300

// ConversationFailed tells the person who delegated a conversation that it
// failed. It is a notice: a failed conversation is finished, and there is
// nothing to resolve. It is high, unlike a notice's default, because the
// person asked for the work, so the badge and the arrival display are how they
// learn it did not happen. The key is the conversation, so a second failure
// report for it coalesces.
var ConversationFailed = notify.Notice(notify.NoticeSpec[FailedConversation]{
	Name:     "run.failed",
	Priority: notify.Always(notify.High),
	TTL:      notify.DefaultNoticeTTL,
	Key:      func(f FailedConversation) string { return f.ConversationID },
	Audience: notify.User(func(f FailedConversation) string { return f.UserID }),
	Render: func(f FailedConversation, _ notify.Recipient) notify.Content {
		body := "The run's transcript says what went wrong."
		switch {
		case f.FailureKind == domain.ConversationFailureMemoryLimit:
			body = "It used more memory than a run is allowed, so Triage Factory stopped it. " +
				"If it needs more, the deployment's operator can raise the limit."
		case f.Summary != "":
			body = truncate(f.Summary, summaryLimit)
		}
		return notify.Content{
			Title:   "Your run" + on(f.Subject) + " failed",
			Body:    body,
			Actions: []notify.Action{notify.Link("Open the run", notify.Routes.RunStation(f.ConversationID))},
		}
	},
	Example: FailedConversation{
		ConversationID: "00000000-0000-0000-0000-000000000010",
		UserID:         "00000000-0000-0000-0000-000000000011",
		Subject:        "acme/widgets#18",
		FailureKind:    domain.ConversationFailureCrash,
		Summary:        "the agent process exited with status 1",
	},
})

// PausedConversation is a conversation a person delegated that is parked
// waiting on a person (domain.ParkReason.AwaitsPerson).
type PausedConversation struct {
	ConversationID string `json:"conversation_id"`
	// UserID is the person who delegated it.
	UserID string `json:"user_id"`
	// Subject is the key of the entity its task is about. Display only.
	Subject string            `json:"subject"`
	Reason  domain.ParkReason `json:"reason"`
}

// PausedFor is a's occurrence.
func PausedFor(a domain.ConversationAwaitingPerson) PausedConversation {
	return PausedConversation{
		ConversationID: a.ConversationID,
		UserID:         a.CreatorUserID,
		Subject:        a.EntitySourceID,
		Reason:         a.ParkReason,
	}
}

// PausedForAll is as's occurrences.
func PausedForAll(as []domain.ConversationAwaitingPerson) []PausedConversation {
	out := make([]PausedConversation, len(as))
	for i, a := range as {
		out[i] = PausedFor(a)
	}
	return out
}

// ConversationPaused is open while a conversation a person delegated is
// parked waiting on them: nothing retries it, and it moves again only when
// someone sends it a message or fixes what stopped it. It resolves when the
// conversation leaves that park, whether a message woke it, someone stopped
// it, or it ended. Dismissible, because the person may decide the run is not
// worth continuing. The copy names the cause; the run's transcript, which the
// row links to, has the detail.
var ConversationPaused = notify.Condition(notify.ConditionSpec[PausedConversation]{
	Name:        "run.paused",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(p PausedConversation) string { return p.ConversationID },
	Audience:    notify.User(func(p PausedConversation) string { return p.UserID }),
	Render: func(p PausedConversation, _ notify.Recipient) notify.Content {
		var cause string
		next := "Send it a message to continue."
		switch p.Reason {
		case domain.ParkReasonSandboxLost:
			// A message continues it in a new sandbox, which a run that keeps
			// running out of memory will lose the same way.
			cause = "The sandbox its tools run in stopped working several times in a row. The run's transcript says why; " +
				"if it ran out of memory, the deployment's operator can raise the limit before it continues."
		case domain.ParkReasonLaunchFailed:
			cause = "It could not start."
			next = "Send it a message to try again."
		case domain.ParkReasonModelNotEnabled:
			// A message would wake it into the same refusal.
			cause = "Its team no longer allows the model it runs on."
			next = "It can continue once that model is allowed again; the transcript names it."
		case domain.ParkReasonStalled:
			cause = "It stopped making progress, so Triage Factory stopped it."
		case domain.ParkReasonUpstreamUnavailable:
			cause = "The model provider or GitHub stayed unavailable through every retry."
		default:
			cause = "It stopped and nothing will restart it on its own."
		}
		return notify.Content{
			Title:   "Your run" + on(p.Subject) + " is waiting for you",
			Body:    cause + " " + next,
			Actions: []notify.Action{notify.Link("Open the run", notify.Routes.RunStation(p.ConversationID))},
		}
	},
	Resolved: func(_ PausedConversation, r notify.Resolution) string {
		if r.Reason == domain.NotificationResolvedAudienceRemoved {
			return noLongerApplies(r.Recipient)
		}
		return "The run is no longer waiting: it continued, someone stopped it, or it ended."
	},
	Example: PausedConversation{
		ConversationID: "00000000-0000-0000-0000-000000000012",
		UserID:         "00000000-0000-0000-0000-000000000013",
		Subject:        "acme/widgets#18",
		Reason:         domain.ParkReasonSandboxLost,
	},
})

// on is " on <subject>", or nothing when there is no subject to name.
func on(subject string) string {
	if subject == "" {
		return ""
	}
	return " on " + subject
}

// truncate caps s at limit runes, ending in an ellipsis when it cut.
func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}
