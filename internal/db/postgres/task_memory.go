package postgres

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// humanFeedbackHeader marks the start of the human verdict in
// materialized memory. Stable so the next agent's prompt context can
// parse the boundary regardless of which run wrote which half.
const humanFeedbackHeader = "## Human feedback (post-run)\n\n"

// humanFeedbackSeparator is the divider rendered when both halves of
// a memory row are populated.
const humanFeedbackSeparator = "\n\n---\n" + humanFeedbackHeader

// taskMemoryStore is the Postgres impl of db.TaskMemoryStore. SQL is
// written fresh against D3's schema: $N placeholders, explicit org_id
// bind (the column is NOT NULL with no default), and org_id in every
// WHERE clause as defense in depth alongside RLS policy run_memory_all.
//
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Request-handler equivalents
//     (review submit, PR submit, swipe-discard cleanup) route here.
//     RLS policy run_memory_all (an EXISTS subquery against runs)
//     gates the statement; the caller must be inside WithTx so
//     request.jwt.claims is set.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The delegate
//     spawner's runAgent goroutine routes here for both the
//     post-completion UpsertAgentMemorySystem and the run-start
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

func (s *taskMemoryStore) UpsertAgentMemory(ctx context.Context, orgID, runID, entityID, blueprintRunID, content string) error {
	return upsertAgentMemory(ctx, s.q, orgID, runID, entityID, blueprintRunID, content)
}

func (s *taskMemoryStore) UpsertAgentMemorySystem(ctx context.Context, orgID, runID, entityID, blueprintRunID, content string) error {
	return upsertAgentMemory(ctx, s.admin, orgID, runID, entityID, blueprintRunID, content)
}

// upsertAgentMemory is the shared body for the app- and admin-pool
// variants. ON CONFLICT(run_id) is supported on both Postgres and
// SQLite given the UNIQUE(run_id) constraint; the OVERWRITE only
// touches agent_content so any already-attached human_content stays
// intact across retries (the SKY-205 invariant).
//
// created_at is bound from Go-side time.Now() rather than the schema
// DEFAULT now() so multi-run bursts within the same Postgres tx don't
// tie on the tx-start timestamp — matches the EventStore pattern.
// Empty / whitespace-only content canonicalizes to SQL NULL so
// downstream consumers (factory's memory_missing derivation) see a
// single truth condition for "agent didn't comply with the gate."
func upsertAgentMemory(ctx context.Context, q queryer, orgID, runID, entityID, blueprintRunID, content string) error {
	var agentContent any
	if strings.TrimSpace(content) != "" {
		agentContent = content
	}
	var blueprintRun any
	if blueprintRunID != "" {
		blueprintRun = blueprintRunID
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO run_memory (id, org_id, run_id, entity_id, blueprint_run_id, agent_content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (run_id) DO UPDATE SET agent_content = EXCLUDED.agent_content, blueprint_run_id = EXCLUDED.blueprint_run_id
	`, uuid.New().String(), orgID, runID, entityID, blueprintRun, agentContent, time.Now().UTC())
	return err
}

func (s *taskMemoryStore) UpdateRunMemoryHumanContent(ctx context.Context, orgID, runID, content string) error {
	var humanContent any
	if strings.TrimSpace(content) != "" {
		humanContent = content
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE run_memory SET human_content = $1 WHERE org_id = $2 AND run_id = $3`,
		humanContent, orgID, runID,
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
		err := s.q.QueryRowContext(ctx,
			`SELECT 1 FROM run_memory WHERE org_id = $1 AND run_id = $2 LIMIT 1`,
			orgID, runID,
		).Scan(&exists)
		switch err {
		case nil:
			// Row exists; UPDATE was a no-op.
		case sql.ErrNoRows:
			memoryLog.Warn("no run_memory row; human_content not recorded", "run_id", runID)
		default:
			memoryLog.Warn("verify run_memory row after no-op human_content update failed", "run_id", runID, "error", err)
		}
	}
	return nil
}

func (s *taskMemoryStore) GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error) {
	return getMemoriesForEntity(ctx, s.q, orgID, entityID)
}

func (s *taskMemoryStore) GetMemoriesForEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error) {
	return getMemoriesForEntity(ctx, s.admin, orgID, entityID)
}

func getMemoriesForEntity(ctx context.Context, q queryer, orgID, entityID string) ([]domain.TaskMemory, error) {
	rows, err := q.QueryContext(ctx, `
		WITH related AS (
			SELECT id FROM entities WHERE org_id = $1 AND id = $2
			UNION
			SELECT to_entity_id FROM entity_links WHERE org_id = $1 AND from_entity_id = $2
			UNION
			SELECT from_entity_id FROM entity_links WHERE org_id = $1 AND to_entity_id = $2
		)
		SELECT rm.id, rm.run_id, rm.entity_id, rm.blueprint_run_id, rm.agent_content, rm.human_content, rm.created_at
		FROM run_memory rm
		WHERE rm.org_id = $1 AND rm.entity_id IN (SELECT id FROM related)
		ORDER BY rm.created_at ASC
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaskMemory
	for rows.Next() {
		var m domain.TaskMemory
		var blueprintRunID, agentContent, humanContent sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&m.ID, &m.RunID, &m.EntityID, &blueprintRunID, &agentContent, &humanContent, &createdAt); err != nil {
			return nil, err
		}
		m.BlueprintRunID = blueprintRunID.String
		m.Content = materializeMemory(agentContent.String, humanContent.String)
		m.CreatedAt = createdAt
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *taskMemoryStore) GetRunMemory(ctx context.Context, orgID, runID string) (*domain.TaskMemory, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, run_id, entity_id, blueprint_run_id, agent_content, human_content, created_at
		FROM run_memory WHERE org_id = $1 AND run_id = $2
	`, orgID, runID)

	var m domain.TaskMemory
	var blueprintRunID, agentContent, humanContent sql.NullString
	var createdAt time.Time
	err := row.Scan(&m.ID, &m.RunID, &m.EntityID, &blueprintRunID, &agentContent, &humanContent, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.BlueprintRunID = blueprintRunID.String
	m.Content = materializeMemory(agentContent.String, humanContent.String)
	m.CreatedAt = createdAt
	return &m, nil
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
