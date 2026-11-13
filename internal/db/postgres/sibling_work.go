package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// siblingWorkRelationSQL is the relation between a task and a conversation
// whose work overlaps it (TaskStore.SiblingWorkRecipientsSystem), over three
// aliases the statement binds: x is the task, c the conversation, and t the
// conversation's own task. Both reads of the relation interpolate it, so a
// conversation is told of a task exactly when the task is in its listing.
//
// A team sees a task it owns or one whose visibility set names it. A task's
// own team is not always in that set, so both are asked.
var siblingWorkRelationSQL = `(x.id <> t.id
	   AND (x.team_id = c.team_id
	        OR EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = x.id AND tt.team_id = c.team_id))
	   AND (x.entity_id = t.entity_id
	        OR EXISTS (
	            SELECT 1 FROM conversation_memory_entities cme
	             WHERE cme.org_id = c.org_id AND cme.conversation_id = c.id AND cme.entity_id = x.entity_id)
	        OR EXISTS (
	            SELECT 1 FROM conversations lx
	              JOIN conversation_memory_entities cmx ON cmx.org_id = lx.org_id AND cmx.conversation_id = lx.id
	             WHERE lx.org_id = x.org_id AND lx.task_id = x.id
	               AND ` + liveConversationSQL("lx") + `
	               AND cmx.entity_id = t.entity_id)))`

func (s *taskStore) AssignmentSystem(ctx context.Context, orgID, taskID string) (*domain.TaskAssignment, error) {
	if !isValidUUID(taskID) {
		return nil, nil
	}
	var a domain.TaskAssignment
	err := s.admin.QueryRowContext(ctx, `
		SELECT x.id::text, x.entity_id::text, x.event_type, x.dedup_key, e.source_id,
		       COALESCE(x.claimed_by_agent_id::text, ''), COALESCE(x.claimed_by_user_id::text, ''),
		       COALESCE(u.display_name, ''), COALESCE(tm.name, '')
		  FROM tasks x
		  JOIN entities e ON e.org_id = x.org_id AND e.id = x.entity_id
		  LEFT JOIN users u ON u.id = x.claimed_by_user_id
		  LEFT JOIN teams tm ON tm.org_id = x.org_id AND tm.id = x.team_id
		 WHERE x.org_id = $1 AND x.id = $2
		   AND x.status NOT IN ('done', 'dismissed')
		   AND (x.claimed_by_agent_id IS NOT NULL OR x.claimed_by_user_id IS NOT NULL)
	`, orgID, taskID).Scan(&a.TaskID, &a.EntityID, &a.EventType, &a.DedupKey, &a.EntitySourceID,
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
	if !isValidUUID(taskID) {
		return nil, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT c.id::text, t.id::text, t.entity_id::text, t.event_type, t.dedup_key
		  FROM tasks x
		  JOIN conversations c ON c.org_id = x.org_id
		  JOIN tasks t ON t.org_id = c.org_id AND t.id = c.task_id
		 WHERE x.org_id = $1 AND x.id = $2
		   AND `+liveConversationSQL("c")+`
		   AND `+siblingWorkRelationSQL+`
		 ORDER BY c.started_at, c.id
	`, orgID, taskID)
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
	if !isValidUUID(conversationID) {
		return nil, nil
	}
	working := taskWorkingSQL("x.org_id", "x.id")
	rows, err := s.admin.QueryContext(ctx, `
		SELECT x.id::text, x.event_type, e.source_id,
		       x.status IN ('done', 'dismissed'),
		       x.claimed_by_agent_id IS NOT NULL, x.claimed_by_user_id IS NOT NULL,
		       COALESCE(tm.name, ''),
		       `+working+`
		  FROM conversations c
		  JOIN tasks t ON t.org_id = c.org_id AND t.id = c.task_id
		  JOIN tasks x ON x.org_id = c.org_id
		  JOIN entities e ON e.org_id = x.org_id AND e.id = x.entity_id
		  LEFT JOIN teams tm ON tm.org_id = x.org_id AND tm.id = x.team_id
		 WHERE c.org_id = $1 AND c.id = $2
		   AND `+siblingWorkRelationSQL+`
		   AND ((x.status NOT IN ('done', 'dismissed')
		         AND (x.claimed_by_agent_id IS NOT NULL OR x.claimed_by_user_id IS NOT NULL))
		        OR `+working+`)
		 ORDER BY x.created_at, x.id
	`, orgID, conversationID)
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
