package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// orgMembersHandler serves /api/orgs/{org_id}/members — the org People
// roster (TFAC-417) and its role-change / remove affordances. It mirrors
// teamsHandler's shape (transactional store runner + authz checker) and the
// /api/orgs/{org_id}/... gating the github-app handlers use: read is any-
// member, mutate is org-admin (or self for a leave), all 404-not-403 so a
// non-admin never learns the org exists.
//
// Multi-mode only: the whole surface 404s in local mode (N=1 has a single
// implicit owner and never mounts the page). The last-owner invariant is
// enforced by the tf.guard_org_owners DB trigger, never re-implemented here;
// the store surfaces its SQLSTATE 23514 as db.ErrLastOwnerGuard, which the
// mutators translate into a friendly 409.
type orgMembersHandler struct {
	tx db.TxRunner
	az *authz.Checker
}

// orgMemberRow is one roster row. github_username / jira_account_id are null
// when the member holds no host-scoped identity binding on the org's host
// (the frontend renders that as a "Not connected" readiness badge).
// is_current_user lets the frontend swap the Remove control for Leave on the
// caller's own row.
type orgMemberRow struct {
	UserID         string  `json:"user_id"`
	DisplayName    string  `json:"display_name"`
	GitHubUsername *string `json:"github_username"`
	JiraAccountID  *string `json:"jira_account_id"`
	Role           string  `json:"role"`
	IsCurrentUser  bool    `json:"is_current_user"`
}

type orgMembersResponse struct {
	Members []orgMemberRow `json:"members"`
}

// orgRoles is the closed set of org_role enum values the PATCH handler
// accepts. Validating here keeps an invalid value from reaching the column
// (where it would surface as a driver error, not a clean 400) and pins the
// vocabulary the frontend adapter mirrors.
var orgRoles = map[string]bool{"owner": true, "admin": true, "member": true}

// handleOrgMembersList returns every member of the org with their role and
// identity readiness. Any org member may read (org_memberships_select RLS),
// so it gates on membership, not admin.
//
// GET /api/orgs/{org_id}/members
func (h *orgMembersHandler) handleOrgMembersList(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := h.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var members []domain.OrgMember
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Identity is host-scoped: resolve the org's GitHub/Jira hosts from
		// org_settings, then look up each member's login on those hosts —
		// the same keys the per-user readers and PAT writers use.
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		members, e = tx.OrgMemberships.ListWithIdentity(r.Context(), orgID, orgSet.GitHubBaseURL, orgSet.JiraBaseURL)
		return e
	}); err != nil {
		internalError(w, "org-members", err)
		return
	}

	out := make([]orgMemberRow, len(members))
	for i, m := range members {
		out[i] = orgMemberRow{
			UserID:         m.UserID,
			DisplayName:    m.DisplayName,
			GitHubUsername: m.GitHubUsername,
			JiraAccountID:  m.JiraAccountID,
			Role:           m.Role,
			IsCurrentUser:  m.UserID == userID,
		}
	}
	writeJSON(w, http.StatusOK, orgMembersResponse{Members: out})
}

// handleOrgMemberRoleChange changes a member's org role. Org-admin only
// (org_memberships_update RLS + the RequireOrgAdmin front gate, both
// 404-not-403). Demoting the org's last owner is rejected by the guard
// trigger and surfaced as a 409 rather than a 500.
//
// PATCH /api/orgs/{org_id}/members/{user_id}  body: { "role": "<org_role>" }
func (h *orgMembersHandler) handleOrgMemberRoleChange(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	targetID, ok := orgMemberPathID(w, r)
	if !ok {
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}
	role := strings.TrimSpace(body.Role)
	if !orgRoles[role] {
		badRequest(w, "role must be one of owner, admin, member")
		return
	}

	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.OrgMemberships.UpdateRole(r.Context(), orgID, targetID, role)
	})
	if !writeOrgMemberMutationResult(w, "org-members", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOrgMemberRemove removes a member from the org. An org admin may
// remove anyone; any member may remove themselves (a self-leave) — the split
// org_memberships_delete RLS already permits. Removing the org's last owner
// is rejected by the guard trigger and surfaced as a 409.
//
// DELETE /api/orgs/{org_id}/members/{user_id}
func (h *orgMembersHandler) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return
	}
	// Membership is the floor: a non-member 404s (RequireOrgMember). Removing
	// someone *else* additionally requires admin; a self-leave does not.
	orgID, userID, ok := h.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	targetID, ok := orgMemberPathID(w, r)
	if !ok {
		return
	}
	if targetID != userID {
		isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
		if err != nil {
			internalError(w, "org-members", err)
			return
		}
		if !isAdmin {
			// 404-not-403: a non-admin doesn't learn the target exists.
			http.NotFound(w, r)
			return
		}
	}

	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.OrgMemberships.Remove(r.Context(), orgID, targetID)
	})
	if !writeOrgMemberMutationResult(w, "org-members", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orgMemberPathID extracts and validates the {user_id} path value. A
// malformed UUID is a 404 (same response as "no such member") so a bad path
// can't surface as a 500 from the uuid cast in the store query.
func orgMemberPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.PathValue("user_id")
	if _, err := uuid.Parse(raw); err != nil {
		http.NotFound(w, r)
		return "", false
	}
	return raw, true
}

// writeOrgMemberMutationResult renders the shared error mapping for the
// PATCH/DELETE mutators and reports whether the caller should proceed to
// write its success status. The last-owner guard (db.ErrLastOwnerGuard) is a
// 409; a missing target (db.ErrOrgMemberNotFound) is a 404; anything else is
// a redacted 500. Returns true only on nil err.
func writeOrgMemberMutationResult(w http.ResponseWriter, scope string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, db.ErrLastOwnerGuard):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "can't remove or demote the last owner — assign another owner first",
		})
	case errors.Is(err, db.ErrOrgMemberNotFound):
		notFound(w, "member")
	default:
		internalError(w, scope, err)
	}
	return false
}
