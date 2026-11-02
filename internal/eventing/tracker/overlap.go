package tracker

import (
	"path"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Overlaps reports whether a base branch's move can have changed the
// mergeability of a pull request on it, from the paths each changed. No I/O,
// so the tests and pollbench run the rule the poll runs.
//
// A pull request's paths are its changed files plus the previous path of each
// file it renamed or copied. A move's paths are the files it changed plus each
// one's previous path where the compare gave one. They overlap when a pull
// request path equals a move path, or one is a parent directory of the other.
//
// They also overlap when one side added a file anywhere under a directory the
// other side vacated, since git reads a renamed directory as a rename of what
// was added into it. A side's added files are its ADDED entries and the new
// path of each RENAMED or COPIED one. A file it deleted vacates the directory
// the file was in. A file it renamed into another directory vacates the
// directory the file left, and each parent of that directory whose name the
// rename kept: renaming D/sub/x to E/sub/x vacates D/sub and D, as git's
// directory-rename detection reads D/sub moving to E/sub and D to E. The
// repository root is never vacated, and a directory counts as vacated whether
// or not the side emptied it.
func Overlaps(prPaths, movePaths []domain.ChangedPath) bool {
	return indexMove(movePaths).overlaps(prPaths)
}

// moveIndex is a move's side of the overlap rule, built once per move so
// checking each pull request on the base costs its own paths' depth rather
// than the size of the move.
type moveIndex struct {
	// paths is the move's paths; dirs, every directory one of them lies
	// under; vacated, the directories it vacated; addedDirs, every directory
	// a file it added lies under.
	paths, dirs, vacated, addedDirs map[string]bool
}

func indexMove(movePaths []domain.ChangedPath) moveIndex {
	m := moveIndex{
		paths:     pathSet(movePaths, func(c domain.ChangedPath) bool { return c.Previous != "" }),
		dirs:      map[string]bool{},
		vacated:   map[string]bool{},
		addedDirs: map[string]bool{},
	}
	for p := range m.paths {
		for d := path.Dir(p); !isRoot(d) && !m.dirs[d]; d = path.Dir(d) {
			m.dirs[d] = true
		}
	}
	for _, c := range movePaths {
		vacate(c, m.vacated)
		if added(c) {
			for d := path.Dir(c.Path); !isRoot(d) && !m.addedDirs[d]; d = path.Dir(d) {
				m.addedDirs[d] = true
			}
		}
	}
	return m
}

// overlaps is the rule Overlaps states, for a pull request on this move's
// base.
func (m moveIndex) overlaps(prPaths []domain.ChangedPath) bool {
	for p := range pathSet(prPaths, func(c domain.ChangedPath) bool {
		return c.Change == domain.PathRenamed || c.Change == domain.PathCopied
	}) {
		if m.paths[p] || m.dirs[p] {
			return true
		}
		for d := path.Dir(p); !isRoot(d); d = path.Dir(d) {
			if m.paths[d] {
				return true
			}
		}
	}
	if len(m.vacated) > 0 {
		for _, c := range prPaths {
			if !added(c) {
				continue
			}
			for d := path.Dir(c.Path); !isRoot(d); d = path.Dir(d) {
				if m.vacated[d] {
					return true
				}
			}
		}
	}
	if len(m.addedDirs) > 0 {
		vacated := map[string]bool{}
		for _, c := range prPaths {
			vacate(c, vacated)
		}
		for d := range vacated {
			if m.addedDirs[d] {
				return true
			}
		}
	}
	return false
}

// added reports whether c puts a file at its path: an addition, or the new
// path of a rename or copy.
func added(c domain.ChangedPath) bool {
	switch c.Change {
	case domain.PathAdded, domain.PathRenamed, domain.PathCopied:
		return true
	}
	return false
}

// vacate records in into the directories c vacated, as Overlaps defines them.
func vacate(c domain.ChangedPath, into map[string]bool) {
	switch {
	case c.Change == domain.PathDeleted:
		if d := path.Dir(c.Path); !isRoot(d) {
			into[d] = true
		}
	case c.Change == domain.PathRenamed && c.Previous != "" && path.Dir(c.Previous) != path.Dir(c.Path):
		from, to := path.Dir(c.Previous), path.Dir(c.Path)
		for !isRoot(from) {
			into[from] = true
			if isRoot(to) || path.Base(from) != path.Base(to) {
				return
			}
			from, to = path.Dir(from), path.Dir(to)
		}
	}
}

func isRoot(dir string) bool {
	return dir == "." || dir == "/"
}

// pathSet is every entry's path, plus its previous path when withPrevious
// says the entry's previous path counts.
func pathSet(paths []domain.ChangedPath, withPrevious func(domain.ChangedPath) bool) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, c := range paths {
		out[c.Path] = true
		if c.Previous != "" && withPrevious(c) {
			out[c.Previous] = true
		}
	}
	return out
}
