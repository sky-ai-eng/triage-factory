package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// ssoVerificationTXTPrefix is the underscore-prefixed dedicated subdomain the
// DNS-TXT verification record lives under (the WorkOS / 2025 convention).
// Using a dedicated `_…` label rather than the apex keeps the org's TXT set
// uncluttered and avoids apex TXT oversize.
//
//	TXT  _triagefactory-verification.<domain>  "<token>"
const ssoVerificationTXTPrefix = "_triagefactory-verification."

// dnsLookupTimeout bounds the live DNS TXT lookup the verify handler makes so
// a slow/unreachable resolver can't pin a request goroutine open.
const dnsLookupTimeout = 5 * time.Second

// txtResolver is the DNS TXT lookup seam. *net.Resolver satisfies it, so the
// production handler wires net.DefaultResolver; tests inject a fake so the
// suite never depends on real DNS (the ticket's explicit requirement).
type txtResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var _ txtResolver = net.DefaultResolver

// ssoDomainsHandler serves /api/sso/domains — the org-admin domain-claim +
// DNS-TXT verification lifecycle (the multi-org SSO isolation linchpin). An
// admin claims an email domain (pending, inert), proves control by publishing
// a TXT record, then verifies; only a verified domain routes logins, and a
// verified domain belongs to ≤1 org deployment-wide.
//
// Conventions mirror teams_handler.go / invites_handler.go: runmode.ModeLocal
// → 404 (SSO is a multi-org concept; local mode is N=1), requireOrg for the
// active org, ClaimsFrom for the user, az.UserIsOrgAdmin for the gate (404 not
// 403 on a non-admin — non-disclosure), tx.WithTx for app-pool writes gated by
// the sso_domains_* RLS policies. The verify path additionally does a live DNS
// lookup through the injected resolver, outside any DB transaction.
type ssoDomainsHandler struct {
	tx       db.TxRunner
	az       *authz.Checker
	resolver txtResolver
}

// newSSODomainsHandler wires the handler with the production DNS resolver
// (Go's stdlib). The instance is retained on the Server so tests can swap
// .resolver for a fake, keeping the suite off real DNS.
func newSSODomainsHandler(tx db.TxRunner, az *authz.Checker) *ssoDomainsHandler {
	return &ssoDomainsHandler{tx: tx, az: az, resolver: net.DefaultResolver}
}

// ssoDomainJSON is the wire shape for a domain claim across every endpoint.
// txt_host / txt_value give the UI (TFAC-429) the exact DNS record to publish
// for a pending claim; status ∈ {pending, verified}; verified_at rides along
// once stamped (omitted while pending). The token is published publicly by
// design (it proves DNS control), so returning it is not a secret leak.
type ssoDomainJSON struct {
	ID         string     `json:"id"`
	Domain     string     `json:"domain"`
	TXTHost    string     `json:"txt_host"`
	TXTValue   string     `json:"txt_value"`
	Status     string     `json:"status"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// gate runs the shared front-door checks every domain endpoint needs:
// multi-mode only, an active org, and org-admin. It writes the response and
// returns ok=false on any failure (404 in local / non-admin — non-disclosure;
// the requireOrg conflict otherwise), so callers just `return` on !ok.
func (h *ssoDomainsHandler) gate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		// Hosted-only: local mode never registers an SSO connection or claims
		// a domain. 404 matches the "feature absent" posture of POST /api/teams
		// and the invite routes.
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject

	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "sso", err)
		return "", "", false
	}
	if !isAdmin {
		// 404 not 403 — same non-disclosure posture as teams/invites: don't
		// reveal the org (or that SSO domains exist) to a non-admin.
		http.NotFound(w, r)
		return "", "", false
	}
	return orgID, userID, true
}

// handleDomainClaim creates (or idempotently returns) a pending domain claim
// for the caller's org and hands back the DNS-TXT challenge to publish.
//
// The body carries only {domain}; the row's NOT-NULL connection_id is resolved
// server-side from the org's SSO connection (TFAC-424). An org must register a
// connection before claiming domains — with none, this 409s. (In practice an
// org has exactly one connection; if it somehow has several, the most recent
// is used, matching ListByOrg's newest-first order.)
//
// POST /api/sso/domains  body: { "domain": "<domain>" }
func (h *ssoDomainsHandler) handleDomainClaim(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}

	var body struct {
		Domain string `json:"domain"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	domainName, valid := normalizeSSODomain(body.Domain)
	if !valid {
		badRequest(w, "domain must be a bare domain like example.com")
		return
	}

	var (
		resp    ssoDomainJSON
		created bool
		noConn  bool
	)
	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Idempotent re-claim: if this org already holds a claim on the domain
		// (pending or verified), return it verbatim rather than minting a
		// second row (the sso_domains_org_domain_uniq index would reject one
		// anyway). The org has few domains, so the linear scan is trivial.
		existing, e := findOrgDomain(r.Context(), tx, orgID, domainName)
		if e != nil {
			return e
		}
		if existing != nil {
			resp = ssoDomainToJSON(existing)
			return nil
		}

		// Resolve the connection the domain attaches to. ListByOrg is
		// newest-first; an org normally has exactly one connection.
		conns, e := tx.SSOConnections.ListByOrg(r.Context(), orgID)
		if e != nil {
			return e
		}
		if len(conns) == 0 {
			noConn = true
			return nil
		}

		token, e := randomSSOToken()
		if e != nil {
			return e
		}
		id, e := tx.SSODomains.Create(r.Context(), domain.CreateSSODomainParams{
			ConnectionID: conns[0].ID,
			OrgID:        orgID,
			Domain:       domainName,
			Token:        token,
		})
		if e != nil {
			return e
		}
		created = true
		resp = ssoDomainJSON{
			ID:       id,
			Domain:   domainName,
			TXTHost:  ssoTXTHost(domainName),
			TXTValue: token,
			Status:   "pending",
		}
		return nil
	})
	if err != nil {
		// A concurrent claim for the same (org, domain) committed between our
		// idempotency scan and the insert — the per-org unique index rejected
		// us. Re-read and return the row the winner created (idempotent).
		if isUniqueViolation(err) {
			var raced ssoDomainJSON
			reErr := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				existing, e := findOrgDomain(r.Context(), tx, orgID, domainName)
				if e != nil {
					return e
				}
				if existing != nil {
					raced = ssoDomainToJSON(existing)
				}
				return nil
			})
			if reErr == nil && raced.ID != "" {
				writeJSON(w, http.StatusOK, raced)
				return
			}
		}
		internalError(w, "sso", err)
		return
	}
	if noConn {
		// Precondition: domains attach to a connection, so the org must
		// configure SSO (TFAC-424) before claiming a domain.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "register an SSO connection before claiming a domain",
		})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

// handleDomainList returns the caller-org's domain claims (pending +
// verified), driving the management UI.
//
// GET /api/sso/domains
func (h *ssoDomainsHandler) handleDomainList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}

	var domains []domain.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		domains, e = tx.SSODomains.ListByOrg(r.Context(), orgID)
		return e
	}); err != nil {
		internalError(w, "sso", err)
		return
	}

	out := make([]ssoDomainJSON, len(domains))
	for i := range domains {
		out[i] = ssoDomainToJSON(&domains[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDomainVerify does a live DNS TXT lookup at
// _triagefactory-verification.<domain> and, on a token match, stamps the claim
// verified — subject to the deployment-wide verified-domain uniqueness index.
//
// Already-verified is an idempotent 200. No DNS match → 409 "retry" (the
// record may not have propagated). A global-uniqueness collision (another org
// verified this domain first) maps to an opaque 409 that never names the
// holder — it crosses the tenant boundary.
//
// POST /api/sso/domains/{id}/verify
func (h *ssoDomainsHandler) handleDomainVerify(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}

	// Read the claim first — we need its (normalized) domain + token for the
	// lookup, and the lookup must not run inside a DB transaction.
	var claim *domain.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		claim, e = tx.SSODomains.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "sso", err)
		return
	}
	if claim == nil {
		http.NotFound(w, r)
		return
	}
	if claim.VerifiedAt != nil {
		// Verify-once, keep: re-verifying an already-verified claim is a no-op
		// success (the customer may even have removed the TXT record by now).
		writeJSON(w, http.StatusOK, ssoDomainToJSON(claim))
		return
	}

	// Live DNS TXT lookup, bounded + outside any transaction. Any returned
	// value equal to the claim token proves control of the zone.
	lookupCtx, cancel := context.WithTimeout(r.Context(), dnsLookupTimeout)
	defer cancel()
	host := ssoTXTHost(claim.Domain)
	values, lookupErr := h.resolver.LookupTXT(lookupCtx, host)
	matched := false
	if lookupErr == nil {
		for _, v := range values {
			if v == claim.Token {
				matched = true
				break
			}
		}
	}
	if !matched {
		// Missing record, NXDOMAIN, or a value mismatch all land here: the
		// proof isn't visible yet. A resolver error is logged (diagnostics)
		// but presented the same way — "not found yet, retry" — so a slow
		// propagation reads as transient, not a hard failure.
		if lookupErr != nil {
			ssoLog.Debug("sso domain verify: TXT lookup did not match",
				"org", orgID, "domain", claim.Domain, "host", host, "error", lookupErr)
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "DNS record not found yet — propagation can take time, retry.",
		})
		return
	}

	// Proof matched: stamp verified. SetVerified's UPDATE trips the
	// verified-global-unique index if another org already verified this
	// domain; we map that opaque unique violation to a 409 that never names
	// the other org. GetByID in the same tx reads back the stamped row.
	var verified *domain.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if e := tx.SSODomains.SetVerified(r.Context(), orgID, id); e != nil {
			return e
		}
		var e error
		verified, e = tx.SSODomains.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this domain is already verified by another organization",
			})
			return
		}
		internalError(w, "sso", err)
		return
	}
	if verified == nil {
		// The claim was deleted between the lookup and the stamp — nothing to
		// report verified.
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, ssoDomainToJSON(verified))
}

// handleDomainDelete unclaims a domain (pending or verified) for the caller's
// org.
//
// DELETE /api/sso/domains/{id}
func (h *ssoDomainsHandler) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		http.NotFound(w, r)
		return
	}

	var existed bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// GetByID first so a missing / other-org id is a clean 404 rather than
		// a silent idempotent no-op (RLS already scopes both to the org).
		claim, e := tx.SSODomains.GetByID(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if claim == nil {
			return nil
		}
		existed = true
		return tx.SSODomains.Delete(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "sso", err)
		return
	}
	if !existed {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// findOrgDomain returns the org's claim on domainName (already normalized,
// lower-cased), or (nil, nil) when the org holds no claim on it. Used for the
// idempotent re-claim path; scans ListByOrg because the store exposes no
// (org, domain) point read and an org carries only a handful of domains.
func findOrgDomain(ctx context.Context, tx db.TxStores, orgID, domainName string) (*domain.SSODomain, error) {
	list, err := tx.SSODomains.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if strings.EqualFold(list[i].Domain, domainName) {
			return &list[i], nil
		}
	}
	return nil, nil
}

// normalizeSSODomain lower-cases + trims a claimed domain, drops any trailing
// FQDN-root dots, and rejects non-domains: empty, an email, a URL, anything
// with whitespace, a single label (localhost), or a malformed label structure
// (a leading or doubled dot, e.g. ".corp.com" / "corp..com" — these can never
// match an email domain and would only mint a dead claim). Returns
// (normalized, true) on success. Routing (TFAC-427) matches the exact email
// domain against the stored value, so normalization here is what makes
// "Corp.com", "corp.com.", and "corp.com.." all route as "corp.com".
func normalizeSSODomain(raw string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(raw))
	// Tolerate any number of trailing FQDN-root dots ("corp.com." /
	// "corp.com..") — strip them all so the stored value matches a bare email
	// domain and yields the right TXT host. TrimSuffix would peel only one,
	// leaving "corp.com." to pass and route as a domain it never should.
	d = strings.TrimRight(d, ".")
	if d == "" {
		return "", false
	}
	// A bare hostname carries none of these. Catches pasted emails
	// (user@corp.com), URLs (https://corp.com/x), and whitespace.
	if strings.ContainsAny(d, " \t\r\n@/:\\") {
		return "", false
	}
	// A real email domain is always ≥2 non-empty labels: reject a single
	// label (localhost) and any empty label from a leading or doubled dot
	// (.corp.com, corp..com).
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, l := range labels {
		if l == "" {
			return "", false
		}
	}
	return d, true
}

// ssoTXTHost builds the DNS-TXT verification record name for a domain.
func ssoTXTHost(domainName string) string {
	return ssoVerificationTXTPrefix + domainName
}

// randomSSOToken mints the public DNS-TXT verification value — 32 random bytes
// as base64url (raw, no padding). It's published in DNS by design (it proves
// zone control), so it need not be secret, only unguessable + unique.
func randomSSOToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ssoDomainToJSON projects a stored row to the wire shape, deriving txt_host
// and the pending/verified status (with verified_at when stamped).
func ssoDomainToJSON(d *domain.SSODomain) ssoDomainJSON {
	j := ssoDomainJSON{
		ID:       d.ID,
		Domain:   d.Domain,
		TXTHost:  ssoTXTHost(d.Domain),
		TXTValue: d.Token,
		Status:   "pending",
	}
	if d.VerifiedAt != nil {
		j.Status = "verified"
		j.VerifiedAt = d.VerifiedAt
	}
	return j
}
