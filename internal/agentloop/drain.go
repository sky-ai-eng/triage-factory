package agentloop

import (
	"context"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// drain flushes every pending (delivered=false) row in assembly order, so
// the input the model sees next is exactly the input that was queued.
//
// This is the only door input enters through. The delegation prompt itself
// is minted pending by the control plane, so the engagement's entry is just
// its first drain — there is no special first-call case anywhere in the
// engine.
//
// bare distinguishes the two drain moments. A bare drain happens when the
// model is not mid-work (the engagement's first call, or a call following a
// no-tool-call assistant message): the rows keep whatever subtype they were
// written with and read as ordinary input. Every other drain happens between
// turns, while the model is working, and stamps `injection:steer` — the
// column assembly reads to wrap the row in the keep-working envelope — onto
// the human rows only. A row already carrying a system subtype (a staged
// system note, a loop-injected nudge) keeps it: overwriting would both
// mislabel the row for display and make it read as fresh user input to
// everything that keys on the human set.
//
// Each flush is one statement that marks delivered and stamps the subtype
// together. Doing it in two steps would leave a window in which a delivered
// row had lost its provenance, and assembly would then render a steer as an
// ordinary user turn.
func (e *Engine) drain(ctx context.Context, params Params, bare bool) error {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return err
	}
	pending := pendingInOrder(rows)
	if len(pending) == 0 {
		return nil
	}
	if bare {
		return e.Transcript.MarkDelivered(ctx, params.OrgID, params.ConversationID, idsOf(pending), "")
	}
	var human, injected []int
	for _, r := range pending {
		if IsHumanInput(r) {
			human = append(human, r.ID)
		} else {
			injected = append(injected, r.ID)
		}
	}
	if len(injected) > 0 {
		if err := e.Transcript.MarkDelivered(ctx, params.OrgID, params.ConversationID, injected, ""); err != nil {
			return err
		}
	}
	if len(human) > 0 {
		return e.Transcript.MarkDelivered(ctx, params.OrgID, params.ConversationID, human, domain.MessageSubtypeInjectionSteer)
	}
	return nil
}

// IsHumanInput reports whether a role=user row is genuine user input rather
// than something the system inserted on the agent's behalf. The human set is
// closed: a blank subtype — the spelling of an ordinary turn, the mission
// prompt and an API follow-up alike — and the mid-work steer stamp those rows
// receive at flush. Everything system-authored carries a subtype outside this
// set — see the InjectionSubtype constants.
func IsHumanInput(r domain.Message) bool {
	return r.Subtype == "" || r.Subtype == domain.MessageSubtypeInjectionSteer
}

// hasPending reports whether any undelivered row is waiting — the would-stop
// recheck, which catches input that landed while the concluding turn was
// still streaming.
func (e *Engine) hasPending(ctx context.Context, params Params) (bool, error) {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return false, err
	}
	return len(pendingInOrder(rows)) > 0, nil
}

// pendingInOrder returns the undelivered rows in assembly order
// (COALESCE(seq, id)). The store already returns that order, but sorting
// here keeps the flush independent of that guarantee — these rows are what
// the engine reasons about, and their order is what a steer stamp applies
// to.
func pendingInOrder(rows []domain.Message) []domain.Message {
	var pending []domain.Message
	for _, r := range rows {
		if isDelivered(r) {
			continue
		}
		pending = append(pending, r)
	}
	sort.SliceStable(pending, func(i, j int) bool { return assemblyKey(pending[i]) < assemblyKey(pending[j]) })
	return pending
}

func assemblyKey(r domain.Message) float64 {
	if r.Seq != nil {
		return *r.Seq
	}
	return float64(r.ID)
}

func idsOf(rows []domain.Message) []int {
	ids := make([]int, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// insertPending queues a row for the next drain. Used for the loop's own
// injected input (the executor-changed notice, the turn-end nudge): it goes
// through the same queue as user input rather than being spliced into an
// assembly, so there is exactly one injection point and it is durable. Every
// caller passes an explicit injection subtype — the loop never mints a row
// that could read as human input.
func (e *Engine) insertPending(ctx context.Context, params Params, content, subtype string) error {
	pending := false
	_, err := e.Transcript.Insert(ctx, params.OrgID, &domain.Message{
		ConversationID: params.ConversationID,
		UserID:         params.UserID,
		Role:           "user",
		Subtype:        subtype,
		Content:        content,
		Delivered:      &pending,
	})
	return err
}

// insertDelivered writes a system-authored user row straight into the window,
// skipping the pending queue. Used where the row is both a statement about
// the turn that just happened AND the thing the next call must read — the
// output-limit notice, which stands in for a turn the model can no longer
// see. Unlike insertNotice it returns its error: the caller is about to make
// another call that only makes sense with this row in front of it, so a
// failed write must stop the engagement rather than silently repeat the turn
// that failed.
func (e *Engine) insertDelivered(ctx context.Context, params Params, content, subtype string) error {
	_, err := e.Transcript.Insert(ctx, params.OrgID, &domain.Message{
		ConversationID: params.ConversationID,
		UserID:         params.UserID,
		Role:           "user",
		Subtype:        subtype,
		Content:        content,
	})
	return err
}

// insertNotice writes a delivered user row that records something the loop
// decided — a guard park, an unrecoverable provider error. Delivered
// (not pending) because it is a statement of fact about this engagement, not
// input awaiting consumption: if the conversation is picked up later, the
// notice is history the model reads in place, not a message to act on.
func (e *Engine) insertNotice(ctx context.Context, params Params, content string) {
	if _, err := e.Transcript.Insert(ctx, params.OrgID, &domain.Message{
		ConversationID: params.ConversationID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeStopNote,
		Content:        content,
	}); err != nil {
		e.warn("insert loop notice failed", "conversation", params.ConversationID, "error", err)
	}
}

// HasNoticeSince reports whether an identical notice row already exists
// after the last human input — the guard a re-claimed, still-parked
// conversation needs so its park notice is written once, not once per
// claim.
//
// Exported because the stop verb needs the same answer about the notes it
// writes, and the rule has to be one rule: "don't repeat what the transcript
// already says, unless a person has spoken since." Anything that stops a
// conversation is a producer of these rows now, and a second definition of
// when to suppress one is how the two drift.
func HasNoticeSince(rows []domain.Message, content string) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Role != "user" {
			continue
		}
		if IsHumanInput(r) {
			return false
		}
		if r.Subtype == domain.MessageSubtypeStopNote && r.Content == content {
			return true
		}
	}
	return false
}
