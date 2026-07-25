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
// turns, while the model is working, and is stamped `injection:steer` — the
// column assembly reads to wrap the row in the keep-working envelope.
//
// Flush is one statement that marks delivered and stamps the subtype
// together. Doing it in two steps would leave a window in which a delivered
// row had lost its provenance, and assembly would then render a steer as an
// ordinary user turn.
func (e *Engine) drain(ctx context.Context, spec Spec, bare bool) error {
	rows, err := e.Transcript.ListForAssembly(ctx, spec.OrgID, spec.ConversationID)
	if err != nil {
		return err
	}
	ids := pendingIDsInOrder(rows)
	if len(ids) == 0 {
		return nil
	}
	subtype := ""
	if !bare {
		subtype = domain.MessageSubtypeInjectionSteer
	}
	return e.Transcript.MarkDelivered(ctx, spec.OrgID, spec.ConversationID, ids, subtype)
}

// hasPending reports whether any undelivered row is waiting — the would-stop
// recheck, which catches input that landed while the concluding turn was
// still streaming.
func (e *Engine) hasPending(ctx context.Context, spec Spec) (bool, error) {
	rows, err := e.Transcript.ListForAssembly(ctx, spec.OrgID, spec.ConversationID)
	if err != nil {
		return false, err
	}
	return len(pendingIDsInOrder(rows)) > 0, nil
}

// pendingIDsInOrder returns the undelivered rows' ids in assembly order
// (COALESCE(seq, id)). The store already returns that order, but sorting
// here keeps the flush independent of that guarantee — the ids are what the
// engine reasons about, and their order is what a steer stamp applies to.
func pendingIDsInOrder(rows []domain.Message) []int {
	type keyed struct {
		id  int
		key float64
	}
	var pending []keyed
	for _, r := range rows {
		if isDelivered(r) {
			continue
		}
		key := float64(r.ID)
		if r.Seq != nil {
			key = *r.Seq
		}
		pending = append(pending, keyed{id: r.ID, key: key})
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].key < pending[j].key })
	ids := make([]int, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.id)
	}
	return ids
}

// insertPending queues a row for the next drain. Used for the loop's own
// injected input (the executor-changed notice, the turn-end nudge): it goes
// through the same queue as user input rather than being spliced into an
// assembly, so there is exactly one injection point and it is durable.
func (e *Engine) insertPending(ctx context.Context, spec Spec, content, subtype string) error {
	pending := false
	if subtype == "" {
		subtype = "text"
	}
	_, err := e.Transcript.Insert(ctx, spec.OrgID, &domain.Message{
		ConversationID: spec.ConversationID,
		UserID:         spec.UserID,
		Role:           "user",
		Subtype:        subtype,
		Content:        content,
		Delivered:      &pending,
	})
	return err
}

// insertNotice writes a delivered user row that records something the loop
// decided — a guard park, an unrecoverable provider error. Delivered
// (not pending) because it is a statement of fact about this engagement, not
// input awaiting consumption: if the conversation is picked up later, the
// notice is history the model reads in place, not a message to act on.
func (e *Engine) insertNotice(ctx context.Context, spec Spec, content string) {
	if _, err := e.Transcript.Insert(ctx, spec.OrgID, &domain.Message{
		ConversationID: spec.ConversationID,
		Role:           "user",
		Subtype:        "text",
		Content:        content,
	}); err != nil {
		e.warn("insert loop notice failed", "conversation", spec.ConversationID, "error", err)
	}
}
