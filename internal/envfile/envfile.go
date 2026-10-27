// Package envfile reads and edits the KEY=value files docker compose reads as
// .env: one assignment per line, `#` comments, an optional leading `export `,
// and no line continuations or multi-line values, so each line stands alone.
//
// It is shared by the two commands that write a deployment's .env —
// `jwk-init --write-env` and `deploy init` — so both edit the file the same
// way: a variable's line is replaced where it stands rather than appended
// again, and the file is replaced atomically at mode 0600.
package envfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Var is one KEY=value assignment.
type Var struct {
	Key   string
	Value string
}

// Set returns content with each var assigned: a key's existing line is
// replaced in place and any later duplicate line for it is dropped, so a file
// that collected stale duplicates collapses to one line per key. Keys the
// content does not assign are appended in the order given. Every other line,
// comments included, is kept as it is.
func Set(content []byte, vars []Var) []byte {
	targets := make(map[string]string, len(vars))
	for _, v := range vars {
		targets[v.Key] = v.Value
	}

	written := make(map[string]bool, len(vars))
	var out []string
	if len(content) > 0 {
		body := strings.TrimSuffix(string(content), "\n")
		for _, line := range strings.Split(body, "\n") {
			key := LineKey(line)
			value, isTarget := targets[key]
			if !isTarget {
				out = append(out, line)
				continue
			}
			if written[key] {
				continue
			}
			written[key] = true
			// Keep a leading `export ` so an .env that is `source`d in a shell,
			// not only read by compose, keeps exporting the variable.
			prefix := ""
			if strings.HasPrefix(strings.TrimSpace(line), "export ") {
				prefix = "export "
			}
			out = append(out, prefix+key+"="+value)
		}
	}
	for _, v := range vars {
		if !written[v.Key] {
			written[v.Key] = true
			out = append(out, v.Key+"="+v.Value)
		}
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// Upsert applies Set to the file at path, treating a missing file as empty,
// and writes the result atomically at mode 0600.
func Upsert(path string, vars []Var) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read env file: %w", err)
	}
	return WriteAtomic(path, Set(existing, vars), 0o600)
}

// Lookup returns the value content assigns key, with one pair of matching
// surrounding quotes removed, and whether content assigns it at all. When
// several lines assign the key the last one wins, as it does for compose.
func Lookup(content []byte, key string) (string, bool) {
	var (
		value string
		found bool
	)
	for _, line := range strings.Split(string(content), "\n") {
		if LineKey(line) != key {
			continue
		}
		_, v, _ := strings.Cut(line, "=")
		value, found = unquote(strings.TrimSpace(v)), true
	}
	return value, found
}

// LineKey returns the variable name a line assigns, or "" for a blank line, a
// comment, or a line with no `=`. A leading `export ` and surrounding
// whitespace are tolerated, so hand-edited files read the same.
func LineKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}

// unquote strips one pair of matching surrounding quotes. The writers here
// never quote a value, but an operator may have quoted one by hand.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// WriteAtomic writes data to a temp file in path's directory and renames it
// over path, so a crash mid-write never leaves a half-written file. The file
// has mode perm from the moment it is created.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// A no-op once the rename has happened; an orphaned temp file is harmless,
	// so the error is ignored.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file into %s: %w", path, err)
	}
	return nil
}
