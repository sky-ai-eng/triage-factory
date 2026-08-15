package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// UserRepo is the minimal info we need from the GitHub repos endpoint.
type UserRepo struct {
	// ID is GitHub's own numeric id for the repository — the half of its
	// identity a rename does not move. Every enumeration carries it, so the
	// poller records it for the repos it already lists each cycle rather than
	// asking for it.
	ID          int64  `json:"id"`
	FullName    string `json:"full_name"`
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Language    string `json:"language"`
	PushedAt    string `json:"pushed_at"`
	Private     bool   `json:"private"`
}

// ExternalID renders UserRepo.ID the way repo_profiles.external_id stores it:
// decimal text, empty when GitHub sent no id (which is never expected, and is
// still not something to persist as the string "0").
func (r UserRepo) ExternalID() string {
	if r.ID == 0 {
		return ""
	}
	return strconv.FormatInt(r.ID, 10)
}

// ListUserRepos returns all repositories the authenticated user has access to,
// sorted by most recently pushed. Paginates until all repos are fetched, or
// maxFetchPages is reached (TFAC-571) — truncation logs a WARN rather than
// silently returning a partial list.
func (c *Client) ListUserRepos(ctx context.Context) ([]UserRepo, error) {
	var all []UserRepo

	for page := 1; ; page++ {
		if page > maxFetchPages {
			githubLog.Warn("user repos list truncated at page cap; some repos may be missing",
				"resource", "user/repos", "page_cap", maxFetchPages)
			break
		}
		path := fmt.Sprintf("/user/repos?sort=pushed&direction=desc&per_page=100&page=%d", page)
		data, err := c.Get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("fetch repos page %d: %w", page, err)
		}

		var repos []UserRepo
		if err := json.Unmarshal(data, &repos); err != nil {
			return nil, fmt.Errorf("parse repos page %d: %w", page, err)
		}

		if len(repos) == 0 {
			break
		}
		all = append(all, repos...)
	}

	return all, nil
}

// ListInstallationRepos returns every repository a GitHub App installation
// can reach, via GET /installation/repositories. This endpoint requires an
// installation access token — a PAT gets 403 ("You must authenticate with an
// installation access token…"), so only call it on an App-issued client. The
// poller uses the result to map each configured repo onto the installation
// whose token can reach it. Paginates until all repos are fetched, or
// maxFetchPages is reached (TFAC-571) — an installation grant beyond 1,000
// repos truncates with a logged WARN rather than silently under-covering the
// poll's tracked-repo intersection.
//
// A partial list is fine for the poller: the intersection it computes just
// covers fewer repos this cycle. Callers for whom a partial list would be a
// WRONG answer rather than a smaller one — the grant mirror, whose whole
// contract is "this is everything the App can reach" — must use
// ListInstallationReposComplete instead.
func (c *Client) ListInstallationRepos(ctx context.Context) ([]UserRepo, error) {
	repos, _, err := c.ListInstallationReposComplete(ctx)
	return repos, err
}

// ListInstallationReposComplete is ListInstallationRepos plus the one bit the
// slice cannot carry: whether the listing is the WHOLE grant.
//
// Completeness is not a detail for a mirror. A truncated answer written as if it
// were complete makes every repository past the cut look revoked, and the
// difference between "the App cannot reach this" and "we stopped reading"
// is the difference between a security finding and a lie. Two things can make an
// answer partial: the page cap, and GitHub reporting a total_count the pages
// never reached (an empty page arriving early). Both report complete=false, and
// the caller's correct response is to leave the previous answer in place.
func (c *Client) ListInstallationReposComplete(ctx context.Context) ([]UserRepo, bool, error) {
	var all []UserRepo
	total := -1 // -1 until GitHub has told us; a grant of 0 is a legitimate answer

	for page := 1; ; page++ {
		if page > maxFetchPages {
			githubLog.Warn("installation repos list truncated at page cap; some repos may be missing from the poll's tracked-repo intersection",
				"resource", "installation/repositories", "page_cap", maxFetchPages)
			return all, false, nil
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		data, err := c.Get(ctx, path)
		if err != nil {
			return nil, false, fmt.Errorf("fetch installation repositories page %d: %w", page, err)
		}

		var resp struct {
			TotalCount   int        `json:"total_count"`
			Repositories []UserRepo `json:"repositories"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, false, fmt.Errorf("parse installation repositories page %d: %w", page, err)
		}
		total = resp.TotalCount

		if len(resp.Repositories) == 0 {
			break
		}
		all = append(all, resp.Repositories...)
		if len(all) >= resp.TotalCount {
			break
		}
	}

	// Ran out of pages before reaching the count GitHub advertised. Rare, and
	// benign for the poller, but for a mirror it is precisely a partial fetch.
	return all, total >= 0 && len(all) >= total, nil
}

// CheckRepoAccess probes GET /repos/{owner}/{repo} to decide whether this
// client's credential can reach a single repo. It is the per-repo primitive
// behind the write-time reachability fan-out: instead of
// enumerating the whole org to validate a selection, the gate checks only the
// selected slugs, bounded by selection size rather than org size.
//
// It returns (reachable, conclusive):
//
//   - (true, true)   — HTTP 200: reachable by this credential.
//   - (false, true)  — HTTP 404 or 403: definitively unreachable (the repo
//     doesn't exist for this credential, or access is forbidden).
//   - (false, false) — 5xx or a transport/timeout error: indeterminate. The
//     caller should fail OPEN for this slug, matching reachableRepoSet's
//     long-standing posture of never coupling write availability to GitHub's
//     uptime.
//
// Unlike Get/do this reads the raw status code directly — the gate needs to
// discriminate 404/403 (conclusive "no") from 5xx (indeterminate), which the
// flattened error string from do() can't carry. The response body is drained
// and discarded so the connection can be reused; we only care about status.
func (c *Client) CheckRepoAccess(ctx context.Context, owner, repo string) (reachable, conclusive bool) {
	url := c.baseURL + fmt.Sprintf("/repos/%s/%s", owner, repo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport error / context timeout → indeterminate, fail open.
		return false, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusNotFound, http.StatusForbidden:
		return false, true
	default:
		// 5xx and any other unexpected status → indeterminate, fail open.
		return false, false
	}
}

// RepoMeta is a subset of the GitHub repo object.
type RepoMeta struct {
	// ID is GitHub's own numeric id for the repository — the half of its
	// identity a rename or a transfer does not move, unlike owner/repo. It
	// rides along on the response the default branch and clone URL already
	// come from, so recording it costs no extra request.
	ID            int64  `json:"id"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
}

// ExternalID renders RepoMeta.ID the way repo_profiles.external_id stores it:
// decimal text, empty when GitHub sent no id (a response shape we never expect
// but also never want to persist as the string "0", which is not a repository).
func (m RepoMeta) ExternalID() string {
	if m.ID == 0 {
		return ""
	}
	return strconv.FormatInt(m.ID, 10)
}

// GetRepoMeta fetches the default branch and clone URL for a repository.
func (c *Client) GetRepoMeta(ctx context.Context, owner, repo string) (*RepoMeta, error) {
	data, err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s", owner, repo))
	if err != nil {
		return nil, err
	}
	var meta RepoMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse repo meta: %w", err)
	}
	return &meta, nil
}

// Branch is a branch returned by the GitHub branches API.
type Branch struct {
	Name string `json:"name"`
}

// ListBranches returns branches for a repo, optionally filtered by prefix.
// The GitHub REST branches endpoint doesn't support server-side search,
// so we fetch up to 100 branches and filter client-side.
func (c *Client) ListBranches(ctx context.Context, owner, repo, query string, limit int) ([]Branch, error) {
	if limit <= 0 {
		limit = 30
	}
	// Fetch more than we need to allow for filtering
	fetchSize := 100
	path := fmt.Sprintf("/repos/%s/%s/branches?per_page=%d", owner, repo, fetchSize)
	data, err := c.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	var all []Branch
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("parse branches: %w", err)
	}
	if query == "" {
		if len(all) > limit {
			return all[:limit], nil
		}
		return all, nil
	}
	q := strings.ToLower(query)
	var filtered []Branch
	for _, b := range all {
		if strings.Contains(strings.ToLower(b.Name), q) {
			filtered = append(filtered, b)
			if len(filtered) >= limit {
				break
			}
		}
	}
	return filtered, nil
}

// fileContent is the GitHub API response for a repository file.
type fileContent struct {
	Content  string `json:"content"`  // base64-encoded, newline-wrapped
	Encoding string `json:"encoding"` // always "base64" for text files
}

// GetFileContent fetches and decodes a file from a repo's default branch.
// Returns an empty string without error if the file does not exist (404).
func (c *Client) GetFileContent(ctx context.Context, owner, repo, path string) (string, error) {
	data, err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path))
	if err != nil {
		if strings.Contains(err.Error(), "returned 404") {
			return "", nil
		}
		return "", fmt.Errorf("get %s from %s/%s: %w", path, owner, repo, err)
	}

	var f fileContent
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("parse file content %s/%s/%s: %w", owner, repo, path, err)
	}

	if f.Encoding != "base64" {
		return "", fmt.Errorf("unexpected encoding %q for %s/%s/%s", f.Encoding, owner, repo, path)
	}

	// GitHub base64-encodes content with embedded newlines — strip them before decoding.
	clean := strings.ReplaceAll(f.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", fmt.Errorf("decode %s/%s/%s: %w", owner, repo, path, err)
	}

	return string(decoded), nil
}
