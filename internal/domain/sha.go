package domain

// ShortSHA truncates a git SHA to a human-readable 12-char prefix, leaving
// shorter or empty values untouched. 12 hex chars is the conventional
// unambiguous abbreviation for a single repo; an injection naming a SHA is
// informational, so a prefix is clearer than a full 40-char hash repeated
// twice in one sentence.
func ShortSHA(sha string) string {
	const n = 12
	if len(sha) <= n {
		return sha
	}
	return sha[:n]
}
