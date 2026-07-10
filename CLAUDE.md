# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository. This is a public repository. Under no circumstances should you commit sensitive information or custom configuration files related to specific deployments to git.

## Build, lint, test

```bash
# Full build — frontend first (Go embeds frontend/dist/), then the binary.
# Frontend uses pnpm (pinned via the "packageManager" field; `corepack enable`
# provisions it). The agentproc SDK runtime installer still uses npm by design.
cd frontend && pnpm install && pnpm run build && cd ..
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
cd frontend && pnpm run dev

# Nuke local DB + config + keychain entries (fresh first-run flow)
./scripts/clean-slate.sh

# Multi-mode (Postgres + GoTrue + SeaweedFS), on demand only — not part of
# the default flow above and never run automatically:
./scripts/multi-mode-dev.sh up    # start deps via docker-compose.yml + migrate
./scripts/multi-mode-dev.sh run   # run the host binary in TF_MODE=multi against them
./scripts/multi-mode-dev.sh down  # tear down
```

The repo-root `.claude/settings.json` registers a `PostToolUse` hook that runs `goimports -w` on edited `.go` files and `prettier --write` on frontend sources — do not duplicate that work manually.

## Architecture

Triage Factory is a **single Go binary** (HTTP server + pollers + delegated-agent spawner) with a **React SPA embedded via `go:embed`** (see `embed.go`). State lives entirely on the user's machine: SQLite at `~/.triagefactory/triagefactory.db` (user settings persist in the `settings` row — `internal/config` reads/writes via `config.Init(db)` at startup), credentials in the OS keychain (`internal/auth`).

### The binary has two modes

`main.go` dispatches on `os.Args[1]`:

- **Server mode** (default) — HTTP API + websocket hub + pollers + scorer + event router + delegation spawner.
- **CLI mode** (`exec`, `status`) — invoked _by delegated Claude Code agents_ inside a worktree. `cmd/exec/` provides scoped GitHub/Jira subcommands the agent uses instead of calling those APIs directly, so credentials stay in the keychain and activity is auditable via `runs` / `run_artifacts`.

### Core data model (target state)

The product vision and direction live in `docs/where-tf-is-going.html`. Four levels, each with its own lifecycle:

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
- **Tracking changes are forward-only — the mirror of the rule above.** Adding a repo/project to a team's tracked set doesn't retroactively mint tasks for its history; removing one doesn't retroactively prune or close existing tasks. The team↔repo gate (`internal/routing` `handlerScopeMatchesEvent` + `TracksRepoSystem`) filters _future_ matches only; it deliberately does **not** reconcile `task_teams` visibility for tasks already created while the repo was tracked. A task is durable work (may have an in-flight run, an open PR, agent memory), so untracking never silently destroys it. This is symmetric in multi-team (team A untracks a shared repo → A keeps tasks it already had, gets no new ones; B is unaffected) and correct in solo N=1 (one team, so pruning visibility would orphan the task to nobody). Tradeoff acknowledged: an untracked repo stops polling, so its open PRs never emit the close event that would retire stale tasks — the answer is an explicit user-initiated "dismiss" affordance (its own ticket), never an automatic purge wired into a config save.
- **Events split on discriminators that change whether the situation needs attention** (`ci_check_failed` ≠ `ci_check_passed`, `review_approved` ≠ `review_changes_requested`). Attributes that just narrow the same situation (reviewer, check name, repo, label) stay as predicate-filterable metadata. Don't proliferate event types for Cartesian products.
- **Entities are org-wide; the team relationship is derived, not stored.** Repos/projects are configured per-org, so polling produces one shared entity per real object (`org_id`, no `team_id`) — forking per team would break the snapshot-diff re-emit invariant and the append-only log. **Standing rule (multi-mode read scoping):** every new entity-backed read must do one of — (i) join through team-scoped `tasks` (tasked-reads; e.g. `Tasks.ListActiveRefsForEntities`), (ii) be gated by a team-scoped parent like `projects` (panel/backfill — gate on `Projects.Get` under RLS) or by the team's **tracked set** (GitHub repos / Jira projects attached to the viewer's teams, via the `team_github_repos` / `jira_project_status_rules` RLS semi-joins), (iii) filter by the requesting user's identity (personal views like the dashboard), or (iv) be explicitly annotated org-wide for a system job (e.g. `EntityStore.ListUnclassified` for the classifier). A default `org_id`-only entity list is a cross-team leak once polling is org-wide. Two reads use the tracked-set semi-join because they surface *untasked* entities the task semi-join can't reach: the Jira stock/discovery deck (`ListActiveJiraTeamScoped`) and the **factory belt** (`FactoryReadStore`, `internal/db/postgres/factory.go` — TFAC-516 moved it off the prior task-existence semi-join so belt density stops being a side effect of task creation). Postgres-only; SQLite/local is N=1 and stays unscoped.

### Event bus is the central pub/sub

`internal/eventbus` — `main.go` wires subscribers:

- `ws-broadcast` forwards every event to the frontend via websocket.
- `scorer` reacts to `system:poll:*` sentinels and kicks the per-org `ai.Manager.Trigger(orgID)`.
- `classifier` reacts to `system:poll:*` sentinels and kicks the project classifier (rotates through orgs internally).
- `profiler` reacts to `system:poll:*` GitHub completions and kicks the per-org `repoprofile.Manager.Trigger(orgID)` — a TTL-gated repo-profiling pass. This is what profiles new / stale / newly-reachable (App-only) repos with no "github changed" plumbing, in both run modes. The explicit "Re-profile" button (and a tracked-repo-set change) calls `Trigger(orgID, force=true)` to bypass the TTL.
- `router` (`internal/routing/router.go`) consumes `github:*` / `jira:*` events, records them, creates/bumps tasks per task_rules, and fires matching prompt_triggers (auto-delegation). Also owns inline close checks and `ReDeriveAfterScoring` (post-scoring trigger pass for deferred `min_autonomy_suitability` thresholds).
- `poll-tracker` gates `/api/jira/stock` on first-poll-after-restart and surfaces one-shot "config took effect" toasts (announce-pending flag, flipped off after one completion).

Pollers publish events to the bus rather than invoking callbacks directly. This is how a poll cycle, a scorer run, and a UI push all stay decoupled.

### Poller / tracker

`internal/poller` manages GitHub + Jira pollers. `internal/tracker` does the diff logic: snapshot → refresh → diff against prior snapshot → emit typed events only on transitions. The snapshot-diff is the _sole_ source of truth for re-emit prevention — a check-run ID seen last cycle doesn't fire again. See `docs/tracked-events.md` for the taxonomy.

### Delegation (the "Agent" column)

`internal/delegate/spawner.go` + `internal/worktree` — delegation spins up a **headless Claude Code instance inside an isolated git worktree**. Credentials are hot-swapped into the spawner on config change (see `SetOnGitHubChanged` in `main.go`); the spawner instance itself is created once at startup. Agents stream stdout into `run_messages`; structured outputs (PRs opened, reviews posted) land in `run_artifacts` with a unique `is_primary` per run. Orphaned worktrees from crashed runs are cleaned on startup via `worktree.Cleanup()`.

### Instance registry (fleet membership)

`internal/instance` + the `instances` table (`db.InstanceStore`) — every TF process's persistent identity and heartbeat, the substrate the horizontal-scaling epic's later phases (reaper, placement, fleet dashboard) read. The id is a file: `internal/instance.EnsureIdentity` mints (or re-reads) `<TF_STATE_ROOT>/instance-id` under an exclusive flock held for the process lifetime, so a restart keeps the same id and two processes pointed at one state root fail fast instead of silently sharing an identity. `app.New` resolves it first thing at boot, registers it (`Instances.Register` — an atomic upsert that bumps `boot_epoch` on every restart), and hands the (id, boot_epoch) pair to the spawner via `Spawner.SetExecutorID`, replacing the old per-boot random uuid that stamped `runs.executor_id`. `Spawner.RunInstanceHeartbeat` renews the row every ~4s with the live capacity/admission snapshot (host memory headroom, the dispatch memory gate, semaphore occupancy) — moving state that used to live only on the process onto a row other instances (and eventually the fleet dashboard) can read. Every process registers, not just executors (deployment-wide visibility — build versions, health, the eventual lease holder); the row's `role` column carries this process's resolved `TF_ROLE` (`all`/`control`/`executor` — TFAC-582), stamped by `app.registerInstance` from `runmode.Role()`.

### Background-brain leader lease

`internal/lease` + the `leases` table (Postgres-only — see below) — the leader election that gates the background brain (pollers/tracker, the durable event-queue drain worker + sweeper, and — as of TFAC-583 — brain-gated `ExtensionAPI.OnReady` workers) so exactly one control pod runs it at a time under `TF_ROLE=control`. `lease.Manager.Run` drives acquire/renew off a Postgres row (`holder_id`, a `term` fencing token bumped on every acquisition, `acquired_at`/`renewed_at`); a holder self-demotes on its **own monotonic clock** once `TF_LEASE_DEMOTE_SEC` (default 15s) elapses since its last successful renewal, strictly before the `TF_LEASE_TTL_SEC` (default 20s) a successor needs to see elapsed before it may acquire — so a demoted holder is provably stopped before a new one starts. `internal/app`'s `startBrain`/`stopBrain` (`brain.go`) are the actual start/stop unit both the lease callbacks and (at `TF_ROLE=all`/local, which never elects — zero lease I/O) a direct `Run()` call drive. Non-leader callers of a background Manager's `Trigger(orgID)` or the poller's `PollSoon(source, orgID)` — config-save handlers on a standby, the delegation spawner's classifier wait on an executor — relay over the `tf_ctl` Postgres NOTIFY/LISTEN channel (`internal/ctlbus`) to whichever pod holds the lease; the relay is lossy by design (a dropped message just costs one deferred `system:poll:*`-driven pass). `GET /readyz`'s poller-alive hard check is lease-conditional: the holder is byte-identical to the original (TFAC-573) contract, a standby hard-checks only `db`+`migrations` and reports `poller_github`/`poller_jira` as the literal string `"standby"` (never 503, so an LB keeps every standby in rotation), and a `lease` field (`{name, holder_id, is_holder, term}`) appears on every control pod's response — `TF_ROLE=all`/local never wire `SetLeaseStatus`, so the field is omitted there, keeping that contract frozen. The org-scoped `poll_readiness` table (`db.PollReadinessStore`, both dialects) replaced two former in-memory, leader-coupled flags — the `/api/jira/stock` readiness gate and the one-shot "config took effect" toast — so any control pod's API reflects state the actual leader produced.

### AI scoring

`internal/ai/manager.go` + `internal/ai/runner.go` — a per-org `ai.Manager` owns lazy per-org `Runner`s, each with its own trigger channel and single-flight cycle gate, so a slow cycle on one tenant doesn't head-of-line-block scoring on others. `Manager.Trigger(orgID)` is idempotent during an active cycle (signals merge). Scoring does **not** block on repo profiling — there is no `ProfileGate`. Profiling (`internal/repoprofile`) is an independent `system:poll:` subscriber (a sibling per-org `repoprofile.Manager`, same shape as the scorer); the scorer uses whatever profile context exists and improves on the next cycle as profiles land (eventual consistency). Repo profiles have a 3-day TTL and regenerate per poll cycle (the `profiler` subscriber) or on an explicit re-profile (which forces past the TTL).

### HTTP server

`internal/server/server.go` — plain `net/http` + `http.ServeMux` using Go 1.22+ pattern-based routing (`"POST /api/tasks/{id}/swipe"`). Each handler group lives in its own file (`tasks.go`, `settings.go`, `triggers_handler.go`, ...). The SPA is served from `embed.FS`; unknown paths fall through to `index.html` for client-side routing.

### Frontend

React 19 + Vite + TypeScript + Tailwind v4. Router routes live in `frontend/src/main.tsx`. All API calls go to `/api/*`; a long-lived websocket at `/api/ws` streams events (frontend listens via `hooks/useWebSocket`). `AuthGate` blocks the app until setup is complete.

## Conventions to know before editing

- **Schema: goose-managed forward migrations, fresh installs only.** The consolidated baseline (`202605130001`) is a hard reset — pre-baseline DBs are refused at boot via the brick check in `internal/db/migrations.go`. Operators run `triagefactory uninstall` (or `./scripts/clean-slate.sh` if working from source) and reinstall. New migrations land as `internal/db/migrations-sqlite/NNNNNNNNNNNN_description.sql` (12-digit `YYYYMMDDNNNN` version) with `-- +goose Up` / `-- +goose Down` markers. Down blocks are `SELECT 'down not supported';` no-ops. The brick check (`assertFreshOrCurrent`) gates entry to `goose.Up`: empty DB → proceed; `goose_db_version` contains the baseline (202605130001) → proceed; anything else → `ErrPreV1110Install`. Postgres migrations live in `internal/db/migrations-postgres/`; `db.Migrate(db, dialect)` routes to the matching tree. Postgres tests use the `internal/db/pgtest` harness — testcontainer with two connections (AdminDB superuser, AppDB authenticator+tf_app) — and skip cleanly when Docker isn't available. See `docs/specs/sky-247-d3-multi-tenant-postgres-schema.html`.
- **Events catalog** is a read-only system registry seeded from `domain.AllEventTypes()` via `db.SeedEventTypes`. New event types must be added there _and_ the events_catalog table will reject emissions of unregistered types (FK from `events.event_type`).
- **System triggers ship disabled.** They're reference examples — users opt in or replace them. See `seed.go`.
- **Go module path:** `github.com/sky-ai-eng/triage-factory`. The GitHub org is `sky-ai-eng`.
- **Go version:** `go.mod` says 1.26.1, README says 1.23+; keep the floor modern but don't bump without reason.
- **User integration credentials never touch disk.** Per-user tokens that the running TF binary uses (GitHub PAT, Jira PAT, etc.) live in the OS keychain via `internal/auth`. Token fields in Settings show "leave blank to keep current" when a token is stored. This rule covers credentials the TF process reads at request time — it does NOT apply to multi-mode deployment secrets (DB passwords, GoTrue signing material, etc.), which live in the operator's `.env` like any compose deployment.
- **JWT verification.** `internal/auth/verify` wraps `MicahParks/keyfunc/v3` + `golang-jwt/jwt/v5` to verify GoTrue-signed RS256 access tokens against a remote JWKS. The **server auth path** is multi-mode only — local-mode boots without a GoTrue dependency and the request-handler middleware never constructs a Verifier. The `triagefactory jwk-init --verify` CLI smoke helper does construct a Verifier regardless of mode (operator-facing debug tool); that path is separate from the server. `triagefactory jwk-init [--write-env F]` generates the RS256 keypair GoTrue signs with — operator runs it once during install. See `docs/self-host-setup.md`.
- **Runtime mode flag.** `TF_MODE=local|multi` is read once at startup by `internal/runmode` (called from `main()` before the argv-dispatch switch). Default is `local`. Downstream packages branch on `runmode.Current()`: `internal/db` picks SQLite vs Postgres, `internal/paths` (forthcoming with SKY-248 D4b) resolves state-root paths, future auth + sandbox tickets gate multi-only behavior. `runmode.LocalDefaultOrgID` is the synthetic org-context value local-mode callers pass everywhere a real `orgID` is expected.
- **Branch naming.** Use `aa/TFAC-<NNN>` (uppercase ticket ID, no trailing slug) for ticketed work — e.g., `aa/TFAC-327`, NOT `aa/tfac-327-foo` or `aa/TFAC-327-handler-sweep`. Linear holds the descriptive title; the branch name's job is just to point back to the ticket. For un-ticketed work, `aa/<short-kebab-slug>` is fine. `aa/` is the user's initials (Aidan Allchin); the `codex/...` prefix that exists in the repo comes from a different agent (OpenAI Codex) — don't copy it.

## Reference docs

- `docs/where-tf-is-going.html` — product vision + direction for the entity/event/task/run model.
- `docs/tracked-events.md` — GitHub/Jira event taxonomy + snapshot field list.
- `docs/usage.md` — CLI flags, config reference, polling details.
- `docs/for-agents/auto-delegation-briefing.md` — briefing for delegated agents.
