package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
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

// ErrGitHubEmailPermission is returned when a user-scoped credential can read
// /user but cannot read /user/emails. Callers surface permission-specific setup
// guidance instead of treating this as a generic bad token.
var ErrGitHubEmailPermission = errors.New("github email permission missing")

// ErrGitHubPrimaryEmailUnavailable means GitHub returned no address that was
// both primary and verified. Commit attribution requires that exact account
// fact, so identity capture fails rather than selecting an alternate address.
var ErrGitHubPrimaryEmailUnavailable = errors.New("github verified primary email unavailable")

// ErrJiraHostUnreachable is the Jira sibling of ErrGitHubHostUnreachable: it
// wraps the network-level failure of a call to a Jira host (DNS, dial, TLS,
// timeout) — TF's backend couldn't reach the host at all, distinct from the
// host answering with an auth/permission error. The per-user Jira PAT handler
// keys on this via errors.Is to return a 502 ("couldn't reach the host" infra
// state) rather than the 422 it returns for a token the host rejected.
var ErrJiraHostUnreachable = errors.New("jira host unreachable")

// anthropicModelsURL is Anthropic's documented lightweight key-check endpoint:
// listing models authenticates the key without spending tokens, so a GET here
// is the cheapest "is this key valid" probe. It's a var (not a const) only so
// tests can point it at a stub host; production never reassigns it.
// anthropicAPIVersion is the required anthropic-version header value.
var anthropicModelsURL = "https://api.anthropic.com/v1/models"

const anthropicAPIVersion = "2023-06-01"

// ErrAnthropicKeyInvalid is returned by ValidateAnthropicAPIKey when Anthropic
// rejects the key (401/403). Its message is user-facing — the connect handler
// surfaces it verbatim — so it reads as guidance rather than a status code.
var ErrAnthropicKeyInvalid = errors.New("the Anthropic API key was rejected — double-check it and try again")

// ErrAnthropicUnreachable is returned when TF couldn't reach the Anthropic API
// at all (a transport failure, or an unexpected non-2xx status). Callers can
// key on this via errors.Is to separate "your key is wrong" from
// "we couldn't talk to Anthropic"; the message is user-facing too.
var ErrAnthropicUnreachable = errors.New("couldn't reach Anthropic — check your connection and try again")

// GitHubUser is the subset of fields we extract from the GitHub user endpoint.
//
// ID is the account's numeric id. Both halves of the identity are captured
// because they answer different questions: Login is what a human recognises
// and what every GitHub-side lookup takes, while ID is the half that survives
// a rename — so it is what an identity binding persists alongside the login.
type GitHubUser struct {
	ID           int64  `json:"id"`
	Login        string `json:"login"`
	AvatarURL    string `json:"avatar_url"`
	Name         string `json:"name"`
	PrimaryEmail string `json:"-"`
}

// UserID renders the numeric account id in the text form
// user_github_identities.github_user_id stores (the convention every
// GitHub-issued id in this schema follows), or "" when the host reported none.
// A "" is a durable, supported state: the binding keeps working off the login,
// and the id fills in on the next capture.
func (u GitHubUser) UserID() string {
	if u.ID == 0 {
		return ""
	}
	return strconv.FormatInt(u.ID, 10)
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
// Server / DC. This is the value persisted to user_jira_identities.account_id
// (host-scoped) and the value predicate matchers compare against.
// It routes through jira.StableUserID — the one source of truth for this
// precedence — so the bound identity, the polled snapshot, and the claim
// guard can never derive a different id for the same user.
func (u JiraUser) StableID() string {
	return jira.StableUserID(u.AccountID, u.Key, "")
}

// ValidateGitHub checks the PAT against the GitHub API and returns the user
// info. The API mount comes from ghbase.APIBase — the same derivation the
// client and the poller use — so a host class the token works against is a
// host class this probe reaches: notably a *.ghe.com data-residency tenant,
// whose API lives on an api.* subdomain rather than the GHES /api/v3 path.
func ValidateGitHub(ctx context.Context, baseURL, pat string) (*GitHubUser, error) {
	apiURL := ghbase.APIBase(baseURL) + "/user"

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

// CaptureGitHubIdentity validates a user-scoped GitHub credential and captures
// the account's verified primary email. The email endpoint is intentionally
// separate from /user: that response's email field is only the public profile
// email and may be empty even when the account has a primary address.
func CaptureGitHubIdentity(ctx context.Context, baseURL, token string) (*GitHubUser, error) {
	user, err := ValidateGitHub(ctx, baseURL, token)
	if err != nil {
		return nil, err
	}

	apiURL := ghbase.APIBase(baseURL) + "/user/emails"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build email request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitHubHostUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read email response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("bad token: GitHub returned 401 Unauthorized while reading email addresses")
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: GitHub returned 403 Forbidden — grant 'user:email' for a classic token or read access to Email addresses", ErrGitHubEmailPermission)
	default:
		return nil, fmt.Errorf("GitHub email API error %d: %s", resp.StatusCode, string(body))
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal(body, &emails); err != nil {
		return nil, fmt.Errorf("parse email response: %w", err)
	}
	for _, candidate := range emails {
		if candidate.Primary && candidate.Verified && strings.TrimSpace(candidate.Email) != "" {
			user.PrimaryEmail = strings.TrimSpace(candidate.Email)
			return user, nil
		}
	}
	return nil, fmt.Errorf("%w: GitHub account has no verified primary email", ErrGitHubPrimaryEmailUnavailable)
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
		// Wrap the network failure in the sentinel so callers can errors.Is
		// it apart from an auth/permission rejection (the host answered) —
		// the 502-vs-422 split the PAT handlers render. Keep the underlying
		// cause visible via %v for logs.
		return nil, fmt.Errorf("%w: %v", ErrJiraHostUnreachable, err)
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

// ValidateAnthropicAPIKey checks an Anthropic API key against the live API and
// returns nil when it authenticates — the Claude-credentials sibling of
// ValidateGitHub / ValidateJira. It GETs the models endpoint (Anthropic's
// documented lightweight check: no token cost) with the x-api-key +
// anthropic-version headers:
//
//   - 200            → nil (valid)
//   - 401 / 403      → ErrAnthropicKeyInvalid (the host rejected the key)
//   - transport / other non-2xx → ErrAnthropicUnreachable
//
// Both error values carry user-facing messages; the connect handler surfaces
// err.Error() verbatim. The caller is responsible for not passing an empty key
// (an empty key is the "use system credentials" clear path, not a validation).
func ValidateAnthropicAPIKey(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicModelsURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := httpClient.Do(req)
	if err != nil {
		// A dial/TLS/timeout failure — the host never answered. Surface the
		// "couldn't reach" state, not "bad key", so the UI doesn't tell the user
		// to fix a key that may be fine. Keep the underlying cause via %v (as
		// ValidateGitHub / ValidateJira do) so operator logs can tell DNS from
		// TLS from timeout; errors.Is still matches the sentinel in the chain.
		return fmt.Errorf("%w: %v", ErrAnthropicUnreachable, err)
	}
	defer resp.Body.Close()
	// Drain (bounded) so the keep-alive connection can be reused; the body is
	// irrelevant — the status code is the whole signal.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAnthropicKeyInvalid
	default:
		return fmt.Errorf("%w (Anthropic returned HTTP %d)", ErrAnthropicUnreachable, resp.StatusCode)
	}
}
