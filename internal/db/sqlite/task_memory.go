package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskMemoryStore — SQLite impl. The constructor accepts two queryers
// for signature parity with the Postgres impl; SQLite has one
// connection so both collapse to the same queryer. The `...System`
// variants with a non-System counterpart delegate to it;
// GetForConversationSystem, RecordEntityTouchSystem and
// CountMemoriesForEntitySystem have none and implement directly.
type taskMemoryStore struct{ q queryer }

func newTaskMemoryStore(q, _ queryer) db.TaskMemoryStore { return &taskMemoryStore{q: q} }

var _ db.TaskMemoryStore = (*taskMemoryStore)(nil)

// UpsertAgentMemory returns the stored row, sourced from RETURNING on the
// write statement itself. SQLite's RETURNING clause forbids the join
// taskMemorySelect uses, so the producing conversation's naming facts
// (StepIndex, PromptName) become correlated scalar subqueries in
// taskMemoryRowColumns instead — same restriction sqliteClaimReturningColumns
// (conversation.go) exists for.
func (s *taskMemoryStore) UpsertAgentMemory(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.TaskMemory{}, err
	}
	if err := db.ValidateMemorySource(content, source); err != nil {
		return domain.TaskMemory{}, err
	}
	var agentContent any
	if source != domain.MemorySourceNone {
		agentContent = content
	}
	var blueprintRun any
	if blueprintRunID != "" {
		blueprintRun = blueprintRunID
	}
	mem, err := scanTaskMemory(s.q.QueryRowContext(ctx, `
		INSERT INTO conversation_memory (id, conversation_id, blueprint_run_id, agent_content, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET agent_content = excluded.agent_content, source = excluded.source, blueprint_run_id = excluded.blueprint_run_id
		RETURNING `+taskMemoryRowColumns,
		uuid.New().String(), conversationID, blueprintRun, agentContent, string(source), time.Now().UTC()))
	if err != nil {
		return domain.TaskMemory{}, fmt.Errorf("upsert conversation_memory: %w", err)
	}
	return mem, nil
}

func (s *taskMemoryStore) UpsertAgentMemorySystem(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error) {
	return s.UpsertAgentMemory(ctx, orgID, conversationID, blueprintRunID, content, source)
}

// GetForConversationSystem is the one read that does NOT filter on
// agent_content: a 'none' row is the answer to "did this conversation settle
// what it remembered", and hiding it would make an unanswered conversation
// indistinguishable from one that answered "nothing".
func (s *taskMemoryStore) GetForConversationSystem(ctx context.Context, orgID, conversationID string) (*domain.TaskMemory, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	mem, err := scanTaskMemory(s.q.QueryRowContext(ctx, taskMemorySelect+`
		WHERE rm.conversation_id = ?
	`, conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation_memory for conversation: %w", err)
	}
	return &mem, nil
}

func (s *taskMemoryStore) GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return getMemoriesForEntity(ctx, s.q, entityID)
}

// GetMemoriesForEntitySystem ignores teamID: local mode is single-tenant
// (N=1, one team), so there is no cross-team memory to scope out — every
// conversation on the entity belongs to the lone team. The param exists for
// parity with the Postgres store, where the admin pool bypasses RLS and must
// hand-roll the team filter off the materializing conversation's team_id
// (TFAC-506).
func (s *taskMemoryStore) GetMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) ([]domain.TaskMemory, error) {
	_ = teamID
	return s.GetMemoriesForEntity(ctx, orgID, entityID)
}

// GetRecentMemoriesForEntitySystem ignores teamID like its unbounded sibling
// (local mode is single-team) and pushes the cap into the query so a
// long-history entity isn't fully fetched to keep only the tail. A non-positive
// limit returns no rows (never unbounded); the host resolves it to a default
// first. The query orders DESC LIMIT (the most recent N) and the result is
// reversed to ASC so the composition matches the unbounded read.
func (s *taskMemoryStore) GetRecentMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string, limit int) ([]domain.TaskMemory, error) {
	_ = teamID
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.QueryContext(ctx, taskMemorySelect+`
		WHERE rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE entity_id = ?)
		ORDER BY rm.created_at DESC
		LIMIT ?
	`, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mems, err := scanTaskMemories(rows)
	if err != nil {
		return nil, err
	}
	reverseTaskMemories(mems)
	return mems, nil
}

func getMemoriesForEntity(ctx context.Context, q queryer, entityID string) ([]domain.TaskMemory, error) {
	rows, err := q.QueryContext(ctx, taskMemorySelect+`
		WHERE rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE entity_id = ?)
		ORDER BY rm.created_at ASC
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskMemories(rows)
}

// taskMemorySelect is the shared projection + FROM every conversation_memory
// read starts from: the memory row itself, plus the three facts about the
// producing conversation a reader needs and the memory row does not carry —
// the task it ran on, which splits an entity's memories into this task's and
// the rest, and the step index plus prompt name that let the memory be named
// after the work it records rather than after a row id. Both joins are LEFT so
// a row whose conversation or prompt is gone still comes back — it loses its
// legible name, never its content.
const taskMemorySelect = `
	SELECT rm.id, rm.conversation_id, rm.blueprint_run_id, rm.agent_content, rm.source, rm.created_at,
	       c.task_id, c.blueprint_step_index, p.name
	FROM conversation_memory rm
	LEFT JOIN conversations c ON c.id = rm.conversation_id
	LEFT JOIN prompts p ON p.id = c.prompt_id
`

// taskMemoryRowColumns is the RETURNING-safe mirror of taskMemorySelect's
// projection, in the same order scanTaskMemory reads them: SQLite's
// RETURNING clause forbids the LEFT JOIN taskMemorySelect uses, so the
// producing conversation's naming facts become correlated scalar subqueries
// against the bare conversations/prompts table names instead — the same
// restriction sqliteClaimReturningColumns (conversation.go) exists for.
const taskMemoryRowColumns = `
	id, conversation_id, blueprint_run_id, agent_content, source, created_at,
	(SELECT c.task_id FROM conversations c WHERE c.id = conversation_memory.conversation_id),
	(SELECT c.blueprint_step_index FROM conversations c WHERE c.id = conversation_memory.conversation_id),
	(SELECT p.name FROM conversations c JOIN prompts p ON p.id = c.prompt_id WHERE c.id = conversation_memory.conversation_id)
`

// scanTaskMemory decodes one row in taskMemorySelect/taskMemoryRowColumns
// order — shared by the multi-row entity reads (via scanTaskMemories), the
// per-conversation point read, and the single-row write's RETURNING.
func scanTaskMemory(row interface{ Scan(...any) error }) (domain.TaskMemory, error) {
	var m domain.TaskMemory
	var blueprintRunID, agentContent, taskID, promptName sql.NullString
	var source string
	var stepIndex sql.NullInt64
	var createdAt time.Time
	if err := row.Scan(&m.ID, &m.ConversationID, &blueprintRunID, &agentContent, &source, &createdAt,
		&taskID, &stepIndex, &promptName); err != nil {
		return domain.TaskMemory{}, err
	}
	m.BlueprintRunID = blueprintRunID.String
	m.Content = agentContent.String
	m.Source = domain.MemorySource(source)
	m.CreatedAt = createdAt
	m.TaskID = taskID.String
	if stepIndex.Valid {
		idx := int(stepIndex.Int64)
		m.StepIndex = &idx
	}
	m.PromptName = promptName.String
	return m, nil
}

// scanTaskMemories drains a conversation_memory result set into TaskMemory
// rows, one scanTaskMemory call per row.
func scanTaskMemories(rows *sql.Rows) ([]domain.TaskMemory, error) {
	var out []domain.TaskMemory
	for rows.Next() {
		m, err := scanTaskMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// reverseTaskMemories flips a DESC-ordered slice in place to ASC — used by the
// bounded read, whose ORDER BY created_at DESC LIMIT selects the most recent N
// but must hand them back oldest-first to match the unbounded read's contract.
func reverseTaskMemories(mems []domain.TaskMemory) {
	for i, j := 0, len(mems)-1; i < j; i, j = i+1, j-1 {
		mems[i], mems[j] = mems[j], mems[i]
	}
}

// memoryRoleRankCASE is the SQL CASE expression mapping a role column
// reference to its domain.MemoryRoleOutranks rank — kept in sync with
// domain.memoryRoleRank (primary=3 > produced=2 > touched=1 > else=0).
// Duplicated inline (not a shared const) because it's substituted twice
// per statement with different column references.
const memoryRoleRankCASE = "(CASE %s WHEN 'primary' THEN 3 WHEN 'produced' THEN 2 WHEN 'touched' THEN 1 ELSE 0 END)"

func (s *taskMemoryStore) RecordEntityTouchSystem(ctx context.Context, orgID, conversationID, entityID, role string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	query := `
		INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id, entity_id) DO UPDATE SET role = excluded.role
		WHERE ` + fmt.Sprintf(memoryRoleRankCASE, "excluded.role") + ` > ` + fmt.Sprintf(memoryRoleRankCASE, "conversation_memory_entities.role")
	_, err := s.q.ExecContext(ctx, query, orgID, conversationID, entityID, role, time.Now().UTC())
	return err
}

func (s *taskMemoryStore) CountMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) (int, error) {
	_ = teamID
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversation_memory rm
		WHERE rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE entity_id = ?)
	`, entityID).Scan(&n)
	return n, err
}
