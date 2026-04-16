package events

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

func intEq(pred *int, meta int) bool {
	if pred == nil {
		return true
	}
	return *pred == meta
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
