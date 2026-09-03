package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Bring-your-own-App import. The second way into App mode, for orgs
// that can't or shouldn't create the App themselves (only org owners may create
// an org App on GitHub; enterprises gate App creation behind a platform team;
// one App may be reused across deployments). The user supplies only an App ID +
// private key PEM; a GitHub App authenticates itself, so a single app-JWT
// GET /app round trip validates the pair AND derives everything the manifest
// path collects (slug, owner login + type, permissions, events, client_id).
//
// Everything downstream (resolver minting, per-installation polling, refresh,
// staging/cutover) keys off the stored org_github_apps row and works unchanged
// regardless of how the row was created — so this persists through the same path
// the manifest callback does, with the same staging rule.

// githubAppImportRequest is the import endpoint body. client_secret,
// webhook_secret, and accept_partial are optional: the client secret only
// enables per-user OAuth Connect (skipping it lands on the PAT-capture identity
// path); the webhook secret is multi-mode only (local is hookless/backfill);
// accept_partial acknowledges soft permission gaps.
type githubAppImportRequest struct {
	AppID         string `json:"app_id"`
	PEM           string `json:"pem"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	AcceptPartial bool   `json:"accept_partial"`
}

// githubAppPermissionRow is one entry in the import preflight's granted-vs-
// required table. Severity is "required" (a hard gap blocks the import) or
// "optional" (a soft gap blocks only until accept_partial acknowledges it);
// Feature names the degraded capability for an unmet optional permission.
type githubAppPermissionRow struct {
	Permission string `json:"permission"`
	Required   string `json:"required"`
	Granted    string `json:"granted"`
	Satisfied  bool   `json:"satisfied"`
	Severity   string `json:"severity"`
	Feature    string `json:"feature,omitempty"`
}

// githubAppImportErrorResponse is the 422 body for a permission gap: the
// standard error envelope, plus the full granted-vs-required table and whether
// the gap is Blocking. Blocking=true is a hard gap (a core permission is
// missing — accept_partial can't override it); Blocking=false means only soft
// gaps remain, which the frontend resubmits past with accept_partial=true after
// an acknowledgment. The extra keys ride alongside `errors` because the table is
// structured data no envelope field can hold; the message itself is in the
// envelope like every other fault, so the shared client parser reads it.
type githubAppImportErrorResponse struct {
	Errors      []httpx.ErrorItem        `json:"errors"`
	Permissions []githubAppPermissionRow `json:"permissions"`
	Blocking    bool                     `json:"blocking"`
}

// permissionGapErrors wraps a preflight message in the standard envelope.
func permissionGapErrors(msg string) []httpx.ErrorItem {
	return []httpx.ErrorItem{{Reason: httpx.ReasonPermissionGap, Message: msg}}
}

// githubAppImportResponse is the success body: the same status payload the
// status GET serves (App, installations, connect_callback_url) plus the
// preflight table and the client-secret validation outcome. ClientSecretStored
// reports whether a secret was provided; ClientSecretValidated is false when one
// was stored without GitHub confirming it (the check-token status contract was
// inconclusive) — see checkAppClientSecret.
type githubAppImportResponse struct {
	githubAppStatusResponse
	Permissions           []githubAppPermissionRow `json:"permissions"`
	ClientSecretStored    bool                     `json:"client_secret_stored"`
	ClientSecretValidated bool                     `json:"client_secret_validated"`
}

// importRequiredPermission is one permission the manifest path requests, with
// the minimum level and whether a gap is hard (blocks) or soft (degrades a named
// feature). The set mirrors buildManifestAndState's default_permissions so an
// imported App is held to the same bar a TF-created one is granted.
type importRequiredPermission struct {
	name    string
	level   string // "read" / "write"
	hard    bool
	feature string // degraded feature, for a soft gap's message
}

// importRequiredPermissions is the bar an imported App is preflighted against —
// the exact set buildManifestAndState requests (github_app_register.go).
//
//   - Hard (block): emails:read, issues:write, pull_requests:write,
//     contents:write, metadata:read — core function (identity capture, open
//     PRs, comment, push branches) breaks without them.
//   - Soft (warn): checks:read / actions:read (CI check + workflow events),
//     statuses:read (the open-PR statusCheckRollup CI query — its contexts union
//     touches the commit-statuses resource), members:read (GitHub team import +
//     team-based review-request detection) — specific features degrade, the rest
//     works.
var importRequiredPermissions = []importRequiredPermission{
	{name: "emails", level: "read", hard: true},
	{name: "issues", level: "write", hard: true},
	{name: "pull_requests", level: "write", hard: true},
	{name: "contents", level: "write", hard: true},
	{name: "metadata", level: "read", hard: true},
	{name: "checks", level: "read", feature: "CI check status events"},
	{name: "actions", level: "read", feature: "workflow run events"},
	{name: "statuses", level: "read", feature: "open-PR CI status rollup"},
	{name: "members", level: "read", feature: "GitHub team import and team-based review-request detection"},
}

// permissionLevel maps a GitHub App permission value to a comparable rank so a
// higher grant satisfies a lower requirement ("write" satisfies a "read" ask).
// An absent or unrecognized value is 0 (ungranted).
func permissionLevel(v string) int {
	switch v {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	default:
		return 0
	}
}

// preflightImportPermissions compares the App's granted permissions against the
// required set and returns the full table plus the unmet hard and soft gaps.
// "write" satisfies "read" via permissionLevel ordering, not string equality.
func preflightImportPermissions(granted map[string]string) (rows []githubAppPermissionRow, hardGaps, softGaps []githubAppPermissionRow) {
	rows = make([]githubAppPermissionRow, 0, len(importRequiredPermissions))
	for _, req := range importRequiredPermissions {
		have := granted[req.name] // "" when the App wasn't granted it at all
		satisfied := permissionLevel(have) >= permissionLevel(req.level)
		severity := "optional"
		if req.hard {
			severity = "required"
		}
		row := githubAppPermissionRow{
			Permission: req.name,
			Required:   req.level,
			Granted:    have,
			Satisfied:  satisfied,
			Severity:   severity,
			Feature:    req.feature,
		}
		rows = append(rows, row)
		if !satisfied {
			if req.hard {
				hardGaps = append(hardGaps, row)
			} else {
				softGaps = append(softGaps, row)
			}
		}
	}
	return rows, hardGaps, softGaps
}

// hardGapMessage builds the 422 message for unmet core permissions.
func hardGapMessage(gaps []githubAppPermissionRow) string {
	parts := make([]string, 0, len(gaps))
	for _, g := range gaps {
		if g.Granted == "" {
			parts = append(parts, fmt.Sprintf("%s (needs %s, not granted)", g.Permission, g.Required))
		} else {
			parts = append(parts, fmt.Sprintf("%s (needs %s, has %s)", g.Permission, g.Required, g.Granted))
		}
	}
	return "This GitHub App is missing permissions Triage Factory requires to function: " +
		strings.Join(parts, ", ") + ". Update the App's permissions on GitHub, then import again."
}

// softGapMessage builds the 422 message for unmet optional permissions, naming
// the feature each gap degrades.
func softGapMessage(gaps []githubAppPermissionRow) string {
	parts := make([]string, 0, len(gaps))
	for _, g := range gaps {
		parts = append(parts, fmt.Sprintf("%s (%s)", g.Permission, g.Feature))
	}
	return "This GitHub App is missing optional permissions that power some features: " +
		strings.Join(parts, ", ") + ". You can import it anyway — those features stay off until the App is granted them on GitHub."
}

// clientSecretCheck is the outcome of probing a client secret via GitHub's
// check-token endpoint.
type clientSecretCheck int

const (
	clientSecretValid   clientSecretCheck = iota // 404 — the credentials are good (the probe token just doesn't exist)
	clientSecretBad                              // 401 — the client credentials themselves are rejected
	clientSecretUnknown                          // anything else / network — inconclusive; store unvalidated
)

// checkAppClientSecret probes whether clientSecret is valid for clientID via
// GitHub's check-token endpoint (POST /applications/{client_id}/token, Basic
// auth client_id:client_secret, a bogus access_token in the body). GitHub's
// status contract: 401 means the client credentials are bad; 404 means they're
// good (the token simply doesn't exist). Any other status — or a transport error
// — is inconclusive, so the caller stores the secret unvalidated and notes it.
// There is no API to validate the OTHER half of OAuth (the registered callback
// URL), so a "valid" here only confirms the secret, not that Connect will work.
func checkAppClientSecret(ctx context.Context, apiBase, clientID, clientSecret string) clientSecretCheck {
	endpoint := apiBase + "/applications/" + url.PathEscape(clientID) + "/token"
	// A deliberately-bogus token: a valid client_id:client_secret pair yields 404
	// (no such token); a bad pair is rejected 401 before the token is even looked
	// up. The probe value is meaningless and never a real credential.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(`{"access_token":"triage-factory-client-secret-probe"}`))
	if err != nil {
		return clientSecretUnknown
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "triage-factory-githubapp")
	req.Header.Set("Content-Type", "application/json")

	// Reuse the package's GitHub HTTP client (a plain 30s-timeout client defined
	// in github_app_register.go) — fine for this one-shot probe.
	resp, err := manifestHTTPClient.Do(req)
	if err != nil {
		return clientSecretUnknown
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return clientSecretBad
	case http.StatusNotFound:
		return clientSecretValid
	default:
		return clientSecretUnknown
	}
}

// mapAppOwnerType folds GitHub's verbatim account type ("User" / "Organization"
// from GET /app's owner.type) into the org_github_apps.owner_type domain
// ("user" / "org"). Anything that isn't an organization is treated as a personal
// account, matching NormalizedOwnerType's default.
func mapAppOwnerType(githubType string) string {
	if strings.EqualFold(githubType, "Organization") {
		return "org"
	}
	return "user"
}

// handleGitHubAppImport validates a bring-your-own GitHub App (App ID + private
// key) via an app-JWT GET /app round trip, permission-preflights it, optionally
// validates a client secret, and persists it through the same path the manifest
// callback uses — with the same staging rule (an import while an org PAT is live
// stages active=false; a fresh setup imports active=true). Org-admin only; works
// in both local and multi mode.
//
// POST /api/orgs/{org_id}/github/app/import
func (s *Server) handleGitHubAppImport(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req githubAppImportRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	appIDStr := strings.TrimSpace(req.AppID)
	if appIDStr == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "An App ID is required.", Field: "app_id"})
		return
	}
	if strings.TrimSpace(req.PEM) == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "The App's private key (PEM) is required.", Field: "pem"})
		return
	}

	// One-slot rule, checked unlocked here as a fast bail-out for the common
	// case (resubmitting against an org that's already registered) before
	// spending a PEM parse and several GitHub API round-trips on a request
	// that's going to be rejected anyway. Non-authoritative — a concurrent
	// registration racing this read is still caught below. A staged app
	// occupies the slot too — the discard endpoint frees it. System read:
	// the admin gate already authorized orgID.
	existing, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if existing != nil {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "org already has a GitHub App registered; remove it first"})
		return
	}
	// A managed_app org has no App row and passes the gate above; the managed
	// guard is what stops it importing its own App beside live deployment-App
	// installation rows. Advisory here, like the slot check; authoritative
	// under the lock below.
	if s.refuseManagedInTheWay(w, ctx, orgID) {
		return
	}

	// Resolve the org's GitHub base URL through the resolver so the validation
	// calls below hit the right host. BaseURLFor applies the same precedence the
	// poller / git paths use — org_settings.github_base_url, then the github_url
	// secret (load-bearing for GHES / local-mode orgs whose host lives only in
	// the keychain, with the settings column empty), then public github.com —
	// and propagates a read error rather than papering a possibly-GHES org over
	// with github.com. Deriving from org_settings alone would mint the app JWT
	// against the wrong host for exactly those orgs.
	base, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	apiBase := ghbase.APIBase(base)

	// Parse the PEM (garbage → 422).
	key, err := githubapp.ParsePrivateKey([]byte(req.PEM))
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "That private key (PEM) couldn't be parsed. Paste the full contents of the App's .pem file, including the BEGIN/END lines.", Field: "pem"})
		return
	}

	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil || appID <= 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "App ID must be a positive number (the numeric App ID, not the client ID).", Field: "app_id"})
		return
	}

	// Mint an app JWT (off the submitted key, iss=appID) and GET /app. GitHub 401s
	// a JWT whose iss doesn't match the signing key, so a successful fetch proves
	// the ID+key are a valid pair and hands back the App's own metadata.
	minter, err := githubapp.NewMinter(githubapp.Config{PrivateKey: key, AppID: appID, APIBase: apiBase})
	if err != nil {
		internalError(w, "github-app", fmt.Errorf("init minter: %w", err))
		return
	}
	app, err := minter.GetApp(ctx)
	if err != nil {
		// The detail (status + GitHub body) goes to the log; the user sees the
		// dominant cause. A transient GitHub outage would also land here — rare
		// enough that the actionable message is the better default.
		githubAppLog.Error("import: get app failed", "org", orgID, "error", err)
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "App ID and private key don't match, or the key was revoked. Double-check the App ID and re-download the private key from the App's GitHub settings.", Field: "app_id"})
		return
	}

	// Verify GitHub's returned id equals the submitted one. A PEM for a *different*
	// app would otherwise import under the wrong identity — though the JWT iss
	// mechanism makes the cross-pair case 401 above, this is the cheap belt-and-
	// suspenders that also pins the canonical id we persist under.
	if app.ID != appID {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: fmt.Sprintf("That private key belongs to a different App (ID %d) than the App ID you entered (%d).", app.ID, appID), Field: "app_id"})
		return
	}

	// Permission preflight. Hard gaps always block (accept_partial can't override
	// a missing core permission); soft gaps block only until accept_partial
	// acknowledges them.
	rows, hardGaps, softGaps := preflightImportPermissions(app.Permissions)
	if len(hardGaps) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, githubAppImportErrorResponse{
			Errors:      permissionGapErrors(hardGapMessage(hardGaps)),
			Permissions: rows,
			Blocking:    true,
		})
		return
	}
	if len(softGaps) > 0 && !req.AcceptPartial {
		writeJSON(w, http.StatusUnprocessableEntity, githubAppImportErrorResponse{
			Errors:      permissionGapErrors(softGapMessage(softGaps)),
			Permissions: rows,
			Blocking:    false,
		})
		return
	}

	// Optional client-secret validation. A bad secret is rejected; an inconclusive
	// check stores it unvalidated and flags that in the response.
	clientSecret := strings.TrimSpace(req.ClientSecret)
	hasClientSecret := clientSecret != ""
	clientSecretValidated := false
	if hasClientSecret {
		switch checkAppClientSecret(ctx, apiBase, app.ClientID, clientSecret) {
		case clientSecretBad:
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "That client secret is not valid for this App. Generate a fresh client secret in the App's GitHub settings (Apps can hold two at once, so this won't disturb an existing consumer), then paste it here.", Field: "client_secret"})
			return
		case clientSecretValid:
			clientSecretValidated = true
		case clientSecretUnknown:
			// Store it anyway; the response flags client_secret_validated=false.
			githubAppLog.Warn("import: client-secret check inconclusive, storing unvalidated", "org", orgID, "app", app.ID)
		}
	}

	webhookSecret := strings.TrimSpace(req.WebhookSecret)
	hasWebhookSecret := webhookSecret != ""

	canonicalAppID := strconv.FormatInt(app.ID, 10)
	// Compose the secret-key names through integrations so the names written here
	// match the names the uninstall sweep later removes (single source of truth).
	appKeys := integrations.GitHubAppKeysFor(canonicalAppID)
	pemKey := appKeys.PEM
	clientSecretKey := appKeys.ClientSecret
	webhookSecretKey := appKeys.WebhookSecret

	clientSecretRef := ""
	if hasClientSecret {
		clientSecretRef = clientSecretKey
	}
	webhookSecretRef := ""
	if hasWebhookSecret {
		webhookSecretRef = webhookSecretKey
	}

	// Secrets actually written, for the local-mode keychain cleanup on tx failure.
	writtenKeys := []string{pemKey}
	if hasClientSecret {
		writtenKeys = append(writtenKeys, clientSecretKey)
	}
	if hasWebhookSecret {
		writtenKeys = append(writtenKeys, webhookSecretKey)
	}

	// Best-effort: resolve the bot's numeric user-account id for the numeric-id
	// noreply commit email so App-bot commits link on github.com (TFAC-474).
	// Read against the same resolved base the validation calls used; a failure
	// leaves it 0 → NULL → the plain noreply form, self-healing on re-import, and
	// never blocks the import.
	botUserID := s.fetchBotUserID(ctx, base, app.Slug, orgID)

	// Serialize per-org against the manifest callback + concurrent imports so two
	// requests can't both pass the one-slot check and race to CreateForOrg. The
	// unique constraint is the durable fallback (handled below via
	// db.ErrGitHubAppExists) — this closes the common case — across every
	// control pod in multi mode, not just this process (TFAC-579). Same lock
	// the register callback uses. Acquired here, after every GitHub API
	// round-trip above has already completed, so the held connection (in
	// multi mode) only spans the DB write below rather than the whole
	// validation sequence — a slow/rate-limited GitHub API window shouldn't
	// pin a pool connection for its duration.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	defer release()

	// The managed guard's authoritative evaluation, now that nothing can bind a
	// managed installation between it and the write. The App-row half of the
	// one-slot rule needs no re-read: CreateForOrg's primary key refuses a
	// second registration by itself (db.ErrGitHubAppExists below).
	if s.refuseManagedInTheWay(w, ctx, orgID) {
		return
	}

	// Persist exactly as the manifest callback does, including the staging rule:
	// an org PAT still live ⇒ active=false (staged, the PAT stays live until a
	// cutover); a fresh setup ⇒ active=true. integrations.Load reads the PAT
	// inside the same tx so the decision and the write are consistent.
	var staged bool
	var created domain.OrgGitHubApp
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// Same reasoning as the register callback: a failed credential read
		// must not read as "no PAT" and activate the App beside a live one.
		// The load runs before any write, so failing here leaves no App row
		// and no vaulted secrets behind.
		creds, lerr := integrations.Load(ctx, tx.Secrets, orgID)
		if lerr != nil {
			return fmt.Errorf("load org credentials: %w", lerr)
		}
		staged = creds.GitHubPAT != ""
		if staged {
			githubAppLog.Info("import: live pat present, staging app inactive until cutover", "org", orgID, "app_id", canonicalAppID)
		}
		var cerr error
		created, cerr = tx.GitHubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              canonicalAppID,
			Slug:               app.Slug,
			ClientID:           app.ClientID,
			ClientSecretRef:    clientSecretRef,
			PEMRef:             pemKey,
			WebhookSecretRef:   webhookSecretRef,
			OwnerType:          mapAppOwnerType(app.OwnerType),
			RegisteredByUserID: userID,
			Active:             !staged,
			BotUserID:          botUserID,
		})
		if cerr != nil {
			return cerr
		}
		// The org is now in the BYO-App credential system — including when the
		// App is STAGED behind a still-live PAT. Same reasoning as the manifest
		// register path: the class names which system the org is in, Active
		// names which credential is live, and this transaction is what keeps
		// the two from disagreeing.
		if _, err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassBYOApp); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		// Store the PEM verbatim (req.PEM, not a trimmed copy): a private key's
		// internal newlines are load-bearing, ParsePrivateKey tolerates the
		// surrounding whitespace, and the resolver reads it back through the same
		// parser — so the original round-trips correctly.
		if err := tx.Secrets.Put(ctx, orgID, pemKey, req.PEM, "GitHub App private key"); err != nil {
			return fmt.Errorf("vault put pem: %w", err)
		}
		if hasClientSecret {
			if err := tx.Secrets.Put(ctx, orgID, clientSecretKey, clientSecret, "GitHub App client secret"); err != nil {
				return fmt.Errorf("vault put client_secret: %w", err)
			}
		}
		if hasWebhookSecret {
			if err := tx.Secrets.Put(ctx, orgID, webhookSecretKey, webhookSecret, "GitHub App webhook secret"); err != nil {
				return fmt.Errorf("vault put webhook_secret: %w", err)
			}
		}
		// The same audit row the manifest-register path writes — importing
		// an existing App binds the identical credential, just sourced by paste
		// rather than by manifest exchange.
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, base, app.Slug),
		})
	}); err != nil {
		// Local-mode SecretStore writes hit the OS keychain outside the SQLite tx;
		// clean up any entries that landed before the error so a failed import
		// leaves no orphan credentials. Mirrors the register callback.
		if runmode.Current() == runmode.ModeLocal {
			for _, k := range writtenKeys {
				_, _ = s.secrets.Delete(ctx, orgID, k)
			}
		}
		var exists *db.ErrGitHubAppExists
		if errors.As(err, &exists) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "org already has a GitHub App registered; remove it first"})
			return
		}
		internalError(w, "github-app", err)
		return
	}

	githubAppLog.Info("imported app",
		"app_id", canonicalAppID, "slug", app.Slug, "owner_login", app.OwnerLogin, "owner_type", app.OwnerType, "org", orgID, "active", !staged)

	// An imported App brings its own webhook secret, so the receiver's cached
	// resolution for this org is out of date the moment the row lands. Same
	// reasoning as the manifest register path.
	s.invalidateWebhookSecret(orgID)

	// Post-commit hooks, same as the cutover/credentials paths: a fresh import is
	// immediately the live credential, so re-due polling + re-profile under it. A
	// staged import doesn't change the live credential (the PAT stays live), but
	// firing it is harmless — the resolver still resolves to the PAT until cutover.
	if s.onGitHubChanged != nil {
		go s.onGitHubChanged(orgID)
	}

	// Build the response from the row CreateForOrg persisted, above — no
	// follow-up read needed, the write already handed it back. No installations
	// yet (no backfill at import; the install step reconciles), so this carries
	// an empty list the wizard's install step refreshes.
	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}

	writeJSON(w, http.StatusOK, githubAppImportResponse{
		// The class the transaction above just committed for this org — passed
		// as the literal rather than re-read, since the import IS what put the
		// org in the BYO-App system.
		githubAppStatusResponse: s.githubAppStatus(ctx, orgID, userID, domain.GitHubCredentialClassBYOApp, &created, insts,
			// No webhook health: nothing has probed this App yet, and the block
			// is deliberately absent rather than optimistic. The panel's next
			// status read runs the first probe — the import form's own copy is
			// what names the cost of a blank webhook secret at this moment.
			nil),
		Permissions:           rows,
		ClientSecretStored:    hasClientSecret,
		ClientSecretValidated: clientSecretValidated,
	})
}
