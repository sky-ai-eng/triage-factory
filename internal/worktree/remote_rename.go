package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RemoteRename is one remote URL of a checkout that names the checkout's
// repository by a name it no longer has, and the URL naming it by its current
// one.
type RemoteRename struct {
	// ConfigPath is the git config file the remote is set in: the checkout's
	// own, or for a linked worktree the shared bare's.
	ConfigPath string
	// Key is remote.<name>.url or remote.<name>.pushurl.
	Key      string
	From, To string
}

// Args is the git invocation that applies the rename to the config file at
// configPath (ConfigPath, or the same file as another process sees it). It
// replaces exactly From, so a key holding more than one value keeps the rest.
func (r RemoteRename) Args(configPath string) []string {
	return []string{"config", "--file", configPath, "--replace-all", r.Key, r.To, "^" + regexp.QuoteMeta(r.From) + "$"}
}

// StaleRemoteURLs returns the remote URLs of the checkout at path that are
// fromSlug's clone URL on host (domain.RewriteRepoCloneURL), each paired with
// the URL naming toSlug. fromSlug is the name the checkout was made under, the
// owner/repo its directory spells: TF set its origin, and for a pull request
// from the same repository its push remote, to that name's clone URL, and a
// rename of the repository since leaves them there. A remote naming any other
// repository, such as a fork's push remote, is not one of them.
//
// Read-only, and the read follows no include in the config (gitConfigFileValue),
// so it is safe on a tree the sandbox identity owns.
func StaleRemoteURLs(path, host, fromSlug, toSlug string) ([]RemoteRename, error) {
	_, commonDir, ok := worktreeGitPaths(path)
	if !ok {
		return nil, fmt.Errorf("%s is not a git checkout", path)
	}
	configPath := filepath.Join(commonDir, "config")
	raw, err := gitConfigFileValue(configPath, "--get-regexp", `^remote\..*\.(url|pushurl)$`)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil // no remote has a URL
	}
	if err != nil {
		return nil, err
	}
	var out []RemoteRename
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if to, ok := domain.RewriteRepoCloneURL(value, host, fromSlug, toSlug); ok && to != value {
			out = append(out, RemoteRename{ConfigPath: configPath, Key: key, From: value, To: to})
		}
	}
	return out, nil
}

// RewriteRemoteURLs applies renames from this process, which must be able to
// write their config files: a run tree it has not handed to a sandbox
// identity. git runs outside any repository, as the read does. The first
// failure stops the rest and is returned.
func RewriteRemoteURLs(ctx context.Context, renames []RemoteRename) error {
	for _, r := range renames {
		args := r.Args(r.ConfigPath)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = os.TempDir()
		cmd.Env = gitConfigReadEnv()
		if _, err := runGitOutput(ctx, cmd, args); err != nil {
			return err
		}
	}
	return nil
}
