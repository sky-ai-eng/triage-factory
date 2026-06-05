package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// ErrGitHubHostUnreachable wraps the network-level failure of a call to a
// GitHub host (DNS, dial, TLS, timeout) — the case where TF's backend
// couldn't reach the host at all, as distinct from the host answering with
// an auth/permission error. The Connect OAuth handler keys on this via
// errors.Is to render the "we tried to reach the host and couldn't"
// onboarding state separately from "you haven't connected yet."
var ErrGitHubHostUnreachable = errors.New("github host unreachable")

// GitHubUser is the subset of fields we extract from the GitHub user endpoint.
type GitHubUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Name      string `json:"name"`
}

// JiraUser is the subset of fields we extract from the Jira myself endpoint.
//
// Atlassian's stable identifier moved from `key` (Jira Server / DC, the
// legacy username-style key) to `accountId` (Jira Cloud, an opaque hash).
// /rest/api/2/myself returns whichever is appropriate for the deployment.
// We capture both and let StableID() pick — AccountID first because
// Cloud is dominant, falling back to Key for Server / DC installs.
type JiraUser struct {
	AccountID   string `json:"accountId"`
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
}

// StableID returns the deployment-appropriate stable identifier for this
// Jira user — accountId on Cloud, falling back to the legacy key on
// Server / DC. This is the value persisted to users.jira_account_id and
// the value predicate matchers compare against.
func (u JiraUser) StableID() string {
	if u.AccountID != "" {
		return u.AccountID
	}
	return u.Key
}

// ValidateGitHub checks the PAT against the GitHub API and returns the user info.
func ValidateGitHub(ctx context.Context, baseURL, pat string) (*GitHubUser, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	// github.com API lives at api.github.com; GHE uses {host}/api/v3
	var apiURL string
	if baseURL == "https://github.com" {
		apiURL = "https://api.github.com/user"
	} else {
		apiURL = baseURL + "/api/v3/user"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// "Bearer" works for both fine-grained and classic PATs
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitHubHostUnreachable, err)
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

// ValidateJira checks the configured credential against the Jira API and
// returns the user info. The auth scheme (Basic/Bearer) and REST API version
// come from cfg, so a DC PAT and a Cloud API token each validate against the
// same /myself endpoint they will use at request time. Build cfg with
// jira.DataCenterPAT or jira.CloudAPIToken.
func ValidateJira(ctx context.Context, cfg jira.Config) (*JiraUser, error) {
	req, err := cfg.NewAPIRequest(ctx, http.MethodGet, "myself", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

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
