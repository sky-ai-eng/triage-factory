package server

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// handleDeploymentGitHubWebhook receives the deployment App's webhooks at
// POST /api/webhooks/github. One App has one webhook URL, so a delivery on
// this route cannot carry an org id in its path; the receiver's order is the
// reverse of the per-org route's: secret → verify → route. The one deployment
// secret is in hand from config before any org is, the HMAC is checked against
// it, and only then is the installation the verified payload names looked up
// in the binding table to learn whose delivery this was. Nothing ahead of the
// signature check touches a store — a forged delivery costs the deployment a
// hash and nothing else, which is the property the inversion buys and the
// per-org route structurally cannot have.
//
// The two receivers coexist and are deliberately not one function. They
// resolve different secrets, in different orders, for different credential
// classes, and a tenant-selection branch inside the one handler where a
// mistake is worst is the thing keeping them apart avoids. A managed org's
// delivery to the per-org URL still answers 401 there (resolveWebhookSecret
// returns "" for the class), and a BYO org's delivery here fails the HMAC.
//
// The org is never the payload's own claim about itself. It is whichever
// workspace the bind ceremony recorded for (host, installation id) — the same
// key the ceremony's uniqueness gate reads — so an installation.created for an
// installation nobody has bound writes nothing: creation is the bind's job
// alone, and a webhook that could mint a binding would reintroduce exactly the
// spoofing the ceremony exists to prevent. An unbound installation is an
// ordinary state (a request an owner approves later, an install from the
// App's public page), not a fault: the delivery is acknowledged with 2xx and
// nothing is published, written or logged beyond a debug line naming the
// installation. A 4xx would paint the operator's delivery log red for a normal
// condition and teach them to ignore it. Because nothing is recorded, a
// redelivery after the workspace binds the installation applies normally.
//
// Multi-mode only: the deployment App is a multi-mode credential, so in local
// mode this route does not exist. Rate-limited at the signed-webhook tier like
// its sibling; that limiter keys on client IP alone, so here every tenant's
// deliveries share buckets sized for one — acceptable at the volumes in view,
// since GitHub's hook egress is a small IP set.
func (s *Server) handleDeploymentGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() != runmode.ModeMulti {
		notFound(w, "route")
		return
	}

	eventName, sigHeader, body, ok := readWebhookDelivery(w, r)
	if !ok {
		return
	}

	// The deployment secret comes from the environment, read once at
	// construction; no store holds it and none is consulted to find it. A
	// deployment without one has nothing to verify against and refuses exactly
	// as a bad signature does — the empty secret must short-circuit rather than
	// fall into the HMAC check, which anyone can pass against an empty key.
	// Bare 401, no body, no payload logging.
	secret := s.deploymentApp.WebhookSecret
	if secret == "" || !validWebhookSignature(secret, body, sigHeader) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Signature verified past this point. Structural validation still precedes
	// the first store read, so a malformed delivery keeps its 4xx on every
	// attempt rather than depending on what the binding table says that day.
	d, ok := parseVerifiedDelivery(w, r, eventName, body)
	if !ok {
		return
	}
	if d.installationID == "" {
		// A delivery naming no installation (ping, github_app_authorization)
		// names no tenant either. Acknowledged and dropped: there is no org to
		// publish under, and recording it would be a write on behalf of nobody.
		githubAppLog.Debug("acknowledging deployment webhook delivery naming no installation",
			"event", eventName, "delivery", d.deliveryID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	host, ok := deliveryGitHubHost(body)
	if !ok {
		// Without a host there is no key to look the installation up under, and
		// assuming github.com is the assumption github_host exists to make
		// checkable. Same answer as an unbound installation: acknowledged, no
		// side effect.
		githubAppLog.Debug("acknowledging deployment webhook delivery naming no github host",
			"event", eventName, "delivery", d.deliveryID, "installation", d.installationID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// The first store read on this route, and it happens only for a delivery
	// GitHub signed. Live rows only: a removed installation reaches nothing, so
	// a delivery for one is as unbound as a delivery for an installation the
	// mirror has never seen.
	//
	// TODO(TFAC-937): this routes on the row, not the org's credential class.
	// Nothing removes a workspace's bound rows when it leaves managed_app, and
	// the PAT / BYO doors do not refuse a managed workspace, so a workspace that
	// bound a PAT keeps receiving the deployment App's deliveries here until the
	// disconnect verb and the door guards exist.
	orgID, err := s.githubApps.InstallationOwnerSystem(r.Context(), host, d.installationID)
	if err != nil {
		internalError(w, "github-webhook", err)
		return
	}
	if orgID == "" {
		githubAppLog.Debug("acknowledging deployment webhook delivery for an unbound installation",
			"event", eventName, "delivery", d.deliveryID, "installation", d.installationID, "host", host)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.applyWebhookDelivery(w, r, orgID, eventName, d)
}

// deliveryGitHubHost reads which GitHub a verified delivery came from, in the
// form the binding table keys on (the web base the org configured, normalized
// by db.EffectiveGitHubHost — "https://github.com", "https://ghe.example.com").
//
// The source is the payload's sender.html_url. GitHub includes sender on every
// webhook payload whatever the event, and its html_url is a URL on the GitHub
// that generated the delivery, so one field answers for every event the App
// subscribes to — where installation.html_url appears only on installation
// events and repository.html_url only on repository-scoped ones. It is
// payload-derived by design and to the same standard as the installation id
// beside it: both are read only after the signature has proved the deployment
// secret signed the body, and the host does not choose an org, it only narrows
// which row the installation id may match. Two GHES deployments can issue the
// same installation id, which is why the key has two parts.
//
// ok=false for a payload that carries no parseable sender URL. The caller
// treats that as unresolvable rather than defaulting to github.com — a
// delivery that cannot say where it came from is not matched to a row on the
// strength of an assumption.
//
// TODO(TFAC-936): the deployment has no declared GitHub host yet, which is the
// only reason the host is read off the payload. Once TF_DEFAULT_GITHUB_HOST
// exists the lookup keys on it, and this function and the caller's no-host arm
// go away.
func deliveryGitHubHost(body []byte) (string, bool) {
	var envelope struct {
		Sender struct {
			HTMLURL string `json:"html_url"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Sender.HTMLURL == "" {
		return "", false
	}
	u, err := url.Parse(envelope.Sender.HTMLURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}
