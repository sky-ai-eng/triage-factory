package kbstore

import (
	"fmt"
	"sort"
	"strings"
)

// underPrefix reports whether path is at, or beneath, the listing filter
// prefix. Matching is by path segment, not by bytes: "notes" selects
// "notes/plan.md" but never "notes-archive/plan.md", which a raw
// strings.HasPrefix would hand back as a sibling folder's contents.
// An empty prefix selects everything.
func underPrefix(path, prefix string) bool {
	if prefix == "" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// sortEntries puts a listing in the one order every backend answers with:
// root first (private before shared, the order Roots() declares), then path.
// Two backends that sorted differently would make a page's rows depend on the
// deployment mode.
func sortEntries(entries []FileInfo) {
	rank := map[Root]int{}
	for i, r := range Roots() {
		rank[r] = i
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Root != entries[j].Root {
			return rank[entries[i].Root] < rank[entries[j].Root]
		}
		return entries[i].Path < entries[j].Path
	})
}

// movePlanRoots validates both sides of a move and returns the source root.
// It rejects a destination that sits inside the source subtree, which would
// otherwise be a move of a folder into itself: the plan would name a
// destination that the same plan is also relocating, and the result depends on
// the order the entries happened to be walked in.
func movePlanRoots(from, to Ref) (Root, error) {
	srcRoot, err := ParseRoot(string(from.Root))
	if err != nil {
		return "", fmt.Errorf("source %w", err)
	}
	if _, err := ParseRoot(string(to.Root)); err != nil {
		return "", fmt.Errorf("destination %w", err)
	}
	if err := ValidatePath(from.Path); err != nil {
		return "", fmt.Errorf("source %w", err)
	}
	if err := ValidatePath(to.Path); err != nil {
		return "", fmt.Errorf("destination %w", err)
	}
	if from.Root == to.Root && underPrefix(to.Path, from.Path) {
		return "", fmt.Errorf("%w: %s is inside %s", ErrInvalidName, to, from)
	}
	return srcRoot, nil
}
