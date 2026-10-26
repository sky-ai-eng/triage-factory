package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// ListSSHSigningKeys returns the public keys login holds as SSH signing keys
// (GET /users/{login}/ssh_signing_keys), each as GitHub prints it,
// "ssh-ed25519 AAAA…" with no comment, so a key is found by comparing the
// string exactly. The listing is public and served with a 60-second cache, so
// a key added or removed can take a minute to show.
//
// A 404 comes back as an *HTTPError (IsHTTP404): for a login no account holds,
// and from a GitHub that does not serve SSH signing keys at all. The caller
// tells the two apart by whether the account exists.
func (c *Client) ListSSHSigningKeys(ctx context.Context, login string) ([]string, error) {
	var keys []string
	for page := 1; ; page++ {
		data, err := c.Get(ctx, fmt.Sprintf("/users/%s/ssh_signing_keys?per_page=100&page=%d", url.PathEscape(login), page))
		if err != nil {
			return nil, fmt.Errorf("list ssh signing keys: %w", err)
		}
		var items []struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("parse ssh signing keys: %w", err)
		}
		for _, it := range items {
			keys = append(keys, it.Key)
		}
		if len(items) < 100 {
			return keys, nil
		}
		if page >= maxFetchPages {
			githubLog.Warn("ssh signing key listing truncated at cap; a key past it reads as absent",
				"login", login, "cap", maxFetchPages*100)
			return keys, nil
		}
	}
}

// CommitVerification is GitHub's verdict on one commit's signature, and the
// committer address the verdict was reached for.
type CommitVerification struct {
	Verified bool
	// Reason is GitHub's verification reason: "valid" for a verified commit,
	// else why not ("unknown_key", "no_user", "unverified_email", "unsigned",
	// …).
	Reason         string
	CommitterEmail string
}

// GetCommitVerification reads sha's verification in owner/repo
// (GET /repos/{owner}/{repo}/commits/{sha}). GitHub stores a passing result
// when the commit is pushed and evaluates a failing one again on every read,
// so a commit that failed reads valid once its fault is fixed. A commit or
// repository the credential cannot see is a 404 *HTTPError.
func (c *Client) GetCommitVerification(ctx context.Context, owner, repo, sha string) (CommitVerification, error) {
	data, err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha)))
	if err != nil {
		return CommitVerification{}, fmt.Errorf("get commit: %w", err)
	}
	var out struct {
		Commit struct {
			Committer struct {
				Email string `json:"email"`
			} `json:"committer"`
			Verification *struct {
				Verified bool   `json:"verified"`
				Reason   string `json:"reason"`
			} `json:"verification"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return CommitVerification{}, fmt.Errorf("parse commit: %w", err)
	}
	if out.Commit.Verification == nil {
		return CommitVerification{}, fmt.Errorf("commit %s: response carried no verification", sha)
	}
	return CommitVerification{
		Verified:       out.Commit.Verification.Verified,
		Reason:         out.Commit.Verification.Reason,
		CommitterEmail: out.Commit.Committer.Email,
	}, nil
}
