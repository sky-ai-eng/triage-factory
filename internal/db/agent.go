package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CreateAgentRun inserts a new agent run.
func CreateAgentRun(database *sql.DB, run domain.AgentRun) error {
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	_, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, status, model, worktree_path, trigger_type, trigger_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath, triggerType, nullIfEmpty(run.TriggerID))
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CompleteAgentRun updates a run with completion info.
func CompleteAgentRun(database *sql.DB, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary string) error {
	now := time.Now()
	_, err := database.Exec(`
		UPDATE runs
		SET status = ?, completed_at = ?, total_cost_usd = ?, duration_ms = ?, num_turns = ?, stop_reason = ?, result_summary = ?
		WHERE id = ?
	`, status, now, costUSD, durationMs, numTurns, stopReason, resultSummary, runID)
	return err
}

// GetAgentRun returns a single agent run by ID.
func GetAgentRun(database *sql.DB, runID string) (*domain.AgentRun, error) {
	row := database.QueryRow(`
		SELECT id, task_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, memory_missing
		FROM runs WHERE id = ?
	`, runID)

	var r domain.AgentRun
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns sql.NullInt64
	var stopReason, worktreePath, model, resultSummary, sessionID sql.NullString

	err := row.Scan(&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
		&resultSummary, &sessionID, &r.MemoryMissing)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	r.Model = model.String
	r.StopReason = stopReason.String
	r.WorktreePath = worktreePath.String
	r.ResultSummary = resultSummary.String
	r.SessionID = sessionID.String
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	if costUSD.Valid {
		r.TotalCostUSD = &costUSD.Float64
	}
	if durationMs.Valid {
		v := int(durationMs.Int64)
		r.DurationMs = &v
	}
	if numTurns.Valid {
		v := int(numTurns.Int64)
		r.NumTurns = &v
	}

	return &r, nil
}

// AgentRunsForTask returns all runs for a given task.
func AgentRunsForTask(database *sql.DB, taskID string) ([]domain.AgentRun, error) {
	rows, err := database.Query(`
		SELECT id, task_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, memory_missing
		FROM runs WHERE task_id = ? ORDER BY started_at DESC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []domain.AgentRun
	for rows.Next() {
		var r domain.AgentRun
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64
		var stopReason, worktreePath, model, resultSummary, sessionID sql.NullString

		if err := rows.Scan(&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
			&resultSummary, &sessionID, &r.MemoryMissing); err != nil {
			return nil, err
		}

		r.Model = model.String
		r.StopReason = stopReason.String
		r.WorktreePath = worktreePath.String
		r.ResultSummary = resultSummary.String
		r.SessionID = sessionID.String
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}

		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// SetAgentRunSession stores the Claude Code session_id captured from
// `claude -p --output-format json` output. Called as soon as the spawner
// parses the init event from the stream so subsequent resume calls have a
// session to attach to. Separate from CompleteAgentRun because the session
// id needs to be persisted mid-run, before any terminal state is reached —
// the write-gate retry loop in SKY-141 depends on being able to resume a
// run whose initial invocation returned but failed the memory-file check.
func SetAgentRunSession(database *sql.DB, runID, sessionID string) error {
	_, err := database.Exec(`
		UPDATE runs SET session_id = ? WHERE id = ?
	`, sessionID, runID)
	return err
}

// MarkAgentRunMemoryMissing flags a run whose pre-complete memory-file gate
// exhausted all retries without the agent producing a memory file. The run
// still completes (we don't punish the agent for partial success by failing
// the run outright), but downstream UI and diagnostics surface the flag so
// the gap is visible. Called from the write-gate retry loop in SKY-141.
func MarkAgentRunMemoryMissing(database *sql.DB, runID string) error {
	_, err := database.Exec(`
		UPDATE runs SET memory_missing = 1 WHERE id = ?
	`, runID)
	return err
}

// HasActiveRunForTask returns true if the task has any agent run that hasn't
// reached a terminal state. Used as an in-flight gate for auto-delegation.
func HasActiveRunForTask(database *sql.DB, taskID string) (bool, error) {
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*) FROM runs
		WHERE task_id = ? AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval')
	`, taskID).Scan(&count)
	return count > 0, err
}

// ActiveRunIDsForTask returns the IDs of runs on the task that haven't
// reached a terminal state. Used by the task-close → run-cancel cascade
// so any in-flight agent stops work the moment the system decides the
// task is resolved (auto close, entity close, user dismiss).
//
// The terminal-state list matches HasActiveRunForTask — same answer to
// "is this run still running," different shape. pending_approval counts
// as terminal here: the process has exited and the user is deliberating,
// cancelling it would discard work that's ready to apply.
func ActiveRunIDsForTask(database *sql.DB, taskID string) ([]string, error) {
	rows, err := database.Query(`
		SELECT id FROM runs
		WHERE task_id = ? AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable', 'pending_approval')
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// InsertAgentMessage inserts a message and returns its ID.
func InsertAgentMessage(database *sql.DB, msg domain.AgentMessage) (int64, error) {
	var toolCallsJSON, metadataJSON sql.NullString

	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = sql.NullString{String: string(b), Valid: true}
	}

	result, err := database.Exec(`
		INSERT INTO run_messages (run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RunID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, nullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		nullStr(msg.Model), nullInt(msg.InputTokens), nullInt(msg.OutputTokens),
		nullInt(msg.CacheReadTokens), nullInt(msg.CacheCreationTokens),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// MessagesForRun returns all messages for a given agent run, ordered by ID.
func MessagesForRun(database *sql.DB, runID string) ([]domain.AgentMessage, error) {
	rows, err := database.Query(`
		SELECT id, run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
		       model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
		FROM run_messages WHERE run_id = ? ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []domain.AgentMessage
	for rows.Next() {
		var m domain.AgentMessage
		var content, subtype, toolCallsStr, toolCallID, metadataStr, model sql.NullString
		var inputTok, outputTok, cacheReadTok, cacheCreateTok sql.NullInt64

		if err := rows.Scan(
			&m.ID, &m.RunID, &m.Role, &content, &subtype, &toolCallsStr,
			&toolCallID, &m.IsError, &metadataStr, &model,
			&inputTok, &outputTok, &cacheReadTok, &cacheCreateTok, &m.CreatedAt,
		); err != nil {
			return nil, err
		}

		m.Content = content.String
		m.Subtype = subtype.String
		m.ToolCallID = toolCallID.String
		m.Model = model.String

		if toolCallsStr.Valid {
			_ = json.Unmarshal([]byte(toolCallsStr.String), &m.ToolCalls)
		}
		if metadataStr.Valid {
			_ = json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
		}
		if inputTok.Valid {
			v := int(inputTok.Int64)
			m.InputTokens = &v
		}
		if outputTok.Valid {
			v := int(outputTok.Int64)
			m.OutputTokens = &v
		}
		if cacheReadTok.Valid {
			v := int(cacheReadTok.Int64)
			m.CacheReadTokens = &v
		}
		if cacheCreateTok.Valid {
			v := int(cacheCreateTok.Int64)
			m.CacheCreationTokens = &v
		}

		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// TokenTotals holds summed token counts for a run.
type TokenTotals struct {
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	NumTurns            int
}

// RunTokenTotals sums token usage across all assistant messages in a run.
func RunTokenTotals(database *sql.DB, runID string) (*TokenTotals, error) {
	row := database.QueryRow(`
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM run_messages
		WHERE run_id = ? AND role = 'assistant'
	`, runID)

	var t TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
