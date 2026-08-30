// Package authz is the org/team authorization layer for the Triage Factory
// server: the shared gates and membership checks several handler domains run
// before touching team- or org-scoped state. It owns the friendly front gates
// that translate a failed check into the right 403/404, the resolve-error
// renderer they share, and the SQL those checks run.
//
// The gates hold no SQL. Every fact they need about a caller comes from the
// membership seam (membership.go), which is where the deployment shape is
// decided and where the reads themselves live — they stay in this package
// rather than behind db.Stores because each is an authorization input read in
// the same claims transaction as the probe it accompanies, and moving one onto
// a different pool is a trade to make deliberately.
//
// It imports the httpx kernel for responses + request identity and depends
// otherwise only on leaf packages (db, runmode), so handler subpackages can
// import it without pulling in package server.
package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Checker performs the org/team authorization checks: the friendly front gates
// that turn a failed check into the right 403 or 404, and the raw probes a
// handler composing its own decision calls directly.
//
// No method here asks what mode it is in. Every fact about who the caller is
// comes from the membership seam, resolved once for this deployment, so a gate
// states its rule — "an admin may do this" — and the seam states what that
// means where the process is running. Adding a gate that needs a new fact is a
// change to that interface, which the local implementation has to answer before
// anything builds.
//
// It holds the pool the seam reads from and the transactional store runner for
// ResolveTeamID's default-team lookup, the one check with a store method behind
// it; handlers hold a single *Checker instead of re-deriving these checks
// against their own store fields.
type Checker struct {
	db *sql.DB
	tx db.TxRunner
}

// New builds a Checker over the raw pool and the transactional store runner.
func New(database *sql.DB, tx db.TxRunner) *Checker {
	return &Checker{db: database, tx: tx}
}

// members is the seam every gate reads its facts through, resolved per call so
// the zero-value Checker several tests build behaves like a real one.
func (az *Checker) members() membership {
	if runmode.Current() == runmode.ModeLocal {
		return localMembership{}
	}
	return pgMembership{db: az.db}
}

// DefaultTeamAlias is the literal a {team_id} path segment may carry instead of
// a uuid. It resolves to the org's only team, and only in local mode — see
// ResolveTeamID.
const DefaultTeamAlias = "default"

// ResolveTeamID converts a raw {team_id} path value to a concrete team UUID.
// It is the ONE grammar for that segment: every route with a {team_id} goes
// through it, so a path that works on one team route works on all of them.
//
//   - Local mode additionally accepts the literal "default", which resolves to
//     the org's only team. N=1 has no picker and no uuid a user could know, so
//     without the alias the team routes would be unaddressable by hand.
//   - Multi mode requires a uuid everywhere. There is no single "the" team to
//     alias, and a magic value that silently retargets a write to whichever
//     team happens to be oldest is the worst kind of default.
//
// A failed resolve returns a *resolveError; render it with WriteResolveError.
// A malformed segment is not-found rather than a 400: a path id that cannot
// name a row names nothing the caller may learn about (the disclosure rule),
// and the answer is then identical across dialects.
func (az *Checker) ResolveTeamID(ctx context.Context, orgID, userID, raw string) (string, error) {
	if raw != DefaultTeamAlias || runmode.Current() != runmode.ModeLocal {
		if _, err := uuid.Parse(raw); err != nil {
			return "", &resolveError{notFound: true, err: fmt.Errorf("invalid team_id")}
		}
		return raw, nil
	}
	var teamID string
	err := az.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		teamID, e = tx.Teams.GetDefaultForOrg(ctx, orgID)
		return e
	})
	if err != nil {
		return "", &resolveError{notFound: false, err: err}
	}
	if teamID == "" {
		return "", &resolveError{notFound: true, err: fmt.Errorf("org %s has no default team", orgID)}
	}
	return teamID, nil
}

// VerifyTeamInOrg confirms that team_id belongs to the active org.
// Returns 404 (not 403) to avoid leaking team existence across orgs.
func (az *Checker) VerifyTeamInOrg(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	belongs, err := az.members().teamInOrg(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("team-in-org check %s/%s: %w", teamID, orgID, err))
		return false
	}
	if !belongs {
		httpx.NotFound(w, "team")
		return false
	}
	return true
}

// archivedTeamMessage is the 403 body the archive write-block returns. An
// archived team (TFAC-448) is intentionally read-only-and-vanished: the write
// isn't an error to retry, it's a lifecycle boundary, so name it clearly so the
// frontend can prompt "restore first" rather than surfacing a generic failure.
const archivedTeamMessage = "team is archived: restore it before making changes"

// VerifyTeamNotArchived blocks a team-scoped write when teamID is archived
// (teams.deleted_at IS NOT NULL). It sits next to VerifyTeamInOrg on the
// team-settings-family handlers, which gate on tf.user_is_team_admin and so
// don't pick up the archived filter baked into tf.user_can_write_team (the DB
// backstop covering the task / prompt / delegate write paths). Writes a 403
// "team is archived" and returns false when archived; returns true otherwise.
//
// A missing team is treated as not-archived: VerifyTeamInOrg is the authority on
// existence and runs first, and a vanished team's write fails downstream
// regardless. teams_select RLS gates on org access, not deleted_at, so an
// archived team is still visible to the read.
func (az *Checker) VerifyTeamNotArchived(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	archived, err := az.members().teamArchived(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("team-archived check %s/%s: %w", teamID, orgID, err))
		return false
	}
	if archived {
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
			Reason: httpx.ReasonTeamArchived, Message: archivedTeamMessage,
		})
		return false
	}
	return true
}

// RequireTeamAdmin checks the user is an admin of the given team.
// Returns 403 on non-admin.
func (az *Checker) RequireTeamAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	isAdmin, err := az.members().userIsTeamAdmin(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("team-admin check %s/%s/%s: %w", userID, orgID, teamID, err))
		return false
	}
	if !isAdmin {
		writeForbidden(w, "team admin role required")
		return false
	}
	return true
}

// RequireOrgAdminRole checks the user is an admin of the given org.
// Returns 403 on non-admin.
//
// Unlike RequireOrgAdmin the org arrives as an argument, and most callers pass
// the request's own cursor — for those the token-scope check below is
// trivially true, since the cursor IS the token's org. It earns its place for
// the handful that let the CALLER name an org (the fleet placement and queue
// reads take it from ?org=), where a token would otherwise reach an org it was
// never sealed to just by asking for it. 404 rather than the 403 beneath it,
// per the disclosure rule.
func (az *Checker) RequireOrgAdminRole(w http.ResponseWriter, r *http.Request, orgID, userID string) bool {
	if !tokenScopeAllows(r, orgID) {
		httpx.NotFound(w, "org")
		return false
	}
	isAdmin, err := az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("org-admin check %s/%s: %w", userID, orgID, err))
		return false
	}
	if !isAdmin {
		writeForbidden(w, "org admin role required")
		return false
	}
	return true
}

// OrgMemberCount returns the number of members in the org.
func (az *Checker) OrgMemberCount(ctx context.Context, orgID, userID string) (int, error) {
	return az.members().orgMemberCount(ctx, orgID, userID)
}

// TeamMemberCountAndRole returns the team's member count and the caller's
// role in it ("admin" / "member", or "" when not a member).
func (az *Checker) TeamMemberCountAndRole(ctx context.Context, orgID, userID, teamID string) (int, string, error) {
	return az.members().teamMemberCountAndRole(ctx, orgID, userID, teamID)
}

// UserHasOrgAccess reports whether the user is a member of the org. The raw
// probe behind RequireOrgMember, for a handler composing its own decision.
func (az *Checker) UserHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error) {
	return az.members().userHasOrgAccess(ctx, userID, orgID)
}

// UserIsOrgAdmin returns true when the calling user holds an 'owner' or 'admin'
// role in the given org. Used by endpoints that gate on org-admin privilege
// (GitHub App registration, team management).
func (az *Checker) UserIsOrgAdmin(ctx context.Context, userID, orgID string) (bool, error) {
	return az.members().userIsOrgAdmin(ctx, userID, orgID)
}

// UserIsTeamAdmin returns true when the calling user holds the 'admin' role in
// the given team. The raw-probe sibling of RequireTeamAdmin, for endpoints that
// OR team-admin with another right — the team roster's "team admin OR org
// admin" mutate gate — and compose the decision themselves.
func (az *Checker) UserIsTeamAdmin(ctx context.Context, userID, orgID, teamID string) (bool, error) {
	return az.members().userIsTeamAdmin(ctx, orgID, userID, teamID)
}

// viewOnlyMessage is the 403 body the team-write gates return. The viewer role
// is read-only by design (TFAC-447), so a write attempt isn't an error to fix —
// it's a role boundary. Keep the copy non-hostile and actionable: name the
// boundary ("view-only access") so the frontend can surface it gracefully if a
// stale/forced mutation slips past the affordance gating.
const viewOnlyMessage = "view-only access: your role on this team is read-only"

// UserCanWriteTeam reports whether the calling user may perform team-scoped
// writes on the given team — a member with the 'admin' or 'member' role, i.e.
// NOT a viewer. The write-path sibling of the membership-only check the read
// gates use, behind the same helper the team-scoped write RLS policies enforce.
func (az *Checker) UserCanWriteTeam(ctx context.Context, userID, orgID, teamID string) (bool, error) {
	return az.members().userCanWriteTeam(ctx, orgID, userID, teamID)
}

// RequireTeamWrite is the friendly front gate for a team-scoped write: it writes
// a 403 "view-only access" and returns false when the caller is a viewer (or not
// a member) of teamID. The team-scoped write RLS policies enforce this at the
// row level regardless; this gate exists so a viewer reaching a write handler
// gets a clean, role-named 403 instead of a generic RLS-blocked error (a
// silently-zero-rows UPDATE or a WITH CHECK violation surfaced as a 500).
func (az *Checker) RequireTeamWrite(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	canWrite, err := az.UserCanWriteTeam(r.Context(), userID, orgID, teamID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("team-write check %s/%s/%s: %w", userID, orgID, teamID, err))
		return false
	}
	if !canWrite {
		writeForbidden(w, viewOnlyMessage)
		return false
	}
	return true
}

// RequireTaskWrite is the front gate for a write against an existing task whose
// team isn't in the URL (the swipe / snooze / requeue / advance family). It
// mirrors the team branches of the tasks_update write RLS policy: the caller may
// write the task when they can write at least one of its teams (its own team_id
// or any of its task_teams), own it as a private task, or are an org admin of an
// org-visible task. A viewer of every team the task belongs to gets a 403
// "view-only access".
//
// Crucially it does NOT mask a 404: when the task isn't visible to the caller at
// all (no read access, or it doesn't exist) the gate returns true and lets the
// handler's own not-found path render the 404 — so a viewer probing for a
// task they can't see never learns whether it exists.
func (az *Checker) RequireTaskWrite(w http.ResponseWriter, r *http.Request, orgID, userID, taskID string) bool {
	if _, err := uuid.Parse(taskID); err != nil {
		// A malformed id is "not found" downstream, not a role failure — let
		// the handler's own uuid/preload path render it (404 parity).
		return true
	}
	visible, canWrite, err := az.members().taskWritable(r.Context(), orgID, userID, taskID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("task-write check %s/%s/%s: %w", userID, orgID, taskID, err))
		return false
	}
	// Not visible → let the handler 404 (don't disclose existence). Visible but
	// not writable → the role boundary; 403.
	if visible && !canWrite {
		writeForbidden(w, viewOnlyMessage)
		return false
	}
	return true
}

// UserOwnsOrg returns true when the calling user is the founder/owner of the
// given org — the holder of orgs.owner_user_id. Distinct from UserIsOrgAdmin
// because ownership transfer is owner-only: a plain org admin can't reassign
// the founder sentinel.
func (az *Checker) UserOwnsOrg(ctx context.Context, userID, orgID string) (bool, error) {
	return az.members().userOwnsOrg(ctx, userID, orgID)
}

// writeForbidden answers a visible-but-refused action: the caller may see the
// resource, so the denial names the missing role rather than hiding behind a
// 404. Which of the two a given gate uses is the disclosure rule's call, not
// this helper's.
func writeForbidden(w http.ResponseWriter, msg string) {
	httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
		Reason: httpx.ReasonForbidden, Message: msg,
	})
}

// tokenScopeAllows reports whether the caller's credential may address rawOrgID
// at all, ahead of any question about their role in it.
//
// A session cursor is movable, so a session may name any org its holder belongs
// to and this is always true for one. An API token is that same cursor sealed:
// its org was fixed at mint and it can never name another, which is a deliberate
// divergence from sessions — multi-org automation mints one token per org and
// swaps the Authorization header, never the URLs. Outside its org the answer is
// 404 by the disclosure rule: to that credential the org does not exist, and
// answering 403 would confirm it does.
func tokenScopeAllows(r *http.Request, rawOrgID string) bool {
	tok := httpx.TokenAuthFrom(r.Context())
	return tok == nil || sameOrgID(tok.OrgID, rawOrgID)
}

// sameOrgID compares two org ids as IDENTITIES rather than as strings, because
// the two sides reach here spelled differently: the token's org is canonical
// text out of the database, while the path or query value is whatever the
// caller typed. uuid.Parse accepts uppercase and unhyphenated forms, and every
// gate around this one hands such a value straight to Postgres, which equates
// them — so a raw string compare would 404 a token on its OWN org for a request
// a session answers 200. Anything that is not a pair of parseable uuids falls
// back to exact equality, which is what local mode's sentinel org needs.
func sameOrgID(a, b string) bool {
	if a == b {
		return true
	}
	parsedA, errA := uuid.Parse(a)
	parsedB, errB := uuid.Parse(b)
	return errA == nil && errB == nil && parsedA == parsedB
}

// RequireOrgMember validates {org_id} from the URL path and checks the caller
// is a member of that org (any role). Returns (orgID, userID, true) on success;
// writes an error and returns ("", "", false) on failure. The read-only sibling
// of RequireOrgAdmin.
func (az *Checker) RequireOrgMember(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	rawOrgID := r.PathValue("org_id")
	if _, err := uuid.Parse(rawOrgID); err != nil {
		httpx.NotFound(w, "org")
		return
	}
	// Ahead of the membership probe: the caller may well be a member here and
	// still not be able to reach the org with the credential they used.
	if !tokenScopeAllows(r, rawOrgID) {
		httpx.NotFound(w, "org")
		return
	}

	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return
	}
	userID = claims.Subject

	hasAccess, err := az.UserHasOrgAccess(r.Context(), userID, rawOrgID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("member check %s/%s: %w", userID, rawOrgID, err))
		return
	}
	if !hasAccess {
		httpx.NotFound(w, "org")
		return
	}
	return rawOrgID, userID, true
}

// RequireOrgAdmin validates {org_id} from the URL path and checks the caller is
// both a member and an admin of that org. Returns (orgID, userID, true) on
// success; writes an error and returns ("", "", false) on failure.
func (az *Checker) RequireOrgAdmin(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	rawOrgID := r.PathValue("org_id")
	if _, err := uuid.Parse(rawOrgID); err != nil {
		httpx.NotFound(w, "org")
		return
	}
	// Before the admin probe, and before the membership read on its denial
	// path: out-of-scope is 404 whatever role the caller holds there, so the
	// two-valued denial below never gets to disclose an org this credential
	// cannot name.
	if !tokenScopeAllows(r, rawOrgID) {
		httpx.NotFound(w, "org")
		return
	}

	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return
	}
	userID = claims.Subject

	isAdmin, err := az.UserIsOrgAdmin(r.Context(), userID, rawOrgID)
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("admin check %s/%s: %w", userID, rawOrgID, err))
		return
	}
	if !isAdmin {
		// Two-valued denial, per the disclosure rule: a caller who can see
		// the org gets 403 for the role they lack, and only a caller who
		// cannot see it at all gets 404. Answering 404 to a member reads as
		// a bug — they can list the org everywhere else in the app. The
		// membership read only runs on the denial path, so the admin path
		// still costs one query.
		hasAccess, accessErr := az.UserHasOrgAccess(r.Context(), userID, rawOrgID)
		if accessErr != nil {
			httpx.InternalError(w, "authz", fmt.Errorf("member check %s/%s: %w", userID, rawOrgID, accessErr))
			return
		}
		if hasAccess {
			writeForbidden(w, "org admin role required")
			return
		}
		httpx.NotFound(w, "org")
		return
	}
	return rawOrgID, userID, true
}

// resolveError carries whether a failed team-ID resolve should render as a 404
// (a missing/invalid team) or a 500 (a real lookup failure). ResolveTeamID
// returns it; WriteResolveError renders it.
type resolveError struct {
	notFound bool
	err      error
}

func (e *resolveError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying cause so errors.Is/As traverse it. In
// particular it lets httpx.InternalError's client-gone check (reached via
// WriteResolveError) see a context.Canceled that surfaced from ResolveTeamID's
// default-team lookup, instead of logging the abort as a 500.
func (e *resolveError) Unwrap() error { return e.err }

// WriteResolveError renders a ResolveTeamID failure: 404 "team not found" for a
// missing/invalid team, otherwise a redacted 500 logged under scope.
func WriteResolveError(w http.ResponseWriter, scope string, err error) {
	var re *resolveError
	if errors.As(err, &re) && re.notFound {
		httpx.NotFound(w, "team")
		return
	}
	httpx.InternalError(w, scope, err)
}

// TeamIDFromPath is ResolveTeamID plus its error rendering — the shape a
// handler wants at the top of its body:
//
//	teamID, ok := az.TeamIDFromPath(w, r, "teams", orgID, userID)
//	if !ok {
//	    return
//	}
//
// It exists so every {team_id} route spells the grammar the same way. A
// handler that parsed the segment itself would be a second grammar, which is
// exactly what the settings family and the teams family used to be.
func (az *Checker) TeamIDFromPath(w http.ResponseWriter, r *http.Request, scope, orgID, userID string) (string, bool) {
	teamID, err := az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		WriteResolveError(w, scope, err)
		return "", false
	}
	return teamID, true
}
