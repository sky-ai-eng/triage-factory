package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// handleGitHubAppRegisterStart generates the manifest JSON, a signed
// state token, and the manifest POST URL for the frontend to submit.
// Org-admin only. Works in both local and multi mode.
//
// POST /api/orgs/{org_id}/github-app/register/start
func (s *Server) handleGitHubAppRegisterStart(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.requireOrgAdmin(w, r)
	if !ok {
		return
	}

	var req struct {
		Host       string `json:"host"`
		OwnerType  string `json:"owner_type"`
		OwnerLogin string `json:"owner_login"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.OwnerType == "" || req.OwnerLogin == "" {
		badRequest(w, "owner_type and owner_login are required")
		return
	}
	if req.OwnerType != "user" && req.OwnerType != "org" {
		badRequest(w, "owner_type must be \"user\" or \"org\"")
		return
	}

	var existing *domain.OrgGitHubApp
	var org *domain.Org
	var orgSettings domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		existing, err = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if err != nil {
			return err
		}
		org, err = tx.Orgs.GetOrg(r.Context(), orgID)
		if err != nil {
			return err
		}
		orgSettings, err = tx.Orgs.GetSettings(r.Context(), orgID)
		return err
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "org already has a GitHub App registered; remove it first",
		})
		return
	}
	if org == nil {
		notFound(w, "org")
		return
	}

	ghBase := orgSettings.GitHubBaseURL
	if ghBase == "" {
		ghBase = "https://github.com"
	}
	ghBase = strings.TrimRight(ghBase, "/")

	publicURL := s.deployCfg.publicURL

	appName := "Triage Factory"
	if org.Name != "" {
		appName += " (" + org.Name + ")"
	}
	// GitHub App names max 34 chars.
	if len(appName) > 34 {
		appName = appName[:34]
	}

	manifest := map[string]any{
		"name": appName,
		"url":  publicURL,
		"hook_attributes": map[string]any{
			"url":    publicURL + "/api/webhooks/github/" + orgID,
			"active": false,
		},
		"redirect_url":  publicURL + "/api/orgs/" + orgID + "/github-app/register/callback",
		"callback_urls": []string{publicURL + "/api/orgs/" + orgID + "/github-app/register/callback"},
		"public":        false,
		"default_permissions": map[string]string{
			"issues":        "write",
			"pull_requests": "write",
			"contents":      "read",
			"metadata":      "read",
			"checks":        "read",
			"actions":       "read",
		},
		"default_events": []string{
			"pull_request",
			"pull_request_review",
			"pull_request_review_comment",
			"issue_comment",
			"push",
			"check_run",
			"check_suite",
			"installation",
			"installation_repositories",
		},
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		internalError(w, "github-app", fmt.Errorf("marshal manifest: %w", err))
		return
	}

	state := appRegisterState{
		OrgID:     orgID,
		ExpiresAt: timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(s.deployCfg.hmacKey)
	if err != nil {
		internalError(w, "github-app", fmt.Errorf("sign state: %w", err))
		return
	}

	var manifestPostURL string
	switch req.OwnerType {
	case "org":
		manifestPostURL = ghBase + "/organizations/" + req.OwnerLogin + "/settings/apps/new"
	default:
		manifestPostURL = ghBase + "/settings/apps/new"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"manifest_post_url": manifestPostURL,
		"manifest_json":     string(manifestJSON),
		"state":             signed,
	})
}

// handleGitHubAppRegisterCallback exchanges GitHub's temporary code
// for the App's credentials, writes org_github_apps + vault secrets,
// and redirects the browser to the workspace settings page.
//
// GET /api/orgs/{org_id}/github-app/register/callback?code=...&state=...
func (s *Server) handleGitHubAppRegisterCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.requireOrgAdmin(w, r)
	if !ok {
		return
	}

	code := r.URL.Query().Get("code")
	stateRaw := r.URL.Query().Get("state")
	if code == "" || stateRaw == "" {
		badRequest(w, "missing code or state parameter")
		return
	}

	state, err := parseAppRegisterState(stateRaw, s.deployCfg.hmacKey)
	if err != nil {
		log.Printf("[github-app] invalid state: %v", err)
		http.Error(w, "invalid or expired state token", http.StatusUnauthorized)
		return
	}
	if state.OrgID != orgID {
		http.Error(w, "state org mismatch", http.StatusUnauthorized)
		return
	}

	// Check for an existing registration BEFORE calling GitHub, so a
	// stale callback or second tab doesn't create an orphan App on
	// GitHub that we then discard at the insert constraint.
	var orgSettings domain.OrgSettings
	var existing *domain.OrgGitHubApp
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		existing, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		orgSettings, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "org already has a GitHub App registered; remove it first",
		})
		return
	}

	ghBase := orgSettings.GitHubBaseURL
	if ghBase == "" {
		ghBase = "https://github.com"
	}
	apiBase := githubAPIBase(ghBase)

	conversionURL := apiBase + "/app-manifests/" + code + "/conversions"
	convResp, err := exchangeManifestCode(r.Context(), conversionURL)
	if err != nil {
		log.Printf("[github-app] manifest exchange: %v", err)
		internalError(w, "github-app", fmt.Errorf("GitHub manifest exchange failed"))
		return
	}

	appIDStr := fmt.Sprintf("%d", convResp.ID)
	clientSecretKey := "github_app_" + appIDStr + "_client_secret"
	pemKey := "github_app_" + appIDStr + "_pem"
	webhookSecretKey := "github_app_" + appIDStr + "_webhook_secret"

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.GitHubApps.CreateForOrg(r.Context(), domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              appIDStr,
			Slug:               convResp.Slug,
			ClientID:           convResp.ClientID,
			ClientSecretRef:    clientSecretKey,
			PEMRef:             pemKey,
			WebhookSecretRef:   webhookSecretKey,
			RegisteredByUserID: userID,
		}); err != nil {
			return err
		}
		if err := tx.Secrets.Put(r.Context(), orgID, clientSecretKey, convResp.ClientSecret, "GitHub App client secret"); err != nil {
			return fmt.Errorf("vault put client_secret: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, pemKey, convResp.PEM, "GitHub App private key"); err != nil {
			return fmt.Errorf("vault put pem: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, webhookSecretKey, convResp.WebhookSecret, "GitHub App webhook secret"); err != nil {
			return fmt.Errorf("vault put webhook_secret: %w", err)
		}
		return nil
	}); err != nil {
		var exists *db.ErrGitHubAppExists
		if errors.As(err, &exists) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "org already has a GitHub App registered; remove it first",
			})
			return
		}
		internalError(w, "github-app", err)
		return
	}

	log.Printf("[github-app] registered app_id=%s slug=%s for org=%s", appIDStr, convResp.Slug, orgID)

	http.Redirect(w, r, "/settings/workspace#github-app", http.StatusFound)
}

// requireOrgAdmin validates {org_id} from the URL path and checks the
// caller is both a member and an admin of that org. Returns (orgID,
// userID, true) on success; writes an error and returns ("", "", false)
// on failure.
func (s *Server) requireOrgAdmin(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	rawOrgID := r.PathValue("org_id")
	if _, err := uuid.Parse(rawOrgID); err != nil {
		http.NotFound(w, r)
		return
	}

	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	userID = claims.Subject

	if runmode.Current() == runmode.ModeLocal {
		return rawOrgID, userID, true
	}

	isAdmin, err := s.userIsOrgAdmin(r.Context(), userID, rawOrgID)
	if err != nil {
		log.Printf("[github-app] admin check %s/%s: %v", userID, rawOrgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !isAdmin {
		http.NotFound(w, r)
		return
	}
	return rawOrgID, userID, true
}

// --- GitHub API URL derivation ---

// githubAPIBase derives the REST API base from a user-facing GitHub
// URL. Mirrors internal/github.NewClient's derivation.
func githubAPIBase(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "https://github.com" {
		return "https://api.github.com"
	}
	return base + "/api/v3"
}

// --- manifest code exchange ---

type manifestConversionResponse struct {
	ID            int    `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

var manifestHTTPClient = &http.Client{Timeout: 30 * time.Second}

func exchangeManifestCode(ctx context.Context, conversionURL string) (*manifestConversionResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conversionURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := manifestHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out manifestConversionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if strings.TrimSpace(out.ClientID) == "" ||
		strings.TrimSpace(out.ClientSecret) == "" ||
		strings.TrimSpace(out.WebhookSecret) == "" ||
		strings.TrimSpace(out.PEM) == "" {
		return nil, fmt.Errorf("incomplete response from GitHub (missing client_id, client_secret, webhook_secret, or pem)")
	}
	return &out, nil
}

// --- state token (HMAC-signed, ~10min TTL) ---

type appRegisterState struct {
	OrgID     string `json:"org_id"`
	ExpiresAt int64  `json:"exp"`
}

func (s appRegisterState) sign(key [32]byte) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseAppRegisterState(raw string, key [32]byte) (*appRegisterState, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode mac: %w", err)
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return nil, errors.New("mac mismatch")
	}
	var s appRegisterState
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if timeNow().Unix() > s.ExpiresAt {
		return nil, errors.New("expired")
	}
	return &s, nil
}
