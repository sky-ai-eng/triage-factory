package events

import "strings"

// Tiny helpers used by every Matches() implementation. A nil pointer on the
// predicate means "no filter" — always matches; a non-nil pointer means
// "must equal." Slices on metadata (Labels) are checked with set semantics.

func boolEq(pred *bool, meta bool) bool {
	if pred == nil {
		return true
	}
	return *pred == meta
}

func strEq(pred *string, meta string) bool {
	if pred == nil {
		return true
	}
	return *pred == meta
}

// strEqFold is the case-insensitive sibling of strEq, for singular exact-match
// filters on GitHub case-insensitive handles — a mentioned login or "org/slug"
// team handle, where a rule's `mentioned_login: "Alice"` must match an event
// carrying "alice". Nil predicate means "no filter."
func strEqFold(pred *string, meta string) bool {
	if pred == nil {
		return true
	}
	return strings.EqualFold(*pred, meta)
}

// hasLabel returns true when the predicate is unset, or when the requested
// label is present in the metadata snapshot.
func hasLabel(pred *string, labels []string) bool {
	if pred == nil {
		return true
	}
	for _, l := range labels {
		if l == *pred {
			return true
		}
	}
	return false
}

// stringInSliceFold is the matcher primitive for the Jira and Linear identity
// allowlists, such as `assignee_in`. An empty (or nil) slice means "no
// filter," matching the nil-pointer convention above — if the rule didn't say
// who it cared about, it doesn't filter on identity. A non-empty slice
// requires meta to match at least one entry under case-insensitive comparison
// using Unicode case folding (Jira account IDs are case-insensitive in
// practice).
func stringInSliceFold(pred []string, meta string) bool {
	if len(pred) == 0 {
		return true
	}
	for _, v := range pred {
		if strings.EqualFold(v, meta) {
			return true
		}
	}
	return false
}

// idIn is the matcher primitive for `_in` lists of ids, such as `repo_in`'s
// registry ids and `author_id_in`'s GitHub account ids: an empty list means no
// filter, and a non-empty one requires meta to be one of its entries exactly.
// Ids are not names, so nothing is folded. An empty meta (an event recorded
// without the id) matches only an empty list.
func idIn(pred []string, meta string) bool {
	if len(pred) == 0 {
		return true
	}
	for _, v := range pred {
		if v == meta {
			return true
		}
	}
	return false
}

// anyIDIn is idIn for metadata that names several ids, such as the ids of the
// labels an issue bears or the GitHub account id of everyone who wrote one of
// a poll's new comments: an empty predicate matches, and a non-empty one
// matches when any of metas is in it.
func anyIDIn(pred, metas []string) bool {
	if len(pred) == 0 {
		return true
	}
	for _, m := range metas {
		if m != "" && idIn(pred, m) {
			return true
		}
	}
	return false
}

// anyInSliceFold is stringInSliceFold for metadata that names several
// identities, such as everyone who wrote one of a poll's new comments: an empty
// predicate matches, and a non-empty one matches when any of metas is in it.
func anyInSliceFold(pred, metas []string) bool {
	if len(pred) == 0 {
		return true
	}
	for _, m := range metas {
		if stringInSliceFold(pred, m) {
			return true
		}
	}
	return false
}
