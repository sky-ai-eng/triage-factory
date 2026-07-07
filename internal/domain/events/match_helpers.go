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
// filters on GitHub case-insensitive identifiers — a requested reviewer login
// or "org/slug" team handle, where a rule's `requested_login: "Alice"` must
// match an event carrying "alice". Nil predicate means "no filter."
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

// stringInSliceFold is the matcher primitive for `author_in` /
// `reviewer_in` allowlists. An empty (or nil) slice means "no filter,"
// matching the nil-pointer convention above — if the rule didn't say who
// it cared about, it doesn't filter on identity. A non-empty slice
// requires meta to match at least one entry under case-insensitive
// comparison using Unicode case folding (GitHub logins and Jira account
// IDs are case-insensitive in practice).
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
