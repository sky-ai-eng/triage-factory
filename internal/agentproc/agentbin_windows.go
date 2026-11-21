//go:build windows

package agentproc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
// after making sure <StateRoot>\bin\triagefactory.exe is a copy of this binary,
// and turns off the current directory in the executable search. It reads
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
	return setEnvVar(setPathLeading(env, dir), noDefaultCwdInExePathEnv, "1"), nil
}

// noDefaultCwdInExePathEnv makes the Windows executable search skip the current
// directory. Without it, a bare command name resolves in the working directory
// first, and the agent's working directory is a repository checkout, so a
// program the checkout carries (git.exe, triagefactory.exe) would run in place
// of the one on PATH.
const noDefaultCwdInExePathEnv = "NoDefaultCurrentDirectoryInExePath"

// setEnvVar returns env with exactly one entry for key, set to value. Windows
// environment names are case-insensitive, so every spelling already in env is
// dropped.
func setEnvVar(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); ok && strings.EqualFold(k, key) {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+value)
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

// ensureAgentBinary makes <dir>\triagefactory.exe a copy of the running binary,
// cheaply when it already is. It is always a copy and never a link: the agent
// runs as the operator and can write to <dir>, and a hardlink would alias the
// installed binary, so a write through the agent-visible path would modify it.
// A copy is recognised as current only when its size and SHA-256 equal the
// running binary's, so an agent that rewrote the file (and reset its
// modification time) is caught and the file replaced.
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
	selfSum, err := ownBinaryDigest(self, selfInfo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create agent bin directory %s: %w", dir, err)
	}
	dst := filepath.Join(dir, agentBinaryFile)
	aside := dst + ".old"

	dstInfo, err := os.Lstat(dst)
	switch {
	case err == nil:
		current, err := isCurrentAgentBinary(dst, dstInfo, selfInfo, selfSum)
		if err != nil {
			agentprocLog.Debug("agent binary could not be verified; replacing it", "path", dst, "error", err)
		}
		if current {
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

	if err := copyAgentBinary(self, dst); err != nil {
		return err
	}
	if err := os.Remove(aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		agentprocLog.Debug("remove replaced agent binary failed; it is replaced on the next refresh", "path", aside, "error", err)
	}
	return nil
}

// isCurrentAgentBinary reports whether the file at the agent-visible path is a
// regular file that is not the running binary itself (a leftover hardlink
// aliases the install and is replaced by a copy) and holds the same bytes: equal
// size, then equal SHA-256. The file is hashed on every call, since a cheaper
// stamp such as its modification time is one the agent can reset.
func isCurrentAgentBinary(dst string, dstInfo, selfInfo fs.FileInfo, selfSum string) (bool, error) {
	if !dstInfo.Mode().IsRegular() || os.SameFile(dstInfo, selfInfo) || dstInfo.Size() != selfInfo.Size() {
		return false, nil
	}
	sum, err := fileDigest(dst)
	if err != nil {
		return false, err
	}
	return sum == selfSum, nil
}

// ownDigest caches the digest of the running binary so it is hashed once per
// process rather than once per spawn. The key is the file's identity at the
// time it was hashed, so a TF upgrade that replaces the file at the same path
// is rehashed.
var ownDigest struct {
	path    string
	size    int64
	modTime time.Time
	sum     string
}

// ownBinaryDigest returns the hex SHA-256 of the running binary at self. The
// caller holds agentBinaryMu.
func ownBinaryDigest(self string, info fs.FileInfo) (string, error) {
	if ownDigest.sum != "" && ownDigest.path == self && ownDigest.size == info.Size() && ownDigest.modTime.Equal(info.ModTime()) {
		return ownDigest.sum, nil
	}
	sum, err := fileDigest(self)
	if err != nil {
		return "", fmt.Errorf("hash own binary %s: %w", self, err)
	}
	ownDigest.path, ownDigest.size, ownDigest.modTime, ownDigest.sum = self, info.Size(), info.ModTime(), sum
	return sum, nil
}

// fileDigest returns the hex SHA-256 of the file at path.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyAgentBinary copies self to dst through a temporary file, so a reader
// never sees a half-written executable.
func copyAgentBinary(self, dst string) error {
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
