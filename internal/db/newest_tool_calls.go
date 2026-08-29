package db

import (
	"database/sql"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ScanNewestToolCalls drains a two-column (conversation_id, tool_calls) result
// set into the map NewestAssistantToolCallsForConversations returns. Both
// dialects own the SQL that picks the newest assistant row — DISTINCT ON in
// Postgres, a partitioned ROW_NUMBER in SQLite — and share what happens to the
// rows it produces, so the two can never disagree about which stored values
// mean "no tool calls".
//
// A row whose tool_calls are NULL, empty, unparseable, or an empty array
// contributes no entry. Unparseable is silent on purpose, and on the same terms
// as the display read's own tool_calls decode: this backs a display annotation
// whose contract is to omit rather than guess, so a malformed value has exactly
// the same answer as an absent one.
func ScanNewestToolCalls(rows *sql.Rows) (map[string][]domain.ToolCall, error) {
	out := map[string][]domain.ToolCall{}
	for rows.Next() {
		var conversationID string
		var toolCallsJSON sql.NullString
		if err := rows.Scan(&conversationID, &toolCallsJSON); err != nil {
			return nil, err
		}
		if !toolCallsJSON.Valid || toolCallsJSON.String == "" {
			continue
		}
		var calls []domain.ToolCall
		if err := json.Unmarshal([]byte(toolCallsJSON.String), &calls); err != nil || len(calls) == 0 {
			continue
		}
		out[conversationID] = calls
	}
	return out, rows.Err()
}
