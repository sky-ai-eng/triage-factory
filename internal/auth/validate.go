package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// GitHubUser is the subset of fields we extract from the GitHub user endpoint.
type GitHubUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Name      string `json:"name"`
}

// JiraUser is the subset of fields we extract from the Jira myself endpoint.
type JiraUser struct {
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
}

// ValidateGitHub checks the PAT against the GitHub API and returns the user info.
func ValidateGitHub(baseURL, pat string) (*GitHubUser, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	// github.com API lives at api.github.com; GHE uses {host}/api/v3
	var apiURL string
	if baseURL == "https://github.com" {
		apiURL = "https://api.github.com/user"
	} else {
		apiURL = baseURL + "/api/v3/user"
	}

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// "Bearer" works for both fine-grained and classic PATs
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("bad token: GitHub returned 401 Unauthorized")
	case http.StatusForbidden:
		return nil, fmt.Errorf("missing scopes: GitHub returned 403 Forbidden — ensure token has 'repo' and 'read:org' scopes")
	default:
		return nil, fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))
	}

	var user GitHubUser
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &user, nil
}

// ValidateJira checks the PAT against the Jira API and returns the user info.
func ValidateJira(baseURL, pat string) (*JiraUser, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	apiURL := baseURL + "/rest/api/2/myself"

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("bad token: Jira returned 401 Unauthorized")
	case http.StatusForbidden:
		return nil, fmt.Errorf("insufficient permissions: Jira returned 403 Forbidden")
	default:
		return nil, fmt.Errorf("jira API error %d: %s", resp.StatusCode, string(body))
	}

	var user JiraUser
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	// Extract avatar from Jira's nested avatarUrls if present
	if user.AvatarURL == "" {
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err == nil {
			if avatarURLs, ok := raw["avatarUrls"].(map[string]any); ok {
				if url48, ok := avatarURLs["48x48"].(string); ok {
					user.AvatarURL = url48
				}
			}
		}
	}

	return &user, nil
}
