package db

import (
	"encoding/json"
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
// resume takes: every row, oldest first, separated by a blank line. Peek and
// Consume both go through it, which keeps the routing decision and delivery
// from disagreeing about what is pending.
//
// The userID is the newest row's; see ConversationPendingInputStore for why a
// queue with several authors reports one, and why it is that one.
func JoinPendingRows(rows []PendingRow) (message, userID string, ok bool) {
	if len(rows) == 0 {
		return "", "", false
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, r.Content)
	}
	return strings.Join(parts, PendingInputSeparator), rows[len(rows)-1].UserID, true
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
		Body:           r.Content,
		CreatedAt:      r.CreatedAt,
	}
	if r.Metadata != "" {
		var meta struct {
			Producer string `json:"producer"`
		}
		_ = json.Unmarshal([]byte(r.Metadata), &meta)
		n.Producer = meta.Producer
	}
	return n
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
