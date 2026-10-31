//go:build live

// Live tests in this file hit api.github.com with a real GitHub App's
// private key + installation ID. Build-tagged so CI doesn't run them
// by default; no API spend (mint + REST calls are free) but they
// require operator-side credentials.
//
// To run: `go test -tags live ./internal/github/githubapp/ -v -run TestLive`
//
// Env vars required:
//
//   - GITHUB_APP_PRIVATE_KEY_PATH: path to the App's PEM file
//   - GITHUB_APP_ID:               numeric App ID
//   - GITHUB_INSTALLATION_ID:      installation ID to mint against
//
// The installation must have at least metadata:read on one repo for
// the smoke check to succeed.

package githubapp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
)

// liveConfig reads the env vars and returns a fully-resolved Minter
// plus the installation ID to mint against. Skips the test if any
// required var is missing so developer machines without the App fail
// cleanly rather than panic.
func liveConfig(t *testing.T) (*githubapp.Minter, int64) {
	t.Helper()
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	if keyPath == "" {
		t.Skip("GITHUB_APP_PRIVATE_KEY_PATH not set; skipping live test")
	}
	appIDStr := os.Getenv("GITHUB_APP_ID")
	if appIDStr == "" {
		t.Skip("GITHUB_APP_ID not set; skipping live test")
	}
	installIDStr := os.Getenv("GITHUB_INSTALLATION_ID")
	if installIDStr == "" {
		t.Skip("GITHUB_INSTALLATION_ID not set; skipping live test")
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		t.Fatalf("GITHUB_APP_ID parse: %v", err)
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		t.Fatalf("GITHUB_INSTALLATION_ID parse: %v", err)
	}

	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	key, err := githubapp.ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}

	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m, installID
}

// TestLive_MintInstallationToken is the load-bearing live check: a
// real JWT signed with the App's key is accepted by GitHub, and the
// returned token (a) has the expected shape and (b) actually works
// when used against another endpoint.
//
// If this fails, either the App / installation config is wrong, or
// our JWT shape has drifted from what GitHub accepts — and there's
// no point running the more expensive sandbox tests downstream.
func TestLive_MintInstallationToken(t *testing.T) {
	m, installID := liveConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := m.MintInstallationToken(ctx, installID)
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	if tok.Value == "" {
		t.Error("Token.Value empty")
	}
	if tok.ExpiresAt.Before(time.Now()) {
		t.Errorf("Token.ExpiresAt = %v already in the past", tok.ExpiresAt)
	}
	// Documented TTL is 1 hour; allow a wide window (45min–75min)
	// because GitHub may shorten if the App's permissions changed
	// during minting or for other operational reasons.
	ttl := time.Until(tok.ExpiresAt)
	if ttl < 45*time.Minute || ttl > 75*time.Minute {
		t.Errorf("Token TTL = %v outside expected ~1h window", ttl)
	}

	// Smoke-check the token by calling /installation/repositories —
	// the standard "what can this installation see" endpoint. A
	// successful 200 proves the minted token is valid auth, not just
	// well-formed.
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://api.github.com/installation/repositories", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "triage-factory-live-test")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("/installation/repositories: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("/installation/repositories status = %d, body = %s", resp.StatusCode, body)
	}

	// Parse total_count just to confirm the response is the expected
	// shape (defensive against an unrelated endpoint change).
	var parsed struct {
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Errorf("parse repo list response: %v; body: %s", err, body)
	}
	t.Logf("installation %d sees %d repo(s); token TTL %v", installID, parsed.TotalCount, ttl)
}
