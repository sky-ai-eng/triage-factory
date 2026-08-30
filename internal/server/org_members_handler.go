package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// orgMembersHandler serves /api/orgs/{org_id}/members — the org People
// roster and its role-change / remove affordances. It mirrors
// teamsHandler's shape (transactional store runner + authz checker) and the
// /api/orgs/{org_id}/... gating the github/app handlers use: read is any-
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
	// ws is the websocket hub, used to actively close a removed member's
	// org-scoped connections (TFAC-75) so their event stream stops at
	// removal rather than lingering until the socket drops.
	ws *websocket.Hub
	// revokeUserOrgSessions revokes a removed member's auth sessions scoped
	// to one org (TFAC-487) — deprovisioning hygiene alongside the WS-kick.
	// It's a closure over the Server because s.authDeps.sessions is wired
	// late (SetAuthDeps runs after routes()), so the store can't be captured
	// at construction. routes() always wires a non-nil closure; the closure
	// itself nil-checks authDeps for the boot race (and local mode never
	// reaches it — handleOrgMemberRemove 404s first). The nil guard on the
	// field is belt-and-suspenders for a future caller that constructs the
	// handler without it (e.g. a test). Returns the count of revoked rows.
	revokeUserOrgSessions func(ctx context.Context, userID, orgID string) (int64, error)
	// revokeUserOrgTokens revokes a removed member's API tokens scoped to one
	// org — the headless half of the same deprovisioning, and a separate
	// closure from the session revoke above rather than a second statement
	// inside it so a failure of either still leaves the other to run. It takes
	// the removing admin as well as the target because each revocation writes
	// an audit row, and a log saying an admin revoked a token without saying
	// whose would be no audit at all. Same late-binding and nil-guard shape as
	// its sibling. Returns the count of revoked rows.
	revokeUserOrgTokens func(ctx context.Context, targetUserID, orgID, actorUserID string) (int, error)
	// publishKick fans the removal's WS-kick to every OTHER control pod
	// (TFAC-584), on top of the local ws.CloseUserConnections above. Same
	// late-binding closure shape as revokeUserOrgSessions — SetWSBackplane
	// runs after routes() — and the closure nil-checks wsBackplane itself
	// (local mode, or before wiring), so a nil-field test/caller is still
	// safe to omit this and get local-only behavior.
	publishKick func(ctx context.Context, userID, sid, orgID string, code int, reason string)
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

// orgMemberListRequest is the body of POST /api/orgs/{org_id}/members/list.
// The resource is the org's roster, which has no filters of its own.
type orgMemberListRequest struct {
	httpx.PageRequest
}

// orgRoles is the closed set of org_role enum values the PATCH handler
// accepts. Validating here keeps an invalid value from reaching the column
// (where it would surface as a driver error, not a clean 400) and pins the
// vocabulary the frontend adapter mirrors.
var orgRoles = map[string]bool{"owner": true, "admin": true, "member": true}

// handleOrgMembersList returns the org's members with their role and identity
// readiness. Any org member may read (org_memberships_select RLS), so it gates
// on membership, not admin.
//
// POST /api/orgs/{org_id}/members/list
func (h *orgMembersHandler) handleOrgMembersList(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// The org-membership surface is multi-mode only; a route absent in
		// this deployment mode is a 404.
		notFound(w, "route")
		return
	}
	orgID, userID, ok := h.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var req orgMemberListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		members []domain.OrgMember
		total   int
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Identity is host-scoped: resolve the org's GitHub/Jira hosts from
		// org_settings, then look up each member's login on those hosts —
		// the same keys the per-user readers and PAT writers use.
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		members, total, e = tx.OrgMemberships.ListWithIdentity(r.Context(), orgID,
			orgSet.GitHubBaseURL, orgSet.JiraBaseURL, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
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
	httpx.WriteList(w, page, out, total)
}

// handleOrgMemberRoleChange changes a member's org role. Org-admin only
// (org_memberships_update RLS + the RequireOrgAdmin front gate, both
// 404-not-403). Demoting the org's last owner is rejected by the guard
// trigger and surfaced as a 409 rather than a 500.
//
// PATCH /api/orgs/{org_id}/members/{user_id}  body: { "role": "<org_role>" }
func (h *orgMembersHandler) handleOrgMemberRoleChange(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// The org-membership surface is multi-mode only; a route absent in
		// this deployment mode is a 404.
		notFound(w, "route")
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
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	role := strings.TrimSpace(body.Role)
	if !orgRoles[role] {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "role must be one of owner, admin, member", Field: "role",
		})
		return
	}

	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		oldRole, err := tx.OrgMemberships.UpdateRole(r.Context(), orgID, targetID, role)
		if err != nil {
			return err
		}
		// TFAC-471: record the role change in the same tx — a log-write failure
		// rolls the role change back, so the audit can't diverge from reality.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionOrgRoleChanged,
			TargetUserID: targetID,
			DetailJSON:   accessDetailRoleChange(oldRole, role),
		})
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
		// The org-membership surface is multi-mode only; a route absent in
		// this deployment mode is a 404.
		notFound(w, "route")
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
			// The caller is a member of this org (RequireOrgMember is the
			// floor above), so the org is visible to them: name the missing
			// role rather than denying the org exists.
			forbidden(w, "org admin role required to remove another member")
			return
		}
	}

	err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// TFAC-471: audit the revoke BEFORE the delete. The audit row's RLS
		// WITH CHECK requires the writer to still have org access
		// (tf.user_has_org_access); a self-leave removes the actor's own
		// org_memberships row, so recording after Remove would fail the policy
		// (SQLSTATE 42501). Same tx → the audit row rolls back if Remove fails
		// (last-owner guard, missing target), so the ordering doesn't let a
		// phantom revoke persist.
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionOrgMemberRevoked,
			TargetUserID: targetID,
		}); err != nil {
			return err
		}
		return tx.OrgMemberships.Remove(r.Context(), orgID, targetID)
	})
	if !writeOrgMemberMutationResult(w, "org-members", err) {
		return
	}
	// Removal committed: actively close the removed member's connections
	// scoped to THIS org (TFAC-75). The org filter leaves a multi-org
	// user's connections to their other orgs alone. The client refreshes
	// and re-handshakes; handleWS's stale-active-org gate then stops it
	// from re-scoping back to the org it just lost. Logged for the
	// revocation audit trail (SOC2).
	n := h.ws.CloseUserConnections(targetID, "", orgID,
		websocket.CloseMembershipChanged, "membership changed")
	membershipLog.Info("kicked ws connections on membership removal",
		"user", targetID, "org", orgID,
		"code", int(websocket.CloseMembershipChanged), "n", n)
	// Cross-pod (TFAC-584): see orgMembersHandler.publishKick's doc comment.
	if h.publishKick != nil {
		h.publishKick(r.Context(), targetID, "", orgID,
			int(websocket.CloseMembershipChanged), "membership changed")
	}
	// Deprovisioning hygiene (TFAC-487): revoke the removed member's auth
	// sessions scoped to THIS org, best-effort, mirroring the WS-kick's org
	// filter (their sessions active in other orgs are untouched). Per-request
	// gates (RLS + role checks) already deny the org's data on the next
	// request, so a revoke failure isn't a data hole — log and continue.
	// routes() always wires the closure; the nil guard only matters for a
	// handler constructed without it (a test). Local mode never reaches here
	// — handleOrgMemberRemove 404s at the top.
	if h.revokeUserOrgSessions != nil {
		revoked, err := h.revokeUserOrgSessions(r.Context(), targetID, orgID)
		if err != nil {
			membershipLog.Warn("revoke sessions on membership removal failed",
				"user", targetID, "org", orgID, "error", err)
		} else {
			membershipLog.Info("revoked sessions on membership removal",
				"user", targetID, "org", orgID, "n", revoked)
		}
	}
	// The same hygiene for the removed member's headless credentials: an API
	// token is a session cursor with its org fixed, so a removal that killed
	// only the browser half would leave the org reachable by whatever
	// automation the member had running. Best-effort for the same reason —
	// the per-request gates deny the org's data either way — and independent
	// of the session revoke above so one failing does not skip the other.
	if h.revokeUserOrgTokens != nil {
		revoked, err := h.revokeUserOrgTokens(r.Context(), targetID, orgID, userID)
		if err != nil {
			membershipLog.Warn("revoke api tokens on membership removal failed",
				"user", targetID, "org", orgID, "error", err)
		} else {
			membershipLog.Info("revoked api tokens on membership removal",
				"user", targetID, "org", orgID, "n", revoked)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferOwnershipRequest is the POST body for the ownership-transfer
// endpoint: the user id of the member who becomes the new owner.
type transferOwnershipRequest struct {
	NewOwnerUserID string `json:"new_owner_user_id"`
}

// handleOrgOwnershipTransfer hands org ownership to another member. Owner-only:
// the founder sentinel orgs.owner_user_id is the authority (checked up front
// via tf.user_owns_org and reinforced by the guard_org_owner_transfer
// trigger), so a plain org admin can't reassign it. The work runs in one
// app-pool transaction AS THE CALLER so the trigger sees the owner's claims in
// tf.current_user_id(): promote the target to 'owner', repoint
// orgs.owner_user_id, then demote the former owner to 'admin' (the ordering
// the trigger demands — see OrgMembershipsStore.TransferOwnership).
//
// Multi-mode only (404 in local). Failure modes: 403 a non-owner caller, 400 a
// malformed/self target, 422 a target that isn't an org member, 409 a write
// that would leave the org without an owner.
//
// POST /api/orgs/{org_id}/transfer-ownership  body: { "new_owner_user_id": "<uuid>" }
func (h *orgMembersHandler) handleOrgOwnershipTransfer(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// The org-membership surface is multi-mode only; a route absent in
		// this deployment mode is a 404.
		notFound(w, "route")
		return
	}
	// Floor: a non-member 404s (no existence leak). Membership is checked
	// before ownership so a stranger never learns the org exists.
	orgID, userID, ok := h.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	// Owner-only. A non-owner *member* can already read the roster, so 403
	// (not 404) is honest here — and it matches the guard trigger's 42501.
	owns, err := h.az.UserOwnsOrg(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "org-transfer", err)
		return
	}
	if !owns {
		forbidden(w, "only the current owner can transfer ownership")
		return
	}

	var body transferOwnershipRequest
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	newOwner := strings.TrimSpace(body.NewOwnerUserID)
	if _, err := uuid.Parse(newOwner); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "new_owner_user_id must be a valid user id", Field: "new_owner_user_id",
		})
		return
	}
	if newOwner == userID {
		// Without this guard a self-transfer would promote-then-demote the same
		// row and trip guard_org_owners (a confusing 409). Reject it plainly.
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "you already own this org", Field: "new_owner_user_id",
		})
		return
	}

	// The target must already be an org member (else 422 — the picker only
	// offers members, so this is the defensive path). UserHasOrgAccess answers
	// "is newOwner a member of orgID"; pre-checking gives the friendly 422
	// rather than a rolled-back transaction.
	isMember, err := h.az.UserHasOrgAccess(r.Context(), newOwner, orgID)
	if err != nil {
		internalError(w, "org-transfer", err)
		return
	}
	if !isMember {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonCrossTeamRef, Message: "the new owner must be a member of this org", Field: "new_owner_user_id",
		})
		return
	}

	// Run as the owner: guard_org_owner_transfer reads tf.current_user_id(),
	// so the owner's claims must be in context (the app pool, not admin).
	err = h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.OrgMemberships.TransferOwnership(r.Context(), orgID, userID, newOwner); err != nil {
			return err
		}
		// TFAC-471: audit the transfer in the same tx. actor = the former owner
		// who initiated it; target = the member who became owner.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID:  userID,
			Action:       domain.AccessActionOrgOwnershipTransferred,
			TargetUserID: newOwner,
		})
	})
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, db.ErrNotOrgOwner):
		// The front gate caught the common case; this is the trigger's 42501
		// surfacing through a between-check-and-write ownership race.
		forbidden(w, "only the current owner can transfer ownership")
	case errors.Is(err, db.ErrLastOwnerGuard):
		conflict(w, "transfer would leave the org without an owner")
	case errors.Is(err, db.ErrOrgMemberNotFound):
		// Raced: the target left the org between the pre-check and the tx.
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonCrossTeamRef, Message: "the new owner must be a member of this org", Field: "new_owner_user_id",
		})
	default:
		internalError(w, "org-transfer", err)
	}
}

// orgMemberPathID extracts and validates the {user_id} path value. A
// malformed UUID is a 404 (same response as "no such member") so a bad path
// can't surface as a 500 from the uuid cast in the store query.
func orgMemberPathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	return uuidPathOr404(w, r, "user_id", "member")
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
		conflict(w, "can't remove or demote the last owner — assign another owner first")
	case errors.Is(err, db.ErrOrgMemberNotFound):
		notFound(w, "member")
	default:
		internalError(w, scope, err)
	}
	return false
}
