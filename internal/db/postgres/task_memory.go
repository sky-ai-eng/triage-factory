package postgres

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

// taskMemoryStore is the Postgres impl of db.TaskMemoryStore. SQL is
// written fresh against D3's schema: $N placeholders, explicit org_id
// bind (the column is NOT NULL with no default), and org_id in every
// WHERE clause as defense in depth alongside RLS policy conversation_memory_all.
//
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Request-handler equivalents route
//     here. RLS policy conversation_memory_all (an EXISTS subquery against
//     conversations) gates the statement; the caller must be inside WithTx so
//     request.jwt.claims is set.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The delegate
//     spawner's runAgent goroutine routes here for both the
//     post-completion UpsertAgentMemorySystem and the engagement-start
//     GetMemoriesForEntitySystem materialization. org_id stays bound
//     in the INSERT/SELECT as defense in depth.
type taskMemoryStore struct {
	q     queryer
	admin queryer
}

func newTaskMemoryStore(q, admin queryer) db.TaskMemoryStore {
	return &taskMemoryStore{q: q, admin: admin}
}

var _ db.TaskMemoryStore = (*taskMemoryStore)(nil)

func (s *taskMemoryStore) UpsertAgentMemory(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error) {
	return upsertAgentMemory(ctx, s.q, orgID, conversationID, blueprintRunID, content, source)
}

func (s *taskMemoryStore) UpsertAgentMemorySystem(ctx context.Context, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error) {
	return upsertAgentMemory(ctx, s.admin, orgID, conversationID, blueprintRunID, content, source)
}

// upsertAgentMemory is the shared body for the app- and admin-pool
// variants. ON CONFLICT(conversation_id) is supported on both Postgres and
// SQLite given the UNIQUE(conversation_id) constraint; the overwrite replaces
// agent_content, source and blueprint_run_id together, which is what lets an
// agent's own conclusion supersede a generated stand-in.
//
// created_at is bound from Go-side time.Now() rather than the schema
// DEFAULT now() so multi-conversation bursts within the same Postgres tx
// don't tie on the tx-start timestamp — matches the EventStore pattern.
// Empty / whitespace-only content lands as SQL NULL, the shape
// db.ValidateMemorySource has already tied to source = 'none'.
//
// The write is wrapped in a data-modifying CTE and re-projected through
// taskMemoryWrittenSelect so RETURNING hands back the same shape
// GetMemoriesForEntity does (including the producing conversation's naming
// facts) rather than a bare conversation_memory row — matching
// updateConversationReturning's pattern in conversation.go.
func upsertAgentMemory(ctx context.Context, q queryer, orgID, conversationID, blueprintRunID, content string, source domain.MemorySource) (domain.TaskMemory, error) {
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
	mem, err := scanTaskMemory(q.QueryRowContext(ctx, `
		WITH written AS (
			INSERT INTO conversation_memory (id, org_id, conversation_id, blueprint_run_id, agent_content, source, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (conversation_id) DO UPDATE SET agent_content = EXCLUDED.agent_content, source = EXCLUDED.source, blueprint_run_id = EXCLUDED.blueprint_run_id
			RETURNING *
		)
	`+taskMemoryWrittenSelect,
		uuid.New().String(), orgID, conversationID, blueprintRun, agentContent, string(source), time.Now().UTC()))
	if err != nil {
		return domain.TaskMemory{}, fmt.Errorf("upsert conversation_memory: %w", err)
	}
	return mem, nil
}

// GetForConversationSystem is the one read that does NOT filter on
// agent_content: a 'none' row is the answer to "did this conversation settle
// what it remembered", and hiding it would make an unanswered conversation
// indistinguishable from one that answered "nothing".
func (s *taskMemoryStore) GetForConversationSystem(ctx context.Context, orgID, conversationID string) (*domain.TaskMemory, error) {
	mem, err := scanTaskMemory(s.admin.QueryRowContext(ctx, taskMemorySelect+`
		WHERE rm.org_id = $1 AND rm.conversation_id = $2
	`, orgID, conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation_memory for conversation: %w", err)
	}
	return &mem, nil
}

func (s *taskMemoryStore) GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error) {
	return getMemoriesForEntity(ctx, s.q, orgID, entityID)
}

// GetMemoriesForEntitySystem reads on the admin pool (BYPASSRLS), so the
// team scoping the app-pool variant inherits from RLS (conversation_memory_all
// delegates to conversations_select) is hand-rolled here off the
// materializing conversation's owning team_id. See
// getMemoriesForEntityTeamScoped + TFAC-506.
func (s *taskMemoryStore) GetMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) ([]domain.TaskMemory, error) {
	return getMemoriesForEntityTeamScoped(ctx, s.admin, orgID, entityID, teamID)
}

func getMemoriesForEntity(ctx context.Context, q queryer, orgID, entityID string) ([]domain.TaskMemory, error) {
	rows, err := q.QueryContext(ctx, taskMemorySelect+`
		WHERE rm.org_id = $1
		  AND rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE org_id = $1 AND entity_id = $2)
		ORDER BY rm.created_at ASC
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskMemories(rows)
}

// getMemoriesForEntityTeamScoped is the admin-pool (BYPASSRLS) read that
// reproduces, without RLS, the team scoping the app-pool path gets for
// free: a JOIN to the parent conversation plus the visibility branches of
// conversations_select. The engagement-start materializer has no JWT-claims
// context, so we scope by the materializing conversation's owning team_id
// directly — return the memory whose parent conversation that team can see:
// any org-visible conversation, plus team-visible conversations the team
// owns. Private-visibility conversations are excluded (creator-scoped, no
// user to match here; every conversation is visibility='team' today, so the
// 'org' arm is forward-compat). teamID binds through NULLIF(...)::uuid so an
// (in practice impossible) empty team_id degrades to "org-visible only"
// rather than a uuid cast error. org_id stays in the WHERE clause and on
// the JOIN as defense in depth alongside the now-bypassed RLS policy.
// GetRecentMemoriesForEntitySystem is getMemoriesForEntityTeamScoped with the
// cap pushed into the query (ORDER BY created_at DESC LIMIT), so an on-demand
// read on a hot entity doesn't transfer its whole history to keep the tail. A
// non-positive limit returns no rows (never unbounded — Postgres rejects a
// negative LIMIT); the host resolves it to a default first. The DESC LIMIT
// selects the most recent N; the result is reversed to ASC so callers compose
// identically to the unbounded read.
func (s *taskMemoryStore) GetRecentMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string, limit int) ([]domain.TaskMemory, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.admin.QueryContext(ctx, taskMemorySelectTeamScoped+`
		WHERE rm.org_id = $1
		  AND rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE org_id = $1 AND entity_id = $2)
		  AND (c.visibility = 'org' OR (c.visibility = 'team' AND c.team_id = NULLIF($3, '')::uuid))
		ORDER BY rm.created_at DESC
		LIMIT $4
	`, orgID, entityID, teamID, limit)
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

// reverseTaskMemories flips a DESC-ordered slice in place to ASC — used by the
// bounded read, whose ORDER BY created_at DESC LIMIT selects the most recent N
// but must hand them back oldest-first to match the unbounded read's contract.
func reverseTaskMemories(mems []domain.TaskMemory) {
	for i, j := 0, len(mems)-1; i < j; i, j = i+1, j-1 {
		mems[i], mems[j] = mems[j], mems[i]
	}
}

func getMemoriesForEntityTeamScoped(ctx context.Context, q queryer, orgID, entityID, teamID string) ([]domain.TaskMemory, error) {
	rows, err := q.QueryContext(ctx, taskMemorySelectTeamScoped+`
		WHERE rm.org_id = $1
		  AND rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE org_id = $1 AND entity_id = $2)
		  AND (c.visibility = 'org' OR (c.visibility = 'team' AND c.team_id = NULLIF($3, '')::uuid))
		ORDER BY rm.created_at ASC
	`, orgID, entityID, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskMemories(rows)
}

// taskMemoryColumns is the canonical projection of a conversation_memory row
// joined with the three facts about its producing conversation a reader needs
// and the memory row does not carry: the task it ran on, which splits an
// entity's memories into this task's and the rest, and the step index plus
// prompt name that let the memory be named after the work it records rather
// than after a row id — the columns
// scanTaskMemory reads, off the row alias `rm` (+ `c`/`p` for the join).
// Every read SELECTs it and every write RETURNs it (via taskMemoryWrittenSelect,
// re-applying the same join over the write's own output row), so the write
// shape cannot drift from the read shape.
const taskMemoryColumns = `rm.id, rm.conversation_id, rm.blueprint_run_id, rm.agent_content, rm.source, rm.created_at,
	       c.task_id, c.blueprint_step_index, p.name`

// taskMemorySelect / taskMemorySelectTeamScoped are the SELECT + FROM the
// reads start from. They differ only in how the conversation is
// reached — the app-pool read leaves visibility to RLS and LEFT JOINs, the
// admin-pool reads hand-roll the team filter off an INNER JOIN. The prompts
// arm is LEFT in both: a memory row whose prompt is gone still comes back,
// minus its legible name.
const taskMemorySelect = `
	SELECT ` + taskMemoryColumns + `
	FROM conversation_memory rm
	LEFT JOIN conversations c ON c.id = rm.conversation_id AND c.org_id = rm.org_id
	LEFT JOIN prompts p ON p.id = c.prompt_id
`

const taskMemorySelectTeamScoped = `
	SELECT ` + taskMemoryColumns + `
	FROM conversation_memory rm
	JOIN conversations c ON c.id = rm.conversation_id AND c.org_id = rm.org_id
	LEFT JOIN prompts p ON p.id = c.prompt_id
`

// taskMemoryWrittenSelect re-projects a write's `written` CTE output —
// aliased rm to match taskMemoryColumns' rm.-prefixed columns — through the
// same join to the producing conversation + prompt every read uses, so
// UpsertAgentMemory(System) hands back the identical shape
// GetMemoriesForEntity(System) would show for the same row.
const taskMemoryWrittenSelect = `
	SELECT ` + taskMemoryColumns + `
	FROM written rm
	LEFT JOIN conversations c ON c.id = rm.conversation_id AND c.org_id = rm.org_id
	LEFT JOIN prompts p ON p.id = c.prompt_id
`

// scanTaskMemory decodes one row in taskMemoryColumns order — shared by the
// multi-row entity reads (via scanTaskMemories), the per-conversation point
// read, and the single-row write's RETURNING.
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

// memoryRoleRankCASE is the SQL CASE expression mapping a role column
// reference to its domain.MemoryRoleOutranks rank — kept in sync with
// domain.memoryRoleRank (primary=3 > produced=2 > touched=1 > else=0).
// Duplicated inline (not a shared const) because it's substituted twice
// per statement with different column references.
const memoryRoleRankCASE = "(CASE %s WHEN 'primary' THEN 3 WHEN 'produced' THEN 2 WHEN 'touched' THEN 1 ELSE 0 END)"

func (s *taskMemoryStore) RecordEntityTouchSystem(ctx context.Context, orgID, conversationID, entityID, role string) error {
	query := `
		INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (conversation_id, entity_id) DO UPDATE SET role = EXCLUDED.role
		WHERE ` + fmt.Sprintf(memoryRoleRankCASE, "EXCLUDED.role") + ` > ` + fmt.Sprintf(memoryRoleRankCASE, "conversation_memory_entities.role")
	_, err := s.admin.ExecContext(ctx, query, orgID, conversationID, entityID, role, time.Now().UTC())
	return err
}

func (s *taskMemoryStore) CountMemoriesForEntitySystem(ctx context.Context, orgID, entityID, teamID string) (int, error) {
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM conversation_memory rm
		JOIN conversations c ON c.id = rm.conversation_id AND c.org_id = rm.org_id
		WHERE rm.org_id = $1
		  AND rm.agent_content IS NOT NULL
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE org_id = $1 AND entity_id = $2)
		  AND (c.visibility = 'org' OR (c.visibility = 'team' AND c.team_id = NULLIF($3, '')::uuid))
	`, orgID, entityID, teamID).Scan(&n)
	return n, err
}
