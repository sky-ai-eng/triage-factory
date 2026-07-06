package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=ProjectStore --output=./mocks --case=underscore --with-expecter

// ErrVisibilityForbidden is returned by ProjectStore.Create / Update when
// the caller lacks the role a requested visibility requires — org
// visibility needs an org admin, private visibility needs the project's
// own creator — mirroring the WITH CHECK branches of the projects_insert /
// projects_update RLS policies. Postgres surfaces the denial as SQLSTATE
// 42501; the impl translates it to this sentinel so the handler can answer
// a clean 403 instead of a raw RLS error. SQLite (single-tenant, always
// visibility="team") never returns it.
var ErrVisibilityForbidden = errors.New("db: insufficient privilege for that project visibility")

// ProjectStore owns the projects table — user-curated work groupings
// (a Linear/Jira "project" mirrored locally, with pinned repos and
// the curator session that maintains the project's knowledge dir).
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID.
// Create additionally takes teamID — projects are user-driven writes
// and the human picks which team owns the project at the Create UI;
// the store does not synthesize a team. creator_user_id is resolved
// from tf.current_user_id() set by WithTx (falling back to org owner
// only on the admin-pool test path where claims aren't set). SKY-294
// owns the broader team-selection UX work (per-page filter + write-
// time picker + sticky default).
//
// Postgres wires against the app pool — every consumer is request-
// equivalent (projects handler, curator, backfill, project_entities)
// or runs in a startup goroutine that already operates within the
// org's identity scope (projectclassify runner). RLS policies
// projects_select / projects_insert / projects_update / projects_delete
// gate every statement; org_id defense-in-depth fires alongside.
type ProjectStore interface {
	// Create inserts a new project and returns its id. If p.ID is
	// non-empty it's used verbatim; otherwise a uuid is generated.
	// PinnedRepos serializes to JSON (nil → []). teamID populates
	// team_id (required by the projects_team_visibility_requires_team
	// CHECK when the row defaults to visibility='team'); the SQLite
	// impl uses the local sentinel teamID, the Postgres impl binds
	// the caller-supplied value directly and refuses the SQLite
	// sentinel.
	Create(ctx context.Context, orgID, teamID string, p domain.Project) (string, error)

	// Get returns a project by id, or (nil, nil) if not found.
	Get(ctx context.Context, orgID, id string) (*domain.Project, error)

	// List returns all projects ordered by name (case-insensitive).
	// No pagination — counts stay small (≤100 in any plausible install).
	// Empty result returns []domain.Project{}, not nil.
	List(ctx context.Context, orgID string) ([]domain.Project, error)

	// Update writes the full mutable row from p (caller is responsible
	// for merging partial PATCH input over an existing row first).
	// updated_at is stamped server-side. created_at + creator_user_id
	// + team_id are preserved — team_id is create-time-only in v1, no
	// PATCH path re-teams a project. p.Visibility IS written (the
	// handler merges any patched value in); Postgres translates a
	// resulting RLS denial to ErrVisibilityForbidden. Returns
	// sql.ErrNoRows when the project doesn't exist so handlers can map
	// to 404.
	Update(ctx context.Context, orgID string, p domain.Project) error

	// Delete removes the project. The entities.project_id FK is
	// declared ON DELETE SET NULL so tagged entities become untagged
	// automatically — callers don't need to clear them first. Returns
	// sql.ErrNoRows when the project doesn't exist.
	//
	// On-disk knowledge artifacts (`~/.triagefactory/projects/<id>/`)
	// are NOT removed here — the handler owns that to keep this layer
	// pure DB. Same split as the rest of the codebase.
	Delete(ctx context.Context, orgID, id string) error

	// SetCuratorSessionID persists the Claude Code session id on the
	// project row. The curator's first dispatch on a project captures
	// the session id from the agent's init event; subsequent turns
	// resume against the same id. App pool in Postgres — the goroutine
	// wraps this in SyntheticClaimsWithTx under the requesting user's
	// identity, since the bookkeeping write is part of that user's
	// turn. updated_at is stamped server-side. Idempotent — repeating
	// the same value is a harmless no-op (the per-request sink also
	// short-circuits via sync.Once).
	//
	// Best-effort on missing row: if the project was deleted between
	// request dispatch and first-session capture, the UPDATE silently
	// affects 0 rows and returns nil. The caller (curator sink) has
	// nothing useful to do with a sql.ErrNoRows here — the chat turn
	// is already in flight and the FK cascade on project delete will
	// drop the curator_requests row alongside the project. Diverges
	// intentionally from Update/Delete's sql.ErrNoRows behavior.
	SetCuratorSessionID(ctx context.Context, orgID, projectID, sessionID string) error

	// BumpUpdatedAt stamps updated_at = now() without changing any
	// other column. The knowledge-base upload/delete handlers call
	// this after writing or removing files on disk so the UI's
	// "recently active" sort reflects the activity — Update would
	// require loading + writing the full row just to refresh the
	// timestamp. A no-op when the project doesn't exist (the handler
	// has already responded against the in-memory project id; a
	// vanished row is rare and doesn't affect the on-disk action's
	// success).
	BumpUpdatedAt(ctx context.Context, orgID, id string) error

	// --- Admin-pool variants (`...System`) ---
	//
	// ListSystem mirrors List but routes through the admin pool in
	// Postgres. The consumer is the project classifier (SKY-297) —
	// a background goroutine spawned from main.go that pairs each
	// org's unclassified entities against that org's project set.
	// The classifier has no JWT-claims context, so the read needs
	// to bypass RLS the same way EntityStore.ListUnclassifiedSystem
	// does for the sibling read on the entity side. org_id stays in
	// the WHERE clause as defense in depth; behavior matches List.
	ListSystem(ctx context.Context, orgID string) ([]domain.Project, error)

	// ResolveOrgSystem returns the project's owning org id, or
	// ("", nil) when no row matches the supplied id. Admin-pool by
	// construction so it can be called from background goroutines
	// that have a projectID but no JWT-claims context (today: the
	// kbwatcher's broadcast scoping). Bypasses RLS on purpose — the
	// caller is system-level and uses the result only to fan
	// broadcasts to the right tenant. Errors other than "no row"
	// propagate so callers can decide between "treat as system-wide"
	// and "give up".
	ResolveOrgSystem(ctx context.Context, projectID string) (string, error)
}
