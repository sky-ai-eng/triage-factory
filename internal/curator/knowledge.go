// Package curator owns the per-project long-lived Claude Code
// session that turns a project's name + knowledge files into chat-
// shaped help.
//
// One Curator instance per process. Each project gets its own
// goroutine + cancel handle on first SendMessage, so cross-project
// chats run in parallel. Within a single project, messages are
// drained serially to keep conversation history coherent.
package curator

import (
	"errors"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// ProjectsRoot returns the parent of every curator project working dir
// for orgID, routed through internal/paths so it lands under the
// mode/org-aware state root: ~/.triagefactory/projects in local mode and
// <state-root>/orgs/<orgID>/projects in multi mode. Single source of
// truth for the per-project working-dir layout — both KnowledgeDir below
// and the kbwatcher derive paths from here.
//
// It pre-flights paths.StateRootErr so a missing $HOME surfaces as the
// actionable error every caller already handles, rather than letting the
// underlying paths.ProjectsRoot panic.
func ProjectsRoot(orgID string) (string, error) {
	if _, err := paths.StateRootErr(); err != nil {
		return "", fmt.Errorf("resolve state root: %w", err)
	}
	return paths.ProjectsRoot(orgID), nil
}

// KnowledgeDir returns one project's working directory —
// <ProjectsRoot>/<projectID> — routed through internal/paths
// (paths.ProjectKBDir). The Curator's CC subprocess runs from here and
// the knowledge base lives under knowledge-base/, pinned-repo worktrees
// under repos/, and the materialized skills under .claude/. Path
// resolution lives in this package because it's the producer; the
// projects HTTP handlers and the classifier import it.
//
// orgID is the project's owning tenant. In multi mode it inserts the
// per-org subtree so two tenants' projects never collide on disk; in
// local mode (or when orgID is the local-default sentinel) the org
// segment is stripped and the layout is byte-for-byte what it was
// before.
func KnowledgeDir(orgID, projectID string) (string, error) {
	if projectID == "" {
		return "", errors.New("project id is required")
	}
	if _, err := paths.StateRootErr(); err != nil {
		return "", fmt.Errorf("resolve state root: %w", err)
	}
	return paths.ProjectKBDir(orgID, projectID), nil
}

// ensureKnowledgeDir creates the project's working directory if it
// doesn't exist. Called lazily on the first message of each project
// so empty projects don't create directories on disk before they're
// chatted with.
func ensureKnowledgeDir(orgID, projectID string) (string, error) {
	dir, err := KnowledgeDir(orgID, projectID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir knowledge dir: %w", err)
	}
	return dir, nil
}
