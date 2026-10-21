package domain

import (
	"strconv"
	"strings"
)

// IsGitHubID reports whether s is one of GitHub's numeric ids — a user's, a
// team's, an organization's or a repository's — in the form TF stores them:
// a positive decimal with no sign, no leading zero and no surrounding space.
// Any other spelling of the same number names nothing, since the ids are
// stored and compared as text.
func IsGitHubID(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == s
}

// GitHubAccount is a GitHub account as a TF user's identity binding records
// it: its login, and GitHub's id for it ("" when the binding recorded none).
type GitHubAccount struct {
	Login string
	ID    string
}

// Is reports whether the actor a snapshot or event names — its login, and
// GitHub's id when it carries one — is this account. The ids decide when both
// sides have one, so a renamed account is still itself and a login GitHub has
// handed to another account is not; otherwise the logins do, compared without
// regard to case as GitHub compares them. An account with neither is nobody.
func (a GitHubAccount) Is(login, id string) bool {
	if a.ID != "" && id != "" {
		return a.ID == id
	}
	return a.Login != "" && strings.EqualFold(a.Login, login)
}

// GitHubAccountOf is the account an identity binding records, the zero
// account for no binding.
func GitHubAccountOf(id *UserGitHubIdentity) GitHubAccount {
	if id == nil {
		return GitHubAccount{}
	}
	return GitHubAccount{Login: id.Login, ID: id.GitHubUserID}
}
