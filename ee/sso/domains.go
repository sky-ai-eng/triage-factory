package sso

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/ee/sso/dnsoverride"
	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
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
	ID         string     `json:"id" doc:"The domain claim's id."`
	Domain     string     `json:"domain" doc:"The claimed domain, in lowercase."`
	TXTHost    string     `json:"txt_host" doc:"The DNS name the verification TXT record goes under."`
	TXTValue   string     `json:"txt_value" doc:"The value the verification TXT record must hold."`
	Status     string     `json:"status" doc:"pending until the domain is verified, then verified."`
	VerifiedAt *time.Time `json:"verified_at,omitempty" doc:"When the domain was verified, as an RFC 3339 timestamp, absent while it is pending."`
}

// ssoDomainDeleteResponse is the body of DELETE /api/sso/domains/{id}.
type ssoDomainDeleteResponse struct {
	Status string `json:"status" doc:"Always deleted."`
}

func (h *ssoDomainsHandler) gate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		httpx.NotFound(w, "route")
		return "", "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", "", false
	}
	if !entitlements.For(orgID).Has(entitlements.FeatureSSO) {
		httpx.NotFound(w, "route")
		return "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject

	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return "", "", false
	}
	if !isAdmin {
		// Visible org, missing role: 403, matching the core admin gates.
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
			Reason: httpx.ReasonForbidden, Message: "org admin role required",
		})
		return "", "", false
	}
	return orgID, userID, true
}

// ssoDomainClaimRequest is the body of POST /api/sso/domains.
type ssoDomainClaimRequest struct {
	Domain string `json:"domain" doc:"The domain to claim, a bare domain such as example.com, compared without regard to case. Required."`
}

var opDomainClaim = httpx.NewJSONOp[ssoDomainClaimRequest, ssoDomainJSON](httpx.OpSpec{
	Pattern:       "POST /api/sso/domains",
	ID:            "create_sso_domain",
	Summary:       "Claim an email domain for SSO",
	Description:   "Claims an email domain for the organization's SSO connection and returns it pending, with the DNS TXT record that verifies it; a domain the organization has already claimed is returned as it is, with status 200. The organization must have a connection." + ssoAccess,
	Status:        http.StatusCreated,
	OtherStatuses: []int{http.StatusOK},
	Hints:         httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// POST /api/sso/domains  body: { "domain": "<domain>" }
func (h *ssoDomainsHandler) handleDomainClaim(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}

	body, ok := opDomainClaim.Decode(w, r)
	if !ok {
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
		// Guarded by the existing-claim early return above, so re-claiming a
		// domain the org already holds records nothing.
		if e := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionSSODomainClaimed,
			DetailJSON:  domain.AccessDetailSSODomain(domainName),
		}); e != nil {
			return fmt.Errorf("audit sso domain claim: %w", e)
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
			reErr := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
				opDomainClaim.WriteStatus(w, http.StatusOK, raced)
				return
			}
		}
		httpx.InternalError(w, "sso", err)
		return
	}
	if noConn {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "register an SSO connection before claiming a domain"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	opDomainClaim.WriteStatus(w, status, resp)
}

var opDomainList = httpx.NewJSONOp[httpx.NoBody, []ssoDomainJSON](httpx.OpSpec{
	Pattern:     "GET /api/sso/domains",
	ID:          "list_sso_domains",
	Summary:     "List claimed SSO domains",
	Description: "Returns every email domain the organization has claimed for SSO, pending or verified, with the DNS TXT record that verifies each." + ssoAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// GET /api/sso/domains
func (h *ssoDomainsHandler) handleDomainList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}

	var domains []ssostore.SSODomain
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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
	opDomainList.Write(w, out)
}

var opDomainVerify = httpx.NewJSONOp[httpx.NoBody, ssoDomainJSON](httpx.OpSpec{
	Pattern:     "POST /api/sso/domains/{id}/verify",
	ID:          "verify_sso_domain",
	Summary:     "Verify a claimed SSO domain",
	Description: "Looks up the domain's verification TXT record in DNS and, when it holds the expected value, marks the domain verified, so people with an address at it can join the organization by signing in through SSO. Returns the domain; one already verified is returned without a lookup." + ssoAccess,
	PathParams:  map[string]string{"id": "The domain claim's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// POST /api/sso/domains/{id}/verify
func (h *ssoDomainsHandler) handleDomainVerify(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.NotFound(w, "sso connection")
		return
	}

	var claim *ssostore.SSODomain
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		claim, e = ssostore.FromTx(tx).Domains.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if claim == nil {
		httpx.NotFound(w, "sso connection")
		return
	}
	if claim.VerifiedAt != nil {
		opDomainVerify.Write(w, ssoDomainToJSON(claim))
		return
	}

	lookupCtx, cancel := context.WithTimeout(r.Context(), dnsLookupTimeout)
	defer cancel()
	host := ssoTXTHost(claim.Domain)
	// Production uses h.resolver (net.DefaultResolver); tests inject a fake via
	// the dnsoverride seam (the verify suite can't import ee directly).
	resolver := h.resolver
	if o := dnsoverride.Get(); o != nil {
		resolver = o
	}
	values, lookupErr := resolver.LookupTXT(lookupCtx, host)
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
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "DNS record not found yet — propagation can take time, retry."})
		return
	}

	var verified *ssostore.SSODomain
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		if e := sso.Domains.SetVerified(r.Context(), orgID, id); e != nil {
			return e
		}
		// Verification is the step that arms a domain for JIT provisioning —
		// anyone with an address at it can auto-join from here — so it records
		// separately from the claim. Guarded by the already-verified early
		// return above, so a re-verify is silent.
		if e := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionSSODomainVerified,
			DetailJSON:  domain.AccessDetailSSODomain(claim.Domain),
		}); e != nil {
			return fmt.Errorf("audit sso domain verify: %w", e)
		}
		var e error
		verified, e = sso.Domains.GetByID(r.Context(), orgID, id)
		return e
	}); err != nil {
		if httpx.IsUniqueViolation(err) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this domain is already verified by another organization"})
			return
		}
		httpx.InternalError(w, "sso", err)
		return
	}
	if verified == nil {
		httpx.NotFound(w, "sso connection")
		return
	}
	ssoLog.Info("sso domain verified", "org", orgID, "domain", verified.Domain, "id", verified.ID)
	opDomainVerify.Write(w, ssoDomainToJSON(verified))
}

var opDomainDelete = httpx.NewJSONOp[httpx.NoBody, ssoDomainDeleteResponse](httpx.OpSpec{
	Pattern:     "DELETE /api/sso/domains/{id}",
	ID:          "delete_sso_domain",
	Summary:     "Remove a claimed SSO domain",
	Description: "Removes one of the organization's claimed domains. The last verified domain cannot be removed while the organization requires SSO." + ssoAccess,
	PathParams:  map[string]string{"id": "The domain claim's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// DELETE /api/sso/domains/{id}
func (h *ssoDomainsHandler) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.gate(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.NotFound(w, "sso connection")
		return
	}

	var existed bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		// Locked before the last-verified-domain check reads the enforced flag,
		// so a concurrent Require SSO toggle cannot pass its own domain check
		// against this domain while this one passes against the flag unset.
		if e := sso.Connections.LockForOrg(r.Context(), orgID); e != nil {
			return e
		}
		claim, e := sso.Domains.GetByID(r.Context(), orgID, id)
		if e != nil {
			return e
		}
		if claim == nil {
			return nil
		}
		existed = true
		// Turning Require SSO on needs a verified domain, and the sign-in rule
		// uses it to recognise and refuse a GitHub sign-in on the org's
		// addresses. Removing the last one while required would leave that
		// rule matching nothing, so it is refused.
		if claim.VerifiedAt != nil {
			last, e := lastVerifiedWhileEnforced(r.Context(), sso, orgID, id)
			if e != nil {
				return e
			}
			if last {
				return errTurnOffEnforcementFirst
			}
		}
		if e := sso.Domains.Delete(r.Context(), orgID, id); e != nil {
			return e
		}
		// Named from the row read above — after the delete there is nothing
		// left to resolve the domain from, and the id alone would make the log
		// unreadable.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionSSODomainRemoved,
			DetailJSON:  domain.AccessDetailSSODomain(claim.Domain),
		})
	}); err != nil {
		if errors.Is(err, errTurnOffEnforcementFirst) {
			turnOffEnforcementFirst(w)
			return
		}
		httpx.InternalError(w, "sso", err)
		return
	}
	if !existed {
		httpx.NotFound(w, "sso connection")
		return
	}
	opDomainDelete.Write(w, ssoDomainDeleteResponse{Status: "deleted"})
}

// lastVerifiedWhileEnforced reports whether domainID is the org's only
// verified domain while one of its connections requires SSO. The caller holds
// the org's connection lock, which is what keeps the answer true until its
// delete commits.
func lastVerifiedWhileEnforced(ctx context.Context, sso *ssostore.Bundle, orgID, domainID string) (bool, error) {
	conns, err := sso.Connections.ListByOrg(ctx, orgID)
	if err != nil {
		return false, err
	}
	enforced := false
	for _, c := range conns {
		if c.Enforced {
			enforced = true
			break
		}
	}
	if !enforced {
		return false, nil
	}
	doms, err := sso.Domains.ListByOrg(ctx, orgID)
	if err != nil {
		return false, err
	}
	for _, d := range doms {
		if d.ID != domainID && d.VerifiedAt != nil {
			return false, nil
		}
	}
	return true, nil
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
