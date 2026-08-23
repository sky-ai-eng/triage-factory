package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=PromptStore --output=./mocks --case=underscore --with-expecter

// ErrNoSuchPrompt means an id-keyed write was handed a prompt id no row
// answers to. Only the writes return it: Get and GetBySystemSlug keep
// answering a miss with (nil, nil), because a read is a question and "no
// prompt" is an answer to it, while a write that matched nothing is a caller
// that resolved an id and then acted on something that is not there.
var ErrNoSuchPrompt = errors.New("no prompt with that id")

// PromptStore owns the prompts table. Three audiences:
//
//   - HTTP handlers (server/prompts_handler.go, server/triggers_handler.go,
//     server/projects.go) — full CRUD.
//   - Delegated agents (delegate/*) — read prompt body before dispatch +
//     bump usage_count.
//   - Skills importer (skills/importer.go) — Get/Create/UpdateImported/Hide
//     to mirror local SKILL.md files into the prompts table.
//
// New teams seed their prompts directly from the shipped Go slices via
// ShippedDefaultsStore, and a boot-time sync keeps every unmodified team copy
// equal to the current shipped content (ShippedDefaultsStore.SyncShippedIntoTeam,
// which decides "unmodified equals shipped" by direct content comparison).
// PromptStore itself carries no shipped-content seeder.
//
// Postgres / RLS note: the request-facing methods run on the app pool
// (RLS-gated); the ...System reads (GetSystem, IncrementUsageSystem) route
// through the admin pool for the claims-less delegation goroutines. SQLite has
// no role concept; both pools collapse to one connection and assertLocalOrg
// pins orgID to LocalDefaultOrgID.
//
// # Every single-row write returns the row it persisted
//
// Create, Update, UpdateImported, Hide and Unhide hand back the stored row,
// read off RETURNING on the write statement itself rather than from a
// follow-up SELECT, projecting the point read's column list and scanner so the
// write shape cannot drift from the read shape. The row differs from what the
// caller passed in ways only the row can tell it: Create stamps usage_count,
// created_at, updated_at and — in Postgres — the creator identity and the
// team_id RLS checked; every update restamps updated_at. A miss is
// ErrNoSuchPrompt rather than the silent zero-rows these writes used to
// report, so a handler that resolved an id and then wrote cannot answer
// "updated" for a write that did nothing.
//
// Exempt, each said so at the method: Delete (a delete — soft-stamped so the
// FKs never fire, but a delete, and the row it leaves reads as absent through
// every request-facing read) and IncrementUsage / IncrementUsageSystem
// (fire-and-forget bookkeeping on a sort heuristic).
type PromptStore interface {
	// List returns one page of non-hidden prompts plus the unpaged total,
	// ordered by updated_at DESC with an id tiebreaker so the order is total
	// and the pages partition it. When
	// teamID is non-empty — the multi-team prompts page narrowed to one
	// team — the result is scoped to that team's prompts (team_id=teamID).
	// Every prompt is team-scoped, so there is no org-visible
	// tier to union in. Empty teamID returns everything visible (solo/local,
	// or an unfiltered view). The SQLite impl ignores teamID (local mode is
	// single-team).
	List(ctx context.Context, orgID string, teamID string, opts ListOpts) ([]domain.Prompt, int, error)

	// Get returns one prompt by id (regardless of hidden state) or
	// (nil, nil) if not found. Request-facing, so it filters
	// deleted_at IS NULL — a soft-deleted prompt reads as absent here
	// (use GetSystem to resolve it for in-flight runs / past timelines).
	Get(ctx context.Context, orgID string, id string) (*domain.Prompt, error)

	// GetBySystemSlug resolves a team's copy of a shipped prompt by its
	// stable system_slug (e.g. "system-ci-fix"). Returns (nil, nil) when the
	// team has no copy. The id moved to a random UUID per team copy, so a
	// caller that needs a shipped prompt by name resolves through this
	// instead of Get(slug).
	// The Postgres impl filters org+team and runs on the app
	// pool (RLS-gated); the SQLite impl filters by slug only (single team)
	// but honors a non-empty teamID when supplied.
	GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Prompt, error)

	// Create inserts a new prompt (user or imported source) owned by
	// teamID — the acting team the handler resolved for the request.
	// Caller-provided ID — the handler generates UUIDs upstream. The
	// Postgres impl binds teamID directly (it satisfies the team-
	// membership RLS); the SQLite impl ignores it (local mode is
	// single-team and pins the sentinel).
	//
	// Returns the persisted row. p is an input: it carries no usage_count, no
	// timestamps, and — because each dialect resolves ownership its own way —
	// not necessarily the team_id the row ends up with. The returned row is
	// what a caller renders instead of echoing p back.
	Create(ctx context.Context, orgID, teamID string, p domain.Prompt) (domain.Prompt, error)

	// Update changes name + body + model and stamps user_modified=true.
	// The flag tells the boot-time shipped-defaults sync to leave the row
	// (and its whole blueprint unit) alone on subsequent shipped-content
	// updates. model="" means "inherit the global default at dispatch time".
	//
	// Returns the updated row — which carries the restamped updated_at and
	// the usage_count this write left alone — or ErrNoSuchPrompt.
	Update(ctx context.Context, orgID string, id, name, body, model string) (domain.Prompt, error)

	// UpdateImported updates a re-imported skill's metadata + body
	// + allowed_tools WITHOUT setting user_modified, because the
	// change came from a file re-import not a user edit.
	//
	// Returns the updated row, or ErrNoSuchPrompt. The importer resolves the
	// row before writing, so a miss here means the prompt went away
	// mid-import rather than that the file named nothing.
	UpdateImported(ctx context.Context, orgID string, id, name, body, allowedTools string) (domain.Prompt, error)

	// Delete soft-deletes a prompt (stamps deleted_at). The row + the
	// conversations referencing it stay as the durable audit trail —
	// conversations.prompt_id is RESTRICT, so
	// a hard DELETE on a prompt with conversation history would error, and
	// auto-wrapping every new prompt as a 1-step blueprint (the step FK
	// is also RESTRICT) makes hard-delete impossible. Request-facing
	// reads (List/Get/GetBySystemSlug)
	// filter deleted_at IS NULL; the ...System reads keep resolving it so
	// in-flight runs + past-run timelines still render the name/body.
	//
	// Exempt from the returned-row rule: it is a delete. The stamp is how the
	// row survives its RESTRICT FKs, not a state anyone renders — every
	// request-facing read filters deleted_at, so the row this leaves behind is
	// precisely the one a caller must not be handed as a result.
	Delete(ctx context.Context, orgID string, id string) error

	// Hide soft-deletes a prompt (hidden=true) — the system/imported analog of
	// Delete. Used so shipped/imported prompts disappear from List but remain
	// available to historical conversations that already reference them.
	//
	// Returns the hidden row, or ErrNoSuchPrompt. Unlike Delete, the row stays
	// readable through Get, so hiding one is a state change a caller can be
	// handed rather than a removal.
	Hide(ctx context.Context, orgID string, id string) (domain.Prompt, error)

	// Unhide reverses Hide, and returns the row it un-hid or ErrNoSuchPrompt.
	Unhide(ctx context.Context, orgID string, id string) (domain.Prompt, error)

	// CountConversationReferences returns the number of conversations rows that reference
	// the given prompt. Used to surface execution history before a
	// destructive edit / delete.
	CountConversationReferences(ctx context.Context, orgID string, id string) (int, error)

	// IncrementUsage bumps usage_count by 1. Called from the
	// delegate spawner when a conversation picks the prompt; the count
	// drives the prompts page's sort heuristic.
	//
	// Exempt from the returned-row rule: fire-and-forget bookkeeping. The
	// spawner bumps the counter on its way past and never reads the answer;
	// the count is spent by a list read on the next page load.
	IncrementUsage(ctx context.Context, orgID string, id string) error

	// Stats aggregates conversations.* for this prompt — totals, success
	// rate, cost, last-used, runs-per-day-for-30-days. The
	// underlying queries hit the conversations table; logically a
	// conversation-side concern but keyed on prompt_id, so it lives here so
	// the prompts handler can depend on a single store. It stays here now
	// that ConversationStore exists — the read still keys on prompt_id and
	// PromptStore is the right ownership root.
	Stats(ctx context.Context, orgID string, id string) (*domain.PromptStats, error)

	// GetSystem mirrors Get but routes through the admin pool in
	// Postgres AND does NOT filter deleted_at — so a soft-deleted prompt
	// still resolves for in-flight conversations and past-conversation
	// timelines. The router's breaker-tripped toast looks up the prompt
	// name from its eventbus subscriber goroutine, which has no
	// JWT-claims context.
	GetSystem(ctx context.Context, orgID string, id string) (*domain.Prompt, error)

	// IncrementUsageSystem mirrors IncrementUsage but routes through
	// the admin pool in Postgres. The delegate spawner bumps
	// usage_count from inside a goroutine that continues past the
	// request lifecycle (initial Delegate dispatch + per-step chain
	// orchestration), so the call may run without JWT-claims context.
	//
	// Exempt for the same reason as IncrementUsage.
	IncrementUsageSystem(ctx context.Context, orgID string, id string) error
}
