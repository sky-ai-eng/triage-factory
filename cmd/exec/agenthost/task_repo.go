package agenthost

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// IsTaskOwnRepo reports whether (owner, repo) is the GitHub repo of the
// conversation's own task — the repo the run was created to work on.
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
// trusted from a field a caller stamped. Non-GitHub tasks and any resolution
// failure report false, leaving the ledger gate to answer — fail closed.
func IsTaskOwnRepo(ctx context.Context, stores db.Stores, info ConversationInfo, owner, repo string) bool {
	if stores.Conversations == nil || stores.Tasks == nil || info.ConversationID == "" {
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
	taskRepo := domain.GitHubTaskRepo(*task)
	return taskRepo != "" && strings.EqualFold(taskRepo, owner+"/"+repo)
}
