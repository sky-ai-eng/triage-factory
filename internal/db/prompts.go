package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=PromptStore --output=./mocks --case=underscore --with-expecter

// PromptStore owns prompts + the system_prompt_versions sidecar that
// tracks shipped-content hashes. Three audiences:
//
//   - HTTP handlers (server/prompts_handler.go, server/triggers_handler.go,
//     server/projects.go) — full CRUD.
//   - Delegated agents (delegate/*, curator/skill.go) — read prompt body
//     before dispatch + bump usage_count.
//   - Startup seeder (seed.go) — SeedOrUpdate per shipped system prompt.
//   - Skills importer (skills/importer.go) — Get/Create/UpdateImported/Hide
//     to mirror local SKILL.md files into the prompts table.
//
// Postgres / RLS note: in multi mode, system_prompt_versions has
// INSERT/UPDATE/DELETE REVOKE'd from tf_app per D3 — only the deploy
// actor (supabase_admin) can write the sidecar. SeedOrUpdate is the
// one method that touches that sidecar, so the Postgres impl routes
// it to the admin pool internally; every other method runs on the
// app pool. SQLite has no role concept; both pools collapse to one
// connection and assertLocalOrg pins orgID to LocalDefaultOrgID.
type PromptStore interface {
	// SeedOrUpdate inserts a shipped system prompt as the team's own copy
	// if missing, or updates it when the shipped (name, body, source) hash
	// changed since the last seed, skipping user-modified rows so local
	// customizations survive a re-seed. Identity is per-team: the row is
	// keyed by (org_id, team_id, system_slug) — p.SystemSlug carries the
	// shipped slug ("system-ci-fix", …) and the id is a random UUID minted
	// per team copy. Recorded in system_prompt_versions so identical
	// re-seeds are no-ops (no churn to prompts.updated_at — the UI orders
	// by it).
	//
	// Returns the team copy's prompt id (existing or freshly inserted) so
	// callers can resolve slug→id for the trigger seed's same-team FK
	// (two-phase seed).
	//
	// Atomic: prompts insert/update + system_prompt_versions upsert
	// happen in one transaction so the version row never races ahead
	// of the prompt row.
	//
	// Source contract: p.Source must be "" (defaulted to "system") or
	// "system" and p.SystemSlug must be non-empty. Non-system sources are
	// rejected — SeedOrUpdate is the shipped-content seeder; anything else
	// (user prompts via the HTTP handler, imported skills via the file
	// importer) goes through Create / UpdateImported. The version-sidecar
	// would otherwise track non-system rows and re-seeds could silently
	// overwrite them.
	//
	// Postgres-only: must run on the admin connection because
	// system_prompt_versions writes are REVOKE'd from tf_app. The
	// impl picks the right pool internally — callers don't (and
	// shouldn't) choose.
	SeedOrUpdate(ctx context.Context, orgID, teamID string, p domain.Prompt) (string, error)

	// List returns non-hidden prompts ordered by updated_at DESC. When
	// teamID is non-empty — the multi-team prompts page narrowed to one
	// team — the result is scoped to that team's prompts (team_id=teamID).
	// Every prompt is team-scoped, so there is no org-visible
	// tier to union in. Empty teamID returns everything visible (solo/local,
	// or an unfiltered view). The SQLite impl ignores teamID (local mode is
	// single-team).
	List(ctx context.Context, orgID string, teamID string) ([]domain.Prompt, error)

	// Get returns one prompt by id (regardless of hidden state) or
	// (nil, nil) if not found. Request-facing, so it filters
	// deleted_at IS NULL — a soft-deleted prompt reads as absent here
	// (use GetSystem to resolve it for in-flight runs / past timelines).
	Get(ctx context.Context, orgID string, id string) (*domain.Prompt, error)

	// GetBySystemSlug resolves a team's copy of a shipped prompt by its
	// stable system_slug (e.g. domain.SystemTicketSpecPromptID). Returns
	// (nil, nil) when the team has no copy. The id moved to a random UUID
	// per team copy, so callers that used to Get(slug) — the
	// curator spec fallback, the project-create default — resolve through
	// this instead. The Postgres impl filters org+team and runs on the app
	// pool (RLS-gated); the SQLite impl filters by slug only (single team)
	// but honors a non-empty teamID when supplied.
	GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Prompt, error)

	// Create inserts a new prompt (user or imported source) owned by
	// teamID — the acting team the handler resolved for the request.
	// Caller-provided ID — the handler generates UUIDs upstream. The
	// Postgres impl binds teamID directly (it satisfies the team-
	// membership RLS); the SQLite impl ignores it (local mode is
	// single-team and pins the sentinel).
	Create(ctx context.Context, orgID, teamID string, p domain.Prompt) error

	// Update changes name + body + model and stamps user_modified=true.
	// The flag tells SeedOrUpdate to leave the row alone on subsequent
	// shipped-content updates. model="" means "inherit the global default
	// at dispatch time".
	Update(ctx context.Context, orgID string, id, name, body, model string) error

	// UpdateImported updates a re-imported skill's metadata + body
	// + allowed_tools WITHOUT setting user_modified, because the
	// change came from a file re-import not a user edit.
	UpdateImported(ctx context.Context, orgID string, id, name, body, allowedTools string) error

	// Delete soft-deletes a prompt (stamps deleted_at). The row + its runs stay
	// as the durable audit trail — runs.prompt_id is RESTRICT, so a hard DELETE
	// on a prompt with run history would error, and auto-wrapping every new
	// prompt as a 1-step blueprint (the step FK is also RESTRICT) makes
	// hard-delete impossible. Request-facing reads (List/Get/GetBySystemSlug)
	// filter deleted_at IS NULL; the ...System reads keep resolving it so
	// in-flight runs + past-run timelines still render the name/body.
	Delete(ctx context.Context, orgID string, id string) error

	// Hide soft-deletes a prompt (hidden=true) — the system/imported analog of
	// Delete. Used so shipped/imported prompts disappear from List but remain
	// available to historical runs that already reference them.
	Hide(ctx context.Context, orgID string, id string) error

	// Unhide reverses Hide.
	Unhide(ctx context.Context, orgID string, id string) error

	// CountRunReferences returns the number of runs rows that reference
	// the given prompt. Used to surface execution history before a
	// destructive edit / delete.
	CountRunReferences(ctx context.Context, orgID string, id string) (int, error)

	// IncrementUsage bumps usage_count by 1. Called from the
	// delegate spawner when a run picks the prompt; the count
	// drives the prompts page's sort heuristic.
	IncrementUsage(ctx context.Context, orgID string, id string) error

	// Stats aggregates runs.* for this prompt — totals, success
	// rate, cost, last-used, runs-per-day-for-30-days. The
	// underlying queries hit the runs table; logically a Run-side
	// concern but keyed on prompt_id, so it lives here so the
	// prompts handler can depend on a single store. When RunStore
	// lands (wave 3b) this stays put — the read still keys on
	// prompt_id and PromptStore is the right ownership root.
	Stats(ctx context.Context, orgID string, id string) (*domain.PromptStats, error)

	// GetSystem mirrors Get but routes through the admin pool in
	// Postgres AND does NOT filter deleted_at — so a soft-deleted prompt
	// still resolves for in-flight runs and past-run timelines. The
	// router's breaker-tripped toast looks up the prompt name from its
	// eventbus subscriber goroutine, which has no JWT-claims context.
	GetSystem(ctx context.Context, orgID string, id string) (*domain.Prompt, error)

	// IncrementUsageSystem mirrors IncrementUsage but routes through
	// the admin pool in Postgres. The delegate spawner bumps
	// usage_count from inside a goroutine that continues past the
	// request lifecycle (initial Delegate dispatch + per-step chain
	// orchestration), so the call may run without JWT-claims context.
	IncrementUsageSystem(ctx context.Context, orgID string, id string) error
}
