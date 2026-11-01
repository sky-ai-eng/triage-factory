package server

import "net/http"

func (s *Server) registerGitHubRoutes() {
	// The org-scoped provider surfaces are provider-first: everything the org
	// configures about GitHub hangs off /api/orgs/{org_id}/github/, and Jira
	// mirrors it. Each is sub-foldered by concern — access/ (how the org
	// reaches the provider: the org credential and the either/or transitions),
	// app/ (the registered App or OAuth app), identity/ (how the caller binds
	// their own account), connect/ (the OAuth leg of that binding). The App is
	// an access mode, not a sibling of access, which is why cutover (PAT→App)
	// and switch-to-pat (App→PAT) live one level apart rather than under two
	// unrelated parents. Provider is the stable axis: a GHES host change
	// ripples through access, identity, and webhooks together.

	// GitHub App manifest registration. The launch endpoint serves a
	// script-free bounce page (carrying its own per-response CSP) that POSTs
	// the manifest cross-origin to the org's GitHub host; the callback
	// exchanges the temp code for App credentials. Both validate org
	// membership + admin role inside the handler via r.PathValue("org_id").
	// Works in both local and multi mode.
	s.api("GET /api/orgs/{org_id}/github/app/register/launch", s.handleGitHubAppRegisterLaunch)
	s.api("GET /api/orgs/{org_id}/github/app/register/callback", s.handleGitHubAppRegisterCallback)
	// Bring-your-own-App import: the second way into App mode for orgs that
	// can't or shouldn't create the App themselves. Validates an App ID +
	// private key via an app-JWT GET /app, permission-preflights, and persists
	// through the same path the manifest callback uses (staging rule
	// unchanged). A JSON fetch from the SPA (not a top-level navigation like
	// launch/callback), so it rides apiMutating (CSRF). Org-admin (gated
	// inside the handler).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/import", s.handleGitHubAppImport)
	// Read-only status + install deep-link for the organization settings panel.
	// Any org member (read), so RequireOrgMember rather than RequireOrgAdmin.
	s.api("GET /api/orgs/{org_id}/github/app", s.handleGitHubAppStatus)
	s.api("GET /api/orgs/{org_id}/github/app/install-url", s.handleGitHubAppInstallURL)
	// On-demand installation reconcile — the "UI panel refresh" half of
	// installation discovery; the poller cycle is the other. Admin-only (the
	// setup wizard's install step + the Settings App panel call it) and
	// mode-agnostic. Mutating: it reconciles the installation mirror via the
	// same API backfill the poller runs, so it rides apiMutating (CSRF).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/installations/refresh", s.handleGitHubAppInstallationsRefresh)
	// Replay the App's failed installation webhook deliveries — the repair for
	// an org whose receiver was rejecting them (no webhook secret, or the
	// wrong one) while its installation mirror went stale. Admin-only, and
	// mutating in the sense that matters: it asks GitHub to deliver again, so
	// it rides apiMutating (CSRF).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/webhook/replay", s.handleGitHubAppWebhookReplay)
	// The two grant findings, computed from the reachable-repo mirror: what
	// the App can reach that nobody tracks, and what is tracked that the App
	// cannot reach. Paginated list reads (POST because the paging pair is a
	// body), org-admin, side-effect-free — opening the panel never asks
	// GitHub anything. Under github/grant rather than github/app because the
	// grant is what they are about, whichever App holds it: an org on
	// the deployment's App has no App resource of its own and the same two
	// findings. See github_grant_findings.go.
	s.apiMutating("POST /api/orgs/{org_id}/github/grant/reach-without-purpose/list", s.handleGitHubGrantReachWithoutPurposeList)
	s.apiMutating("POST /api/orgs/{org_id}/github/grant/scope-drift/list", s.handleGitHubGrantScopeDriftList)

	// The bind ceremony — how an org claims an installation of the
	// DEPLOYMENT App, the one key that serves many orgs. Multi-mode only
	// (a local binary ships no shared key). Both halves are top-level browser
	// navigations rather than fetches, so neither takes the CSRF origin check:
	// the callback arrives from github.com, and that gate would reject the very
	// request the flow depends on. Its CSRF protection is the pending-bind
	// cookie + record instead, which is a stronger claim than an Origin header
	// and the only one that survives the GitHub round trip.
	//
	// The callback carries NO org id, and that is forced: a GitHub App has one
	// registered callback URL for the whole deployment, so the org comes
	// from the consumed pending-bind record rather than from the path.
	//
	// It is also mounted RAW rather than through s.api, and takes withSession
	// itself — see managedBindCallback. One URL serves three callers: a member
	// linking their GitHub identity through the deployment App, a ceremony
	// coming back (both authenticated, because the next thing each does is a
	// write), and someone who installed the App from its public page on
	// GitHub, who has no session at all and must not be answered with a JSON
	// 401. The Connect and bind cookies decide which, before any session
	// lookup.
	s.apiMutating("POST /api/orgs/{org_id}/github/managed/connect-account", s.handleGitHubManagedConnectAccount)
	// Leaving the class — the bind's mirror. Verb routes because each is a
	// multi-row transition (soft-remove the rows, reset the class, record the
	// change) serialized under the App-registration lock, rather than a field
	// write; on Postgres the row removals commit on the admin pool ahead of the
	// class reset, in an order a retry converges — see
	// disconnectManagedInstallations. The narrowed one drops a single account
	// and is the full disconnect when that account was the last. JSON fetches
	// from the Settings panel, so both ride apiMutating (CSRF). Org-admin,
	// gated inside the handler.
	s.apiMutating("POST /api/orgs/{org_id}/github/managed/disconnect", s.handleGitHubManagedDisconnect)
	s.apiMutating("POST /api/orgs/{org_id}/github/managed/installations/{installation_id}/disconnect", s.handleGitHubManagedInstallationDisconnect)
	// Spelled literally rather than built from ManagedBindCallbackPath: the
	// pre-auth allowlist is audited by parsing this file, so a mount it cannot
	// read as a constant string is a mount it cannot check. The two cannot
	// drift silently — every managed-bind test requests the constant and would
	// 404 into the unmatched-/api/* handler if this literal disagreed.
	s.mux.Handle("GET /api/github/managed/callback", s.managedBindCallback())

	// GitHub access either/or transitions. GitHub access is strictly App XOR
	// PAT per org; these commit the switches and surface the inform-only
	// reachability diffs. All org-admin (gated inside the handler).
	//   - app/cutover: commit a staged PAT→App switch (activate the App and
	//     delete the PAT).
	//   - pat/switch-to: full App teardown, validate + store the new PAT.
	//   - DELETE github/app: discard a staged (not-yet-live) registration.
	//   - app/disconnect: full teardown of the LIVE App with nothing bound
	//     after it — the way out for an org moving to the deployment's
	//     App. Its own route rather than a widening of the discard, because
	//     the two are different intents: one leaves the org running on
	//     its token, the other takes it dark until something else is bound.
	//   - app/cutover-preflight, pat/preflight: inform-only reachability
	//     diffs, each under the flavor it is a preflight for.
	// The two commits + the discard mutate state (apiMutating, CSRF); the
	// cutover preflight is a read (api); the PAT preflight POSTs a token to
	// probe reach but stores nothing — still apiMutating for the same-origin
	// guard.
	s.apiMutating("POST /api/orgs/{org_id}/github/app/cutover", s.handleGitHubAppCutover)
	s.apiMutating("POST /api/orgs/{org_id}/github/pat/switch-to", s.handleGitHubAccessSwitchToPAT)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/app", s.handleGitHubAppDiscard)
	s.apiMutating("POST /api/orgs/{org_id}/github/app/disconnect", s.handleGitHubAppDisconnect)
	s.api("GET /api/orgs/{org_id}/github/app/cutover-preflight", s.handleGitHubAppCutoverPreflight)
	s.apiMutating("POST /api/orgs/{org_id}/github/pat/preflight", s.handleGitHubAccessPATPreflight)

	// The org integration credentials, each an addressable resource with an
	// explicit bind + unbind rather than a field on the bulk settings save.
	// Everything a credential write owes — live validation, the vault write,
	// the poller re-due, an access change-log row — belongs to exactly one
	// route per credential, which is what keeps it from being re-derived (and
	// forgotten) from whichever fields a bulk save happened to carry. See
	// org_credentials.go. The org config save carries no secret and makes no
	// outbound call, so it cannot revoke access as a side effect. The Jira
	// half of the pair is mounted by registerSettingsHandlerRoutes, beside the
	// handler that binds it; this rationale covers both. The PAT is a
	// credential flavor, and the flavor is the resource: its bind and unbind
	// here, its preflight and the switch onto it beside them, all under
	// github/pat as the App's routes sit under github/app.
	s.apiMutating("PUT /api/orgs/{org_id}/github/pat", s.handleGitHubPATPut)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/pat", s.handleGitHubPATDelete)

	// The org's commit signer: the GitHub user account its agents' commits
	// are authored, committed and signed as, and the key pair TF signs with.
	// A resource of its own beside the credentials, because it is one: its
	// writes take the same lock, record the same access log, and hold the
	// org's GitHub host in place while it exists. The read is member-gated,
	// every write org-admin. verify and rotate are verb routes because each
	// is a GitHub read or a key generation, not a field write; the email is
	// the one field a PATCH writes. commit-identity is the resolution a run's
	// commits take — the signer, or the identity the org commits as without
	// one — and whether they sign. See github_commit_signer.go.
	s.api("GET /api/orgs/{org_id}/github/commit-signer", s.handleCommitSignerGet)
	s.apiMutating("POST /api/orgs/{org_id}/github/commit-signer", s.handleCommitSignerCreate)
	s.apiMutating("PATCH /api/orgs/{org_id}/github/commit-signer", s.handleCommitSignerPatch)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/commit-signer", s.handleCommitSignerDelete)
	s.apiMutating("POST /api/orgs/{org_id}/github/commit-signer/verify", s.handleCommitSignerVerify)
	s.apiMutating("POST /api/orgs/{org_id}/github/commit-signer/rotate", s.handleCommitSignerRotate)
	s.api("GET /api/orgs/{org_id}/github/commit-identity", s.handleCommitIdentityGet)

	// "Connect GitHub" user-to-server OAuth — binds a host-verified GitHub
	// login to the signed-in user (identity, not access, not login). start
	// redirects to {github_base_url}/login/oauth/authorize; callback exchanges
	// the code, whoamis, and writes
	// user_github_identities(source='connect_oauth'). Both are GETs reached
	// via top-level navigation (the start from the gate page, the callback
	// from GitHub's redirect), so they ride s.api (withSession, no CSRF wrap)
	// and carry their own OAuth state-cookie CSRF defense. Any org member
	// binds their own identity. The identity-status read backs the gate.
	//
	// These two paths are frozen — they are the one part of the org-scoped
	// GitHub surface that isn't ours to rename. The callback is baked into a
	// manifest-created App's callback_urls at creation time, and a
	// bring-your-own-App owner registers it by hand (githubAppStatusResponse
	// .ConnectCallbackURL exists to hand them the exact string). It is the
	// redirect_uri sent on every user consent, and GitHub rejects a mismatch,
	// so a rename breaks per-user Connect for every already-registered App
	// until each owner edits their App settings on GitHub. start shares the
	// parent and moves with the callback or not at all.
	s.api("GET /api/orgs/{org_id}/github/connect/start", s.handleGitHubConnectStart)
	s.api("GET /api/orgs/{org_id}/github/connect/callback", s.handleGitHubConnectCallback)
	s.api("GET /api/orgs/{org_id}/github/identity", s.handleGitHubIdentityStatus)
	// Capture-and-discard per-user identity from a user-supplied PAT (PAT_2):
	// validate → whoami → write user_github_identities → drop the token. Local
	// mode only — the fallback to Connect there, and the only path when no App
	// is registered; multi mode links identity through Connect alone, and the
	// handler 404s. Never stores the token.
	s.apiMutating("POST /api/orgs/{org_id}/github/identity/pat", s.handleGitHubIdentityPAT)
	// GitHub accounts on the org's GitHub, read through the org's credential:
	// a login resolved to the account id a GitHub predicate's identity filters
	// take, and an id named by its current login.
	s.api("GET /api/orgs/{org_id}/github/users/by-name/{login}", s.handleGitHubAccountByName)
	s.api("GET /api/orgs/{org_id}/github/users/{github_user_id}", s.handleGitHubAccountByID)

	// Per-org GitHub App webhook receiver. Pre-auth (GitHub has no
	// session) and identified by org_id from the path; the handler
	// verifies the HMAC signature against that org's stored webhook
	// secret before any side effect, so it's on the preAuthAllowlist.
	//
	// Wrapped in the signed-webhook tier — the same one the Slack receiver
	// takes, and the tier that was built for this description: a route that
	// authenticates every request itself, where the cap defends the cost of
	// getting to the rejection rather than the rejection itself. Getting
	// there is not free here: resolving the secret to verify against reads
	// the org's settings, its registration row, and the vault, all before
	// the signature is checked (github_webhook_secret.go caches the result
	// per org; this bounds how fast an anonymous caller can miss that
	// cache). No-op in local mode.
	s.mux.Handle("POST /api/webhooks/github/{org_id}", s.signedWebhookRateLimit(http.HandlerFunc(s.handleGitHubWebhook)))

	// The deployment App's receiver: one App, one webhook URL, no tenant in the
	// path. Pre-auth for the same reason as its sibling, and on the same
	// limiter tier — but the cost it defends is smaller, because this handler
	// reads no store until the signature has passed against the one secret it
	// already holds. The two mounts are siblings, not one route: Go's mux
	// resolves the exact path and the {org_id} segment independently, and the
	// handlers stay separate on purpose (see handleDeploymentGitHubWebhook).
	// Spelled literally: routes_coverage_test audits string-literal patterns.
	s.mux.Handle("POST /api/webhooks/github", s.signedWebhookRateLimit(http.HandlerFunc(s.handleDeploymentGitHubWebhook)))
}
