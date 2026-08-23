package kbstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// objectKB is the multi-mode backend: one blob per KB entry in the shared
// object store, keyed by the package's convention. Every pod is equal — a team's
// knowledge belongs to the deployment, not to whichever executor last touched
// it — which is the whole reason it is not a per-host directory in multi.
type objectKB struct {
	blobs storage.Storage
}

// NewObject wraps a blob store as a KB. Exported for the multi-mode wiring and
// for tests that want the object semantics over a filesystem-backed
// storage.Storage (storage.NewFS) without a live S3.
func NewObject(blobs storage.Storage) KB { return &objectKB{blobs: blobs} }

// rootPrefix is the OBJECT-KEY namespace for one root of one team's KB — not a
// filesystem path; the local backend builds its own from paths.TeamKBDir, and
// that one carries no org segment. It always ends in a trailing slash so a List
// over it cannot match a sibling team whose id is a prefix of this one, nor a
// sibling root that starts with the same letters.
func rootPrefix(orgID, teamID string, root Root) string {
	return orgID + "/teams/" + teamID + "/kb/" + string(root) + "/"
}

// key builds the full object key for one entry after validating its path. This
// and rootPrefix are the only two places a KB key is assembled.
func key(orgID, teamID string, ref Ref) (string, error) {
	if _, err := ParseRoot(string(ref.Root)); err != nil {
		return "", err
	}
	if err := ValidatePath(ref.Path); err != nil {
		return "", err
	}
	return rootPrefix(orgID, teamID, ref.Root) + ref.Path, nil
}

func (s *objectKB) List(ctx context.Context, orgID, teamID string, roots []Root, pathPrefix string) ([]FileInfo, error) {
	if err := ValidatePrefix(pathPrefix); err != nil {
		return nil, err
	}
	out := []FileInfo{}
	for _, root := range roots {
		if _, err := ParseRoot(string(root)); err != nil {
			return nil, err
		}
		pfx := rootPrefix(orgID, teamID, root)
		objs, err := s.blobs.List(ctx, pfx)
		if err != nil {
			return nil, fmt.Errorf("list team kb: %w", err)
		}
		for _, o := range objs {
			path := strings.TrimPrefix(o.Key, pfx)
			if path == o.Key {
				continue // key didn't carry the prefix — defensive, cannot happen
			}
			// A key that would not survive a round-trip through the per-file
			// routes is not a listing entry: it could be named but never
			// fetched or deleted back. Writes go through key() so this only
			// ever fires on something written outside TF.
			if ValidatePath(path) != nil {
				continue
			}
			if !underPrefix(path, pathPrefix) {
				continue
			}
			out = append(out, FileInfo{Root: root, Path: path, Size: o.Size, ModTime: o.ModTime})
		}
	}
	sortEntries(out)
	return out, nil
}

func (s *objectKB) Stat(ctx context.Context, orgID, teamID string, ref Ref) (FileInfo, error) {
	k, err := key(orgID, teamID, ref)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := s.blobs.Stat(ctx, k)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Root: ref.Root, Path: ref.Path, Size: info.Size, ModTime: info.ModTime}, nil
}

func (s *objectKB) Open(ctx context.Context, orgID, teamID string, ref Ref) (io.ReadCloser, error) {
	k, err := key(orgID, teamID, ref)
	if err != nil {
		return nil, err
	}
	return s.blobs.Get(ctx, k)
}

func (s *objectKB) Put(ctx context.Context, orgID, teamID string, ref Ref, r io.Reader) error {
	k, err := key(orgID, teamID, ref)
	if err != nil {
		return err
	}
	return s.blobs.Put(ctx, k, r)
}

func (s *objectKB) Delete(ctx context.Context, orgID, teamID string, ref Ref) (int, error) {
	keys, err := s.subtreeKeys(ctx, orgID, teamID, ref)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, k := range keys {
		if err := s.blobs.Delete(ctx, k); err != nil {
			return removed, fmt.Errorf("delete team kb entry: %w", err)
		}
		removed++
	}
	return removed, nil
}

func (s *objectKB) Move(ctx context.Context, orgID, teamID string, from, to Ref) (int, error) {
	srcRoot, err := movePlanRoots(from, to)
	if err != nil {
		return 0, err
	}
	srcKeys, err := s.subtreeKeys(ctx, orgID, teamID, from)
	if err != nil {
		return 0, err
	}
	if len(srcKeys) == 0 {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, from)
	}

	srcBase := rootPrefix(orgID, teamID, srcRoot) + from.Path
	dstBase := rootPrefix(orgID, teamID, to.Root) + to.Path

	// Plan every destination before writing any of them, and refuse the whole
	// move on the first collision. Half a publish is worse than none: the team
	// would have to reconstruct which files made it across.
	type pair struct{ src, dst string }
	plan := make([]pair, 0, len(srcKeys))
	for _, src := range srcKeys {
		dst := dstBase + strings.TrimPrefix(src, srcBase)
		if src == dst {
			return 0, fmt.Errorf("%w: %s is already at %s", ErrExists, from, to)
		}
		exists, err := s.blobs.Exists(ctx, dst)
		if err != nil {
			return 0, fmt.Errorf("check move destination: %w", err)
		}
		if exists {
			return 0, fmt.Errorf("%w: %s", ErrExists, to)
		}
		plan = append(plan, pair{src: src, dst: dst})
	}

	// Copy every entry across first. Each destination was provably absent a
	// moment ago, so unwinding a partial copy deletes only what this call
	// wrote — never a file that was already there.
	written := make([]string, 0, len(plan))
	for _, p := range plan {
		if err := s.copyKey(ctx, p.src, p.dst); err != nil {
			for _, w := range written {
				if delErr := s.blobs.Delete(ctx, w); delErr != nil {
					return 0, errors.Join(err, fmt.Errorf("roll back copied entry %q: %w", w, delErr))
				}
			}
			return 0, err
		}
		written = append(written, p.dst)
	}

	// Sources go last, so an interruption anywhere above leaves the knowledge
	// readable at the old path, the new one, or both — never at neither.
	for _, p := range plan {
		if err := s.blobs.Delete(ctx, p.src); err != nil {
			return len(plan), fmt.Errorf("remove moved entry %q: %w", p.src, err)
		}
	}
	return len(plan), nil
}

// copyKey streams one blob to a new key. The object seam has no server-side
// copy, and a KB entry is a document rather than a workspace tarball, so the
// round trip through this process is the honest cost of a move.
func (s *objectKB) copyKey(ctx context.Context, src, dst string) error {
	rc, err := s.blobs.Get(ctx, src)
	if err != nil {
		return fmt.Errorf("read %q for move: %w", src, err)
	}
	defer func() { _ = rc.Close() }()
	if err := s.blobs.Put(ctx, dst, rc); err != nil {
		return fmt.Errorf("write %q for move: %w", dst, err)
	}
	return nil
}

// subtreeKeys returns the keys ref addresses: the entry at exactly that path
// plus everything beneath it, sorted. A path naming nothing yields an empty
// slice — the caller decides whether that is an answer (Delete) or a fault
// (Move).
func (s *objectKB) subtreeKeys(ctx context.Context, orgID, teamID string, ref Ref) ([]string, error) {
	base, err := key(orgID, teamID, ref)
	if err != nil {
		return nil, err
	}
	objs, err := s.blobs.List(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("list team kb subtree: %w", err)
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		// A prefix List matches on bytes, so "notes/plan" would also match
		// "notes/planning.md". Only the exact key and true children count.
		if o.Key == base || strings.HasPrefix(o.Key, base+"/") {
			out = append(out, o.Key)
		}
	}
	sort.Strings(out)
	return out, nil
}
