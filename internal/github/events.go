package github

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// UserRepo is the minimal info we need from the GitHub repos endpoint.
type UserRepo struct {
	FullName    string `json:"full_name"`
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Language    string `json:"language"`
	PushedAt    string `json:"pushed_at"`
	Private     bool   `json:"private"`
}

// ListUserRepos returns all repositories the authenticated user has access to,
// sorted by most recently pushed. Paginates until all repos are fetched.
func (c *Client) ListUserRepos() ([]UserRepo, error) {
	var all []UserRepo

	for page := 1; ; page++ {
		path := fmt.Sprintf("/user/repos?sort=pushed&direction=desc&per_page=100&page=%d", page)
		data, err := c.Get(path)
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

// RepoMeta is a subset of the GitHub repo object.
type RepoMeta struct {
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
}

// GetRepoMeta fetches the default branch and clone URL for a repository.
func (c *Client) GetRepoMeta(owner, repo string) (*RepoMeta, error) {
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s", owner, repo))
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
// Uses GitHub's server-side filtering to avoid fetching all branches.
func (c *Client) ListBranches(owner, repo, query string, limit int) ([]Branch, error) {
	if limit <= 0 {
		limit = 30
	}
	path := fmt.Sprintf("/repos/%s/%s/branches?per_page=%d", owner, repo, limit)
	if query != "" {
		path += "&query=" + url.QueryEscape(query)
	}
	data, err := c.Get(path)
	if err != nil {
		return nil, err
	}
	var branches []Branch
	if err := json.Unmarshal(data, &branches); err != nil {
		return nil, fmt.Errorf("parse branches: %w", err)
	}
	return branches, nil
}

// fileContent is the GitHub API response for a repository file.
type fileContent struct {
	Content  string `json:"content"`  // base64-encoded, newline-wrapped
	Encoding string `json:"encoding"` // always "base64" for text files
}

// GetFileContent fetches and decodes a file from a repo's default branch.
// Returns an empty string without error if the file does not exist (404).
func (c *Client) GetFileContent(owner, repo, path string) (string, error) {
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path))
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
