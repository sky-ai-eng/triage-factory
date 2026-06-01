package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ExchangeGitHubOAuthCode trades a GitHub App user-to-server authorization
// code for a user access token (a `ghu_` token) against the host's OAuth
// token endpoint. It is the "access" → "identity" hinge in the Connect flow:
// the App installation token carries org access but no person, so a separate
// user-to-server token is minted purely to answer whoami.
//
// webBase is the user-facing GitHub web base (github.com or a GHES host).
// The OAuth token endpoint lives at {webBase}/login/oauth/access_token for
// both github.com and GHES — note this is the WEB host, not the /api/v3 REST
// base. clientID/clientSecret are the org's registered App credentials.
// redirectURI must match the callback registered on the App.
//
// Returns ErrGitHubHostUnreachable (wrapped) when the host can't be reached
// at the network level, so the caller can distinguish an infra problem from
// a credential/code problem. GitHub's token endpoint answers 200 even on
// logical failures (e.g. bad_verification_code), reporting them in an
// `error` field rather than the status code — both shapes map to a non-nil,
// non-unreachable error here.
//
// The returned token is meant to be used once (a single GET /user) and
// discarded — v1 keeps no per-user token store.
func ExchangeGitHubOAuthCode(ctx context.Context, webBase, clientID, clientSecret, code, redirectURI string) (string, error) {
	webBase = strings.TrimRight(webBase, "/")
	endpoint := webBase + "/login/oauth/access_token"

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this GitHub replies with a urlencoded body; JSON is easier to
	// parse and unambiguous about the error shape.
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGitHubHostUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github oauth token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("github oauth error: %s (%s)", out.Error, out.ErrorDescription)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("github oauth response missing access_token")
	}
	return out.AccessToken, nil
}
