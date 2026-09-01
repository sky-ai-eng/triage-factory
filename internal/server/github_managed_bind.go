package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/githubbind"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The bind ceremony — the only thing that maps a workspace to an installation
// of the DEPLOYMENT App, the one App key that serves many workspaces.
//
// Everything else GitHub-shaped in this codebase gets that mapping for free:
// each org owns its own App key, so "which installations exist?" is answered by
// the credential and GitHub enforces the boundary. The moment one key serves
// every workspace, GET /app/installations returns everyone's installations and
// the question stops having a free answer. This file is where TF answers it,
// and everything downstream — the scoped reconcile, webhook routing — is
// defined in terms of what it writes.
//
// GitHub's redirect cannot be the source. It is an unsigned GET, installation
// ids are sequential, and GitHub's own documentation says so:
//
//	Bad actors can hit this URL with a spoofed installation_id. Therefore, you
//	should not rely on the validity of the installation_id parameter.
//
// The threat that shapes the whole file is PLANTING, not theft. A binding
// always lands in whoever's TF session is active, so an attacker cannot steal
// one — but they can complete an install themselves, obtain a valid code +
// installation_id, and induce a signed-in TF admin to load this callback with
// those parameters, putting THEIR repositories into the victim's workspace
// where the victim's agents, spend and surrounding credentials then operate on
// attacker-authored content. Confused deputy, and the reason for the ceremony.
//
// Four proofs have to line up before anything is written, and each is
// independent of the others:
//
//  1. the pending-bind cookie + record, which prove the callback belongs to a
//     bind THIS workspace's admin started (github_pending_binds);
//  2. association — GitHub's own prescribed check, that the installation is one
//     the authorizing user can see (githubbind.Associated);
//  3. authority — that the same user administers the account the installation
//     targets (githubbind.Authority), which is the half GitHub does NOT
//     prescribe and which a read-only contractor would otherwise walk straight
//     through;
//  4. uniqueness — that no other workspace already holds this installation.
//
// And one rule over all of them: EVERY NON-DEFINITIVE OUTCOME REFUSES. There is
// no "couldn't determine, proceed" arm anywhere in this file.

// Managed-bind cookie and route constants.
const (
	// managedBindCookieName carries the ceremony's nonce. The record holds only
	// its hash, so neither half alone completes a bind.
	managedBindCookieName = "tf_gh_bind"

	// ManagedBindCallbackPath is the path GitHub returns the installer to, and
	// it deliberately carries NO org id.
	//
	// A GitHub App has ONE registered callback URL list, fixed at registration.
	// The deployment App is hand-registered once for the whole deployment, so
	// there is no per-org URL to register and the org cannot come from the
	// path. It comes from the pending-bind record instead, which is the same
	// reason that record carries an org id at all.
	//
	// Exported because an operator has to type this path into the App's
	// registration for the ceremony to work at all.
	ManagedBindCallbackPath = "/api/github/managed/callback"
)

// managedBindNonceBytes is the nonce's entropy. 32 bytes is the same order as
// a session id, and the nonce is a bearer capability of the same kind: whoever
// holds it can spend the record it names.
const managedBindNonceBytes = 32

// bindRefusal is one outcome of the ceremony that is not a completed bind.
//
// The set below is CLOSED, and that is a property worth protecting: each arm
// says what happened and what the reader can do about it, none of them
// discloses anything about another workspace, and none of them is a catch-all
// that would let a new failure arrive wearing somebody else's explanation. A
// new failure mode gets a new member here.
//
// status is what a machine caller sees; the page a browser gets carries the
// message. Both are needed because this route is reached by a top-level
// navigation from github.com — a JSON body would dead-end the tab — while the
// status is still the honest answer for anything that reads it.
type bindRefusal struct {
	code    string
	status  int
	message string
	// heading overrides the page's title for an outcome that is not a failure
	// of the caller's. Empty means the default, which says the connection
	// didn't happen.
	heading string
}

// The closed set. Ordered as the ceremony meets them.
var (
	// The deployment has no usable shared App, so there is nothing for this
	// workspace to bind to. A statement about the DEPLOYMENT, not the
	// workspace: the workspace did nothing wrong and has nothing to fix.
	refuseNoDeploymentApp = bindRefusal{
		code:   "deployment_app_unavailable",
		status: http.StatusServiceUnavailable,
		message: "This deployment isn't set up to connect GitHub accounts. " +
			"Ask your operator to configure the deployment's GitHub App.",
	}

	// The operator's App is missing "Request user authorization (OAuth) during
	// installation". GET /app does not report that checkbox, so it cannot be
	// asserted in the preflight; the callback arriving with an installation_id
	// and no code IS the symptom, and naming it turns an operator mistake into
	// one clear message instead of a mysterious dead end.
	refuseNoOAuthSetting = bindRefusal{
		code:   "missing_oauth_setting",
		status: http.StatusBadGateway,
		message: "The deployment's GitHub App is missing the “Request user authorization (OAuth) during installation” setting. " +
			"Ask your operator to enable it on the App registration, then connect again.",
	}

	// No record, expired, already consumed, or a session that isn't the one
	// that started it. One message for all four: the caller's next step is
	// identical, and distinguishing them would offer a way to probe which
	// nonces once existed.
	refuseStaleCeremony = bindRefusal{
		code:   "link_expired",
		status: http.StatusBadRequest,
		message: "That connection didn't finish in time, or was already used. " +
			"Start again from Workspace Settings.",
	}

	// GitHub returned no installation to connect. Distinct from the stale
	// ceremony above because the ceremony was fine and the return leg was not:
	// telling someone their link expired when it did not sends them looking for
	// the wrong problem.
	refuseNoInstallation = bindRefusal{
		code:   "no_installation",
		status: http.StatusBadRequest,
		message: "GitHub didn't say which installation to connect. " +
			"Start again from Workspace Settings.",
	}

	// An API token tried to complete a ceremony. See the gate for why this
	// leg takes a session specifically.
	refuseSessionRequired = bindRefusal{
		code:   "session_required",
		status: http.StatusUnauthorized,
		message: "Finish connecting GitHub from the browser you started in, signed in to Triage Factory. " +
			"An API token can't complete this step.",
	}

	// The TF-side authorization re-check at write time. The record was written
	// by an admin; the role is read again here because minutes have passed.
	refuseNotWorkspaceAdmin = bindRefusal{
		code:    "not_workspace_admin",
		status:  http.StatusForbidden,
		message: "You need to be an admin of this workspace to connect a GitHub account.",
	}

	// GitHub did not confirm who the caller is: the code exchange failed, or
	// the whoami did.
	refuseIdentityUnproven = bindRefusal{
		code:    "identity_unproven",
		status:  http.StatusBadGateway,
		message: "GitHub couldn't confirm who you are. Try connecting again.",
	}

	// The App could not read its own installation — including the 404 that a
	// spoofed installation_id produces.
	refuseInstallationUnreadable = bindRefusal{
		code:    "installation_unreadable",
		status:  http.StatusBadGateway,
		message: "GitHub didn't return that installation. Try connecting again.",
	}

	// The association gate's definitive no.
	refuseNotAssociated = bindRefusal{
		code:    "not_your_installation",
		status:  http.StatusForbidden,
		message: "That installation isn't one you can access.",
	}

	// The authority gate's definitive no. Names the account, which the caller
	// just chose and already knows.
	refuseNotAccountAdmin = bindRefusal{
		code:    "not_account_admin",
		status:  http.StatusForbidden,
		message: "You need to be an admin of %s on GitHub to connect it.",
	}

	// Either gate, undetermined. Distinct from the two noes above because the
	// remedy differs — retry, rather than go and get permissions — and because
	// telling someone they lack a role GitHub never reported would be a claim
	// TF did not establish.
	refuseGatesUndetermined = bindRefusal{
		code:    "verification_failed",
		status:  http.StatusBadGateway,
		message: "GitHub couldn't confirm your access to that account. Try connecting again in a moment.",
	}

	// Uniqueness. Never names the other workspace.
	refuseBoundElsewhere = bindRefusal{
		code:    "bound_elsewhere",
		status:  http.StatusConflict,
		message: "That installation is already connected to another workspace.",
	}

	// One credential slot per workspace. Two arms, because the credential in
	// the way decides what the admin has to go and disconnect.
	refuseOwnAppInTheWay = bindRefusal{
		code:   "credential_app_in_use",
		status: http.StatusConflict,
		message: "This workspace already has its own GitHub App. " +
			"Disconnect it in Workspace Settings before connecting a managed installation.",
	}
	refusePATInTheWay = bindRefusal{
		code:   "credential_pat_in_use",
		status: http.StatusConflict,
		message: "This workspace already has a GitHub personal access token. " +
			"Disconnect it in Workspace Settings before connecting a managed installation.",
	}

	// Not a failure of ours: GitHub sent the install to an owner for approval
	// rather than installing it, so there is no installation to bind yet.
	refuseInstallPending = bindRefusal{
		code:    "install_requested",
		status:  http.StatusAccepted,
		heading: "Install request sent",
		message: "GitHub sent your install request to an owner of that account. " +
			"Once they approve it, connect again from Workspace Settings.",
	}
)

// withAccount fills the one refusal whose message names the GitHub account.
func (b bindRefusal) withAccount(login string) bindRefusal {
	if strings.Contains(b.message, "%s") {
		b.message = fmt.Sprintf(b.message, login)
	}
	return b
}

// handleGitHubManagedConnect starts the ceremony: mint a nonce, write the
// pending-bind record, set the cookie, and send the admin to GitHub's install
// page for the deployment App.
//
// The redirect target is the deployment App's PUBLIC install page rather than
// anything carrying state. GitHub's /installations/select_target does reportedly
// preserve a state parameter, but that behaviour is undocumented, and the cookie
// is needed regardless — one owned mechanism beats two where the cheaper one is
// unowned.
//
// GET /api/orgs/{org_id}/github/managed/connect
func (s *Server) handleGitHubManagedConnect(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil || runmode.Current() != runmode.ModeMulti {
		// The deployment App is a multi-mode credential — a distributed local
		// binary ships no shared key — so in local mode this route does not
		// exist, and a route that doesn't exist answers 404.
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	ghWeb, identity, refusal, err := s.deploymentAppIdentity(r.Context(), orgID)
	if err != nil {
		internalError(w, "github-managed-bind", err)
		return
	}
	if refusal != nil {
		s.renderBindOutcome(w, orgID, *refusal)
		return
	}
	if identity.Slug == "" {
		// An App GitHub reports with no slug has no install page to send
		// anyone to.
		s.renderBindOutcome(w, orgID, refuseNoDeploymentApp)
		return
	}

	// One credential slot, stated twice — once here, once authoritatively
	// inside the lock at write time. This evaluation is ADVISORY and its only
	// job is to fail fast: without it an admin whose workspace already holds a
	// PAT would be sent to GitHub, complete a real installation, and only then
	// be told it cannot be connected. Both evaluations owe the caller the same
	// sentence; which one they hit is a matter of when they arrived.
	if refusal, err := s.credentialSlotRefusal(r.Context(), orgID, userID); err != nil {
		internalError(w, "github-managed-bind", err)
		return
	} else if refusal != nil {
		s.renderBindOutcome(w, orgID, *refusal)
		return
	}

	// The nonce goes to the browser and its hash to the database, so a database
	// read yields nothing that can complete a bind.
	nonce := make([]byte, managedBindNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		internalError(w, "github-managed-bind", err)
		return
	}
	raw := hex.EncodeToString(nonce)
	now := timeNow().UTC()
	if _, err := s.githubPendingBinds.CreateSystem(r.Context(), domain.GitHubPendingBind{
		NonceHash: hashBindNonce(raw),
		OrgID:     orgID,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(db.GitHubPendingBindTTL),
	}); err != nil {
		internalError(w, "github-managed-bind", err)
		return
	}

	http.SetCookie(w, s.managedBindCookie(r, raw, int(db.GitHubPendingBindTTL.Seconds())))

	// The deployment App's install page. url.PathEscape on the slug because it
	// is GitHub's answer to GET /app rather than a constant of ours.
	target := ghWeb + "/apps/" + url.PathEscape(identity.Slug) + "/installations/new"
	http.Redirect(w, r, target, http.StatusFound)
}

// managedBindCallback is the front door for the return leg, and the reason that
// leg is not simply mounted through s.api.
//
// Two callers arrive at this one URL and they need opposite treatment. Someone
// finishing a ceremony this deployment started carries the bind cookie, and
// what happens next WRITES a credential — so it must be authenticated, and
// withSession's 401 is exactly right for it. Someone who installed the
// deployment App straight from its public page on GitHub carries no cookie and,
// in the ordinary case, no Triage Factory session at all: GitHub's install page
// is reachable by anyone the account's owner points at it, with no reason ever
// to have visited TF in that browser. Behind withSession that person gets a
// JSON 401, which dead-ends a top-level navigation and says nothing true about
// what happened.
//
// So the cookie decides which door, before any session lookup. The pre-auth
// branch reads nothing, writes nothing, and resolves no identity — it renders
// one fixed page — which is also why it is not IP-rate-limited: there is no
// work behind it to flood, and a 429 on a shared NAT would break the flow of
// the one person it is for. Everything past it stays behind withSession.
//
// GET /api/github/managed/callback?code=…&installation_id=…&setup_action=…
func (s *Server) managedBindCallback() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deployCfg == nil || runmode.Current() != runmode.ModeMulti {
			// The deployment App is a multi-mode credential, so in local mode
			// this route does not exist — and a route that doesn't exist in
			// this deployment mode answers like one that doesn't exist at all.
			notFound(w, "route")
			return
		}

		cookie, err := r.Cookie(managedBindCookieName)
		if err != nil || cookie.Value == "" {
			// No cookie is not an error. It is the GitHub-initiated install:
			// the installation exists and belongs to no workspace, which is an
			// ordinary state rather than an anomaly.
			//
			// TODO(TFAC-931): make the unbound installation recoverable from
			// here rather than only explaining itself — this page can say what
			// happened but cannot yet offer the workspace picker that finishes
			// the job, which is the half that needs a session anyway.
			s.renderBindUnbound(w)
			return
		}

		// The nonce travels as an argument rather than being re-read past the
		// session middleware, so the completing half has no "what if there is
		// no cookie" arm to reason about: by construction there is one.
		nonce := cookie.Value
		s.withSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.completeManagedBindCallback(w, r, nonce)
		})).ServeHTTP(w, r)
	})
}

// completeManagedBindCallback is the authenticated half: a ceremony this
// deployment started is coming back. See the file comment for the four proofs
// and the rule they share.
func (s *Server) completeManagedBindCallback(w http.ResponseWriter, r *http.Request, nonce string) {
	// Cleared before anything else, so a stale cookie can never be replayed —
	// including down every path that refuses below.
	http.SetCookie(w, s.managedBindCookie(r, "", -1))

	// A SESSION, specifically, and this is the substitute for a check this
	// route structurally cannot make.
	//
	// withSession treats a Bearer API token as the cookie's peer, not its
	// fallback: any Authorization header sends the request down that branch and
	// it is decided there, so claims.Subject below would be the TOKEN's owner.
	// Every other org-admin route defends the token's sealed org with
	// tokenScopeAllows against the {org_id} in its path — and this route has no
	// org in its path to check, by construction. Its org comes from a record the
	// caller does not name. Without this gate a user who administers two
	// workspaces could start a ceremony for one in a browser and complete it on
	// a request that also carried a token sealed to the OTHER, binding a
	// credential outside the scope that token was minted for.
	//
	// Requiring a session is the honest fix rather than re-deriving the scope
	// check, because every proof this leg consumes is browser state: the cookie
	// is set on a navigation and read on a navigation, and the `code` reaches us
	// only because GitHub redirected a browser. A token holder cannot have
	// started this ceremony, so there is nothing for one to legitimately finish.
	// Same shape as invite-accept and org-create, which read the session for the
	// same reason.
	//
	// Checked BEFORE the record is consumed: nothing has been proven yet, so a
	// stray header on a proxied request must not spend the admin's ceremony.
	if SessionFrom(r.Context()) == nil {
		s.renderBindOutcome(w, "", refuseSessionRequired)
		return
	}

	record, err := s.githubPendingBinds.ConsumeSystem(r.Context(), hashBindNonce(nonce), timeNow().UTC())
	if err != nil {
		internalError(w, "github-managed-bind", err)
		return
	}
	if record == nil {
		// Absent, expired, or already spent — the store does not distinguish
		// them and neither does the answer.
		s.renderBindOutcome(w, "", refuseStaleCeremony)
		return
	}

	// From here the org is the RECORD's, never a caller-supplied value. The
	// session must be the admin who started this ceremony: the cookie proves
	// the browser, this proves the person, and re-reading the role proves they
	// still hold it now rather than when they clicked.
	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil || claims.Subject != record.UserID {
		// The one refusal below that renders with NO org, and the difference is
		// the point. Every other arm here is answering the person who started
		// the ceremony, so a back-link into their own workspace's settings is
		// both useful and something they already know. This viewer is a
		// different signed-in user holding a colleague's still-live cookie — a
		// shared machine — and has proven no relationship to the record's
		// workspace at all. Naming it in a back-link would hand out an org id
		// they never asked for, which is the disclosure the bound-elsewhere
		// refusal is careful to avoid two arms down.
		s.renderBindOutcome(w, "", refuseStaleCeremony)
		return
	}
	userID := claims.Subject
	orgID := record.OrgID
	isAdmin, err := s.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "github-managed-bind", err)
		return
	}
	if !isAdmin {
		s.renderBindOutcome(w, orgID, refuseNotWorkspaceAdmin)
		return
	}

	refusal, err := s.completeManagedBind(r, orgID, userID)
	if err != nil {
		internalError(w, "github-managed-bind", err)
		return
	}
	if refusal != nil {
		githubAppLog.Warn("managed bind refused", "org", orgID, "user", userID, "reason", refusal.code)
		s.renderBindOutcome(w, orgID, *refusal)
		return
	}

	http.Redirect(w, r, settingsRedirectPath(orgID)+"#github-app", http.StatusFound)
}

// completeManagedBind runs the ceremony's proofs and, if every one of them
// holds, writes the binding.
//
// Its two returns are exclusive and neither is a completed bind: a refusal is
// the ceremony's own verdict, an error is TF's fault. Nothing here writes to the
// response, so a fault cannot leave a half-written answer behind a redirect.
//
// No secret, token or code is logged on any path through here, including the
// refusals.
func (s *Server) completeManagedBind(r *http.Request, orgID, userID string) (*bindRefusal, error) {
	ctx := r.Context()

	// GitHub reports setup_action=request when the installer lacked the right
	// to install and GitHub asked an owner instead. There is no installation
	// yet, so there is nothing to bind and nothing has gone wrong.
	if r.URL.Query().Get("setup_action") == "request" {
		return &refuseInstallPending, nil
	}

	installationID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("installation_id")), 10, 64)
	if err != nil || installationID <= 0 {
		return &refuseNoInstallation, nil
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		// The symptom of the missing registration checkbox. With
		// "Request user authorization (OAuth) during installation" enabled,
		// code and installation_id arrive together at this one callback; with
		// it off, GitHub sends the installation_id alone and the ceremony has
		// no way to prove anything about the person.
		return &refuseNoOAuthSetting, nil
	}

	ghWeb, identity, refusal, err := s.deploymentAppIdentity(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return refusal, nil
	}

	// The user access token is PROOF, not a credential: it authenticates the
	// two gates below and is then discarded. Nothing persists it and nothing
	// reuses it — which is also why the exchange happens here rather than
	// through any of the credential plumbing.
	token, err := auth.ExchangeGitHubOAuthCode(ctx, ghWeb, identity.ClientID, s.deploymentApp.ClientSecret, code, s.deployCfg.publicURL+ManagedBindCallbackPath)
	if err != nil {
		githubAppLog.Warn("managed bind: user token exchange failed", "org", orgID, "error", err)
		return &refuseIdentityUnproven, nil
	}
	ghUser, err := auth.ValidateGitHub(ctx, ghWeb, token)
	if err != nil || ghUser == nil || ghUser.Login == "" {
		githubAppLog.Warn("managed bind: whoami failed", "org", orgID, "error", err)
		return &refuseIdentityUnproven, nil
	}

	// The installation as the APP reports it. The association read below could
	// answer the same questions and is deliberately not asked to: it is a gate,
	// never a source, so the facts that get persisted come from here.
	minter, err := s.deploymentApp.Minter(ghbase.APIBase(ghWeb))
	if err != nil {
		return &refuseNoDeploymentApp, nil
	}
	inst, err := minter.GetInstallation(ctx, installationID)
	if err != nil {
		// Includes the 404 a spoofed installation_id produces.
		githubAppLog.Warn("managed bind: installation read failed", "org", orgID, "error", err)
		return &refuseInstallationUnreadable, nil
	}
	if inst.ID != installationID {
		// GitHub answered about a different installation than the one the gates
		// are about to be asked about. Nothing downstream may proceed on two
		// ids, and there is no arm here that picks one.
		githubAppLog.Warn("managed bind: installation read answered for another installation", "org", orgID)
		return &refuseInstallationUnreadable, nil
	}

	// Gate 1 — association. GitHub's own prescribed check.
	if err := githubbind.Associated(ctx, ghWeb, token, installationID); err != nil {
		return bindGateRefusal(err, refuseNotAssociated, inst.AccountLogin), nil
	}
	// Gate 2 — authority. The half GitHub does not prescribe, and the one a
	// read-only contractor inside somebody else's installation would otherwise
	// walk straight through.
	if err := githubbind.Authority(ctx, ghWeb, token,
		githubbind.Account{Type: inst.AccountType, Login: inst.AccountLogin, ID: inst.AccountID},
		githubbind.Actor{Login: ghUser.Login, ID: ghUser.ID},
	); err != nil {
		return bindGateRefusal(err, refuseNotAccountAdmin, inst.AccountLogin), nil
	}

	return s.writeManagedBinding(ctx, orgID, userID, ghWeb, inst)
}

// bindGateRefusal maps a gate's error onto the closed refusal set. A definitive
// no gets the gate's own copy; anything undetermined gets the retry copy,
// because reporting a verdict GitHub never gave would be a claim TF did not
// establish. Both refuse.
func bindGateRefusal(err error, definitive bindRefusal, accountLogin string) *bindRefusal {
	if errors.Is(err, githubbind.ErrNotAssociated) || errors.Is(err, githubbind.ErrNotAdmin) {
		out := definitive.withAccount(accountLogin)
		return &out
	}
	githubAppLog.Warn("managed bind: gate undetermined", "account", accountLogin, "error", err)
	return &refuseGatesUndetermined
}

// writeManagedBinding is the ceremony's only write: the installation row, and
// the org's credential class set to managed_app.
//
// It runs under the App-registration advisory lock, keyed by org — the same
// lock the PAT bind/unbind, the manifest callback, the import and the two
// either/or transitions take. One credential slot per workspace means one
// transition at a time, deployment-wide, and the checks below are only worth
// anything if nothing can commit a different credential between them and the
// write.
//
// The class flips on the first bind and subsequent binds are additive: a
// workspace may hold several installations, one per GitHub account, and each
// one runs this whole ceremony.
func (s *Server) writeManagedBinding(ctx context.Context, orgID, userID, ghWeb string, inst githubapp.Installation) (*bindRefusal, error) {
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		return nil, err
	}
	defer release()

	installationID := strconv.FormatInt(inst.ID, 10)
	host := db.EffectiveGitHubHost(ghWeb)

	// A SECOND lock, keyed by the installation rather than by the workspace,
	// and it is what makes the uniqueness check below mean anything.
	//
	// The org lock above serializes this workspace's credential transitions
	// against each other. Uniqueness is not that question: it asks whether
	// ANOTHER workspace holds this installation, and two workspaces racing to
	// claim one hold two DIFFERENT org keys — so under the org lock alone both
	// read owner == "" and both write, landing one installation in two tenants.
	// The lock has to be keyed by the thing being claimed.
	//
	// The lock is not the only enforcement, and it is still the one that
	// matters here: org_github_app_installations carries a UNIQUE
	// (github_host, installation_id) index over live rows, so a claim path that
	// skips this lock is refused by the database rather than admitted. What the
	// lock buys is the DIFFERENCE between a refusal and a constraint violation —
	// the loser of a race reads the winner's row and returns the refusal below,
	// instead of taking a write error out of a transaction and rendering it as
	// an internal failure to a user who did nothing wrong.
	//
	// The key is the id then the host, joined by a space. Order matters: the id
	// is decimal digits and can hold no space, so the first space is
	// unambiguously the separator and no two distinct pairs can spell one key.
	// (The obvious NUL separator is not available — Postgres text cannot carry
	// one, and the lock key is a bound text parameter.)
	//
	// Taken while holding the org lock and never the other way round — see the
	// salt's registry entry for why that ordering is deadlock-free.
	instRelease, err := s.acquireKeyedLock(ctx, &s.githubInstallationBindMu,
		githubInstallationBindLockSalt, installationID+" "+host)
	if err != nil {
		return nil, err
	}
	defer instRelease()

	// An installation another workspace holds must not be bound here;
	// re-binding one this workspace already holds is idempotent rather than an
	// error, which is what makes a retry after a browser back-button harmless.
	owner, err := s.githubApps.InstallationOwnerSystem(ctx, host, installationID)
	if err != nil {
		return nil, err
	}
	if owner != "" && owner != orgID {
		// The refusal deliberately says nothing about which workspace: the
		// caller has no business learning that another one exists.
		return &refuseBoundElsewhere, nil
	}

	// One credential slot, evaluated authoritatively now that the lock is held:
	// whatever the Connect click's advisory check saw, this is the read the
	// write is allowed to trust.
	slotRefusal, err := s.credentialSlotRefusal(ctx, orgID, userID)
	if err != nil {
		return nil, err
	}
	if slotRefusal != nil {
		return slotRefusal, nil
	}

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if _, lerr := tx.GitHubApps.UpsertInstallation(ctx, domain.OrgGitHubAppInstallation{
			InstallationID:      installationID,
			OrgID:               orgID,
			AccountType:         inst.AccountType,
			AccountID:           strconv.FormatInt(inst.AccountID, 10),
			AccountLogin:        inst.AccountLogin,
			GitHubHost:          host,
			InstalledAt:         inst.CreatedAt,
			SuspendedAt:         inst.SuspendedAt,
			SuspendedBy:         inst.SuspendedBy,
			RepositorySelection: inst.RepositorySelection,
		}); lerr != nil {
			return lerr
		}
		// Written in the same transaction as the installation for the same
		// reason the BYO registration writes it beside the App row: the class
		// and the rows it describes can never be allowed to disagree.
		if _, lerr := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassManagedApp); lerr != nil {
			return fmt.Errorf("set github credential class: %w", lerr)
		}
		// Binding an installation is what gives this workspace's agents access
		// to an account's repositories, so it belongs in the change log beside
		// the credential binds — recorded as the App credential it is, named by
		// the account it reaches.
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, ghWeb, inst.AccountLogin),
		})
	}); err != nil {
		return nil, err
	}

	githubAppLog.Info("bound managed installation",
		"org", orgID, "installation", installationID, "account", inst.AccountLogin,
		"account_type", inst.AccountType, "host", host)

	// The org's reachable-repo mirror is now answerable and empty. Kick the
	// refresh so the repository picker has something to show rather than
	// waiting for the next poll cycle.
	s.kickReachRefresh(orgID, true)
	return nil, nil
}

// credentialSlotRefusal answers the one-credential-slot rule: a workspace that
// already holds its own App registration or a live PAT cannot bind a managed
// installation until it disconnects, and the refusal names which credential is
// in the way.
//
// It is read twice per ceremony — advisory at the Connect click so nobody is
// sent to GitHub to complete an install that cannot land, authoritative inside
// the write lock — so it lives in one place rather than being spelled out at
// both.
func (s *Server) credentialSlotRefusal(ctx context.Context, orgID, userID string) (*bindRefusal, error) {
	var (
		existingApp *domain.OrgGitHubApp
		pat         string
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var lerr error
		if existingApp, lerr = tx.GitHubApps.GetForOrg(ctx, orgID); lerr != nil {
			return lerr
		}
		loaded, lerr := integrations.Load(ctx, tx.Secrets, orgID)
		if lerr != nil {
			// A failed read cannot be treated as "no PAT": that would bind a
			// managed installation beside a live token and break the one-slot
			// invariant this check exists to hold.
			return fmt.Errorf("load org credentials: %w", lerr)
		}
		pat = loaded.GitHubPAT
		return nil
	}); err != nil {
		return nil, err
	}
	if existingApp != nil {
		return &refuseOwnAppInTheWay, nil
	}
	if pat != "" {
		return &refusePATInTheWay, nil
	}
	return nil, nil
}

// deploymentAppIdentity resolves the org's GitHub host and the deployment App's
// preflight-established identity — its slug and client id, which the operator
// never configures because GET /app reports them.
//
// The preflight is also the members-permission gate, and running it here rather
// than trusting a cached verdict is deliberate: `members: read` is what the
// authority gate reads, so an App that has lost it must not be able to start a
// ceremony whose second gate cannot answer. One extra GET /app per bind
// attempt, on a human-paced flow.
//
// The three returns are exclusive: a non-nil error is TF's own fault (render a
// 500), a non-nil refusal is the deployment's, and neither means the identity
// is usable.
func (s *Server) deploymentAppIdentity(ctx context.Context, orgID string) (ghWeb string, identity githubapp.DeploymentAppIdentity, refusal *bindRefusal, err error) {
	if !s.deploymentApp.Configured() {
		return "", identity, &refuseNoDeploymentApp, nil
	}
	ghWeb, err = s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		return "", identity, nil, fmt.Errorf("resolve github base for org %s: %w", orgID, err)
	}
	minter, merr := s.deploymentApp.Minter(ghbase.APIBase(ghWeb))
	if merr != nil {
		githubAppLog.Warn("managed bind: deployment app minter unavailable", "org", orgID, "error", merr)
		return "", identity, &refuseNoDeploymentApp, nil
	}
	identity, perr := githubapp.PreflightDeploymentApp(ctx, minter)
	if perr != nil {
		githubAppLog.Warn("managed bind: deployment app preflight failed", "org", orgID, "error", perr)
		return "", identity, &refuseNoDeploymentApp, nil
	}
	if identity.ClientID == "" {
		// Without a client id there is no code to exchange, so the ceremony
		// cannot prove anything about the person completing it.
		githubAppLog.Warn("managed bind: deployment app reports no client id", "org", orgID)
		return "", identity, &refuseNoDeploymentApp, nil
	}
	return ghWeb, identity, nil, nil
}

// managedBindCookie builds the ceremony cookie — the set and the clear, from
// one place, because a clear whose attributes don't match the set may leave the
// browser holding a copy.
//
// SameSite=Lax, NOT Strict, and this is load-bearing: the callback is a
// top-level GET navigation arriving from github.com, which Lax permits and
// Strict drops. Under Strict the cookie would never be sent, every bind would
// refuse as a stale ceremony, and the failure would look like a bug in the
// record rather than in the cookie. It is not an oversight to "harden" later.
//
// Path-scoped to the callback alone: the cookie has no business travelling with
// any other request, and the callback is one fixed path because the deployment
// App has one registered callback URL.
func (s *Server) managedBindCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     managedBindCookieName,
		Value:    value,
		Path:     ManagedBindCallbackPath,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// hashBindNonce is the one place the nonce becomes a stored value. SHA-256 with
// no salt or work factor is right here and a password hash would not be: the
// input is 32 bytes of process entropy, so there is no dictionary to run and
// nothing to slow down.
func hashBindNonce(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(sum[:])
}

// bindOutcomeData feeds the outcome page.
type bindOutcomeData struct {
	Heading   string
	Message   string
	BackURL   string
	BackLabel string
}

// bindOutcomeTemplate renders the ceremony's terminal page. The callback is
// reached by a top-level navigation, so a JSON body would dead-end the tab;
// this states what happened and links back. No form, so its CSP needs no
// form-action. html/template escapes every field in its context.
var bindOutcomeTemplate = template.Must(template.New("ghbind-outcome").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect GitHub</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;background:#0d1117;color:#e6edf3;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center}
.card{max-width:28rem;padding:2rem;text-align:center}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:#8b949e;line-height:1.5;margin:0 0 1rem}
a{color:#58a6ff;font-weight:600;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="card">
<h1>{{.Heading}}</h1>
<p>{{.Message}}</p>
<p><a href="{{.BackURL}}">&larr; {{.BackLabel}}</a></p>
</div>
</body>
</html>`))

// renderBindOutcome writes a refusal page with its own CSP and no-store. orgID
// may be empty — a refusal that happens before the record is consumed has no
// workspace to send anyone back to, and the app root is where the SPA will
// resolve one.
func (s *Server) renderBindOutcome(w http.ResponseWriter, orgID string, refusal bindRefusal) {
	// The code rides a header rather than the body so a machine caller — and a
	// test — can key on the closed set without parsing prose, while the browser
	// still gets a page it can read.
	w.Header().Set("X-TF-Bind-Outcome", refusal.code)
	heading := refusal.heading
	if heading == "" {
		heading = "Couldn't connect GitHub"
	}
	s.renderBindPage(w, refusal.status, orgID, heading, refusal.message)
}

// renderBindUnbound is the callback with no ceremony behind it: somebody
// installed the deployment App from its public page. Not a refusal — the
// installation is real and simply belongs to no workspace yet — so it answers
// 200 and says what to do next.
func (s *Server) renderBindUnbound(w http.ResponseWriter) {
	w.Header().Set("X-TF-Bind-Outcome", "unbound")
	s.renderBindPage(w, http.StatusOK, "", "GitHub App installed",
		"This installation isn't connected to a workspace yet. "+
			"Open Workspace Settings in Triage Factory and use Connect GitHub to finish.")
}

func (s *Server) renderBindPage(w http.ResponseWriter, status int, orgID, heading, message string) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	backURL, backLabel := "/", "Back to Triage Factory"
	if orgID != "" {
		backURL, backLabel = settingsRedirectPath(orgID)+"#github-app", "Back to Settings"
	}
	if err := bindOutcomeTemplate.Execute(w, bindOutcomeData{
		Heading:   heading,
		Message:   message,
		BackURL:   backURL,
		BackLabel: backLabel,
	}); err != nil {
		githubAppLog.Error("render bind outcome page failed", "error", err)
	}
}
