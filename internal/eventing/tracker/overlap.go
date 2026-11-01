package tracker

import (
	"path"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// overlaps reports whether a base branch's move can have changed the
// mergeability of a pull request on it, from the paths each changed. No I/O,
// so the tests and pollbench run the rule the poll runs.
//
// A pull request's paths are its changed files plus the previous path of each
// file it renamed or copied. A move's paths are the files it changed plus each
// one's previous path where the compare gave one. They overlap when a pull
// request path equals a move path, or one is a parent directory of the other.
//
// They also overlap when the move vacated a directory the pull request added
// a file under: a file the move deleted, or renamed into a different
// directory, vacates the directory D it was in (never the repository root),
// and a file the pull request added anywhere under D overlaps. The pull
// request's added files are its ADDED entries and the new path of each
// RENAMED or COPIED one. D counts as vacated whether or not the move emptied
// it.
func overlaps(prPaths, movePaths []domain.ChangedPath) bool {
	move := pathSet(movePaths, func(c domain.ChangedPath) bool { return c.Previous != "" })
	// Every directory a move path lies under, so a pull request path that is
	// a parent of a move path is one lookup rather than a scan of the move.
	moveDirs := map[string]bool{}
	for m := range move {
		for d := path.Dir(m); d != "." && d != "/" && !moveDirs[d]; d = path.Dir(d) {
			moveDirs[d] = true
		}
	}
	for p := range pathSet(prPaths, func(c domain.ChangedPath) bool {
		return c.Change == domain.PathRenamed || c.Change == domain.PathCopied
	}) {
		if move[p] || moveDirs[p] {
			return true
		}
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if move[d] {
				return true
			}
		}
	}

	vacated := map[string]bool{}
	for _, c := range movePaths {
		var old string
		switch {
		case c.Change == domain.PathDeleted:
			old = c.Path
		case c.Change == domain.PathRenamed && c.Previous != "" && path.Dir(c.Previous) != path.Dir(c.Path):
			old = c.Previous
		default:
			continue
		}
		if d := path.Dir(old); d != "." && d != "/" {
			vacated[d] = true
		}
	}
	if len(vacated) == 0 {
		return false
	}
	for _, c := range prPaths {
		switch c.Change {
		case domain.PathAdded, domain.PathRenamed, domain.PathCopied:
		default:
			continue
		}
		for d := path.Dir(c.Path); d != "." && d != "/"; d = path.Dir(d) {
			if vacated[d] {
				return true
			}
		}
	}
	return false
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
