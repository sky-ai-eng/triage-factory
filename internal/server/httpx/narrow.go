package httpx

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// --- list narrowing ---
//
// A list route answers one page, so every filter, search and order a client
// offers over a list is a field on the route itself: a client narrowing the
// page it holds answers a different question from the one on screen. These are
// the two narrowing fields every list route spells the same way — `search`
// and `sort_key`/`sort_dir` — parsed once here so a route's body struct only
// declares them.

// MaxSearchRunes caps a list's `search` needle. A needle longer than this is a
// paste, not a search, and the cap is measured in runes so a multi-byte needle
// isn't refused for being multi-byte.
const MaxSearchRunes = 200

// Search validates a list body's `search` field and returns the needle the
// store matches. It is trimmed, and empty after the trim is absent, so " ci "
// and "ci" are one query and "  " is no query. More than MaxSearchRunes runes
// is an INVALID_FIELD fault on `search`.
//
// The needle is matched as a case-insensitive literal substring over the
// columns the route's doc names (db.SQLiteSearch / db.PostgresSearch for a
// store read, SearchMatches for rows already in hand). Fingerprint it through
// SearchKey, never raw.
func Search(v *Validation, raw string) string {
	needle := strings.TrimSpace(raw)
	if utf8.RuneCountInString(needle) > MaxSearchRunes {
		v.Invalid("search", fmt.Sprintf("search must be at most %d characters", MaxSearchRunes))
	}
	return needle
}

// SearchKey is the needle's spelling in a page fingerprint: case-folded,
// because the predicate is. Two spellings of one needle match the same rows,
// so they must fingerprint as the same query rather than cost the caller its
// page token.
func SearchKey(needle string) string {
	return strings.ToLower(needle)
}

// SearchMatches reports whether any of fields carries needle as a
// case-insensitive substring. An empty needle matches everything. It is the
// in-hand twin of the store predicate, for a list whose rows come from
// somewhere that cannot search them (an upstream API with no search
// parameter, a resolved set built in Go): the filter runs before the page
// window is cut, never over a page already cut.
func SearchMatches(needle string, fields ...string) bool {
	if needle == "" {
		return true
	}
	n := strings.ToLower(needle)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), n) {
			return true
		}
	}
	return false
}

// SortKey is one member of a list route's `sort_key` vocabulary and the
// direction it orders in when the body names the key without a `sort_dir`.
type SortKey struct {
	Name string
	// Default is db.SortAsc or db.SortDesc.
	Default string
}

// Sort validates a list body's `sort_key`/`sort_dir` pair against the route's
// own key vocabulary and returns the order the store applies.
//
//   - Absent `sort_key` asks for the route's default order, and the returned
//     db.Sort is the zero value. There is deliberately no "default" key to
//     send: a name for the absent case is a second spelling of it.
//   - A key outside keys is INVALID_FIELD on `sort_key`, naming the
//     vocabulary.
//   - `sort_dir` must be db.SortAsc or db.SortDesc. Sent without a key it is
//     refused rather than ignored: the default order has no direction, so a
//     lone sort_dir is a caller expecting something the answer will not show.
//   - A key with no `sort_dir` takes that key's own Default.
//
// Both fields belong in the page fingerprint (pass the returned db.Sort): an
// offset addresses a position in an ordering, so a token minted under one sort
// names different rows under another. The store still ends the order in its id
// tiebreaker whatever the key.
func Sort(v *Validation, key, dir string, keys []SortKey) db.Sort {
	var out db.Sort
	if dir != "" && dir != db.SortAsc && dir != db.SortDesc {
		v.Invalid("sort_dir", fmt.Sprintf("unknown sort_dir %q; must be one of: %s, %s", dir, db.SortAsc, db.SortDesc))
	}
	if key == "" {
		if dir != "" {
			v.Invalid("sort_dir", "sort_dir requires sort_key")
		}
		return out
	}
	i := slices.IndexFunc(keys, func(k SortKey) bool { return k.Name == key })
	if i < 0 {
		names := make([]string, len(keys))
		for j, k := range keys {
			names[j] = k.Name
		}
		v.Invalid("sort_key", fmt.Sprintf("unknown sort_key %q; must be one of: %s", key, strings.Join(names, ", ")))
		return out
	}
	out.Key, out.Dir = key, dir
	if out.Dir == "" {
		out.Dir = keys[i].Default
	}
	return out
}

// FacetJSON is one value of a facets node's cut and how many rows carry it.
// Every facets node answers its cuts in this one shape, so a client reads
// `{value, count}` under each named key whichever list it sits beside.
type FacetJSON struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Facets renders a store's facet rows for the wire. Never nil: a cut nothing
// matched is `[]`.
func Facets(in []db.Facet) []FacetJSON {
	out := make([]FacetJSON, len(in))
	for i, f := range in {
		out[i] = FacetJSON{Value: f.Value, Count: f.Count}
	}
	return out
}
