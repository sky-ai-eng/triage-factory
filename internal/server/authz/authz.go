// Package authz is the org/team authorization layer for the Triage Factory
// server: the shared gates and membership checks several handler domains run
// before touching team- or org-scoped state. It owns the friendly front gates
// that translate a failed check into the right 403/404, the resolve-error
// renderer they share, and the SQL those checks run.
//
// That SQL comes in two shapes, both executing on the app pool inside
// db.WithTx so they see the caller's claims:
//
//   - RLS-function probes — tf.team_in_current_org, tf.user_is_team_admin,
//     tf.user_has_org_access, tf.user_can_write_team, tf.user_owns_org, … —
//     which call the same helpers the row policies call, so a gate and the
//     policy behind it can't drift apart. There is nothing to route through a
//     store: the answer lives in a SQL function, not a table.
//   - Plain reads of four columns that no RLS helper exposes:
//     teams.deleted_at (the archive block), a count of org_memberships, and a
//     count-plus-caller's-role over memberships. They stay here rather than
//     behind db.Stores because each is an authorization input read in the same
//     claims transaction as the probe it accompanies — the membership pair in
//     particular has to be one transaction under one claims context, and its
//     role arm resolves the caller through tf.current_user_id() rather than an
//     argument. Routing them through the store layer means minting
//     multi-mode-only methods whose SQLite twins are unreachable stubs (every
//     caller short-circuits local mode before the read) and moving an
//     authorization check onto a different pool — a trade worth making
//     deliberately, not as a side effect.
//
// RequireTaskWrite is a third thing again: it mirrors the tasks_update policy
// arm-for-arm over tasks/task_teams, composing tf.* helpers inline. It reads
// tables, but what it encodes is the policy, so it is written out here where it
// can be diffed against the policy it shadows.
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

// Checker performs the org/team authorization checks. The gate and count
// helpers short-circuit local mode to "allowed" (N=1 has a single implicit
// owner, and the Postgres-only RLS plumbing has nothing to read). The two raw
// probes — UserHasOrgAccess and UserIsOrgAdmin — do NOT short-circuit; they
// always run the tf.* helpers via db.WithTx, so they're multi-mode only and
// rely on their gate callers (and on local-mode routing never mounting an
// {org_id} path) to avoid being reached under local SQLite.
//
// It needs only the raw pool (for both SQL shapes the package doc describes)
// and the transactional store runner (for ResolveTeamID's default-team lookup,
// the one check with a store method behind it); handlers hold a single
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
// Local mode short-circuits to allowed — N=1 never archives its sole team. A
// missing row is treated as not-archived (true): VerifyTeamInOrg is the
// authority on existence and runs first, and a vanished team's write fails
// downstream regardless. The read runs under the caller's claims; teams_select
// RLS gates on org access (not deleted_at), so an archived team is still visible
// to the probe.
func (az *Checker) VerifyTeamNotArchived(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	var archived sql.NullBool
	err := db.WithTx(r.Context(), az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(),
				`SELECT deleted_at IS NOT NULL FROM teams WHERE id = $1::uuid`, teamID,
			).Scan(&archived)
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		httpx.InternalError(w, "authz", fmt.Errorf("team-archived check %s/%s: %w", teamID, orgID, err))
		return false
	}
	if archived.Valid && archived.Bool {
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
func (az *Checker) RequireOrgAdminRole(w http.ResponseWriter, r *http.Request, orgID, userID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
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

// UserIsTeamAdmin returns true when the calling user holds the 'admin' role in
// the given team. The raw-probe sibling of RequireTeamAdmin (which writes a 403
// front gate); endpoints that need to OR team-admin with another right — the
// team roster's "team admin OR org admin" mutate gate — call this and compose
// the decision themselves. The OrgID claim is required: tf.user_is_team_admin
// reads memberships under RLS, which gates on tf.current_org_id(), so a
// Sub-only claim would miscount. Like the other raw probes it always runs the
// tf.* helper via db.WithTx (no local-mode short-circuit), so callers gate
// local mode out before reaching it.
func (az *Checker) UserIsTeamAdmin(ctx context.Context, userID, orgID, teamID string) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT tf.user_is_team_admin($1::uuid)`, teamID,
			).Scan(&ok)
		},
	)
	return ok, err
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
// gates use, delegating to the tf.user_can_write_team SQL helper that backs the
// team-scoped write RLS policies (TFAC-447). Like the other raw probes it always
// runs the tf.* helper via db.WithTx (no local-mode short-circuit), so callers
// must gate local mode out before reaching it. The OrgID claim is required:
// tf.user_can_write_team reads memberships under RLS, which gates on
// tf.current_org_id().
func (az *Checker) UserCanWriteTeam(ctx context.Context, userID, orgID, teamID string) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT tf.user_can_write_team($1::uuid)`, teamID,
			).Scan(&ok)
		},
	)
	return ok, err
}

// RequireTeamWrite is the friendly front gate for a team-scoped write: it writes
// a 403 "view-only access" and returns false when the caller is a viewer (or not
// a member) of teamID. The team-scoped write RLS policies enforce this at the
// row level regardless; this gate exists so a viewer reaching a write handler
// gets a clean, role-named 403 instead of a generic RLS-blocked error (a
// silently-zero-rows UPDATE or a WITH CHECK violation surfaced as a 500). Local
// mode short-circuits to allowed — N=1 has a single implicit owner and no
// viewers.
func (az *Checker) RequireTeamWrite(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
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
// task they can't see never learns whether it exists. Local mode short-circuits
// to allowed.
func (az *Checker) RequireTaskWrite(w http.ResponseWriter, r *http.Request, orgID, userID, taskID string) bool {
	if runmode.Current() == runmode.ModeLocal {
		return true
	}
	if _, err := uuid.Parse(taskID); err != nil {
		// A malformed id is "not found" downstream, not a role failure — let
		// the handler's own uuid/preload path render it (404 parity).
		return true
	}
	var visible, canWrite bool
	err := db.WithTx(r.Context(), az.db, db.Claims{Sub: userID, OrgID: orgID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.Context(), `
				SELECT
				  EXISTS (SELECT 1 FROM tasks t WHERE t.id = $1::uuid AND t.org_id = $2::uuid) AS visible,
				  EXISTS (
				    SELECT 1 FROM tasks t
				    WHERE t.id = $1::uuid AND t.org_id = $2::uuid AND (
				      (t.visibility = 'private' AND t.creator_user_id = tf.current_user_id())
				      OR (t.visibility = 'org' AND tf.user_is_org_admin(t.org_id))
				      OR (t.visibility = 'team' AND (
				           (t.team_id IS NOT NULL AND tf.user_can_write_team(t.team_id))
				           OR EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id AND tf.user_can_write_team(tt.team_id))
				      ))
				    )
				  ) AS can_write`,
				taskID, orgID,
			).Scan(&visible, &canWrite)
		},
	)
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
// given org — the holder of orgs.owner_user_id. Mirrors UserIsOrgAdmin but
// delegates to tf.user_owns_org rather than tf.user_is_org_admin, because
// ownership transfer is owner-only: a plain org admin can't reassign the
// founder sentinel. Like the other raw probes it always runs the tf.* helper
// via db.WithTx (no local-mode short-circuit), so callers must gate local mode
// out before reaching it.
func (az *Checker) UserOwnsOrg(ctx context.Context, userID, orgID string) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, az.db, db.Claims{Sub: userID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT tf.user_owns_org($1::uuid)`, orgID,
			).Scan(&ok)
		},
	)
	return ok, err
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
