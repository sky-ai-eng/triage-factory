package kbstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// localKB is the local-mode backend: plain files under
// <OrgRoot>/teams/<teamID>/kb/{private,shared}. No object store is involved —
// a single-machine install has none, and the on-disk copy IS the durable copy.
// A team's knowledge is therefore browsable and editable with an ordinary text
// editor, which is the posture a local operator expects of their own machine.
type localKB struct {
	// dirFor resolves a team's KB root on disk. A field rather than a direct
	// call so a test can root a whole KB in a temp dir without reaching for
	// process-wide state.
	dirFor func(orgID, teamID string) string
}

// NewLocal builds the plain-files backend over the paths package's team-KB
// resolver.
func NewLocal() KB { return &localKB{dirFor: paths.TeamKBDir} }

// NewLocalAt builds the plain-files backend rooted at dir, laying out
// teams/<teamID>/kb beneath it exactly as the state root does. For tests and
// for a caller that owns its own tree.
func NewLocalAt(dir string) KB {
	return &localKB{dirFor: func(_, teamID string) string {
		return filepath.Join(dir, "teams", teamID, "kb")
	}}
}

// file resolves the on-disk path for one entry after validating it. Together
// with rootDir it is the local backend's half of the "one place builds a KB
// address" rule; ValidatePath is what makes filepath.Join safe here, since a
// traversal segment is refused before it can climb out of the team's tree.
func (s *localKB) file(orgID, teamID string, ref Ref) (string, error) {
	if _, err := ParseRoot(string(ref.Root)); err != nil {
		return "", err
	}
	if err := ValidatePath(ref.Path); err != nil {
		return "", err
	}
	return filepath.Join(s.rootDir(orgID, teamID, ref.Root), filepath.FromSlash(ref.Path)), nil
}

func (s *localKB) rootDir(orgID, teamID string, root Root) string {
	return filepath.Join(s.dirFor(orgID, teamID), string(root))
}

func (s *localKB) List(ctx context.Context, orgID, teamID string, roots []Root, pathPrefix string) ([]FileInfo, error) {
	if err := ValidatePrefix(pathPrefix); err != nil {
		return nil, err
	}
	out := []FileInfo{}
	for _, root := range roots {
		if _, err := ParseRoot(string(root)); err != nil {
			return nil, err
		}
		entries, err := s.walkRoot(ctx, orgID, teamID, root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if underPrefix(e.Path, pathPrefix) {
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	return out, nil
}

// walkRoot enumerates one root's regular files. A root that was never written
// is an empty result, not an error — a team with no knowledge is the normal
// first state, not a misconfiguration.
//
// Anything that is not a plain regular file is skipped: a symlink is not
// followed (it could point outside the team's tree, and the object backend has
// no concept that could produce one), and a name that fails ValidatePath is
// left out for the same round-trip reason the object backend gives.
func (s *localKB) walkRoot(ctx context.Context, orgID, teamID string, root Root) ([]FileInfo, error) {
	base := s.rootDir(orgID, teamID, root)
	var out []FileInfo
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		path := filepath.ToSlash(rel)
		if ValidatePath(path) != nil {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if os.IsNotExist(infoErr) {
				return nil // removed under us mid-walk
			}
			return infoErr
		}
		out = append(out, FileInfo{Root: root, Path: path, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list team kb: %w", err)
	}
	return out, nil
}

func (s *localKB) Stat(_ context.Context, orgID, teamID string, ref Ref) (FileInfo, error) {
	p, err := s.file(orgID, teamID, ref)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return FileInfo{}, mapNotExist(err)
	}
	if info.IsDir() {
		// A folder is a prefix, never an entry: the object backend cannot
		// produce one, so the two backends must agree that this path holds
		// nothing fetchable.
		return FileInfo{}, ErrNotFound
	}
	return FileInfo{Root: ref.Root, Path: ref.Path, Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (s *localKB) Open(_ context.Context, orgID, teamID string, ref Ref) (io.ReadCloser, error) {
	p, err := s.file(orgID, teamID, ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, mapNotExist(err)
	}
	// A directory opens fine on a filesystem and reads as an error only once
	// the caller starts streaming. An object store has no folder key at all,
	// so the two backends agree here rather than at the first Read.
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		if err != nil {
			return nil, mapNotExist(err)
		}
		return nil, ErrNotFound
	}
	return f, nil
}

// Put writes through a temp file and renames into place, so a concurrent read
// never observes a half-written document and a failed write cannot truncate
// the one that was already there.
func (s *localKB) Put(ctx context.Context, orgID, teamID string, ref Ref, r io.Reader) error {
	p, err := s.file(orgID, teamID, ref)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create team kb dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return fmt.Errorf("create temp team kb entry: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds
	if _, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r}); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flush temp team kb entry: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("commit team kb entry: %w", err)
	}
	return nil
}

func (s *localKB) Delete(ctx context.Context, orgID, teamID string, ref Ref) (int, error) {
	files, err := s.subtreeFiles(ctx, orgID, teamID, ref)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, f := range files {
		if err := os.Remove(f.abs); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("delete team kb entry: %w", err)
		}
		removed++
	}
	s.pruneEmptyDirs(orgID, teamID, ref)
	return removed, nil
}

func (s *localKB) Move(ctx context.Context, orgID, teamID string, from, to Ref) (int, error) {
	if _, err := movePlanRoots(from, to); err != nil {
		return 0, err
	}
	files, err := s.subtreeFiles(ctx, orgID, teamID, from)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, from)
	}

	// Plan the whole move and refuse on the first collision, exactly as the
	// object backend does: a publish that half-landed leaves the team to work
	// out which files made it across.
	plan := make([]movePair, 0, len(files))
	for _, f := range files {
		dstRef := Ref{Root: to.Root, Path: joinRelPath(to.Path, f.rel)}
		dst, err := s.file(orgID, teamID, dstRef)
		if err != nil {
			return 0, err
		}
		if dst == f.abs {
			return 0, fmt.Errorf("%w: %s is already at %s", ErrExists, from, to)
		}
		if _, err := os.Lstat(dst); err == nil {
			return 0, fmt.Errorf("%w: %s", ErrExists, dstRef)
		} else if !os.IsNotExist(err) {
			return 0, fmt.Errorf("check move destination: %w", err)
		}
		plan = append(plan, movePair{src: f.abs, dst: dst})
	}

	moved := make([]movePair, 0, len(plan))
	for _, p := range plan {
		if err := os.MkdirAll(filepath.Dir(p.dst), 0o700); err != nil {
			return 0, unwindMove(moved, fmt.Errorf("create team kb dir: %w", err))
		}
		if err := os.Rename(p.src, p.dst); err != nil {
			return 0, unwindMove(moved, fmt.Errorf("move team kb entry: %w", err))
		}
		moved = append(moved, p)
	}
	s.pruneEmptyDirs(orgID, teamID, from)
	return len(plan), nil
}

// unwindMove puts back what a failed multi-file move already relocated, so the
// team's knowledge is where it was rather than split across two paths. A
// rename back can itself fail; that error joins the original rather than
// replacing it, because the first one is what the operator has to act on.
func unwindMove(moved []movePair, cause error) error {
	for i := len(moved) - 1; i >= 0; i-- {
		if err := os.Rename(moved[i].dst, moved[i].src); err != nil {
			cause = errors.Join(cause, fmt.Errorf("roll back moved entry %q: %w", moved[i].dst, err))
		}
	}
	return cause
}

// movePair is one file's before-and-after absolute path in a planned move.
type movePair struct{ src, dst string }

// localFile pairs an entry's absolute path with its path relative to the
// subtree root the caller named — the two halves Move needs to rebuild the
// same shape under a new prefix.
type localFile struct {
	abs string
	rel string
}

// subtreeFiles returns the regular files ref addresses: the file at exactly
// that path, or every file beneath it when the path names a folder. A path
// holding nothing yields an empty slice, which Delete reads as an answer and
// Move as a fault.
func (s *localKB) subtreeFiles(ctx context.Context, orgID, teamID string, ref Ref) ([]localFile, error) {
	base, err := s.file(orgID, teamID, ref)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read team kb path: %w", err)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, nil
		}
		return []localFile{{abs: base, rel: ""}}, nil
	}

	var out []localFile
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		out = append(out, localFile{abs: p, rel: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk team kb subtree: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].abs < out[j].abs })
	return out, nil
}

// pruneEmptyDirs removes the directories a delete or a move just emptied,
// walking up from the affected path to (but never through) the root. The
// object backend has no directories at all, so leaving hollow ones behind
// would make the same KB list differently depending on the deployment mode —
// a folder that exists on one and not the other.
func (s *localKB) pruneEmptyDirs(orgID, teamID string, ref Ref) {
	root := s.rootDir(orgID, teamID, ref.Root)
	dir := filepath.Join(root, filepath.FromSlash(ref.Path))
	// The named path may itself be a folder this call just emptied. When it is
	// not — a single file, already gone — the walk up starts at its parent.
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return // non-empty, or gone — either way there is nothing above to prune
		}
		dir = filepath.Dir(dir)
	}
}

// joinRelPath rebases one subtree member under a new prefix. An empty rel is
// the single-file case, where the destination path IS the prefix.
func joinRelPath(prefix, rel string) string {
	if rel == "" {
		return prefix
	}
	return prefix + "/" + rel
}

// mapNotExist folds the filesystem's missing-file signal onto the package
// sentinel, so a caller branches on ErrNotFound without caring which backend
// answered.
func mapNotExist(err error) error {
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}

// ctxReader makes a long copy abortable: a cancelled request stops streaming
// rather than running to EOF on a body nobody is waiting for.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
