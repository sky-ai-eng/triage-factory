package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// siblingWorkRelationSQL is the Postgres twin's relation in this dialect,
// over the same three aliases: x is the task, c the conversation, and t the
// conversation's own task.
var siblingWorkRelationSQL = `(x.id <> t.id
	   AND (x.team_id = c.team_id
	        OR EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = x.id AND tt.team_id = c.team_id))
	   AND (x.entity_id = t.entity_id
	        OR EXISTS (
	            SELECT 1 FROM conversation_memory_entities cme
	             WHERE cme.conversation_id = c.id AND cme.entity_id = x.entity_id)
	        OR EXISTS (
	            SELECT 1 FROM conversations lx
	              JOIN conversation_memory_entities cmx ON cmx.conversation_id = lx.id
	             WHERE lx.task_id = x.id
	               AND ` + liveConversationSQL("lx") + `
	               AND cmx.entity_id = t.entity_id)))`

func (s *taskStore) AssignmentSystem(ctx context.Context, orgID, taskID string) (*domain.TaskAssignment, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var a domain.TaskAssignment
	err := s.q.QueryRowContext(ctx, `
		SELECT x.id, x.entity_id, x.event_type, x.dedup_key, e.source_id,
		       COALESCE(x.claimed_by_agent_id, ''), COALESCE(x.claimed_by_user_id, ''),
		       COALESCE(u.display_name, ''), COALESCE(tm.name, '')
		  FROM tasks x
		  JOIN entities e ON e.id = x.entity_id
		  LEFT JOIN users u ON u.id = x.claimed_by_user_id
		  LEFT JOIN teams tm ON tm.id = x.team_id
		 WHERE x.id = ?
		   AND x.status NOT IN ('done', 'dismissed')
		   AND (x.claimed_by_agent_id IS NOT NULL OR x.claimed_by_user_id IS NOT NULL)
	`, taskID).Scan(&a.TaskID, &a.EntityID, &a.EventType, &a.DedupKey, &a.EntitySourceID,
		&a.AgentID, &a.UserID, &a.AssigneeName, &a.TeamName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *taskStore) SiblingWorkRecipientsSystem(ctx context.Context, orgID, taskID string) ([]domain.SiblingWorkRecipient, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT c.id, t.id, t.entity_id, t.event_type, t.dedup_key
		  FROM tasks x
		  CROSS JOIN conversations c
		  JOIN tasks t ON t.id = c.task_id
		 WHERE x.id = ?
		   AND `+liveConversationSQL("c")+`
		   AND `+siblingWorkRelationSQL+`
		 ORDER BY c.started_at, c.id
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SiblingWorkRecipient
	for rows.Next() {
		var r domain.SiblingWorkRecipient
		if err := rows.Scan(&r.ConversationID, &r.TaskID, &r.EntityID, &r.EventType, &r.DedupKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *taskStore) SiblingWorkSystem(ctx context.Context, orgID, conversationID string) ([]domain.SiblingTask, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	working := taskWorkingSQL("x.id")
	rows, err := s.q.QueryContext(ctx, `
		SELECT x.id, x.event_type, e.source_id,
		       x.status IN ('done', 'dismissed'),
		       x.claimed_by_agent_id IS NOT NULL, x.claimed_by_user_id IS NOT NULL,
		       COALESCE(tm.name, ''),
		       `+working+`
		  FROM conversations c
		  JOIN tasks t ON t.id = c.task_id
		  CROSS JOIN tasks x
		  JOIN entities e ON e.id = x.entity_id
		  LEFT JOIN teams tm ON tm.id = x.team_id
		 WHERE c.id = ?
		   AND `+siblingWorkRelationSQL+`
		   AND ((x.status NOT IN ('done', 'dismissed')
		         AND (x.claimed_by_agent_id IS NOT NULL OR x.claimed_by_user_id IS NOT NULL))
		        OR `+working+`)
		 ORDER BY x.created_at, x.id
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SiblingTask
	for rows.Next() {
		var (
			task              domain.SiblingTask
			byAgent, byPerson bool
		)
		if err := rows.Scan(&task.TaskID, &task.EventType, &task.EntitySourceID, &task.Closed,
			&byAgent, &byPerson, &task.TeamName, &task.Working); err != nil {
			return nil, err
		}
		task.Holder = db.SiblingTaskHolder(byAgent, byPerson)
		out = append(out, task)
	}
	return out, rows.Err()
}
