package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// inviteTTL is how long a created invite stays redeemable. 7 days matches
// the ticket; re-inviting after expiry mints a fresh row + token.
const inviteTTL = 7 * 24 * time.Hour

// invitesHandler serves /api/invites — the org-admin invite lifecycle
// (create/list/revoke) plus the two redeem surfaces (preview/accept).
//
// The admin-facing handlers follow teams_handler.go: requireOrg for the
// active org, ClaimsFrom for the user, az.UserIsOrgAdmin for the gate (404
// not 403 on non-admin — non-disclosure), tx.WithTx for app-pool writes.
//
// The redeem handlers run on the admin pool: the actor is an outsider
// holding a token, with no membership, so the org_invites RLS policies
// (org-admin only) would reject every operation. The token, validated
// app-side, IS the authorization — the same BYPASSRLS rationale provisionOrg
// documents. invites is the admin-pooled store (GetByTokenHashSystem +
// IsOrgMemberSystem); admin is the BYPASSRLS *sql.DB the accept tx runs on.
type invitesHandler struct {
	tx      db.TxRunner
	az      *authz.Checker
	admin   *sql.DB
	invites db.InvitesStore

	// publicURL returns the externally-visible deployment base for building
	// accept_url. Read lazily: deployCfg is populated after routes() runs.
	publicURL func() string
	// setActiveOrg points the given session at orgID after a successful
	// accept so the recipient lands inside the org. Read lazily: authDeps is
	// populated after routes() runs.
	setActiveOrg func(ctx context.Context, sessID, orgID uuid.UUID) error
}

// createInviteRequest is the POST /api/invites body. target_team_id is
// optional — omitted/empty means an org-only invite (member of the org, on
// zero teams).
type createInviteRequest struct {
	Email        string `json:"email"`
	Role         string `json:"role"`
	TargetTeamID string `json:"target_team_id"`
}

// createInviteResponse echoes the new invite's id, its single-use accept_url
// (shown ONCE — the raw token is never persisted or returned again), and the
// expiry.
type createInviteResponse struct {
	ID        string    `json:"id"`
	AcceptURL string    `json:"accept_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleInviteCreate mints a pending invite + one-time accept_url.
//
// POST /api/invites  body: { email, role, target_team_id? }
func (ih *invitesHandler) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// The invite surface is multi-mode only; a route absent in this
		// deployment mode is a 404.
		notFound(w, "route")
		return
	}
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	isAdmin, err := ih.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "invites", err)
		return
	}
	if !isAdmin {
		// The caller is a member of this org (requireOrg resolved it from
		// their session), so name the missing role rather than hiding it.
		forbidden(w, "org admin role required")
		return
	}

	// The accept_url is the whole point of this endpoint, so refuse early if
	// the deployment's public base isn't configured — better a clear 500 than
	// handing back a relative path the recipient can't use. Matches the
	// s.deployCfg == nil guard on the other public-URL handlers (github_app,
	// jira_connect, github_connect). In practice SetDeployConfig runs at boot
	// before requests are served, so this should never fire.
	base := strings.TrimRight(ih.publicURL(), "/")
	if base == "" {
		internalError(w, "invites", fmt.Errorf("public URL not configured"))
		return
	}

	var body createInviteRequest
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "email is required", Field: "email",
		})
		return
	}
	// Shape-check the address. Unvalidated, a typo minted a durable invite
	// nobody could ever redeem — the accept path matches the caller's verified
	// email exactly, so an address that can't be an address is a ghost row.
	if _, err := mail.ParseAddress(email); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "email must be a valid email address", Field: "email",
		})
		return
	}

	// Reject inviting someone who is already a member of THIS org. The only
	// create-time uniqueness guard otherwise is org_invites_active_uniq, which
	// dedups invite-vs-invite, not invite-vs-existing-member — so without this
	// an admin could mint a pending invite for a current member: a dead ghost
	// row whose sole outcome on accept is the idempotent "you're already a
	// member" short-circuit in handleInviteAccept. We match on a *verified*
	// identity email, mirroring the principal-link rule: if it WOULD link on
	// accept, block on create; an unverified identity email isn't a reliable
	// dedup key. Scoped to THIS org only — inviting an existing TF user who
	// belongs to another org is legitimate, and accept grants them a fresh
	// membership. This is fail-fast UX, not a correctness gate: the accept-time
	// member check in handleInviteAccept stays as the TOCTOU backstop for an
	// email JIT-provisioned between create and accept. Runs on the admin pool
	// for the same reason as the target-team check below — user_identities is
	// admin-pool-only.
	var alreadyMember bool
	if err := ih.admin.QueryRowContext(r.Context(),
		`SELECT EXISTS (
			SELECT 1 FROM public.org_memberships m
			JOIN public.user_identities i ON i.user_id = m.user_id
			WHERE m.org_id = $1 AND lower(i.email) = $2 AND i.email_verified
		)`,
		orgID, email,
	).Scan(&alreadyMember); err != nil {
		internalError(w, "invites", fmt.Errorf("check existing member: %w", err))
		return
	}
	if alreadyMember {
		conflict(w, "that person is already a member of this org")
		return
	}

	// Role ceiling. We only accept admin|member; 'owner' is the founder
	// sentinel on orgs.owner_user_id, never granted via invite (the schema
	// CHECK enforces this too). The caller is already org-admin (owner OR
	// admin via UserIsOrgAdmin), and the only role above 'admin' is 'owner',
	// which is excluded for everyone — so "role ≤ caller's org role" reduces
	// to this membership check with no need to fetch the caller's exact role.
	role := strings.TrimSpace(body.Role)
	if role == "" {
		role = "member"
	}
	if role != "admin" && role != "member" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "role must be 'admin' or 'member'", Field: "role",
		})
		return
	}
	targetTeamID := strings.TrimSpace(body.TargetTeamID)
	if targetTeamID != "" {
		if _, err := uuid.Parse(targetTeamID); err != nil {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Message: "target_team_id must be a valid UUID", Field: "target_team_id",
			})
			return
		}
	}

	// 32 random bytes → base64url raw token. Stored only as sha256(token);
	// the raw value escapes exactly once, in the accept_url below.
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		internalError(w, "invites", fmt.Errorf("generate token: %w", err))
		return
	}
	rawToken := base64.RawURLEncoding.EncodeToString(rawBytes)
	tokenHash := sha256.Sum256([]byte(rawToken))
	expiresAt := timeNow().Add(inviteTTL)

	// Validate any target team belongs to THIS org before binding the invite
	// to it. The org_invites_insert RLS policy gates org-admin but NOT that
	// target_team_id is in-org, so without this a cross-org team id would
	// slip through and accept would write a membership in another org. This
	// runs on the admin pool (existence check by explicit org_id) rather than
	// the app pool because an org admin may invite to a team they don't
	// personally belong to — teams_select RLS returns every in-org team, but
	// a membership-scoped read would falsely reject that legitimate case.
	if targetTeamID != "" {
		var belongs bool
		if err := ih.admin.QueryRowContext(r.Context(),
			`SELECT EXISTS (SELECT 1 FROM teams WHERE id = $1 AND org_id = $2)`,
			targetTeamID, orgID,
		).Scan(&belongs); err != nil {
			internalError(w, "invites", fmt.Errorf("verify target team: %w", err))
			return
		}
		if !belongs {
			badRequest(w, "target team not found in org")
			return
		}
	}

	var inviteID string
	if err := ih.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		inviteID, e = tx.Invites.Create(r.Context(), domain.CreateInviteParams{
			OrgID:        orgID,
			Email:        email,
			Role:         role,
			TargetTeamID: targetTeamID,
			TokenHash:    tokenHash[:],
			InvitedBy:    userID,
			ExpiresAt:    expiresAt,
		})
		if e != nil {
			return e
		}
		// An outstanding invite is granted access waiting to be
		// claimed — the log has to show it at issue time, not only when the
		// accept lands (an invite that is revoked or expires unaccepted would
		// otherwise never appear at all). The later org_member_granted row
		// carries the same invite id, closing the loop.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionInviteCreated,
			TeamID:      targetTeamID,
			DetailJSON:  accessDetailInviteCreated(inviteID, email, role),
		})
	}); err != nil {
		if isUniqueViolation(err) {
			// org_invites_active_uniq — a pending invite to this address
			// already exists. Re-inviting after revoke/accept is allowed.
			conflict(w, "an invite for that email is already pending")
			return
		}
		internalError(w, "invites", err)
		return
	}

	// base was validated non-empty above, before any row was written. The raw
	// token rides in the query string — standard for copy/paste link invites
	// and the right call for v1 (no SMTP), but it means the token can land in
	// access logs, browser history, and Referer headers. When the frontend
	// slice + deploy docs land, note that operators should scrub log patterns
	// for /invite/accept. Mitigations already in place: 7-day expiry and
	// single-use (accepted_at stamped on redeem).
	acceptURL := base + "/invite/accept?token=" + rawToken
	writeJSON(w, http.StatusCreated, createInviteResponse{
		ID:        inviteID,
		AcceptURL: acceptURL,
		ExpiresAt: expiresAt,
	})
}

// inviteListItem is the wire shape of one active invite — the list rows and
// the single read alike.
//
// It deliberately carries no token and no accept URL. Those exist exactly once,
// in the create response, and are the invite's only secret; a read that
// returned them would make every admin list a re-issuance and would put live
// tokens in whatever caches and logs a GET passes through.
type inviteListItem struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Role         string    `json:"role"`
	TargetTeamID string    `json:"target_team_id,omitempty"`
	InvitedBy    string    `json:"invited_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func inviteToJSON(iv domain.OrgInvite) inviteListItem {
	return inviteListItem{
		ID:           iv.ID,
		Email:        iv.Email,
		Role:         iv.Role,
		TargetTeamID: iv.TargetTeamID,
		InvitedBy:    iv.InvitedBy,
		CreatedAt:    iv.CreatedAt,
		ExpiresAt:    iv.ExpiresAt,
	}
}

// inviteListRequest is the body of POST /api/invites/list. The resource is the
// org's redeemable invites, which has no filters of its own.
type inviteListRequest struct {
	httpx.PageRequest
}

// handleInviteList returns the active invites for the active org.
//
// POST /api/invites/list
func (ih *invitesHandler) handleInviteList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := ih.requireOrgAdmin(w, r)
	if !ok {
		return
	}

	var req inviteListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		invites []domain.OrgInvite
		total   int
	)
	if err := ih.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		invites, total, e = tx.Invites.ListActive(r.Context(), orgID, db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "invites", err)
		return
	}

	out := make([]inviteListItem, len(invites))
	for i, iv := range invites {
		out[i] = inviteToJSON(iv)
	}
	httpx.WriteList(w, page, out, total)
}

// handleInviteGet is the canonical single read: one active invite, in the
// list's row shape. An invite that has been accepted, revoked, or expired is
// 404 — the same predicate the list applies, so a row an admin cannot see in
// the table cannot be opened out of it either.
//
// GET /api/invites/{id}
func (ih *invitesHandler) handleInviteGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := ih.requireOrgAdmin(w, r)
	if !ok {
		return
	}
	inviteID, ok := uuidPathOr404(w, r, "id", "invite")
	if !ok {
		return
	}

	var invite *domain.OrgInvite
	if err := ih.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		invite, e = tx.Invites.GetActive(r.Context(), orgID, inviteID)
		return e
	}); err != nil {
		internalError(w, "invites", err)
		return
	}
	if invite == nil {
		notFound(w, "invite")
		return
	}
	writeJSON(w, http.StatusOK, inviteToJSON(*invite))
}

// requireOrgAdmin is the shared front gate for the invite reads and the
// revoke: multi-mode only, an active org, and an org-admin caller. A plain
// member gets 403 rather than 404 — requireOrg resolved the org from their own
// session, so the org is plainly visible to them and only the role is missing.
func (ih *invitesHandler) requireOrgAdmin(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		// The invite surface is multi-mode only; a route absent in this
		// deployment mode is a 404.
		notFound(w, "route")
		return "", "", false
	}
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject

	isAdmin, err := ih.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		internalError(w, "invites", err)
		return "", "", false
	}
	if !isAdmin {
		forbidden(w, "org admin role required")
		return "", "", false
	}
	return orgID, userID, true
}

// handleInviteRevoke revokes a pending invite. Idempotent; revoking an
// accepted invite → 409.
//
// POST /api/invites/{id}/revoke
func (ih *invitesHandler) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := ih.requireOrgAdmin(w, r)
	if !ok {
		return
	}

	inviteID, ok := uuidPathOr404(w, r, "id", "invite")
	if !ok {
		return
	}

	var outcome db.RevokeOutcome
	if err := ih.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Resolve the address BEFORE revoking, so the audit row can name who
		// lost the invite. Best-effort: a miss (already revoked, or expired out
		// of the active set) just leaves the email off the row rather than
		// failing the revoke.
		email := ""
		if iv, e := tx.Invites.GetActive(r.Context(), orgID, inviteID); e == nil && iv != nil {
			email = iv.Email
		}
		var e error
		outcome, e = tx.Invites.Revoke(r.Context(), orgID, inviteID)
		if e != nil {
			return e
		}
		// Only a real revoke is a change. A 404/already-accepted outcome altered
		// nothing, and logging it would put access changes in the log that never
		// happened.
		if outcome != db.RevokeOK {
			return nil
		}
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionInviteRevoked,
			DetailJSON:  accessDetailInviteRevoked(inviteID, email),
		})
	}); err != nil {
		internalError(w, "invites", err)
		return
	}
	switch outcome {
	case db.RevokeNotFound:
		notFound(w, "invite")
	case db.RevokeAlreadyAccepted:
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonAlreadyTerminal, Message: "invite has already been accepted",
		})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	}
}

// invitePreviewResponse is GET /api/invites/preview. The token is the bearer
// secret, so org name + role to its holder is fine; everything else stays
// out. status disambiguates the redeemability for the accept page.
type invitePreviewResponse struct {
	OrgName       string     `json:"org_name,omitempty"`
	Role          string     `json:"role,omitempty"`
	InvitedByName string     `json:"invited_by_name,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	// Status ∈ {valid, expired, revoked, accepted, not_found}.
	Status string `json:"status"`
}

// handleInvitePreview resolves a token to a preview WITHOUT a session — the
// recipient hasn't authenticated yet. Admin pool (no membership to gate on).
//
// GET /api/invites/preview?token=…
func (ih *invitesHandler) handleInvitePreview(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// The invite surface is multi-mode only; a route absent in this
		// deployment mode is a 404.
		notFound(w, "route")
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: "token is required", Field: "token",
		})
		return
	}
	hash := sha256.Sum256([]byte(token))
	invite, err := ih.invites.GetByTokenHashSystem(r.Context(), hash[:])
	if err != nil {
		internalError(w, "invites", err)
		return
	}
	if invite == nil {
		// Don't leak anything beyond "no such token".
		writeJSON(w, http.StatusOK, invitePreviewResponse{Status: "not_found"})
		return
	}
	exp := invite.ExpiresAt
	writeJSON(w, http.StatusOK, invitePreviewResponse{
		OrgName:       invite.OrgName,
		Role:          invite.Role,
		InvitedByName: invite.InvitedByName,
		ExpiresAt:     &exp,
		Status:        inviteStatus(invite),
	})
}

// handleInviteAccept is the redeem state machine. Authenticated (any
// session); runs on the admin pool because the invitee has no membership, so
// the org_memberships_insert RLS policy would reject the grant under the app
// pool. The token, validated here, IS the authorization.
//
// POST /api/invites/accept  body: { token }
func (ih *invitesHandler) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	if runmode.Current() == runmode.ModeLocal {
		// The invite surface is multi-mode only; a route absent in this
		// deployment mode is a 404.
		notFound(w, "route")
		return
	}
	// Accepting an invite grants membership in a DIFFERENT org and repoints the
	// session cursor at it — both outside anything a token, sealed to one org
	// at mint, can express. It also reads the login's verified email, a claim
	// the token path does not carry.
	if refuseTokenAuth(w, r, "accepting an invite requires a session: it joins an org an API token is not scoped to, and moves the session cursor there") {
		return
	}
	claims := ClaimsFrom(r.Context())
	if claims == nil || claims.Subject == runmode.LocalDefaultUserID {
		writeUnauth(w)
		return
	}
	inviteeID, err := uuid.Parse(claims.Subject)
	if err != nil {
		writeUnauth(w)
		return
	}
	sess := SessionFrom(r.Context())
	if sess == nil {
		invitesLog.Error("invite accept: no session in context, route missing withsession")
		writeUnauth(w)
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	// Trim first and hash what was validated: the emptiness check used to trim
	// while the hash used the raw value, so a token with stray whitespace
	// passed the check and then matched nothing.
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "token is required", Field: "token",
		})
		return
	}

	hash := sha256.Sum256([]byte(body.Token))
	invite, err := ih.invites.GetByTokenHashSystem(r.Context(), hash[:])
	if err != nil {
		internalError(w, "invites", err)
		return
	}
	if invite == nil {
		notFound(w, "invite")
		return
	}

	// State gates, in priority order.
	if invite.RevokedAt != nil {
		conflict(w, "this invitation has been revoked")
		return
	}
	if invite.AcceptedAt != nil {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonAlreadyTerminal, Message: "this invitation has already been accepted",
		})
		return
	}
	if timeNow().After(invite.ExpiresAt) {
		conflict(w, "this invitation has expired")
		return
	}

	// Strict email match against the JWT *primary* email. Matching the full
	// GitHub verified-email set is a separate follow-up (capture
	// provider_token at the OAuth callback, match against /user/emails).
	callerEmail := strings.ToLower(strings.TrimSpace(claims.Email))
	if callerEmail == "" || callerEmail != invite.Email {
		// Its own reason rather than the former private invited_email key:
		// the message names both addresses, and the client keys the
		// switch-account affordance on the code.
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason: httpx.ReasonInviteEmailMismatch,
			Message: fmt.Sprintf(
				"This invitation was sent to %s, but you're signed in as %s. "+
					"Sign in with %s to accept it, or ask your admin to re-invite %s.",
				invite.Email, displayEmail(callerEmail), invite.Email, invite.Email,
			),
		})
		return
	}

	orgUUID, err := uuid.Parse(invite.OrgID)
	if err != nil {
		internalError(w, "invites", fmt.Errorf("parse invite org_id: %w", err))
		return
	}

	// Idempotent "already a member": stamp the accept (audit) and land them
	// in the org without re-granting. NOTE: this does NOT upgrade the existing
	// membership's role — an existing 'member' who accepts an 'admin' invite
	// stays a 'member' (see grantOrgMembership's LIMITATION note). Re-roling an
	// existing member is the org roster's job, not the invite flow's.
	alreadyMember, err := ih.invites.IsOrgMemberSystem(r.Context(), claims.Subject, invite.OrgID)
	if err != nil {
		internalError(w, "invites", err)
		return
	}
	if alreadyMember {
		if _, err := ih.admin.ExecContext(r.Context(), `
			UPDATE public.org_invites
			   SET accepted_at = now(), accepted_by = $2
			 WHERE id = $1 AND accepted_at IS NULL
		`, invite.ID, inviteeID); err != nil {
			internalError(w, "invites", fmt.Errorf("stamp accepted (already member): %w", err))
			return
		}
		ih.finishAccept(r.Context(), sess.ID, orgUUID)
		writeJSON(w, http.StatusOK, map[string]string{"org_id": invite.OrgID})
		return
	}

	// Grant + stamp atomically on the admin pool. grantOrgMembership writes
	// the org_memberships row and, if target_team_id is set, the team
	// membership; the org_invites UPDATE marks the invite redeemed. The
	// WHERE accepted_at IS NULL guard makes a concurrent double-redeem a
	// no-op on the second writer.
	var teamID uuid.NullUUID
	if invite.TargetTeamID != "" {
		tid, perr := uuid.Parse(invite.TargetTeamID)
		if perr != nil {
			internalError(w, "invites", fmt.Errorf("parse target_team_id: %w", perr))
			return
		}
		teamID = uuid.NullUUID{UUID: tid, Valid: true}
	}

	tx, err := ih.admin.BeginTx(r.Context(), nil)
	if err != nil {
		internalError(w, "invites", fmt.Errorf("begin accept tx: %w", err))
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := grantOrgMembership(r.Context(), tx, inviteeID, orgUUID, invite.Role, teamID, "member"); err != nil {
		internalError(w, "invites", err)
		return
	}
	// TFAC-471: audit the membership grant in the SAME admin-pool tx as the
	// grant. The actor is the invitee themselves (they redeemed the link); the
	// invite id rides in detail_json. team_id is set when the invite also
	// placed them on a team. This is the raw-execer path because the grant runs
	// on the admin pool (the invitee has no RLS standing to insert their own
	// membership), so it can't route through the app-pool AccessChangeLogStore.
	teamIDStr := ""
	if teamID.Valid {
		teamIDStr = teamID.UUID.String()
	}
	if err := recordAccessChangeTx(r.Context(), tx, orgUUID.String(), domain.AccessChange{
		ActorUserID:  inviteeID.String(),
		Action:       domain.AccessActionOrgMemberGranted,
		TargetUserID: inviteeID.String(),
		TeamID:       teamIDStr,
		DetailJSON:   accessDetailInvite(invite.ID),
	}); err != nil {
		internalError(w, "invites", fmt.Errorf("audit member granted: %w", err))
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
		UPDATE public.org_invites
		   SET accepted_at = now(), accepted_by = $2
		 WHERE id = $1 AND accepted_at IS NULL
	`, invite.ID, inviteeID); err != nil {
		internalError(w, "invites", fmt.Errorf("stamp accepted: %w", err))
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(w, "invites", fmt.Errorf("commit accept: %w", err))
		return
	}

	ih.finishAccept(r.Context(), sess.ID, orgUUID)
	writeJSON(w, http.StatusOK, map[string]string{"org_id": invite.OrgID})
}

// finishAccept points the session at the newly-joined org so the recipient
// lands inside it. Log-and-continue: the membership exists either way, so the
// recipient can switch explicitly if this rare path fails.
func (ih *invitesHandler) finishAccept(ctx context.Context, sessID, orgID uuid.UUID) {
	if ih.setActiveOrg == nil {
		return
	}
	if err := ih.setActiveOrg(ctx, sessID, orgID); err != nil {
		invitesLog.Warn("invite accept: set active org failed", "org", orgID, "error", err)
	}
}

// inviteStatus reduces an invite's terminal/expiry columns to the preview
// status enum. Revoked and accepted are terminal and take precedence over
// expiry; an un-terminal invite past expires_at is "expired", else "valid".
func inviteStatus(iv *domain.OrgInvite) string {
	switch {
	case iv.RevokedAt != nil:
		return "revoked"
	case iv.AcceptedAt != nil:
		return "accepted"
	case timeNow().After(iv.ExpiresAt):
		return "expired"
	default:
		return "valid"
	}
}

// displayEmail renders an empty caller email as a readable placeholder in the
// mismatch message rather than a dangling "signed in as .".
func displayEmail(e string) string {
	if e == "" {
		return "an account with no email"
	}
	return e
}
