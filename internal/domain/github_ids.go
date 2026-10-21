package domain

import "strconv"

// IsGitHubID reports whether s is one of GitHub's numeric ids — a user's, a
// team's, an organization's or a repository's — in the form TF stores them:
// a positive decimal with no sign, no leading zero and no surrounding space.
// Any other spelling of the same number names nothing, since the ids are
// stored and compared as text.
func IsGitHubID(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == s
}
