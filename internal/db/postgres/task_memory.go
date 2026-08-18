package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// humanFeedbackHeader marks the start of the human verdict in
// materialized memory. Stable so the next agent's prompt context can
// parse the boundary regardless of which conversation wrote which half.
const humanFeedbackHeader = "## Human feedback (post-run)\n\n"

// humanFeedbackSeparator is the divider rendered when both halves of
// a memory row are populated.
const humanFeedbackSeparator = "\n\n---\n" + humanFeedbackHeader

// taskMemoryStore is the Postgres impl of db.TaskMemoryStore. SQL is
// written fresh against D3's schema: $N placeholders, explicit org_id
// bind (the column is NOT NULL with no default), and org_id in every
// WHERE clause as defense in depth alongside RLS policy conversation_memory_all.
//
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Request-handler equivalents
//     (review submit, PR submit, swipe-discard cleanup) route here.
//     RLS policy conversation_memory_all (an EXISTS subquery against conversations)
//     gates the statement; the caller must be inside WithTx so
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

func (s *taskMemoryStore) UpsertAgentMemory(ctx context.Context, orgID, conversationID, entityID, blueprintRunID, content string) error {
	return upsertAgentMemory(ctx, s.q, orgID, conversationID, entityID, blueprintRunID, content)
}

func (s *taskMemoryStore) UpsertAgentMemorySystem(ctx context.Context, orgID, conversationID, entityID, blueprintRunID, content string) error {
	return upsertAgentMemory(ctx, s.admin, orgID, conversationID, entityID, blueprintRunID, content)
}

// upsertAgentMemory is the shared body for the app- and admin-pool
// variants. ON CONFLICT(conversation_id) is supported on both Postgres and
// SQLite given the UNIQUE(conversation_id) constraint; the OVERWRITE only
// touches agent_content so any already-attached human_content stays
// intact across retries.
//
// created_at is bound from Go-side time.Now() rather than the schema
// DEFAULT now() so multi-conversation bursts within the same Postgres tx
// don't tie on the tx-start timestamp — matches the EventStore pattern.
// Empty / whitespace-only content canonicalizes to SQL NULL so
// downstream consumers (factory's memory_missing derivation) see a
// single truth condition for "agent didn't comply with the gate."
func upsertAgentMemory(ctx context.Context, q queryer, orgID, conversationID, entityID, blueprintRunID, content string) error {
	var agentContent any
	if strings.TrimSpace(content) != "" {
		agentContent = content
	}
	var blueprintRun any
	if blueprintRunID != "" {
		blueprintRun = blueprintRunID
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, blueprint_run_id, agent_content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (conversation_id) DO UPDATE SET agent_content = EXCLUDED.agent_content, blueprint_run_id = EXCLUDED.blueprint_run_id
	`, uuid.New().String(), orgID, conversationID, entityID, blueprintRun, agentContent, time.Now().UTC())
	return err
}

func (s *taskMemoryStore) UpdateConversationMemoryHumanContent(ctx context.Context, orgID, conversationID, content string) error {
	return updateConversationMemoryHumanContent(ctx, s.q, orgID, conversationID, content)
}

// UpdateConversationMemoryHumanContentSystem overwrites human_content on the admin pool
// (BYPASSRLS) for the artifact reconciler, which has no JWT-claims context. Same
// body as the app-pool variant, different pool. See the interface doc + TFAC-464.
func (s *taskMemoryStore) UpdateConversationMemoryHumanContentSystem(ctx context.Context, orgID, conversationID, content string) error {
	return updateConversationMemoryHumanContent(ctx, s.admin, orgID, conversationID, content)
}

// updateConversationMemoryHumanContent is the shared body for the app- and admin-pool
// variants: a plain overwrite of human_content (empty/whitespace → NULL),
// logged-not-fatal on a missing row.
func updateConversationMemoryHumanContent(ctx context.Context, q queryer, orgID, conversationID, content string) error {
	var humanContent any
	if strings.TrimSpace(content) != "" {
		humanContent = content
	}
	res, err := q.ExecContext(ctx,
		`UPDATE conversation_memory SET human_content = $1 WHERE org_id = $2 AND conversation_id = $3`,
		humanContent, orgID, conversationID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Verify whether the row exists before claiming it's missing
		// — keeps parity with the SQLite branch where RowsAffected
		// can be 0 on a no-op UPDATE. Postgres reports affected rows
		// precisely, so this is mostly defensive; the symmetry is
		// what matters.
		var exists int
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM conversation_memory WHERE org_id = $1 AND conversation_id = $2 LIMIT 1`,
			orgID, conversationID,
		).Scan(&exists)
		switch err {
		case nil:
			// Row exists; UPDATE was a no-op.
		case sql.ErrNoRows:
			memoryLog.Warn("no conversation_memory row; human_content not recorded", "conversation_id", conversationID)
		default:
			memoryLog.Warn("verify conversation_memory row after no-op human_content update failed", "conversation_id", conversationID, "error", err)
		}
	}
	return nil
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

// taskMemorySelect / taskMemorySelectTeamScoped are the projection + FROM the
// entity reads start from: the memory row, plus the two facts about the
// producing conversation that let a reader name the memory after the work it
// records (its blueprint step index and the prompt it ran) rather than after a
// row id. They differ only in how the conversation is reached — the app-pool
// read leaves visibility to RLS and LEFT JOINs, the admin-pool reads hand-roll
// the team filter off an INNER JOIN. The prompts arm is LEFT in both: a memory
// row whose prompt is gone still comes back, minus its legible name.
const taskMemorySelect = `
	SELECT rm.id, rm.conversation_id, rm.entity_id, rm.blueprint_run_id, rm.agent_content, rm.human_content, rm.created_at,
	       c.blueprint_step_index, p.name
	FROM conversation_memory rm
	LEFT JOIN conversations c ON c.id = rm.conversation_id AND c.org_id = rm.org_id
	LEFT JOIN prompts p ON p.id = c.prompt_id
`

const taskMemorySelectTeamScoped = `
	SELECT rm.id, rm.conversation_id, rm.entity_id, rm.blueprint_run_id, rm.agent_content, rm.human_content, rm.created_at,
	       c.blueprint_step_index, p.name
	FROM conversation_memory rm
	JOIN conversations c ON c.id = rm.conversation_id AND c.org_id = rm.org_id
	LEFT JOIN prompts p ON p.id = c.prompt_id
`

// scanTaskMemories drains a conversation_memory result set (the column list the
// two SELECTs above share) into materialized TaskMemory rows.
func scanTaskMemories(rows *sql.Rows) ([]domain.TaskMemory, error) {
	var out []domain.TaskMemory
	for rows.Next() {
		var m domain.TaskMemory
		var blueprintRunID, agentContent, humanContent, promptName sql.NullString
		var stepIndex sql.NullInt64
		var createdAt time.Time
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.EntityID, &blueprintRunID, &agentContent, &humanContent, &createdAt,
			&stepIndex, &promptName); err != nil {
			return nil, err
		}
		m.BlueprintRunID = blueprintRunID.String
		m.Content = materializeMemory(agentContent.String, humanContent.String)
		m.CreatedAt = createdAt
		if stepIndex.Valid {
			idx := int(stepIndex.Int64)
			m.StepIndex = &idx
		}
		m.PromptName = promptName.String
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
		  AND rm.conversation_id IN (SELECT conversation_id FROM conversation_memory_entities WHERE org_id = $1 AND entity_id = $2)
		  AND (c.visibility = 'org' OR (c.visibility = 'team' AND c.team_id = NULLIF($3, '')::uuid))
	`, orgID, entityID, teamID).Scan(&n)
	return n, err
}

// materializeMemory composes the agent's narrative and the human's
// verdict into a single Content string the next agent reads. The
// separator format is stable — the next agent's prompt context parses
// it as a boundary, so any change here needs a matching update to the
// briefing docs that teach agents how to read prior memory.
func materializeMemory(agentContent, humanContent string) string {
	hasAgent := strings.TrimSpace(agentContent) != ""
	hasHuman := strings.TrimSpace(humanContent) != ""
	switch {
	case hasAgent && hasHuman:
		return agentContent + humanFeedbackSeparator + humanContent
	case hasHuman:
		return humanFeedbackHeader + humanContent
	default:
		return agentContent
	}
}
