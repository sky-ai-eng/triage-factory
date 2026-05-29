# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository. This is a public repository. Under no circumstances should you commit sensitive information or custom configuration files related to specific deployments to git.

## Build, lint, test

```bash
# Full build — frontend first (Go embeds frontend/dist/), then the binary.
cd frontend && npm install && npm run build && cd ..
go build -o ./triagefactory .

# Run (default :3000, opens browser)
./triagefactory [--port N] [--no-browser]

# Lint + format (Go + frontend)
./scripts/lint.sh           # check only
./scripts/lint.sh --fix     # auto-fix

# Go tests
go test ./...
go test ./internal/routing -run TestRouter_Dedup

# Frontend dev server (proxies /api to backend)
cd frontend && npm run dev

# Nuke local DB + config + keychain entries (fresh first-run flow)
./scripts/clean-slate.sh
```

The repo-root `.claude/settings.json` registers a `PostToolUse` hook that runs `goimports -w` on edited `.go` files and `prettier --write` on frontend sources — do not duplicate that work manually.

## Architecture

Triage Factory is a **single Go binary** (HTTP server + pollers + delegated-agent spawner) with a **React SPA embedded via `go:embed`** (see `embed.go`). State lives entirely on the user's machine: SQLite at `~/.triagefactory/triagefactory.db` (user settings persist in the `settings` row — `internal/config` reads/writes via `config.Init(db)` at startup), credentials in the OS keychain (`internal/auth`).

### The binary has two modes

`main.go` dispatches on `os.Args[1]`:

- **Server mode** (default) — HTTP API + websocket hub + pollers + scorer + event router + delegation spawner.
- **CLI mode** (`exec`, `status`) — invoked _by delegated Claude Code agents_ inside a worktree. `cmd/exec/` provides scoped GitHub/Jira subcommands the agent uses instead of calling those APIs directly, so credentials stay in the keychain and activity is auditable via `runs` / `run_artifacts`.

### Core data model (target state)

Documented in full in `docs/data-model-target.md`. Four levels, each with its own lifecycle:

```
Entity (PR #18 / Jira SKY-123)     ← long-lived, from first poll until closed/merged
  ↓
Events                              ← append-only; every poller detection + system emission
  ↓  (0 or 1 — only if a task_rule or prompt_trigger predicate matches)
Task                                ← "this entity needs attention, because of this event type"
  ↓
Runs                                ← one prompt execution against one task
```

Key invariants:

- **Entities are durable, events are immutable, tasks are ephemeral, runs are the work.** Memory is written per-run but materialized per-entity via `entity_links`.
- **Dedup:** at most one active task per `(entity_id, event_type, dedup_key)` — enforced by a partial unique index in `tasks`. `dedup_key` is usually empty; open-set discriminators (label name, status name) use it to get separate tasks per value.
- **No retroactive task creation.** A new task_rule or trigger applies to events _going forward_. Historical events in the log are not re-evaluated.
- **Events split on discriminators that change whether the situation needs attention** (`ci_check_failed` ≠ `ci_check_passed`, `review_approved` ≠ `review_changes_requested`). Attributes that just narrow the same situation (reviewer, check name, repo, label) stay as predicate-filterable metadata. Don't proliferate event types for Cartesian products.
- **Entities are org-wide; the team relationship is derived, not stored.** Repos/projects are configured per-org, so polling produces one shared entity per real object (`org_id`, no `team_id`) — forking per team would break the snapshot-diff re-emit invariant and the append-only log. **Standing rule (multi-mode read scoping):** every new entity-backed read must do one of — (i) join through team-scoped `tasks` (tasked-reads; the factory's membership semi-join, `internal/db/postgres/factory.go`), (ii) be gated by a team-scoped parent like `projects` (panel/backfill — gate on `Projects.Get` under RLS), (iii) filter by the requesting user's identity (personal views like the dashboard), or (iv) be explicitly annotated org-wide for a system job (e.g. `EntityStore.ListUnclassified` for the classifier). A default `org_id`-only entity list is a cross-team leak once polling is org-wide. The Jira stock/discovery deck shows *untasked* entities so the task semi-join can't help it — it scopes via the Jira projects attached to the viewer's teams (`ListActiveJiraTeamScoped`, `jira_project_status_rules` RLS semi-join). Postgres-only; SQLite/local is N=1 and stays unscoped.

### Event bus is the central pub/sub

`internal/eventbus` — `main.go` wires subscribers:

- `ws-broadcast` forwards every event to the frontend via websocket.
- `scorer` reacts to `system:poll:*` sentinels and kicks `ai.Runner.Trigger()`.
- `router` (`internal/routing/router.go`) consumes `github:*` / `jira:*` events, records them, creates/bumps tasks per task_rules, and fires matching prompt_triggers (auto-delegation). Also owns inline close checks and `ReDeriveAfterScoring` (post-scoring trigger pass for deferred `min_autonomy_suitability` thresholds).
- `poll-tracker` gates `/api/jira/stock` on first-poll-after-restart and surfaces one-shot "config took effect" toasts (announce-pending flag, flipped off after one completion).

Pollers publish events to the bus rather than invoking callbacks directly. This is how a poll cycle, a scorer run, and a UI push all stay decoupled.

### Poller / tracker

`internal/poller` manages GitHub + Jira pollers. `internal/tracker` does the diff logic: snapshot → refresh → diff against prior snapshot → emit typed events only on transitions. The snapshot-diff is the _sole_ source of truth for re-emit prevention — a check-run ID seen last cycle doesn't fire again. See `docs/tracked-events.md` for the taxonomy.

### Delegation (the "Agent" column)

`internal/delegate/spawner.go` + `internal/worktree` — delegation spins up a **headless Claude Code instance inside an isolated git worktree**. Credentials are hot-swapped into the spawner on config change (see `SetOnGitHubChanged` in `main.go`); the spawner instance itself is created once at startup. Agents stream stdout into `run_messages`; structured outputs (PRs opened, reviews posted) land in `run_artifacts` with a unique `is_primary` per run. Orphaned worktrees from crashed runs are cleaned on startup via `worktree.Cleanup()`.

### AI scoring

`internal/ai/runner.go` — a singleton runner with a trigger channel. `Trigger()` is idempotent during an active cycle. The `ProfileGate` forces the scorer to wait until repo profiling (`internal/repoprofile`) completes so the scorer has project context. Repo profiles have a 3-day TTL and regenerate on GitHub config change.

### HTTP server

`internal/server/server.go` — plain `net/http` + `http.ServeMux` using Go 1.22+ pattern-based routing (`"POST /api/tasks/{id}/swipe"`). Each handler group lives in its own file (`tasks.go`, `settings.go`, `triggers_handler.go`, ...). The SPA is served from `embed.FS`; unknown paths fall through to `index.html` for client-side routing.

### Frontend

React 19 + Vite + TypeScript + Tailwind v4. Router routes live in `frontend/src/main.tsx`. All API calls go to `/api/*`; a long-lived websocket at `/api/ws` streams events (frontend listens via `hooks/useWebSocket`). `AuthGate` blocks the app until setup is complete.

## Conventions to know before editing

- **Schema: goose-managed forward migrations, fresh installs only.** v1.11.0 is a hard reset — pre-v1.11.0 DBs are refused at boot via the brick check in `internal/db/migrations.go`. Operators run `triagefactory uninstall` (or `./scripts/clean-slate.sh` if working from source) and reinstall. New migrations land as `internal/db/migrations-sqlite/NNNNNNNNNNNN_description.sql` (12-digit `YYYYMMDDNNNN` version) with `-- +goose Up` / `-- +goose Down` markers. Down blocks are `SELECT 'down not supported';` no-ops. The brick check (`assertFreshOrCurrent`) gates entry to `goose.Up`: empty DB → proceed; `goose_db_version` contains the v1.11.0 baseline (202605130001) → proceed; anything else → `ErrPreV1110Install`. Postgres migrations live in `internal/db/migrations-postgres/`; `db.Migrate(db, dialect)` routes to the matching tree. Postgres tests use the `internal/db/pgtest` harness — testcontainer with two connections (AdminDB superuser, AppDB authenticator+tf_app) — and skip cleanly when Docker isn't available. See `docs/specs/sky-247-d3-multi-tenant-postgres-schema.html`.
- **Events catalog** is a read-only system registry seeded from `domain.AllEventTypes()` via `db.SeedEventTypes`. New event types must be added there _and_ the events_catalog table will reject emissions of unregistered types (FK from `events.event_type`).
- **System triggers ship disabled.** They're reference examples — users opt in or replace them. See `seed.go`.
- **Go module path:** `github.com/sky-ai-eng/triage-factory`. The GitHub org is `sky-ai-eng`.
- **Go version:** `go.mod` says 1.26.1, README says 1.23+; keep the floor modern but don't bump without reason.
- **User integration credentials never touch disk.** Per-user tokens that the running TF binary uses (GitHub PAT, Jira PAT, etc.) live in the OS keychain via `internal/auth`. Token fields in Settings show "leave blank to keep current" when a token is stored. This rule covers credentials the TF process reads at request time — it does NOT apply to multi-mode deployment secrets (DB passwords, GoTrue signing material, etc.), which live in the operator's `.env` like any compose deployment.
- **JWT verification.** `internal/auth/verify` wraps `MicahParks/keyfunc/v3` + `golang-jwt/jwt/v5` to verify GoTrue-signed RS256 access tokens against a remote JWKS. The **server auth path** is multi-mode only — local-mode boots without a GoTrue dependency and the request-handler middleware never constructs a Verifier. The `triagefactory jwk-init --verify` CLI smoke helper does construct a Verifier regardless of mode (operator-facing debug tool); that path is separate from the server. `triagefactory jwk-init [--write-env F]` generates the RS256 keypair GoTrue signs with — operator runs it once during install. See `docs/self-host-setup.md`.
- **Runtime mode flag.** `TF_MODE=local|multi` is read once at startup by `internal/runmode` (called from `main()` before the argv-dispatch switch). Default is `local`. Downstream packages branch on `runmode.Current()`: `internal/db` picks SQLite vs Postgres, `internal/paths` (forthcoming with SKY-248 D4b) resolves state-root paths, future auth + sandbox tickets gate multi-only behavior. `runmode.LocalDefaultOrg` ("default") is the synthetic org-context value local-mode callers pass everywhere a real `orgID` is expected.
- **Branch naming.** Use `aa/SKY-<NNN>` (uppercase ticket ID, no trailing slug) for ticketed work — e.g., `aa/SKY-327`, NOT `aa/sky-327-foo` or `aa/SKY-327-handler-sweep`. Linear holds the descriptive title; the branch name's job is just to point back to the ticket. For un-ticketed work, `aa/<short-kebab-slug>` is fine. `aa/` is the user's initials (Aidan Allchin); the `codex/...` prefix that exists in the repo comes from a different agent (OpenAI Codex) — don't copy it.

## Reference docs

- `docs/data-model-target.md` — authoritative spec for the entity/event/task/run model (the big ongoing rewrite).
- `docs/tracked-events.md` — GitHub/Jira event taxonomy + snapshot field list.
- `docs/usage.md` — CLI flags, config reference, polling details.
- `docs/for-agents/auto-delegation-briefing.md` — briefing for delegated agents.
