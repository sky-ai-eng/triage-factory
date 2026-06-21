package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// SSO break-glass management. A break-glass principal
// may still authenticate via its non-SSO (GitHub) identity under SSO
// enforcement — the recovery path if the IdP breaks, so they can un-enforce.
// The owner is the default (seeded when enforcement is first enabled); this
// surface lets an admin view the set and add/remove additional principals.
//
// The load-bearing safety rule lives in handleBreakGlassRemove: you can't
// remove the last break-glass principal while the connection is enforced — an
// org can never enforce itself into a no-recovery state (same shape as the
// can't-remove-the-last-owner guard).
//
// Multi-mode only and org-admin-gated, reusing ssoConnectionHandler.adminGate
// for the exact same authorization + org-scoping as the rest of the SSO admin
// surface (active org, org-admin, 404-on-non-admin non-disclosure).
type ssoBreakGlassHandler struct {
	tx   db.TxRunner
	az   *authz.Checker
	conn *ssoConnectionHandler // reused for adminGate + currentConnection
}

// breakGlassView is the wire shape of one break-glass principal.
type breakGlassView struct {
	UserID      string    `json:"user_id"`
	DisplayName string    `json:"display_name,omitempty"`
	Email       string    `json:"email,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func toBreakGlassView(p domain.SSOBreakGlassPrincipal) breakGlassView {
	return breakGlassView{
		UserID:      p.UserID,
		DisplayName: p.DisplayName,
		Email:       p.Email,
		CreatedAt:   p.CreatedAt,
	}
}

// handleBreakGlassList returns the org's break-glass principals.
//
// GET /api/sso/break-glass
func (h *ssoBreakGlassHandler) handleBreakGlassList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.conn.adminGate(w, r)
	if !ok {
		return
	}
	out, err := h.listViews(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listViews returns the org's break-glass principals as wire views. Pulled out
// so handleBreakGlassAdd can call it AFTER its write tx commits — List reads the
// admin pool (a separate connection from the tx-bound app pool), so a List run
// inside the add tx would not see the still-uncommitted Add.
func (h *ssoBreakGlassHandler) listViews(ctx context.Context, orgID, userID string) ([]breakGlassView, error) {
	var principals []domain.SSOBreakGlassPrincipal
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		principals, e = tx.SSOBreakGlass.List(ctx, orgID)
		return e
	}); err != nil {
		return nil, err
	}
	out := make([]breakGlassView, 0, len(principals))
	for _, p := range principals {
		out = append(out, toBreakGlassView(p))
	}
	return out, nil
}

// handleBreakGlassAdd designates an org member (resolved by verified email) as a
// break-glass principal. Resolving by email rather than by user id keeps the UI
// simple — there is no org-wide roster picker yet — and the org-membership
// filter in the resolver means an admin can only ever designate a member of
// their own org.
//
// POST /api/sso/break-glass  body: { "email": "person@corp.com" }
func (h *ssoBreakGlassHandler) handleBreakGlassAdd(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.conn.adminGate(w, r)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		badRequest(w, "email is required")
		return
	}

	var resolved string
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		uid, e := tx.SSOBreakGlass.ResolveVerifiedEmailMember(r.Context(), orgID, email)
		if e != nil {
			return e
		}
		resolved = uid
		if uid == "" {
			// No member with that verified email — nothing to add. Surfaced as a
			// 404 below; return early so we don't write a row.
			return nil
		}
		return tx.SSOBreakGlass.Add(r.Context(), orgID, uid)
	}); err != nil {
		internalError(w, "sso", err)
		return
	}
	if resolved == "" {
		// 404 (not 400): the request is well-formed, the principal just isn't a
		// member of this org with a verified email. Non-disclosure-friendly — it
		// reveals nothing about principals in other orgs.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no member of this organization has that verified email",
		})
		return
	}
	// List AFTER the add tx commits — List reads the admin pool, which wouldn't
	// see the still-uncommitted insert from inside the closure above.
	out, err := h.listViews(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBreakGlassRemove revokes a principal's break-glass designation. It
// refuses to remove the last break-glass principal while the connection is
// enforced — the no-recovery guard. Un-enforce first (which is always allowed),
// then the last one can be removed.
//
// DELETE /api/sso/break-glass/{user_id}
func (h *ssoBreakGlassHandler) handleBreakGlassRemove(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.conn.adminGate(w, r)
	if !ok {
		return
	}
	target := r.PathValue("user_id")
	if _, err := uuid.Parse(target); err != nil {
		badRequest(w, "invalid user id")
		return
	}

	// RemoveGuarded folds the enforced-check, count, presence, and delete into one
	// statement (one snapshot), so the "never enforced with zero break-glass"
	// invariant can't be raced by a concurrent enforce-toggle or sibling remove.
	var lastWhileEnforced bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		lastWhileEnforced, e = tx.SSOBreakGlass.RemoveGuarded(r.Context(), orgID, target)
		return e
	}); err != nil {
		internalError(w, "sso", err)
		return
	}
	if lastWhileEnforced {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "can't remove the last break-glass principal while SSO is required — stop requiring SSO first",
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
