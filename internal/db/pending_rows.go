package db

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PendingRow is the dialect-neutral shape returned after a messages row has
// already been scanned. The dialect stores own the SQL and NULL handling that
// populate it; keeping the normalized DTO here lets every consumer agree on
// what the returned columns mean.
type PendingRow struct {
	ID        int64
	OrgID     string
	ConvID    string
	Content   string
	Subtype   string
	UserID    string
	Metadata  string
	CreatedAt time.Time
}

// ConsumePendingInput maps a dialect-specific flush result to the pending
// input store's return shape. Accepting the result and error separately lets a
// dialect keep its SQL local while spelling the common consume path as:
//
//	return db.ConsumePendingInput(flushPendingInput(...))
func ConsumePendingInput(rows []PendingRow, err error) (message, userID string, ok bool, consumeErr error) {
	if err != nil {
		return "", "", false, err
	}
	message, userID, ok = JoinPendingRows(rows)
	return message, userID, ok, nil
}

// JoinPendingRows assembles queued input into the single prompt string an SDK
// resume takes: every row in id order, separated by a blank line. Peek and
// Consume both go through it, which keeps the routing decision and delivery
// from disagreeing about what is pending.
//
// The rows are the person messages waiting and the stop notes that lie
// between the oldest and the newest of them (PendingRowBounds). A session
// holds only what it was sent, so a stop note in place is how it learns that
// the messages before it were in front of a stop and the ones after it came
// later. With no stop note among them, the text is the person messages alone.
//
// The userID is the newest person message's; see ConversationPendingInputStore
// for why a queue with several authors reports one, and why it is that one.
func JoinPendingRows(rows []PendingRow) (message, userID string, ok bool) {
	sorted := slices.Clone(rows)
	slices.SortStableFunc(sorted, func(a, b PendingRow) int { return cmp.Compare(a.ID, b.ID) })
	parts := make([]string, 0, len(sorted))
	for _, r := range sorted {
		parts = append(parts, r.Content)
		if r.Subtype != domain.MessageSubtypeStopNote {
			userID, ok = r.UserID, true
		}
	}
	if !ok {
		return "", "", false
	}
	return strings.Join(parts, PendingInputSeparator), userID, true
}

// PendingRowBounds returns the smallest and largest id among the person
// messages a pending-input read returned: the stop notes a resume message
// carries are the ones strictly between the two. ok is false with fewer than
// two rows, where nothing lies between.
func PendingRowBounds(rows []PendingRow) (lo, hi int64, ok bool) {
	if len(rows) < 2 {
		return 0, 0, false
	}
	lo, hi = rows[0].ID, rows[0].ID
	for _, r := range rows[1:] {
		lo, hi = min(lo, r.ID), max(hi, r.ID)
	}
	return lo, hi, true
}

// StagedInjectionFromPending maps one normalized messages row onto the
// StagedInjection shape. AppendSystem and FlushPendingSystem both use this
// mapper, so the same row cannot acquire different domain meaning depending
// on which dialect or write/read path returned it.
func StagedInjectionFromPending(r PendingRow) domain.StagedInjection {
	n := domain.StagedInjection{
		ID:             strconv.FormatInt(r.ID, 10),
		ConversationID: r.ConvID,
		OrgID:          r.OrgID,
		Subtype:        r.Subtype,
		Body:           r.Content,
		CreatedAt:      r.CreatedAt,
	}
	if r.Metadata != "" {
		var meta struct {
			Producer     string `json:"producer"`
			EventID      string `json:"event_id"`
			EventType    string `json:"event_type"`
			TaskClose    string `json:"task_close"`
			AssignedTask string `json:"assigned_task"`
			Assignee     string `json:"assignee"`
		}
		_ = json.Unmarshal([]byte(r.Metadata), &meta)
		n.Producer = meta.Producer
		n.Provenance = domain.NoteProvenance{
			EventID: meta.EventID, EventType: meta.EventType, TaskClose: meta.TaskClose,
			AssignedTask: meta.AssignedTask, Assignee: meta.Assignee,
		}
	}
	return n
}

// StagedInjectionMetadata renders the metadata column a staged note is written
// with. It is the write half of the pair StagedInjectionFromPending reads, kept
// beside it so the keys one writes are the keys the other looks for.
func StagedInjectionMetadata(n domain.StagedInjection) (string, error) {
	raw, err := json.Marshal(domain.NoteMetadata(n.Producer, n.Provenance))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// StagedInjectionsFromPending maps flushed pending rows through
// StagedInjectionFromPending.
func StagedInjectionsFromPending(flushed []PendingRow) []domain.StagedInjection {
	var out []domain.StagedInjection
	for _, r := range flushed {
		out = append(out, StagedInjectionFromPending(r))
	}
	return out
}
