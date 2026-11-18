package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The install ceremony for the deployment's Slack app: an org admin adds the
// app to a Slack workspace through Slack's OAuth v2 flow, and the bot token
// Slack returns becomes the org's connection in that workspace.
//
//	GET /api/orgs/{org_id}/slack/install/start?return_to=&workspace_id=
//	GET /api/slack/install/callback?code=&state=
//
// The callback is fixed, because the app registers one redirect URL and serves
// every org. The org and the user the ceremony belongs to travel in a signed
// state cookie; only its CSRF value passes through Slack.
//
// Every outcome short of a stored connection writes nothing and redirects back
// with slack_error set to one of:
//
//	state               the cookie is missing, expired, forged, or not this
//	                    caller's, or the caller may no longer install into its org
//	sso_required        "Require SSO" bars the caller from the org
//	denied              the person declined on Slack's page
//	no_app              the deployment has no Slack app
//	install_failed      the code exchange failed, or Slack's answer is not an
//	                    install of the deployment's app with every scope it needs
//	enterprise_install  Slack answered with an Enterprise Grid org-wide install
//	workspace_taken     another org holds the app's install in the workspace
//
// A stored connection redirects with slack=installed (it is the workspace's
// active connection) or slack=staged (it is stored inactive beside the active
// one), and slack_workspace naming the workspace Slack installed into.
//
// No path revokes the token Slack returns. A token the ceremony does not store
// may be the one another org's connection holds.

const (
	installStateCookieName = "tf_slack_install_state"
	installStatePath       = "/api/slack/install/"
	installCallbackPath    = "/api/slack/install/callback"
	installStateTTL        = 10 * time.Minute
	slackAuthorizeURL      = "https://slack.com/oauth/v2/authorize"
)

// slackWorkspaceIDPattern is the shape of a Slack workspace (team) id.
var slackWorkspaceIDPattern = regexp.MustCompile(`^T[A-Z0-9]{2,}$`)

// installCeremony is the part of server.ExtensionAPI the ceremony uses, named
// so tests can supply the state signing and the SSO answer.
type installCeremony interface {
	SignConnectState(orgID, userID, returnTo string, ttl time.Duration) (string, server.ConnectState, error)
	ParseConnectState(signed string) (server.ConnectState, error)
	CookieSecure(r *http.Request) bool
	SSOBarred(ctx context.Context, orgID string) (bool, error)
	SettingsPath(orgID string) string
	NormalizeReturnTo(raw string) string
	PublicURL() string
}

// installHandler serves the deployment app's install ceremony.
type installHandler struct {
	tx       db.TxRunner
	az       *authz.Checker
	client   *http.Client
	app      deploymentApp
	ceremony installCeremony
}

// available reports whether the ceremony's routes exist: in multi mode, behind
// an HTTPS public URL, because Slack refuses a redirect URL that is not HTTPS.
func (h *installHandler) available() bool {
	return runmode.Current() != runmode.ModeLocal && strings.HasPrefix(h.ceremony.PublicURL(), "https://")
}

func (h *installHandler) callbackURL() string {
	return h.ceremony.PublicURL() + installCallbackPath
}

func (h *installHandler) stateCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     installStateCookieName,
		Value:    value,
		Path:     installStatePath,
		HttpOnly: true,
		Secure:   h.ceremony.CookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// redirect sends the browser to returnTo, or to orgID's Settings page when
// returnTo names nowhere, with outcome on its query in place of any Slack
// outcome already there. A caller with no org lands on the root.
func (h *installHandler) redirect(w http.ResponseWriter, r *http.Request, orgID, returnTo string, outcome url.Values) {
	dest := h.ceremony.NormalizeReturnTo(returnTo)
	if dest == "/" && orgID != "" {
		dest = h.ceremony.SettingsPath(orgID)
	}
	u, err := url.Parse(dest)
	if err != nil {
		u = &url.URL{Path: "/"}
	}
	q := u.Query()
	for _, k := range []string{"slack", "slack_workspace", "slack_error"} {
		q.Del(k)
	}
	for k, v := range outcome {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *installHandler) fail(w http.ResponseWriter, r *http.Request, orgID, returnTo, code string) {
	h.redirect(w, r, orgID, returnTo, url.Values{"slack_error": {code}})
}

// handleStart sends an org admin to Slack's consent page for the deployment's
// app.
//
// GET /api/orgs/{org_id}/slack/install/start?return_to=&workspace_id=
func (h *installHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	if !h.available() {
		httpx.NotFound(w, "route")
		return
	}
	orgID, userID, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	if !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		httpx.NotFound(w, "route")
		return
	}
	// The callback refuses API tokens, so an install a token starts could
	// never finish.
	if httpx.TokenAuthFrom(r.Context()) != nil {
		httpx.Forbidden(w, "installing Slack needs a signed-in session; an API token cannot complete it")
		return
	}
	query := r.URL.Query()
	workspaceID := ""
	if vals, present := query["workspace_id"]; present {
		if len(vals) != 1 || !slackWorkspaceIDPattern.MatchString(vals[0]) {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: "workspace_id must be one Slack workspace id: T followed by capital letters and digits",
				Field:   "workspace_id",
			})
			return
		}
		workspaceID = vals[0]
	}
	returnTo := h.ceremony.NormalizeReturnTo(query.Get("return_to"))
	if !h.app.Configured() {
		h.fail(w, r, orgID, returnTo, "no_app")
		return
	}

	signed, st, err := h.ceremony.SignConnectState(orgID, userID, returnTo, installStateTTL)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	http.SetCookie(w, h.stateCookie(r, signed, int(installStateTTL/time.Second)))

	authorize := url.Values{
		"client_id":    {h.app.ClientID},
		"scope":        {strings.Join(slackBotScopes, ",")},
		"redirect_uri": {h.callbackURL()},
		"state":        {st.CSRF},
	}
	// A hint for Slack's workspace picker, which Slack does not enforce. The
	// callback acts on the workspace Slack reports.
	if workspaceID != "" {
		authorize.Set("team", workspaceID)
	}
	http.Redirect(w, r, slackAuthorizeURL+"?"+authorize.Encode(), http.StatusFound)
}

// handleCallback completes the ceremony: it proves the cookie, the caller and
// the consent, exchanges the code, checks that Slack installed the deployment's
// app with every scope it needs, and stores the connection.
//
// The caller must be the admin who started it, signed in with a session. An
// API token is refused: this path names no org for the token's sealed org to
// be checked against, and no token holder can have started the ceremony,
// which is browser state end to end.
//
// GET /api/slack/install/callback?code=&state=
func (h *installHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !h.available() {
		httpx.NotFound(w, "route")
		return
	}
	ctx := r.Context()

	// Cleared before anything else, so any visit spends the cookie, whichever
	// arm below answers.
	cookie, cookieErr := r.Cookie(installStateCookieName)
	http.SetCookie(w, h.stateCookie(r, "", -1))

	claims := httpx.ClaimsFrom(ctx)
	if claims == nil || claims.Subject == "" || httpx.TokenAuthFrom(ctx) != nil {
		httpx.WriteUnauth(w)
		return
	}
	userID := claims.Subject
	// Until the cookie is proven, its org is not trusted, so these refusals
	// land on the session's active org.
	sessionOrg := httpx.OrgIDFrom(ctx)
	if cookieErr != nil {
		h.fail(w, r, sessionOrg, "", "state")
		return
	}
	st, err := h.ceremony.ParseConnectState(cookie.Value)
	if err != nil {
		slackLog.Warn("slack install: state cookie refused", "error", err)
		h.fail(w, r, sessionOrg, "", "state")
		return
	}
	query := r.URL.Query()
	if st.CSRF == "" || query.Get("state") != st.CSRF || st.UserID != userID {
		h.fail(w, r, sessionOrg, "", "state")
		return
	}

	// From here the org is the cookie's. The role and the entitlement are read
	// again rather than trusted from the start, since minutes have passed.
	orgID, returnTo := st.OrgID, st.ReturnTo
	isAdmin, err := h.az.UserIsOrgAdmin(ctx, userID, orgID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	if !isAdmin || !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		h.fail(w, r, orgID, returnTo, "state")
		return
	}
	// The cookie's org is the one this writes into, so "Require SSO" is asked
	// about it here, rather than left to the session middleware, which judged
	// the session's active org.
	barred, err := h.ceremony.SSOBarred(ctx, orgID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	if barred {
		h.fail(w, r, orgID, returnTo, "sso_required")
		return
	}

	if slackErr := query.Get("error"); slackErr != "" {
		slackLog.Warn("slack install: Slack answered with an error", "org", orgID, "slack_error", slackErr)
		// Only access_denied is the person declining.
		code := "install_failed"
		if slackErr == "access_denied" {
			code = "denied"
		}
		h.fail(w, r, orgID, returnTo, code)
		return
	}
	code := query.Get("code")
	if code == "" {
		h.fail(w, r, orgID, returnTo, "install_failed")
		return
	}
	if !h.app.Configured() {
		h.fail(w, r, orgID, returnTo, "no_app")
		return
	}

	inst, err := slackOAuthV2Access(ctx, h.client, slackCaller{orgID: orgID}, h.app.ClientID, h.app.ClientSecret, code, h.callbackURL())
	if err != nil {
		slackLog.Warn("slack install: code exchange failed", "org", orgID, "error", err)
		h.fail(w, r, orgID, returnTo, "install_failed")
		return
	}
	if inst.AppID != h.app.AppID {
		slackLog.Error("slack install: Slack installed a different app than the deployment's; the configured client id and secret belong to another app",
			"org", orgID, "app_id", inst.AppID, "deployment_app_id", h.app.AppID)
		h.fail(w, r, orgID, returnTo, "install_failed")
		return
	}
	// One org-wide install spans many workspaces, and every connection is
	// keyed by one.
	if inst.IsEnterpriseInstall {
		slackLog.Warn("slack install: Slack answered with an Enterprise Grid org-wide install; refusing", "org", orgID, "enterprise", inst.EnterpriseID)
		h.fail(w, r, orgID, returnTo, "enterprise_install")
		return
	}
	if missing := missingBotScopes(inst.Scope); inst.TokenType != "bot" || len(missing) > 0 {
		slackLog.Error("slack install: the install lacks a bot token with every scope the deployment app needs",
			"org", orgID, "token_type", inst.TokenType, "missing_scopes", strings.Join(missing, ","))
		h.fail(w, r, orgID, returnTo, "install_failed")
		return
	}
	if inst.TeamID == "" {
		slackLog.Error("slack install: Slack answered a workspace install with no workspace", "org", orgID)
		h.fail(w, r, orgID, returnTo, "install_failed")
		return
	}

	conn, err := h.store(ctx, orgID, userID, inst)
	if errors.Is(err, errWorkspaceTaken) {
		slackLog.Warn("slack install: another org holds the deployment app's install in this workspace; refusing",
			"org", orgID, "workspace", inst.TeamID)
		h.fail(w, r, orgID, returnTo, "workspace_taken")
		return
	}
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	notifyConfigChanged(orgID)
	slackLog.Info("slack install: stored the deployment app's connection",
		"org", orgID, "user", userID, "workspace", conn.WorkspaceID, "app", conn.APIAppID, "active", conn.Active)

	result := "staged"
	if conn.Active {
		result = "installed"
	}
	h.redirect(w, r, orgID, returnTo, url.Values{"slack": {result}, "slack_workspace": {conn.WorkspaceID}})
}

// missingBotScopes returns the entries of slackBotScopes that granted, Slack's
// comma-separated scope list, lacks.
func missingBotScopes(granted string) []string {
	have := map[string]bool{}
	for _, s := range strings.Split(granted, ",") {
		have[strings.TrimSpace(s)] = true
	}
	var missing []string
	for _, s := range slackBotScopes {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return missing
}

// errWorkspaceTaken is store's refusal of a workspace where another org holds
// the deployment app's install.
var errWorkspaceTaken = errors.New("slack: another org holds the deployment app's install in this workspace")

// store writes inst as orgID's connection in its workspace and returns the row
// it persisted: a new connection, active unless the workspace already has an
// active one, or a refresh of the org's existing connection of the app, whose
// active flag it keeps. A workspace where another org holds the install is
// errWorkspaceTaken, and nothing is written.
func (h *installHandler) store(ctx context.Context, orgID, userID string, inst *oauthV2AccessResult) (slackstore.Connection, error) {
	keys := integrations.SlackConnectionKeysFor(inst.TeamID, inst.AppID)
	base := slackstore.Connection{
		WorkspaceID:        inst.TeamID,
		APIAppID:           inst.AppID,
		OrgID:              orgID,
		Source:             slackstore.SourceDeployment,
		WorkspaceName:      inst.TeamName,
		AppName:            h.botName(ctx, orgID, inst.AccessToken),
		EnterpriseID:       inst.EnterpriseID,
		BotUserID:          inst.BotUserID,
		BotTokenRef:        keys.BotToken,
		RegisteredByUserID: userID,
	}

	var persisted slackstore.Connection
	write := func(tx db.TxStores) error {
		conns := slackstore.FromTx(tx).Connections
		row := base
		// Every install of the app serializes here. The two reads below run
		// on the admin pool, outside this transaction, and see every install
		// committed before the lock was granted.
		if err := conns.LockApp(ctx, inst.AppID); err != nil {
			return err
		}
		found, err := conns.GetByWorkspaceAppSystem(ctx, inst.TeamID, inst.AppID)
		if err != nil {
			return err
		}
		switch {
		case found != nil && found.OrgID != orgID:
			return errWorkspaceTaken
		case found != nil:
			row.RegisteredByUserID = found.RegisteredByUserID
		default:
			active, err := conns.ActiveForWorkspaceSystem(ctx, orgID, inst.TeamID)
			if err != nil {
				return err
			}
			// Beside an active connection the new one is inactive: the first
			// step of a switch, which nothing reads until it is activated.
			row.Active = active == nil
		}
		if err := tx.Secrets.Put(ctx, orgID, keys.BotToken, inst.AccessToken, "Slack bot token"); err != nil {
			return fmt.Errorf("put bot token: %w", err)
		}
		if persisted, err = conns.Upsert(ctx, row); err != nil {
			return err
		}
		if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  domain.AccessDetailSlackWorkspace(inst.TeamName, inst.TeamID, inst.AppID),
		}); err != nil {
			return fmt.Errorf("audit slack install: %w", err)
		}
		return nil
	}

	err := h.tx.WithTx(ctx, orgID, userID, write)
	if errors.Is(err, slackstore.ErrActiveConnectionExists) {
		// Another app became the workspace's active connection after the
		// read above. Its commit is visible now, so a second pass stores this
		// one inactive.
		err = h.tx.WithTx(ctx, orgID, userID, write)
	}
	return persisted, err
}

// botName is the name of the bot token's bot, for the connection's app_name.
// Slack has completed the install by now, so a failure stores no name rather
// than failing the install.
func (h *installHandler) botName(ctx context.Context, orgID, botToken string) string {
	caller := slackCaller{orgID: orgID}
	at, err := slackAuthTest(ctx, h.client, caller, botToken)
	if err != nil {
		slackLog.Warn("slack install: auth.test with the new bot token failed; storing no app name", "org", orgID, "error", err)
		return ""
	}
	info, err := slackBotsInfo(ctx, h.client, caller, botToken, at.BotID)
	if err != nil {
		slackLog.Warn("slack install: bots.info with the new bot token failed; storing no app name", "org", orgID, "error", err)
		return ""
	}
	return info.Name
}
