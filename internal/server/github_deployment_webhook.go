package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
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
// nothing is published or written; the one trace it leaves is a log line
// naming the installation, for the operator who is the only person placed to
// notice an install that landed nowhere. A 4xx would paint the operator's
// delivery log red for a normal condition and teach them to ignore it. Because nothing is recorded, a
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

	// The host half of the key is the deployment's default GitHub — the one
	// GitHub the deployment App is registered on, so the one GitHub any
	// delivery it signed can have come from. Nothing is read off the payload
	// for it: a sender URL would only ever restate this fact, and a payload
	// that could steer the lookup to another host would be a second
	// assumption stacked on the signature. The host stays in the key all the
	// same, because it is what makes the assumption checkable from the row —
	// an installation bound under any other spelling is one this route does
	// not reach, and boot warns about exactly those rows.
	host := ghbase.DefaultBaseURL()

	// The first store read on this route, and it happens only for a delivery
	// GitHub signed. Live rows only: a removed installation reaches nothing, so
	// a delivery for one is as unbound as a delivery for an installation the
	// mirror has never seen. It routes on the row and never on the org's
	// credential class, and that is safe because the two cannot disagree: the
	// disconnect verb soft-removes the rows in the same motion it resets the
	// class, and every other credential door refuses a managed workspace that
	// still holds a live row.
	orgID, err := s.githubApps.InstallationOwnerSystem(r.Context(), host, d.installationID)
	if err != nil {
		internalError(w, "github-webhook", err)
		return
	}
	if orgID == "" {
		// One line at a level an operator reads by default, because they are
		// the only person who can act on it: a workspace admin never sees this
		// installation (it is bound nowhere, so no tenant surface may list it)
		// and the installer sees only the recovery page. The line names what
		// the operator needs to tell "nobody has connected this yet" from "it
		// was connected to the wrong workspace" — the installation, and the
		// account it targets when the payload is the one that carries it — and
		// no payload beyond that: an installation event's sender, and every
		// other event's body, is text somebody on GitHub wrote.
		attrs := []any{"event", eventName, "delivery", d.deliveryID, "installation", d.installationID, "host", host}
		if d.install != nil {
			attrs = append(attrs, "account", d.install.Installation.Account.Login)
		}
		githubAppLog.Info("acknowledging deployment webhook delivery for an unbound installation", attrs...)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.applyWebhookDelivery(w, r, orgID, eventName, d)
}
