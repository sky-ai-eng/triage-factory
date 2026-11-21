package sso

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ssoBreakGlassHandler serves /api/sso/break-glass — the principals that
// may still authenticate via their non-SSO (GitHub) identity under SSO
// enforcement (the recovery path if the IdP breaks). Reuses
// ssoConnectionHandler.adminGate for identical authorization + org-scoping.
type ssoBreakGlassHandler struct {
	tx   db.TxRunner
	az   *authz.Checker
	conn *ssoConnectionHandler // reused for adminGate
}

type breakGlassView struct {
	UserID      string    `json:"user_id" doc:"The person's user id."`
	DisplayName string    `json:"display_name,omitempty" doc:"The person's display name, absent when they have none."`
	Email       string    `json:"email,omitempty" doc:"The person's email address, absent when none is known."`
	CreatedAt   time.Time `json:"created_at" doc:"When the person was made a break-glass principal, as an RFC 3339 timestamp."`
}

func toBreakGlassView(p ssostore.SSOBreakGlassPrincipal) breakGlassView {
	return breakGlassView{
		UserID:      p.UserID,
		DisplayName: p.DisplayName,
		Email:       p.Email,
		CreatedAt:   p.CreatedAt,
	}
}

var opBreakGlassList = httpx.NewJSONOp[httpx.NoBody, []breakGlassView](httpx.OpSpec{
	Pattern:     "GET /api/sso/break-glass",
	ID:          "list_sso_break_glass_principals",
	Summary:     "List break-glass principals",
	Description: "Returns every break-glass principal: the members who can still sign in without SSO while the organization requires it." + ssoAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// GET /api/sso/break-glass
func (h *ssoBreakGlassHandler) handleBreakGlassList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.conn.adminGate(w, r)
	if !ok {
		return
	}
	out, err := h.listViews(r.Context(), orgID, userID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	opBreakGlassList.Write(w, out)
}

// listViews returns the org's break-glass principals as wire views. Pulled
// out so handleBreakGlassAdd can call it AFTER its write tx commits — List
// reads the admin pool (a separate connection), so a List inside the add tx
// would not see the still-uncommitted Add.
func (h *ssoBreakGlassHandler) listViews(ctx context.Context, orgID, userID string) ([]breakGlassView, error) {
	var principals []ssostore.SSOBreakGlassPrincipal
	if err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		principals, e = ssostore.FromTx(tx).BreakGlass.List(ctx, orgID)
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

// breakGlassAddRequest is the body of POST /api/sso/break-glass.
type breakGlassAddRequest struct {
	Email string `json:"email" doc:"A verified email address of the organization member to make a break-glass principal, compared without regard to case. Required."`
}

var opBreakGlassAdd = httpx.NewJSONOp[breakGlassAddRequest, []breakGlassView](httpx.OpSpec{
	Pattern:     "POST /api/sso/break-glass",
	ID:          "create_sso_break_glass_principal",
	Summary:     "Add a break-glass principal",
	Description: "Makes the organization member with the given verified email address a break-glass principal, who can still sign in without SSO while the organization requires it, and returns every break-glass principal. Adding one who already is changes nothing." + ssoAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// POST /api/sso/break-glass  body: { "email": "person@corp.com" }
func (h *ssoBreakGlassHandler) handleBreakGlassAdd(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.conn.adminGate(w, r)
	if !ok {
		return
	}
	body, ok := opBreakGlassAdd.Decode(w, r)
	if !ok {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "email is required", Field: "email"})
		return
	}

	var resolved string
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		sso := ssostore.FromTx(tx)
		uid, e := sso.BreakGlass.ResolveVerifiedEmailMember(r.Context(), orgID, email)
		if e != nil {
			return e
		}
		resolved = uid
		if uid == "" {
			return nil
		}
		added, e := sso.BreakGlass.Add(r.Context(), orgID, uid)
		if e != nil {
			return e
		}
		if !added {
			return nil
		}
		// A break-glass principal keeps a working non-SSO login under
		// enforcement — the one standing exception to the org's login policy,
		// and the first thing to check when asking how someone got in without
		// the IdP. The email resolves to a user id here, so the row names the
		// principal as a target and needs no detail of its own.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			TargetUserID: uid,
			Action:       domain.AccessActionSSOBreakGlassAdded,
		})
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if resolved == "" {
		httpx.WriteErrors(w, http.StatusNotFound, httpx.ErrorItem{Reason: httpx.ReasonNotFound, Message: "no member of this organization has that verified email"})
		return
	}
	out, err := h.listViews(r.Context(), orgID, userID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	opBreakGlassAdd.Write(w, out)
}

var opBreakGlassRemove = httpx.NewNoContentOp[httpx.NoBody](httpx.OpSpec{
	Pattern:     "DELETE /api/sso/break-glass/{user_id}",
	ID:          "delete_sso_break_glass_principal",
	Summary:     "Remove a break-glass principal",
	Description: "Removes a member from the break-glass principals, and answers the same for a member who is not one. The last one cannot be removed while the organization requires SSO." + ssoAccess,
	PathParams:  map[string]string{"user_id": "The member's user id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// DELETE /api/sso/break-glass/{user_id}
func (h *ssoBreakGlassHandler) handleBreakGlassRemove(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.conn.adminGate(w, r)
	if !ok {
		return
	}
	target := r.PathValue("user_id")
	if _, err := uuid.Parse(target); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidID, Message: "user_id must be a UUID"})
		return
	}

	var lastWhileEnforced bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// RemoveGuarded reads the enforced flag and the list in one statement,
		// but a Require SSO toggle in flight has not committed its flag yet, and
		// it decides against the list as it was. The lock orders the two.
		if e := ssostore.FromTx(tx).Connections.LockForOrg(r.Context(), orgID); e != nil {
			return e
		}
		removed, blocked, e := ssostore.FromTx(tx).BreakGlass.RemoveGuarded(r.Context(), orgID, target)
		if e != nil {
			return e
		}
		lastWhileEnforced = blocked
		// Only a real deletion records. The guard-blocked case changed nothing,
		// and a DELETE for a principal that was never on the list is a no-op the
		// handler still answers 204 to.
		if !removed {
			return nil
		}
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			TargetUserID: target,
			Action:       domain.AccessActionSSOBreakGlassRemoved,
		})
	}); err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if lastWhileEnforced {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "can't remove the last break-glass principal while SSO is required — stop requiring SSO first"})
		return
	}
	opBreakGlassRemove.Write(w)
}
