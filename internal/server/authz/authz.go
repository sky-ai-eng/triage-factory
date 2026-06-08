// Package authz is the org/team authorization layer for the Triage Factory
// server: the shared gates and membership checks several handler domains run
// before touching team- or org-scoped state. It owns the raw RLS-function
// probes (tf.team_in_current_org, tf.user_is_team_admin, tf.user_has_org_access,
// …) that execute on the app pool via db.WithTx, plus the friendly front gates
// that translate a failed check into the right 403/404 and the resolve-error
// renderer they share.
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
	"log"
	"net/http"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Checker performs the org/team authorization checks. The gate and count
// helpers short-circuit local mode to "allowed" (N=1 has a single implicit
// owner, and the Postgres-only RLS plumbing has nothing to read). The two raw
// probes — UserHasOrgAccess and UserIsOrgAdmin — do NOT short-circuit; they
// always run the tf.* helpers via db.WithTx, so they're multi-mode only and
// rely on their gate callers (and on local-mode routing never mounting an
// {org_id} path) to avoid being reached under local SQLite.
//
// It needs only the raw pool (for the RLS probes) and the transactional store
// runner (for ResolveTeamID's default-team lookup); handlers hold a single
// *Checker instead of re-deriving these checks against their own store fields.
type Checker struct {
	db *sql.DB
	tx db.TxRunner
}

// New builds a Checker over the raw pool and the transactional store runner.
func New(database *sql.DB, tx db.TxRunner) *Checker {
	return &Checker{db: database, tx: tx}
}

// ResolveTeamID converts a raw {team_id} path value to a concrete team UUID.
// The literal "default" resolves to the org's default team so the frontend can
// call /api/settings/team/default before team pickers ship. Non-"default"
// values are validated as UUIDs. A failed resolve returns a *resolveError;
// render it with WriteResolveError.
func (az *Checker) ResolveTeamID(ctx context.Context, orgID, userID, raw string) (string, error) {
	if raw != "default" {
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
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	var belongs bool
	err := db.WithTx(r.Context(), az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(),
				`SELECT tf.team_in_current_org($1::uuid)`, teamID,
			).Scan(&belongs)
		},
	)
	if err != nil {
		log.Printf("[authz] team-in-org check %s/%s: %v", teamID, orgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !belongs {
		http.NotFound(w, r)
		return false
	}
	return true
}

// RequireTeamAdmin checks the user is an admin of the given team.
// Returns 403 on non-admin.
func (az *Checker) RequireTeamAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	var isAdmin bool
	err := db.WithTx(r.Context(), az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(),
				`SELECT tf.user_is_team_admin($1::uuid)`, teamID,
			).Scan(&isAdmin)
		},
	)
	if err != nil {
		log.Printf("[authz] team-admin check %s/%s/%s: %v", userID, orgID, teamID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !isAdmin {
		httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "team admin role required"})
		return false
	}
	return true
}

// RequireOrgAdminRole checks the user is an admin of the given org.
// Returns 403 on non-admin.
func (az *Checker) RequireOrgAdminRole(w http.ResponseWriter, r *http.Request, orgID, userID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	isAdmin, err := az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		log.Printf("[authz] org-admin check %s/%s: %v", userID, orgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !isAdmin {
		httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "org admin role required"})
		return false
	}
	return true
}

// OrgMemberCount returns the number of members in the org. Local mode is
// always single-member (one synthetic user), so it short-circuits to 1
// without touching Postgres — db.WithTx's set_config is Postgres-only and
// there are no org_memberships rows in the local SQLite schema anyway.
func (az *Checker) OrgMemberCount(ctx context.Context, orgID, userID string) (int, error) {
	if runmode.Current() == runmode.ModeLocal {
		return 1, nil
	}
	var n int
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM org_memberships WHERE org_id = $1::uuid`, orgID,
			).Scan(&n)
		},
	)
	return n, err
}

// TeamMemberCountAndRole returns the team's member count and the caller's
// role in it ("admin" / "member", or "" when not a member). Local mode is
// the degenerate single-member case: one user who is implicitly the admin.
// The OrgID claim is required — teams/memberships RLS gates on
// tf.current_org_id(), so a Sub-only claim would only ever see the
// caller's own membership row and miscount.
func (az *Checker) TeamMemberCountAndRole(ctx context.Context, orgID, userID, teamID string) (int, string, error) {
	if runmode.Current() == runmode.ModeLocal {
		return 1, "admin", nil
	}
	var (
		n    int
		role string
	)
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			if e := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM memberships WHERE team_id = $1::uuid`, teamID,
			).Scan(&n); e != nil {
				return e
			}
			e := tx.QueryRowContext(ctx,
				`SELECT role FROM memberships
				  WHERE team_id = $1::uuid AND user_id = tf.current_user_id()`, teamID,
			).Scan(&role)
			if errors.Is(e, sql.ErrNoRows) {
				return nil
			}
			return e
		},
	)
	return n, role, err
}

// UserHasOrgAccess reports whether the user is a member of the org. The check
// runs inside a claims-context transaction so it resolves through the
// tf.user_has_org_access SQL helper, which internally reads request.jwt.claims
// via tf.current_user_id(). The claims-context transaction means a
// missing/wrong claim → NULL → no membership, even if a future bug allowed a
// wrong userID argument to land here. Once the app pool is wired the same query
// runs under RLS without further edits.
func (az *Checker) UserHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT tf.user_has_org_access($1::uuid)`, orgID,
			).Scan(&ok)
		},
	)
	return ok, err
}

// UserIsOrgAdmin returns true when the calling user holds an 'owner' or
// 'admin' role in the given org. Mirrors UserHasOrgAccess but delegates to
// tf.user_is_org_admin instead of tf.user_has_org_access. Used by endpoints
// that gate on org-admin privilege (GitHub App registration, team management,
// etc.).
func (az *Checker) UserIsOrgAdmin(ctx context.Context, userID, orgID string) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT tf.user_is_org_admin($1::uuid)`, orgID,
			).Scan(&ok)
		},
	)
	return ok, err
}

// RequireOrgMember validates {org_id} from the URL path and checks the caller
// is a member of that org (any role). Returns (orgID, userID, true) on success;
// writes an error and returns ("", "", false) on failure. The read-only sibling
// of RequireOrgAdmin.
func (az *Checker) RequireOrgMember(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	rawOrgID := r.PathValue("org_id")
	if _, err := uuid.Parse(rawOrgID); err != nil {
		http.NotFound(w, r)
		return
	}

	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return
	}
	userID = claims.Subject

	if runmode.Current() == runmode.ModeLocal {
		return rawOrgID, userID, true
	}

	hasAccess, err := az.UserHasOrgAccess(r.Context(), userID, rawOrgID)
	if err != nil {
		log.Printf("[authz] member check %s/%s: %v", userID, rawOrgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !hasAccess {
		http.NotFound(w, r)
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
		http.NotFound(w, r)
		return
	}

	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return
	}
	userID = claims.Subject

	if runmode.Current() == runmode.ModeLocal {
		return rawOrgID, userID, true
	}

	isAdmin, err := az.UserIsOrgAdmin(r.Context(), userID, rawOrgID)
	if err != nil {
		log.Printf("[authz] admin check %s/%s: %v", userID, rawOrgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !isAdmin {
		http.NotFound(w, r)
		return
	}
	return rawOrgID, userID, true
}

// RequireOrgTemplate gates an org-template endpoint: multi-mode only, active
// org resolved, caller is an org admin. Returns (orgID, userID, true) on
// success. Local mode 404s (no template concept) — mirrors the POST /api/teams
// local-absent posture. The org-admin check is also enforced server-side by
// the org_template_*_all RLS policies; this is the friendly front gate.
func (az *Checker) RequireOrgTemplate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject
	if !az.RequireOrgAdminRole(w, r, orgID, userID) {
		return "", "", false
	}
	return orgID, userID, true
}

// resolveError carries whether a failed team-ID resolve should render as a 404
// (a missing/invalid team) or a 500 (a real lookup failure). ResolveTeamID
// returns it; WriteResolveError renders it.
type resolveError struct {
	notFound bool
	err      error
}

func (e *resolveError) Error() string { return e.err.Error() }

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
