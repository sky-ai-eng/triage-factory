package server

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
)

// authz is the org/team authorization layer: the shared gates and
// membership checks that several handler domains run before touching
// team- or org-scoped state. It owns the raw RLS-function probes
// (tf.team_in_current_org, tf.user_is_team_admin, tf.user_has_org_access,
// …) that execute on the app pool via db.WithTx, plus the friendly front
// gates that translate a failed check into the right 403/404. The gate
// and count helpers short-circuit local mode to "allowed" (N=1 has a
// single implicit owner, and the Postgres-only RLS plumbing has nothing
// to read). The two raw probes — userHasOrgAccess and userIsOrgAdmin —
// do NOT short-circuit; they always run the tf.* helpers via db.WithTx,
// so they're multi-mode only and rely on their gate callers (and on
// local-mode routing never mounting an {org_id} path) to avoid being
// reached under local SQLite.
//
// It needs only the raw pool (for the RLS probes) and the transactional
// store runner (for resolveTeamID's default-team lookup); handlers hold a
// single *authz instead of re-deriving these checks against their own
// store fields.
type authz struct {
	db *sql.DB
	tx db.TxRunner
}

// resolveTeamID converts a raw {team_id} path value to a concrete team
// UUID. The literal "default" resolves to the org's default team so the
// frontend can call /api/settings/team/default before team pickers ship.
// Non-"default" values are validated as UUIDs.
func (az *authz) resolveTeamID(ctx context.Context, orgID, userID, raw string) (string, error) {
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

// verifyTeamInOrg confirms that team_id belongs to the active org.
// Returns 404 (not 403) to avoid leaking team existence across orgs.
func (az *authz) verifyTeamInOrg(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
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

// requireTeamAdmin checks the user is an admin of the given team.
// Returns 403 on non-admin.
func (az *authz) requireTeamAdmin(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
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
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "team admin role required"})
		return false
	}
	return true
}

// requireOrgAdminRole checks the user is an admin of the given org.
// Returns 403 on non-admin.
func (az *authz) requireOrgAdminRole(w http.ResponseWriter, r *http.Request, orgID, userID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	isAdmin, err := az.userIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		log.Printf("[authz] org-admin check %s/%s: %v", userID, orgID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !isAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "org admin role required"})
		return false
	}
	return true
}

// orgMemberCount returns the number of members in the org. Local mode is
// always single-member (one synthetic user), so it short-circuits to 1
// without touching Postgres — db.WithTx's set_config is Postgres-only and
// there are no org_memberships rows in the local SQLite schema anyway.
func (az *authz) orgMemberCount(ctx context.Context, orgID, userID string) (int, error) {
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

// teamMemberCountAndRole returns the team's member count and the caller's
// role in it ("admin" / "member", or "" when not a member). Local mode is
// the degenerate single-member case: one user who is implicitly the admin.
// The OrgID claim is required — teams/memberships RLS gates on
// tf.current_org_id(), so a Sub-only claim would only ever see the
// caller's own membership row and miscount.
func (az *authz) teamMemberCountAndRole(ctx context.Context, orgID, userID, teamID string) (int, string, error) {
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

// userHasOrgAccess reports whether the user is a member of the org. The
// check runs inside a claims-context transaction so it resolves through
// the tf.user_has_org_access SQL helper, which internally reads
// request.jwt.claims via tf.current_user_id(). The claims-context
// transaction means a missing/wrong claim → NULL → no membership, even if
// a future bug allowed a wrong userID argument to land here. Once the app
// pool is wired the same query runs under RLS without further edits.
func (az *authz) userHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error) {
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

// userIsOrgAdmin returns true when the calling user holds an 'owner' or
// 'admin' role in the given org. Mirrors userHasOrgAccess but delegates to
// tf.user_is_org_admin instead of tf.user_has_org_access. Used by
// endpoints that gate on org-admin privilege (GitHub App registration,
// team management, etc.).
func (az *authz) userIsOrgAdmin(ctx context.Context, userID, orgID string) (bool, error) {
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

// requireOrgMember validates {org_id} from the URL path and checks the
// caller is a member of that org (any role). Returns (orgID, userID,
// true) on success; writes an error and returns ("", "", false) on
// failure. The read-only sibling of requireOrgAdmin.
func (az *authz) requireOrgMember(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	rawOrgID := r.PathValue("org_id")
	if _, err := uuid.Parse(rawOrgID); err != nil {
		http.NotFound(w, r)
		return
	}

	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	userID = claims.Subject

	if runmode.Current() == runmode.ModeLocal {
		return rawOrgID, userID, true
	}

	hasAccess, err := az.userHasOrgAccess(r.Context(), userID, rawOrgID)
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

// requireOrgAdmin validates {org_id} from the URL path and checks the
// caller is both a member and an admin of that org. Returns (orgID,
// userID, true) on success; writes an error and returns ("", "", false)
// on failure.
func (az *authz) requireOrgAdmin(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	rawOrgID := r.PathValue("org_id")
	if _, err := uuid.Parse(rawOrgID); err != nil {
		http.NotFound(w, r)
		return
	}

	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	userID = claims.Subject

	if runmode.Current() == runmode.ModeLocal {
		return rawOrgID, userID, true
	}

	isAdmin, err := az.userIsOrgAdmin(r.Context(), userID, rawOrgID)
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

// requireOrgTemplate gates an org-template endpoint: multi-mode only, active
// org resolved, caller is an org admin. Returns (orgID, userID, true) on
// success. Local mode 404s (no template concept) — mirrors the POST /api/teams
// local-absent posture. The org-admin check is also enforced server-side by
// the org_template_*_all RLS policies; this is the friendly front gate.
func (az *authz) requireOrgTemplate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject
	if !az.requireOrgAdminRole(w, r, orgID, userID) {
		return "", "", false
	}
	return orgID, userID, true
}
