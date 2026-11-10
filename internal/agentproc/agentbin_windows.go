//go:build windows

package agentproc

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// agentBinaryName is how the agent invokes the TF CLI on Windows. The agent's
// commands run in Git Bash, which treats backslashes as escapes, so an
// absolute path such as C:\Users\me\triagefactory.exe reaches the program
// mangled; and a permission rule naming such a path would never match the
// command the model writes. A bare name resolves through PATH in every shell
// and is the same string in the prompt and the allowlist.
const agentBinaryName = "triagefactory"

// agentBinaryFile is the file the bare name resolves to, in the directory
// withAgentBinOnPath puts first on the agent's PATH.
const agentBinaryFile = agentBinaryName + ".exe"

func localAgentBinary(string) string { return agentBinaryName }

// AgentVisiblePath is the spelling of a host path in text shown to the agent:
// forward slashes, which Git Bash and Claude Code's file tools both accept and
// which survive being written into a shell command.
func AgentVisiblePath(p string) string { return filepath.ToSlash(p) }

// agentBinaryMu serialises refreshes of the agent-visible copy within this
// process. TF holds an instance lock, so no other TF process races for it.
var agentBinaryMu sync.Mutex

// agentBinDir is the TF-owned directory the agent finds `triagefactory` in:
// <StateRoot>\bin, the directory the pinned gh binary also lives in. It is
// never the directory of the running executable, which the user owns and may
// hold other programs in; putting it on the agent's PATH would hand the agent
// whatever else is there.
func agentBinDir() string { return filepath.Join(paths.StateRoot(), "bin") }

// withAgentBinOnPath returns env with the TF-owned bin directory leading PATH,
// after making sure <StateRoot>\bin\triagefactory.exe is this binary. It reads
// the PATH the child is about to get from env itself, so it composes with
// whatever the credential, gh-channel and operator-settings layers did to it.
//
// The gh channel's own PATH rebuild is unreachable on Windows (there is no
// pinned gh asset, so no run carries a channel); were it ever reached, its
// entry sits before this one in env and this builds on top of it.
func withAgentBinOnPath(env []string) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own binary path for the agent PATH: %w", err)
	}
	dir := agentBinDir()
	if err := ensureAgentBinary(self, dir); err != nil {
		return nil, err
	}
	return setPathLeading(env, dir), nil
}

// setPathLeading returns env with dir first in its PATH entry. Windows
// environment names are case-insensitive and the inherited one is usually
// spelled "Path"; the last entry in env wins, matching how exec resolves
// duplicates, and its spelling is kept.
func setPathLeading(env []string, dir string) []string {
	key, current := "PATH", ""
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok && strings.EqualFold(k, "PATH") {
			key, current = k, v
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+prependPath(dir, current))
}

// ensureAgentBinary makes <dir>\triagefactory.exe the running binary, cheaply
// when it already is. The file is a hardlink to self where the volume allows,
// so a TF upgrade that replaces the install is seen at once; where it does not
// (another volume, a filesystem without links) it is a copy, recognised as
// current by size and modification time.
//
// A stale file is renamed aside rather than overwritten: a run still using the
// previous version holds it open, and Windows allows a rename of an open image
// but not a delete or overwrite. The renamed file is deleted when possible and
// otherwise left for the next refresh to replace.
func ensureAgentBinary(self, dir string) error {
	agentBinaryMu.Lock()
	defer agentBinaryMu.Unlock()

	selfInfo, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("stat own binary %s: %w", self, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create agent bin directory %s: %w", dir, err)
	}
	dst := filepath.Join(dir, agentBinaryFile)
	aside := dst + ".old"

	dstInfo, err := os.Lstat(dst)
	switch {
	case err == nil:
		if isCurrentAgentBinary(dstInfo, selfInfo) {
			return nil
		}
		if err := os.Remove(aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
			agentprocLog.Debug("remove previous agent binary copy failed", "path", aside, "error", err)
		}
		if err := os.Rename(dst, aside); err != nil {
			return fmt.Errorf("move stale agent binary %s aside: %w", dst, err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("stat agent binary %s: %w", dst, err)
	}

	if err := os.Link(self, dst); err != nil {
		agentprocLog.Debug("hardlink agent binary failed; copying", "from", self, "to", dst, "error", err)
		if err := copyAgentBinary(self, dst, selfInfo); err != nil {
			return err
		}
	}
	if err := os.Remove(aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		agentprocLog.Debug("remove replaced agent binary failed; it is replaced on the next refresh", "path", aside, "error", err)
	}
	return nil
}

// isCurrentAgentBinary reports whether the file at the agent-visible path is
// already this binary: the same file (a hardlink), or a regular-file copy with
// the same size and modification time (copyAgentBinary stamps the copy with
// the original's).
func isCurrentAgentBinary(dst, self fs.FileInfo) bool {
	if os.SameFile(dst, self) {
		return true
	}
	return dst.Mode().IsRegular() && dst.Size() == self.Size() && dst.ModTime().Equal(self.ModTime())
}

// copyAgentBinary copies self to dst through a temporary file, so a reader
// never sees a half-written executable, and gives it self's modification time
// so a later call can tell the copy is current.
func copyAgentBinary(self, dst string, selfInfo fs.FileInfo) error {
	src, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open own binary %s: %w", self, err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), agentBinaryFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create agent binary copy in %s: %w", filepath.Dir(dst), err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("copy own binary to %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close agent binary copy %s: %w", tmpName, err)
	}
	if err := os.Chtimes(tmpName, selfInfo.ModTime(), selfInfo.ModTime()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("stamp agent binary copy %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install agent binary copy at %s: %w", dst, err)
	}
	return nil
}

// samePathEntry reports whether two cleaned PATH entries name the same
// directory. Windows file names are case-insensitive, so C:\Tools and c:\tools
// are one entry.
func samePathEntry(a, b string) bool { return strings.EqualFold(a, b) }
