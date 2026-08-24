package kbstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// localKB is the local-mode backend: plain files under
// <OrgRoot>/teams/<teamID>/kb/{private,shared}. No object store is involved —
// a single-machine install has none, and the on-disk copy IS the durable copy.
// A team's knowledge is therefore browsable and editable with an ordinary text
// editor, which is the posture a local operator expects of their own machine.
//
// **Every operation goes through an os.Root opened on the team's KB directory,
// and nothing here ever builds an absolute path.** That is the difference
// between a rule and a guarantee. Path validation rejects `..` in a *string*,
// which is no defense at all against a `..` that lives on the *disk*: a symlink
// anywhere in the tree — dropped by a sync tool, or checked out with a repo —
// turns a well-formed KB path into a read of, or a delete of, a file outside
// the knowledge base entirely. os.Root refuses that in the kernel (openat2's
// RESOLVE_BENEATH on Linux) rather than in a check that a rename can race.
//
// The confinement is the floor, not the whole rule. A symlink resolving *inside*
// the root would still be followed, and List skips every symlink it walks past
// — so a symlink would be invisible in a listing and readable by exact path,
// which is precisely the kind of half-guarantee this package exists to avoid.
// So the point reads reject a non-regular leaf outright, and the two doors agree.
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
		return path.Join(dir, "teams", teamID, "kb")
	}}
}

// openRoot opens teamID's KB directory as a confined root. Every path this
// backend touches is resolved relative to it and can never leave it.
//
// create says what a missing directory means. For a write it means "not yet",
// so the tree is made; for a read it means "nothing here", which is
// ErrNotFound rather than an error about the deployment — a team that has
// never had a document filed is the normal first state.
//
// The caller closes the returned root.
func (s *localKB) openRoot(orgID, teamID string, create bool) (*os.Root, error) {
	dir := s.dirFor(orgID, teamID)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create team kb dir: %w", err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open team kb: %w", err)
	}
	return root, nil
}

// rel is the root-relative name for one entry: "<root>/<path>", validated.
// Slash-separated in every case — os.Root takes forward slashes on every
// platform, so nothing here converts to an OS separator and there is no second
// spelling of an address.
func rel(ref Ref) (string, error) {
	if _, err := ParseRoot(string(ref.Root)); err != nil {
		return "", err
	}
	if err := ValidatePath(ref.Path); err != nil {
		return "", err
	}
	return string(ref.Root) + "/" + ref.Path, nil
}

func (s *localKB) List(ctx context.Context, orgID, teamID string, roots []Root, pathPrefix string) ([]FileInfo, error) {
	if err := ValidatePrefix(pathPrefix); err != nil {
		return nil, err
	}
	for _, root := range roots {
		if _, err := ParseRoot(string(root)); err != nil {
			return nil, err
		}
	}
	if len(roots) == 0 {
		return []FileInfo{}, nil
	}

	kbRoot, err := s.openRoot(orgID, teamID, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return []FileInfo{}, nil // a KB nobody has written to yet
		}
		return nil, err
	}
	defer func() { _ = kbRoot.Close() }()

	out := []FileInfo{}
	for _, root := range roots {
		entries, err := walkRoot(ctx, kbRoot, root)
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
// is an empty result, not an error.
//
// Anything that is not a plain regular file is skipped: a symlink is not
// followed (the object backend has no concept that could produce one), and a
// name that fails ValidatePath is left out because the per-file routes could
// not round-trip it.
func walkRoot(ctx context.Context, kbRoot *os.Root, root Root) ([]FileInfo, error) {
	base := string(root)
	var out []FileInfo
	err := fs.WalkDir(kbRoot.FS(), base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := strings.TrimPrefix(p, base+"/")
		if name == p || ValidatePath(name) != nil {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if os.IsNotExist(infoErr) {
				return nil // removed under us mid-walk
			}
			return infoErr
		}
		out = append(out, FileInfo{Root: root, Path: name, Size: info.Size(), ModTime: info.ModTime()})
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

// resolveEntry walks name one component at a time, requiring every component
// before the last to be a real directory, and returns the leaf's own Lstat.
//
// The walk is about the ERROR CLASS, not about safety — os.Root is what makes
// an escape impossible, and it does so whether or not this runs. But it refuses
// by returning a path-escapes-from-parent error that carries no exported
// sentinel to match on, and a handler rendering that as a 500 would be the
// wrong answer twice over: it is not a server fault, and telling a caller that
// something unusual is at a path they may not read is the disclosure a 404
// exists to prevent. Walking component-wise reaches the same verdict from
// facts this package owns — Lstat does not follow the FINAL component, so a
// symlink is reported as itself rather than traversed — and never has to
// classify Go's wording.
func resolveEntry(kbRoot *os.Root, name string) (os.FileInfo, error) {
	segs := strings.Split(name, "/")
	for i := 1; i < len(segs); i++ {
		info, err := kbRoot.Lstat(strings.Join(segs[:i], "/"))
		if err != nil {
			return nil, mapNotExist(err)
		}
		if !info.IsDir() {
			// A symlink, or a document, where a folder would have to be. Either
			// way nothing beneath it is an entry of this knowledge base.
			return nil, ErrNotFound
		}
	}
	info, err := kbRoot.Lstat(name)
	if err != nil {
		return nil, mapNotExist(err)
	}
	return info, nil
}

// statRegular resolves one entry and refuses anything that is not a plain file.
// A directory is a prefix rather than an entry, and a symlink is not a KB entry
// at all — List skips both, so serving either here would make the same KB
// answer two different questions depending on which door was used.
func statRegular(kbRoot *os.Root, name string) (os.FileInfo, error) {
	info, err := resolveEntry(kbRoot, name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	return info, nil
}

// checkWritableChain refuses a write whose path runs through something that is
// not a folder. Reads answer ErrNotFound for the same shape (nothing is there
// to read); a write is instead told its path is unusable, which is the fault
// class a caller can act on.
func checkWritableChain(kbRoot *os.Root, name string) error {
	segs := strings.Split(name, "/")
	for i := 1; i < len(segs); i++ {
		part := strings.Join(segs[:i], "/")
		info, err := kbRoot.Lstat(part)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // not created yet — MkdirAll will make it
			}
			return fmt.Errorf("resolve team kb path: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %q is not a folder", ErrInvalidName, part)
		}
	}
	return nil
}

func (s *localKB) Stat(_ context.Context, orgID, teamID string, ref Ref) (FileInfo, error) {
	name, err := rel(ref)
	if err != nil {
		return FileInfo{}, err
	}
	kbRoot, err := s.openRoot(orgID, teamID, false)
	if err != nil {
		return FileInfo{}, err
	}
	defer func() { _ = kbRoot.Close() }()

	info, err := statRegular(kbRoot, name)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Root: ref.Root, Path: ref.Path, Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (s *localKB) Open(_ context.Context, orgID, teamID string, ref Ref) (io.ReadCloser, error) {
	name, err := rel(ref)
	if err != nil {
		return nil, err
	}
	kbRoot, err := s.openRoot(orgID, teamID, false)
	if err != nil {
		return nil, err
	}
	// The root can close as soon as the file is open: the returned handle is
	// an independent descriptor, and the confinement has already done its job
	// by deciding which file that is.
	defer func() { _ = kbRoot.Close() }()

	if _, err := statRegular(kbRoot, name); err != nil {
		return nil, err
	}
	f, err := kbRoot.Open(name)
	if err != nil {
		return nil, mapNotExist(err)
	}
	return f, nil
}

// Put writes through a temp file and renames into place, so a concurrent read
// never observes a half-written document and a failed write cannot truncate the
// one that was already there. The rename also means a Put over a symlink
// REPLACES the link rather than writing through it.
func (s *localKB) Put(ctx context.Context, orgID, teamID string, ref Ref, r io.Reader) error {
	name, err := rel(ref)
	if err != nil {
		return err
	}
	kbRoot, err := s.openRoot(orgID, teamID, true)
	if err != nil {
		return err
	}
	defer func() { _ = kbRoot.Close() }()

	if err := checkWritableChain(kbRoot, name); err != nil {
		return err
	}
	dir := path.Dir(name)
	if err := kbRoot.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create team kb dir: %w", err)
	}
	tmp, tmpName, err := createTemp(kbRoot, dir)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = kbRoot.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r}); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flush temp team kb entry: %w", err)
	}
	if err := kbRoot.Rename(tmpName, name); err != nil {
		return fmt.Errorf("commit team kb entry: %w", err)
	}
	committed = true
	return nil
}

// createTemp makes an exclusively-created scratch file inside dir. os.CreateTemp
// cannot be used here because it takes an absolute path and would leave the
// confinement; the random suffix serves the same purpose — two concurrent
// writes to one document must not share a scratch file.
func createTemp(kbRoot *os.Root, dir string) (*os.File, string, error) {
	var buf [8]byte
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", fmt.Errorf("name temp team kb entry: %w", err)
		}
		name := path.Join(dir, ".put-"+hex.EncodeToString(buf[:]))
		f, err := kbRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("create temp team kb entry: %w", err)
		}
	}
	return nil, "", fmt.Errorf("create temp team kb entry: exhausted attempts in %q", dir)
}

func (s *localKB) Delete(ctx context.Context, orgID, teamID string, ref Ref) (int, error) {
	name, err := rel(ref)
	if err != nil {
		return 0, err
	}
	kbRoot, err := s.openRoot(orgID, teamID, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil // nothing filed, so the path is already gone
		}
		return 0, err
	}
	defer func() { _ = kbRoot.Close() }()

	files, err := subtreeFiles(ctx, kbRoot, name)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, f := range files {
		if err := kbRoot.Remove(f.name); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("delete team kb entry: %w", err)
		}
		removed++
	}
	pruneEmptyDirs(kbRoot, name)
	return removed, nil
}

func (s *localKB) Move(ctx context.Context, orgID, teamID string, from, to Ref) (int, error) {
	if _, err := movePlanRoots(from, to); err != nil {
		return 0, err
	}
	fromName, err := rel(from)
	if err != nil {
		return 0, err
	}
	toName, err := rel(to)
	if err != nil {
		return 0, err
	}
	kbRoot, err := s.openRoot(orgID, teamID, true)
	if err != nil {
		return 0, err
	}
	defer func() { _ = kbRoot.Close() }()

	files, err := subtreeFiles(ctx, kbRoot, fromName)
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
		dst := joinRelPath(toName, f.rel)
		if dst == f.name {
			return 0, fmt.Errorf("%w: %s is already at %s", ErrExists, from, to)
		}
		if _, err := kbRoot.Lstat(dst); err == nil {
			return 0, fmt.Errorf("%w: %s", ErrExists, Ref{Root: to.Root, Path: strings.TrimPrefix(dst, string(to.Root)+"/")})
		} else if !os.IsNotExist(err) {
			return 0, fmt.Errorf("check move destination: %w", err)
		}
		plan = append(plan, movePair{src: f.name, dst: dst})
	}

	moved := make([]movePair, 0, len(plan))
	for _, p := range plan {
		if err := kbRoot.MkdirAll(path.Dir(p.dst), 0o700); err != nil {
			return 0, unwindMove(kbRoot, moved, fmt.Errorf("create team kb dir: %w", err))
		}
		if err := kbRoot.Rename(p.src, p.dst); err != nil {
			return 0, unwindMove(kbRoot, moved, fmt.Errorf("move team kb entry: %w", err))
		}
		moved = append(moved, p)
	}
	pruneEmptyDirs(kbRoot, fromName)
	return len(plan), nil
}

// unwindMove puts back what a failed multi-file move already relocated, so the
// team's knowledge is where it was rather than split across two paths. Every
// entry is attempted: stopping at the first failure would strand the rest at
// their destinations with nothing naming them. A rename back that itself fails
// joins the original error rather than replacing it — the first one is what the
// operator has to act on.
func unwindMove(kbRoot *os.Root, moved []movePair, cause error) error {
	for i := len(moved) - 1; i >= 0; i-- {
		if err := kbRoot.Rename(moved[i].dst, moved[i].src); err != nil {
			cause = errors.Join(cause, fmt.Errorf("roll back moved entry %q: %w", moved[i].dst, err))
		}
	}
	return cause
}

// movePair is one file's before-and-after root-relative name in a planned move.
type movePair struct{ src, dst string }

// localFile pairs an entry's root-relative name with its name relative to the
// subtree the caller addressed — the two halves Move needs to rebuild the same
// shape under a new prefix.
type localFile struct {
	name string
	rel  string
}

// subtreeFiles returns the regular files name addresses: the file at exactly
// that name, or every file beneath it when it names a folder. A name holding
// nothing yields an empty slice, which Delete reads as an answer and Move as a
// fault. Symlinks are not entries and are skipped, at the leaf and inside.
func subtreeFiles(ctx context.Context, kbRoot *os.Root, name string) ([]localFile, error) {
	info, err := resolveEntry(kbRoot, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("read team kb path: %w", err)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, nil
		}
		return []localFile{{name: name, rel: ""}}, nil
	}

	var out []localFile
	err = fs.WalkDir(kbRoot.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		out = append(out, localFile{name: p, rel: strings.TrimPrefix(p, name+"/")})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk team kb subtree: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// pruneEmptyDirs removes the directories a delete or a move just emptied.
//
// It walks the affected subtree deepest-first and then climbs, rather than only
// climbing: emptying a folder that branches (docs/sub/ and docs/other/) leaves
// several empty leaves, and a climb from one of them stops at the first
// non-empty parent with the siblings still there. The object backend has no
// directories at all, so a hollow one left behind here would make the same KB
// list differently depending on the deployment mode.
//
// os.Remove refuses a non-empty directory, which is exactly the signal to keep
// it — a folder still holding a document stays, and so does everything above it.
func pruneEmptyDirs(kbRoot *os.Root, name string) {
	var dirs []string
	if info, err := kbRoot.Lstat(name); err == nil && info.IsDir() {
		_ = fs.WalkDir(kbRoot.FS(), name, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				dirs = append(dirs, p)
			}
			return nil
		})
	}
	// Then every ancestor up to (but never including) the visibility root,
	// which is the team's KB and not a folder in it.
	for dir := path.Dir(name); strings.Contains(dir, "/"); dir = path.Dir(dir) {
		dirs = append(dirs, dir)
	}
	// Deepest first, so a directory is only attempted once its children are.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		_ = kbRoot.Remove(dir)
	}
}

// joinRelPath rebases one subtree member under a new prefix. An empty rel is
// the single-file case, where the destination name IS the prefix.
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
