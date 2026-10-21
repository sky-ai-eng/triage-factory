package agenthost

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// IsTaskOwnRepository reports whether the repository whose registry row is
// repositoryID is the GitHub repo of the conversation's own task — the repo
// the run was created to work on.
//
// Two least-privilege gates ask it, and they ask for the same reason: a
// conversation's authority over a repo otherwise comes from a
// conversation_worktrees row, and the row for the task's own repo is written by
// whichever conversation's setup materialized the tree. Any other conversation
// working in that tree — a later blueprint step, a later run on the task —
// holds no row for the one repo it provably needs, and a gate reading only the
// ledger refuses it. Both gates resolve that case through here, so they cannot
// come to different answers about which repo a run is on.
//
// It is a supplement to the ledger, never a replacement: it authorizes exactly
// one repo, the one named by the run's own task. ConversationInfo carries no
// task id, so the conversation is resolved to its task here rather than
// trusted from a field a caller stamped. The task's repository is the registry
// row its entity references (entities.repository_id), and repositoryID is the
// row the asking gate resolved the name it was handed to, on the org's current
// GitHub host — once, at the gate. The two are compared by id, so a repository
// renamed since the task was created is still its own, a task whose pull
// request was polled from another host is about another repository whatever
// its name, and nothing is read out of the entity's key. Non-GitHub tasks,
// whose entities reference no repository, and any resolution failure report
// false, leaving the ledger gate to answer — fail closed.
func IsTaskOwnRepository(ctx context.Context, stores db.Stores, info ConversationInfo, repositoryID string) bool {
	if stores.Conversations == nil || stores.Tasks == nil || stores.Entities == nil || info.ConversationID == "" || repositoryID == "" {
		return false
	}
	conv, err := stores.Conversations.GetSystem(ctx, info.OrgID, info.ConversationID)
	if err != nil || conv == nil || conv.TaskID == "" {
		return false
	}
	task, err := stores.Tasks.GetSystem(ctx, info.OrgID, conv.TaskID)
	if err != nil || task == nil {
		return false
	}
	entity, err := stores.Entities.GetSystem(ctx, info.OrgID, task.EntityID)
	if err != nil || entity == nil || entity.RepositoryID == "" {
		return false
	}
	return entity.RepositoryID == repositoryID
}
