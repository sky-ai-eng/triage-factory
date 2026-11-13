package agentproc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// argsFileFlag is the one flag wrapper.mjs accepts in place of its whole
// argument list: `node wrapper.mjs --args-file <path>`.
const argsFileFlag = "--args-file"

// argsFileDirName is the subdirectory of the state root holding args files.
// It is under the state root, never the run root: the run root is the agent's
// working tree and writable by the agent, and the file carries the run's
// system prompt and settings.
const argsFileDirName = "argv"

// staleArgsFileAge is how old an args file must be before a later spawn
// removes it. The wrapper deletes its file within seconds of starting, so a
// file this old belongs to a process that never started (or a TF that died
// between writing it and spawning).
const staleArgsFileAge = time.Hour

func argsFileDir() string {
	return filepath.Join(paths.StateRoot(), argsFileDirName)
}

// rewriteArgsFile moves everything after the script path in nodeArgs
// (nodeArgs[0], the wrapper) into an args file under dir and returns the
// replacement `<script> --args-file <path>`. The file is a JSON array of
// strings, so the wrapper reads back exactly the arguments it would have
// received, however long and whatever quoting a command line would have
// needed.
//
// The file is created with os.CreateTemp, which makes it readable by the
// creating user only; on Windows that mode bit means little and the file
// inherits the ACL of the user-profile directory it sits in, which is what
// keeps other local users out of it. The wrapper deletes it as soon as it has
// read it.
func rewriteArgsFile(dir string, nodeArgs []string) ([]string, error) {
	if len(nodeArgs) == 0 {
		return nil, errors.New("args file: no wrapper script in the argument list")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("args file: create %s: %w", dir, err)
	}
	pruneStaleArgsFiles(dir)
	payload, err := json.Marshal(nodeArgs[1:])
	if err != nil {
		return nil, fmt.Errorf("args file: encode arguments: %w", err)
	}
	f, err := os.CreateTemp(dir, argsFilePattern)
	if err != nil {
		return nil, fmt.Errorf("args file: create in %s: %w", dir, err)
	}
	name := f.Name()
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return nil, fmt.Errorf("args file: write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return nil, fmt.Errorf("args file: close %s: %w", name, err)
	}
	return []string{nodeArgs[0], argsFileFlag, name}, nil
}

// removeArgsFile deletes the args file cmd was built with, if it has one. The
// wrapper normally deleted it already; this covers a process that died before
// reading it. Only a file inside the args directory is touched, so a command
// that merely carries the flag for another reason cannot make TF delete an
// arbitrary path. Safe to call more than once and on a command with no file.
func removeArgsFile(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	for i, a := range cmd.Args {
		if a != argsFileFlag || i+1 >= len(cmd.Args) {
			continue
		}
		path := cmd.Args[i+1]
		if filepath.Dir(path) != argsFileDir() {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			agentprocLog.Warn("remove agent args file failed", "path", path, "error", err)
		}
	}
}

// argsFilePattern is both the os.CreateTemp pattern rewriteArgsFile names its
// files by and the glob that recognises them: CreateTemp replaces the "*" with
// a random string.
const argsFilePattern = "args-*.json"

// isArgsFileName reports whether name is one rewriteArgsFile could have made.
func isArgsFileName(name string) bool {
	ok, err := filepath.Match(argsFilePattern, name)
	return err == nil && ok
}

// pruneStaleArgsFiles removes args files old enough that no live wrapper can
// still be about to read them. Only files named like an args file are touched,
// so anything else that ends up in the directory is left alone. Best effort: a
// failure is logged and the spawn goes on.
func pruneStaleArgsFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		agentprocLog.Warn("list args file directory failed", "dir", dir, "error", err)
		return
	}
	cutoff := time.Now().Add(-staleArgsFileAge)
	for _, e := range entries {
		if !isArgsFileName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || e.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			agentprocLog.Warn("remove stale args file failed", "name", e.Name(), "error", err)
		}
	}
}
