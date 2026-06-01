// Package curator owns the per-project long-lived Claude Code
// session that turns a project's name + knowledge files into chat-
// shaped help. SKY-216.
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
	"path/filepath"
)

// ProjectsRoot returns ~/.triagefactory/projects/. Single source of
// truth for the per-project working-dir layout — both KnowledgeDir
// below and the kbwatcher derive paths from here.
func ProjectsRoot() (string, error) {
	// TODO(SKY-402): multi-mode should resolve this under internal/paths'
	// state root (org-scoped) instead of the shared ~/.triagefactory home
	// that every tenant shares in single-pod multi. Local mode keeps this path.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".triagefactory", "projects"), nil
}

// KnowledgeDir returns ~/.triagefactory/projects/<id>/. The Curator's
// CC subprocess runs from here, and SKY-218 will populate
// knowledge-base/*.md inside it. Path resolution lives in this
// package because it's the producer; the projects HTTP delete handler
// imports it for cleanup.
func KnowledgeDir(projectID string) (string, error) {
	if projectID == "" {
		return "", errors.New("project id is required")
	}
	root, err := ProjectsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, projectID), nil
}

// ensureKnowledgeDir creates the project's working directory if it
// doesn't exist. Called lazily on the first message of each project
// so empty projects don't create directories on disk before they're
// chatted with.
func ensureKnowledgeDir(projectID string) (string, error) {
	dir, err := KnowledgeDir(projectID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir knowledge dir: %w", err)
	}
	return dir, nil
}
