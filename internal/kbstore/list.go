package kbstore

import (
	"cmp"
	"slices"
	"strings"
)

// ListQuery is what a List reads.
//
// Roots and PathPrefix are the scope: the entries under those roots whose path
// is at or beneath PathPrefix, matched by segment (an empty PathPrefix is the
// whole root). An empty Roots is an empty result, not "everything": widening a
// read whose scope resolved to nothing is how a visibility gate fails open.
//
// Search and Sort are how a listing surface narrows and orders that scope.
// Both backends apply them to the entries they found, through the same code,
// so a page's rows never depend on the deployment mode.
type ListQuery struct {
	Roots      []Root
	PathPrefix string
	// Search keeps the entries whose path carries it as a case-insensitive
	// literal substring. A folder name matches everything filed under it,
	// because the path spells it. Empty keeps every entry.
	Search string
	// Sort orders the result. The zero value is the default order.
	Sort Sort
}

// SortKey names an order a listing can be asked for.
type SortKey string

// The listing's sort keys.
const (
	// SortPath orders by path.
	SortPath SortKey = "path"
	// SortSize orders by byte size.
	SortSize SortKey = "size"
	// SortModTime orders by last-modified time.
	SortModTime SortKey = "mod_time"
)

// Sort is the order a List is asked for. The zero value is the default order:
// root first (private before shared, the order Roots declares), then path.
// A key replaces that order's lead, and the default order breaks every tie,
// ascending whatever the direction, so the order is total: (root, path)
// identifies an entry. A key outside the constants above reads as the default
// order.
type Sort struct {
	Key  SortKey
	Desc bool
}

// narrow applies q's search and order to the entries a backend found in q's
// scope.
func narrow(entries []FileInfo, q ListQuery) []FileInfo {
	if needle := strings.ToLower(strings.TrimSpace(q.Search)); needle != "" {
		entries = slices.DeleteFunc(entries, func(e FileInfo) bool {
			return !strings.Contains(strings.ToLower(e.Path), needle)
		})
	}
	sortEntries(entries, q.Sort)
	return entries
}

// sortEntries puts a listing in order (see Sort). Two backends that sorted
// differently would make a page's rows depend on the deployment mode, which is
// why neither sorts on its own.
func sortEntries(entries []FileInfo, order Sort) {
	rank := map[Root]int{}
	for i, r := range Roots() {
		rank[r] = i
	}
	byDefault := func(a, b FileInfo) int {
		if c := cmp.Compare(rank[a.Root], rank[b.Root]); c != 0 {
			return c
		}
		return comparePaths(a.Path, b.Path)
	}
	slices.SortFunc(entries, func(a, b FileInfo) int {
		var c int
		switch order.Key {
		case SortPath:
			c = comparePaths(a.Path, b.Path)
		case SortSize:
			c = cmp.Compare(a.Size, b.Size)
		case SortModTime:
			c = a.ModTime.Compare(b.ModTime)
		}
		if order.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
		return byDefault(a, b)
	})
}

// comparePaths orders two paths the way a reader expects a list of names to
// run: case-insensitively, so "Runbook.md" sits beside "runbooks/" rather
// than ahead of every lower-case name. Paths that fold to the same text fall
// back to their bytes, so the order stays total.
func comparePaths(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}
