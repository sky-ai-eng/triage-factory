// Package kbstore owns the storage convention for a team knowledge base (KB)
// and is the ONLY place those keys and paths are built. Callers address a KB
// entry by (orgID, teamID, root, path) and never hand-assemble anything.
//
// One KB per team, two roots. In multi mode that is an OBJECT KEY, handed to
// the blob store as-is (the bucket is the store's, not ours):
//
//	<orgID>/teams/<teamID>/kb/private/<path…>
//	<orgID>/teams/<teamID>/kb/shared/<path…>
//
// In local mode it is a path under the state root, and it carries NO org
// segment — paths.OrgRoot collapses it at N=1, so the same team KB reads:
//
//	~/.triagefactory/teams/<teamID>/kb/private/<path…>
//	~/.triagefactory/teams/<teamID>/kb/shared/<path…>
//
// The two forms differ only in that prefix; everything from the root name down
// is identical, which is what lets one interface serve both.
//
// Visibility is the prefix, not an ACL: `private/` is readable only through
// its own team's gate, `shared/` is readable by any member of the org.
// Publishing is a Move between the two roots, which is why there is no
// visibility column anywhere — a second source of truth every listing would
// have to join.
//
// The layout under a root is hierarchical: a path is slash-separated segments,
// each validated on its own (ValidatePath), so folder trees are the way a team
// organizes its knowledge. The two roots are a closed vocabulary — a root
// outside them is invalid, not an empty listing.
//
// Two backends, chosen by runtime mode, behind one KB interface:
//
//   - multi: the shared internal/storage object seam (SeaweedFS S3), addressed
//     by the key above, so every pod reads the same bytes and no executor owns
//     a team's knowledge.
//   - local: plain files at paths.TeamKBDir, with no object store in the
//     picture at all — the single-machine posture, where the on-disk copy IS
//     the durable copy and an operator can edit it with an ordinary editor.
package kbstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// ErrInvalidName is returned when a KB root or path fails validation: an empty
// or dot-prefixed segment, a traversal segment, a separator inside a segment,
// or a root outside the two-value vocabulary. A single handler call surfaces it
// as a client error naming the offending field.
var ErrInvalidName = errors.New("kbstore: invalid knowledge-base path")

// ErrNotFound re-exports storage.ErrNotFound so callers branch on the kbstore
// surface without also importing internal/storage just for the sentinel. The
// local backend maps its own os.IsNotExist onto the same value.
var ErrNotFound = storage.ErrNotFound

// ErrExists is returned by Move when something already occupies a destination
// path. A move that overwrote would destroy knowledge the mover never named,
// so the conflict is reported and nothing is touched.
var ErrExists = errors.New("kbstore: destination already exists")

// Root is one of a team KB's two visibility roots. It is a closed vocabulary:
// the type exists so a root can only ever be one of the two constants below or
// a value ParseRoot refused.
type Root string

const (
	// RootPrivate holds knowledge readable only through its own team's gate.
	RootPrivate Root = "private"
	// RootShared holds knowledge any member of the org may read, and the only
	// root of another team that a delegated run is staged with.
	RootShared Root = "shared"
)

// Roots is the closed vocabulary, in the order every surface presents it:
// private first (where a team writes) then shared (what it publishes).
func Roots() []Root { return []Root{RootPrivate, RootShared} }

// ParseRoot converts a wire value to a Root, rejecting anything outside the
// vocabulary. Case-sensitive on purpose: a root is a key segment, and
// case-folding would make two spellings address one prefix in the object store
// and two directories on a case-sensitive filesystem.
func ParseRoot(s string) (Root, error) {
	switch Root(s) {
	case RootPrivate:
		return RootPrivate, nil
	case RootShared:
		return RootShared, nil
	default:
		return "", fmt.Errorf("%w: root %q is not one of %q, %q", ErrInvalidName, s, RootPrivate, RootShared)
	}
}

// Ref addresses one entry in a team KB: which root, and the slash-separated
// path under it. It is the argument shape for Move, where two of them appear
// and a pair of bare strings would be trivially transposable.
type Ref struct {
	Root Root
	Path string
}

// String renders a Ref the way the wire and the run-tree layout both spell it:
// "<root>/<path>". Used in error messages so a rejected move names the side
// that was wrong.
func (r Ref) String() string { return string(r.Root) + "/" + r.Path }

// FileInfo is one KB entry's listing record: its root, its slash-separated
// path under that root, its byte size, and its last-modified time. Folders are
// not entries — they exist only as the shared prefixes of these paths, in both
// backends, which is what keeps "publish a folder" a pure path operation.
type FileInfo struct {
	Root    Root      `json:"root"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Ref is the addressing form of a listing entry.
func (f FileInfo) Ref() Ref { return Ref{Root: f.Root, Path: f.Path} }

// Limits on a KB path. They are enforced at the one door every write goes
// through, so a name that can be created can always be listed, fetched and
// deleted back — the round-trip guarantee that a cap applied at only one of
// those doors would break.
const (
	// MaxSegmentLen caps one path segment (a folder or file name), in bytes.
	MaxSegmentLen = 255
	// MaxPathLen caps the whole slash-separated path under a root, in bytes.
	MaxPathLen = 1024
	// MaxDepth caps how many segments a path may have. A KB is a filing
	// system a person navigates, not a tree a script generates.
	MaxDepth = 16
)

// ValidatePath enforces the hierarchical layout contract on a path under a
// root. It is exported so a handler can pre-check a name and report the fault
// with the field attribution its own body shape uses.
//
// A path is one or more '/'-separated segments, and each segment must:
//
//   - be non-empty — no leading, trailing or doubled separator, so one entry
//     has exactly one spelling;
//   - not be "." or ".." — traversal never reaches a sibling team's prefix;
//   - contain no '/' (already split on) and no '\' — on Windows the OS treats
//     '\' as a separator, so a name carrying one would round-trip differently
//     depending on which machine served it;
//   - not start with '.' — dot files are agent scratch state, never
//     user-visible knowledge;
//   - carry no control characters — they are unrenderable in a listing and
//     unquotable in the Content-Disposition of a raw read.
//
// The size caps above apply to the whole path and to each segment.
func ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if len(path) > MaxPathLen {
		return fmt.Errorf("%w: path is longer than %d bytes", ErrInvalidName, MaxPathLen)
	}
	segs := strings.Split(path, "/")
	if len(segs) > MaxDepth {
		return fmt.Errorf("%w: path is deeper than %d segments", ErrInvalidName, MaxDepth)
	}
	for _, seg := range segs {
		if err := validateSegment(seg, path); err != nil {
			return err
		}
	}
	return nil
}

func validateSegment(seg, path string) error {
	switch {
	case seg == "":
		return fmt.Errorf("%w: %q has an empty path segment", ErrInvalidName, path)
	case seg == "." || seg == "..":
		return fmt.Errorf("%w: %q contains a %q segment", ErrInvalidName, path, seg)
	case len(seg) > MaxSegmentLen:
		return fmt.Errorf("%w: segment %q is longer than %d bytes", ErrInvalidName, seg, MaxSegmentLen)
	case strings.HasPrefix(seg, "."):
		return fmt.Errorf("%w: segment %q starts with a dot", ErrInvalidName, seg)
	case strings.ContainsRune(seg, '\\'):
		return fmt.Errorf("%w: segment %q contains a path separator", ErrInvalidName, seg)
	}
	for _, r := range seg {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: segment %q contains a control character", ErrInvalidName, seg)
		}
	}
	return nil
}

// ValidatePrefix enforces the path rules on a listing filter, where the empty
// string is the legitimate "everything under this root" and so is accepted. A
// non-empty prefix is a real path — the filter matches an entry at exactly
// that path or anything beneath it, never a sibling whose name merely starts
// with the same letters.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidatePath(prefix)
}

// KB is the team knowledge-base seam every consumer holds: the HTTP surface,
// and the delegation spawner that stages a run's knowledge. One process-wide
// value serves every tenant — (orgID, teamID) are arguments, never state.
//
// Reads are unscoped by design: the two roots are the visibility model, so the
// *caller* decides which roots it may read (a team member passes both, an
// org member reading another team passes shared alone) and this layer never
// second-guesses that with an identity it does not have.
type KB interface {
	// List returns every entry under the given roots whose path is at or
	// beneath pathPrefix, sorted by (root, path) so two calls agree. An empty
	// pathPrefix lists the whole root. A root with nothing in it contributes
	// nothing — a KB that has never been written is an empty slice, not an
	// error. An empty roots slice is an empty result, not "everything":
	// widening a read whose scope resolved to nothing is how a visibility gate
	// fails open.
	List(ctx context.Context, orgID, teamID string, roots []Root, pathPrefix string) ([]FileInfo, error)

	// Stat returns one entry's size and mod time without opening it, or
	// ErrNotFound. It is a single point read — never a prefix scan — because
	// the raw-read handler calls it per request to size its response.
	Stat(ctx context.Context, orgID, teamID string, ref Ref) (FileInfo, error)

	// Open streams one entry's bytes. A missing entry returns ErrNotFound.
	// The caller closes the reader.
	Open(ctx context.Context, orgID, teamID string, ref Ref) (io.ReadCloser, error)

	// Put writes (creating or overwriting) one entry, streaming r to the
	// backing store.
	Put(ctx context.Context, orgID, teamID string, ref Ref, r io.Reader) error

	// Delete removes the entry at ref AND everything beneath it, returning how
	// many entries went away. Deleting a path that holds nothing is (0, nil):
	// the caller asked for that path to be gone and it is. Recursion is what
	// makes "remove this folder" one call rather than a client-side walk that
	// can be interrupted halfway.
	Delete(ctx context.Context, orgID, teamID string, ref Ref) (int, error)

	// Move relocates the entry at from — and everything beneath it — to to,
	// preserving the relative shape of the subtree. This is the single verb
	// behind publish (private→shared), unpublish (shared→private), rename and
	// reorganize: all four are the same transition, and splitting them into
	// four routes would be four spellings of one operation.
	//
	// It refuses rather than overwrites: if any destination path is already
	// occupied, nothing moves and the error is ErrExists. A source that holds
	// nothing is ErrNotFound. Copies land before any source is removed, so an
	// interrupted move leaves the knowledge readable at one path or both,
	// never at neither.
	Move(ctx context.Context, orgID, teamID string, from, to Ref) (int, error)
}

// New constructs the process-wide KB for the current runtime mode: the object
// backend in multi (built over the caller's already-constructed blob store),
// plain files under the state root in local. Call once at startup and share the
// value — it holds no per-team state.
//
// blobs is used only in multi mode; local mode ignores it, because a local
// deployment has no object store to route a team's knowledge through.
func New(blobs storage.Storage) KB {
	if runmode.Current() == runmode.ModeMulti {
		return NewObject(blobs)
	}
	return NewLocal()
}
