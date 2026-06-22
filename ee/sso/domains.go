package sso

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ssoVerificationTXTPrefix is the underscore-prefixed dedicated subdomain
// the DNS-TXT verification record lives under.
//
//	TXT  _triagefactory-verification.<domain>  "<token>"
const ssoVerificationTXTPrefix = "_triagefactory-verification."

const dnsLookupTimeout = 5 * time.Second

// txtResolver is the DNS TXT lookup seam. *net.Resolver satisfies it;
// tests inject a fake so the suite never depends on real DNS.
type txtResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var _ txtResolver = net.DefaultResolver

// ssoDomainsHandler serves /api/sso/domains — the org-admin domain-claim +
// DNS-TXT verification lifecycle (the multi-org SSO isolation linchpin).
type ssoDomainsHandler struct {
	tx       db.TxRunner
	az       *authz.Checker
	resolver txtResolver
}

func newSSODomainsHandler(tx db.TxRunner, az *authz.Checker) *ssoDomainsHandler {
	return &ssoDomainsHandler{tx: tx, az: az, resolver: net.DefaultResolver}
}

type ssoDomainJSON struct {
	ID         string     `json:"id"`
	Domain     string     `json:"domain"`
	TXTHost    string     `json:"txt_host"`
	TXTValue   string     `json:"txt_value"`
	Status     string     `json:"status"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

func (h *ssoDomainsHandler) gate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject

	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return "", "", false
	}
	if !isAdmin {
		http.NotFound(w, r)
		return "", "", false
	}
	return orgID, userID, true
}

// POST /api/sso/domains  body: { "domain": "<domain>" }
func (h *ssoDomainsHandler) handleDomainClaim(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}

	var body struct {
		Domain string `json:"domain"`
	}
	if !httpx.DecodeJSON(w, r, &body, "") {
		return
	}
	domainName, valid := normalizeSSODomain(body.Domain)
	if !valid {
		httpx.BadRequest(w, "domain must be a bare domain like example.com")
		return
	}

	var (
		resp    ssoDomainJSON
		created bool
		noConn  bool
	)
	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		existing, e := findOrgDomain(r.Context(), sso, orgID, domainName)
		if e != nil {
			return e
		}
		if existing != nil {
			resp = ssoDomainToJSON(existing)
			return nil
		}

		conns, e := sso.Connections.ListByOrg(r.Context(), orgID)
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
		id, e := sso.Domains.Create(r.Context(), ssostore.CreateSSODomainParams{
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
		if httpx.IsUniqueViolation(err) {
			var raced ssoDomainJSON
			reErr := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				existing, e := findOrgDomain(r.Context(), ssostore.FromTx(tx), orgID, domainName)
				if e != nil {
					return e
				}
				if existing != nil {
					raced = ssoDomainToJSON(existing)
				}
				return nil
			})
			if reErr != nil {
				httpx.InternalError(w, "sso", reErr)
				return
			}
			if raced.ID != "" {
				httpx.WriteJSON(w, http.StatusOK, raced)
				return
			}
		}
		httpx.InternalError(w, "sso", err)
		return
	}
	if noConn {
		httpx.WriteJSON(w, http.StatusConflict, map[string]string{
			"error": "register an SSO connection before claiming a domain",
		})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, resp)
}

// GET /api/sso/domains
func (h *ssoDomainsHandler) handleDomainList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}

	var domains []ssostore.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		domains, e = ssostore.FromTx(tx).Domains.ListByOrg(r.Context(), orgID)
		return e
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}

	out := make([]ssoDomainJSON, len(domains))
	for i := range domains {
		out[i] = ssoDomainToJSON(&domains[i])
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

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

	var claim *ssostore.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		claim, e = ssostore.FromTx(tx).Domains.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if claim == nil {
		http.NotFound(w, r)
		return
	}
	if claim.VerifiedAt != nil {
		httpx.WriteJSON(w, http.StatusOK, ssoDomainToJSON(claim))
		return
	}

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
		if lookupErr != nil {
			ssoLog.Debug("sso domain verify: TXT lookup did not match",
				"org", orgID, "domain", claim.Domain, "host", host, "error", lookupErr)
		}
		httpx.WriteJSON(w, http.StatusConflict, map[string]string{
			"error": "DNS record not found yet — propagation can take time, retry.",
		})
		return
	}

	var verified *ssostore.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		if e := sso.Domains.SetVerified(r.Context(), orgID, id); e != nil {
			return e
		}
		var e error
		verified, e = sso.Domains.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		if httpx.IsUniqueViolation(err) {
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "this domain is already verified by another organization",
			})
			return
		}
		httpx.InternalError(w, "sso", err)
		return
	}
	if verified == nil {
		http.NotFound(w, r)
		return
	}
	ssoLog.Info("sso domain verified", "org", orgID, "domain", verified.Domain, "id", verified.ID)
	httpx.WriteJSON(w, http.StatusOK, ssoDomainToJSON(verified))
}

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
		sso := ssostore.FromTx(tx)
		claim, e := sso.Domains.GetByID(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if claim == nil {
			return nil
		}
		existed = true
		return sso.Domains.Delete(r.Context(), orgID, id)
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if !existed {
		http.NotFound(w, r)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// findOrgDomain returns the org's claim on domainName (already normalized),
// or (nil, nil). Scans ListByOrg because the store exposes no (org, domain)
// point read and an org carries only a handful of domains.
func findOrgDomain(ctx context.Context, sso *ssostore.Bundle, orgID, domainName string) (*ssostore.SSODomain, error) {
	list, err := sso.Domains.ListByOrg(ctx, orgID)
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

// normalizeSSODomain lower-cases + trims a claimed domain, drops trailing
// FQDN-root dots, and rejects non-domains. Returns (normalized, true) on
// success.
func normalizeSSODomain(raw string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(raw))
	d = strings.TrimRight(d, ".")
	if d == "" {
		return "", false
	}
	if strings.ContainsAny(d, " \t\r\n@/:\\") {
		return "", false
	}
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

func ssoTXTHost(domainName string) string {
	return ssoVerificationTXTPrefix + domainName
}

// randomSSOToken mints the public DNS-TXT verification value — 32 random
// bytes as base64url (raw). Published in DNS by design, so it need not be
// secret, only unguessable + unique.
func randomSSOToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func ssoDomainToJSON(d *ssostore.SSODomain) ssoDomainJSON {
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
