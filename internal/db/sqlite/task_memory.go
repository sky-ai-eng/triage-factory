package sqlite

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
// a memory row are populated — leading newlines and HR push the
// header onto its own visual block after the agent's text. The
// agent-empty + human-set case uses humanFeedbackHeader alone (no
// stray HR ahead of the only block of content).
const humanFeedbackSeparator = "\n\n---\n" + humanFeedbackHeader

// taskMemoryStore — SQLite impl. The constructor accepts two queryers
// for signature parity with the Postgres impl; SQLite has one
// connection so both collapse to the same queryer. The `...System`
// variants delegate to their non-System counterparts.
type taskMemoryStore struct{ q queryer }

func newTaskMemoryStore(q, _ queryer) db.TaskMemoryStore { return &taskMemoryStore{q: q} }

var _ db.TaskMemoryStore = (*taskMemoryStore)(nil)

func (s *taskMemoryStore) UpsertAgentMemory(ctx context.Context, orgID, runID, entityID, blueprintRunID, content string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	var agentContent any
	if strings.TrimSpace(content) != "" {
		agentContent = content
	}
	var blueprintRun any
	if blueprintRunID != "" {
		blueprintRun = blueprintRunID
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO run_memory (id, run_id, entity_id, blueprint_run_id, agent_content, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET agent_content = excluded.agent_content, blueprint_run_id = excluded.blueprint_run_id
	`, uuid.New().String(), runID, entityID, blueprintRun, agentContent, time.Now().UTC())
	return err
}

func (s *taskMemoryStore) UpsertAgentMemorySystem(ctx context.Context, orgID, runID, entityID, blueprintRunID, content string) error {
	return s.UpsertAgentMemory(ctx, orgID, runID, entityID, blueprintRunID, content)
}

func (s *taskMemoryStore) UpdateRunMemoryHumanContent(ctx context.Context, orgID, runID, content string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	var humanContent any
	if strings.TrimSpace(content) != "" {
		humanContent = content
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE run_memory SET human_content = ? WHERE run_id = ?`,
		humanContent, runID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// In SQLite, RowsAffected can be 0 both when no row matches
		// and when the UPDATE is a no-op (writing the same human_content
		// again, or NULL to an already-NULL row). Verify existence
		// before claiming the row is missing.
		var exists int
		err := s.q.QueryRowContext(ctx,
			`SELECT 1 FROM run_memory WHERE run_id = ? LIMIT 1`,
			runID,
		).Scan(&exists)
		switch err {
		case nil:
			// Matching row exists; the UPDATE was a no-op.
		case sql.ErrNoRows:
			// Logged-and-returned-nil: if the run_memory row genuinely
			// doesn't exist (cleanup race, taken-over run, etc.), the
			// human's submit shouldn't fail. The agent-side upsert path
			// will surface its own warning if it failed earlier.
			memoryLog.Warn("no run_memory row; human_content not recorded", "run_id", runID)
		default:
			memoryLog.Warn("verify run_memory row after no-op human_content update failed", "run_id", runID, "error", err)
		}
	}
	return nil
}

// UpdateRunMemoryHumanContentSystem is identical to UpdateRunMemoryHumanContent
// in SQLite: local mode is single-tenant (N=1) with no RLS, so there is no
// admin/app pool split. The method exists for parity with the Postgres store,
// where the reconciler (no JWT-claims context) needs the admin pool. TFAC-464.
func (s *taskMemoryStore) UpdateRunMemoryHumanContentSystem(ctx context.Context, orgID, runID, content string) error {
	return s.UpdateRunMemoryHumanContent(ctx, orgID, runID, content)
}

func (s *taskMemoryStore) GetMemoriesForEntity(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return getMemoriesForEntity(ctx, s.q, entityID)
}

func (s *taskMemoryStore) GetMemoriesForEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.TaskMemory, error) {
	return s.GetMemoriesForEntity(ctx, orgID, entityID)
}

func getMemoriesForEntity(ctx context.Context, q queryer, entityID string) ([]domain.TaskMemory, error) {
	rows, err := q.QueryContext(ctx, `
		WITH related AS (
			SELECT id FROM entities WHERE id = ?
			UNION
			SELECT to_entity_id FROM entity_links WHERE from_entity_id = ?
			UNION
			SELECT from_entity_id FROM entity_links WHERE to_entity_id = ?
		)
		SELECT rm.id, rm.run_id, rm.entity_id, rm.blueprint_run_id, rm.agent_content, rm.human_content, rm.created_at
		FROM run_memory rm
		WHERE rm.entity_id IN (SELECT id FROM related)
		ORDER BY rm.created_at ASC
	`, entityID, entityID, entityID)
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, run_id, entity_id, blueprint_run_id, agent_content, human_content, created_at
		FROM run_memory WHERE run_id = ?
	`, runID)

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
		// Agent-empty + human-set: render just the header + body, no
		// leading HR. The HR only makes sense as a divider between two
		// blocks; without an agent block it would just be visual noise
		// the next agent has to skip past.
		return humanFeedbackHeader + humanContent
	default:
		return agentContent
	}
}
