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
./scripts/multi-mode-dev.sh up            # start deps via docker-compose.yml + migrate
./scripts/multi-mode-dev.sh run           # run the host binary as the CONTROL half (API on :3000)
./scripts/multi-mode-dev.sh run executor  # run the EXECUTOR half (separate terminal; needs runsc + caps)
./scripts/multi-mode-dev.sh down          # tear down
```

The repo-root `.claude/settings.json` registers a `PostToolUse` hook that runs `goimports -w` on edited `.go` files and `prettier --write` on frontend sources — do not duplicate that work manually. In practice this means you can write/edit Go code that references a new package without hand-adding the import line first; goimports fills it in after the edit lands. Caveat: the hook runs after *every* Edit/Write call, so it strips an import as unused if you add it in one call and only add the code that uses it in a later call — add an import and its first use in the same Edit/Write call (or just skip writing the import yourself and let goimports add it once the using code is in place).

## Architecture

Triage Factory is a **single Go binary** (HTTP server + pollers + delegated-agent spawner) with a **React SPA embedded via `go:embed`** (see `embed.go`). State lives entirely on the user's machine: SQLite at `~/.triagefactory/triagefactory.db` (user settings persist in the `settings` row — `internal/config` reads/writes via `config.Init(db)` at startup), credentials in the OS keychain (`internal/auth`).

### The binary has two modes

`main.go` dispatches on `os.Args[1]`:

- **Server mode** (default) — HTTP API + websocket hub + pollers + scorer + event router + delegation spawner.
- **CLI mode** (`exec`, `status`) — invoked _by delegated Claude Code agents_ inside a worktree. `cmd/exec/` provides scoped GitHub/Jira subcommands the agent uses instead of calling those APIs directly, so credentials stay in the keychain and activity is auditable via `conversations` / `artifacts`.

### Core data model

The core data model is four levels, each with its own lifecycle:

```
Entity (PR #18 / Jira SKY-123)     ← long-lived, from first poll until closed/merged
  ↓
Events                              ← append-only; every poller detection + system emission
  ↓  (0 or 1 — only if a task_rule or prompt_trigger predicate matches)
Task                                ← "this entity needs attention, because of this event type"
  ↓
Conversations                       ← durable agent context: one row per transcript, any surface
  ↓
Claims                              ← one row per executor engagement with a conversation
```

**Conversations · messages · claims** is the unified agent data model (it replaced
the former `runs` + `curator_requests` fusion tables): a *conversation* is the
durable context — type (`delegation` | `curator` | `interactive` reserved |
namespaced `subagent:<kind>` reserved), task/trigger/project linkage, work-lifecycle
status (stored vocabulary `queued|running|open|terminals` — spend and KV-cache
warmth are derived from `messages`, never stored), `runtime` ratchet
(`sdk`|`native`), `sdk_session_id`, archive timestamp. A *claim* is one executor
engagement (executor id + boot epoch, sealed-credential pubkey, `phase` — the
live engagement's setup/parked sub-state, `fetching|cloning|agent_starting|
awaiting_credentials`, coalesced over the conversation's stored status on display
reads so a retry never rewrites the conversation row — at most one active per
conversation, schema-enforced). The transcript itself is the
`messages` table: one row per neutral API message, `delivered=false` rows are the
universal pending-input queue (follow-ups, staged injections, curator context
notices — the former three side-tables), `window_state`/`seq` are the native
loop's assembly columns, and `user_id`/`claim_id` attribute each row to the
requesting user and producing engagement. Queue-is-truth generalizes: "needs
driving" = a queued conversation or one with undelivered input and no active
claim.

Key invariants:

- **Entities are durable, events are immutable, tasks are ephemeral, conversations are the work.** Memory is written per-conversation but materialized per-entity via `entity_links`.
- **Dedup:** at most one active task per `(entity_id, event_type, dedup_key)` — enforced by a partial unique index in `tasks`. `dedup_key` is usually empty; open-set discriminators (label name, status name) use it to get separate tasks per value.
- **No retroactive task creation.** A new task_rule or trigger applies to events _going forward_. Historical events in the log are not re-evaluated.
- **Tracking changes are forward-only — the mirror of the rule above.** Adding a repo/project to a team's tracked set doesn't retroactively mint tasks for its history; removing one doesn't retroactively prune or close existing tasks. The team↔repo gate (`internal/routing` `handlerScopeMatchesEvent` + `TracksRepoSystem`) filters _future_ matches only; it deliberately does **not** reconcile `task_teams` visibility for tasks already created while the repo was tracked. A task is durable work (may have an in-flight run, an open PR, agent memory), so untracking never silently destroys it. This is symmetric in multi-team (team A untracks a shared repo → A keeps tasks it already had, gets no new ones; B is unaffected) and correct in solo N=1 (one team, so pruning visibility would orphan the task to nobody). Tradeoff acknowledged: an untracked repo stops polling, so its open PRs never emit the close event that would retire stale tasks — the answer is an explicit user-initiated "dismiss" affordance (its own ticket), never an automatic purge wired into a config save.
- **Events split on discriminators that change whether the situation needs attention** (`ci_check_failed` ≠ `ci_check_passed`, `review_approved` ≠ `review_changes_requested`). Attributes that just narrow the same situation (reviewer, check name, repo, label) stay as predicate-filterable metadata. Don't proliferate event types for Cartesian products.
- **Entities are org-wide; the team relationship is derived, not stored.** Repos/projects are configured per-org, so polling produces one shared entity per real object (`org_id`, no `team_id`) — forking per team would break the snapshot-diff re-emit invariant and the append-only log. **Standing rule (multi-mode read scoping):** every new entity-backed read must do one of — (i) join through team-scoped `tasks` (tasked-reads; e.g. `Tasks.ListActiveRefsForEntities`), (ii) be gated by a team-scoped parent like `projects` (panel/backfill — gate on `Projects.Get` under RLS) or by the team's **tracked set** (GitHub repos / Jira projects attached to the viewer's teams, via the `team_github_repos` / `jira_project_status_rules` RLS semi-joins), (iii) filter by the requesting user's identity (personal views like the dashboard), or (iv) be explicitly annotated org-wide for a system job (e.g. `EntityStore.ListUnclassified` for the classifier). A default `org_id`-only entity list is a cross-team leak once polling is org-wide. Two reads use the tracked-set semi-join because they surface *untasked* entities the task semi-join can't reach: the Jira stock/discovery deck (`ListActiveJiraTeamScoped`) and the **factory belt** (`FactoryReadStore`, `internal/db/postgres/factory.go` — TFAC-516 moved it off the prior task-existence semi-join so belt density stops being a side effect of task creation). Postgres-only; SQLite/local is N=1 and stays unscoped. **Standing rule (multi-mode write scoping):** membership is the read gate, never the write gate. Mutating an org-wide entity — one with no `team_id` to write against, so the change lands on every team tracking it — requires an **admin**: org admin, or team admin of a team whose tracked set contains it (`TracksRepoViewerAdminScoped` is the pattern; `repoMutationAccess` in `internal/server/repos_handler.go` is the reference handler). RLS cannot express this — `repo_profiles_all` is `org_id`-scoped by construction — so the handler predicate is the *only* enforcement and there is no DB backstop to catch a mistake. Split the two gates explicitly rather than reusing the read predicate. Denials are two-valued and both matter: outside the caller's tracked set → 404 (it isn't in their list either, disclose nothing); inside it but not an admin → 403 (a 404 for a row they can see reads as a bug).

### Event bus is the central pub/sub

`internal/eventbus` — `main.go` wires subscribers:

- `ws-broadcast` forwards every event to the frontend via websocket.
- `scorer` reacts to `system:poll:*` sentinels and kicks the per-org `ai.Manager.Trigger(orgID)`.
- `classifier` reacts to `system:poll:*` sentinels and kicks the project classifier (rotates through orgs internally).
- `profiler` reacts to `system:poll:*` GitHub completions and kicks the per-org `repoprofile.Manager.Trigger(orgID)` — a TTL-gated repo-profiling pass. This is what profiles new / stale / newly-reachable (App-only) repos with no "github changed" plumbing, in both run modes. The explicit "Re-profile" button (and a tracked-repo-set change) calls `Trigger(orgID, force=true)` to bypass the TTL.
- `router` (`internal/routing/router.go`) consumes `github:*` / `jira:*` events, records them, creates/bumps tasks per task_rules, and fires matching prompt_triggers (auto-delegation). Also owns inline close checks and `ReDeriveAfterScoring` (post-scoring trigger pass for deferred `min_autonomy_suitability` thresholds).
- `poll-tracker` records poll completions into the durable, org-scoped `poll_readiness` table (both dialects) that gates `/api/jira/stock` and drives the one-shot "config took effect" toast (announce-pending, cleared after one completion). Because it is durable — not the former in-memory flag — the gate reflects the last synced state across a process restart; it is re-armed by a config change (`MarkRestarted`), not by boot.

Pollers publish events to the bus rather than invoking callbacks directly. This is how a poll cycle, a scorer run, and a UI push all stay decoupled.

### Poller / tracker

`internal/poller` manages GitHub + Jira pollers. `internal/tracker` does the diff logic: snapshot → refresh → diff against prior snapshot → emit typed events only on transitions. The snapshot-diff is the _sole_ source of truth for re-emit prevention — a check-run ID seen last cycle doesn't fire again. See `docs/concepts/tracked-events.md` for the taxonomy.

### Delegation (the "Agent" column)

`internal/delegate/spawner.go` + `internal/worktree` — delegation spins up a **headless Claude Code instance inside an isolated git worktree**. Credentials are hot-swapped into the spawner on config change (see `SetOnGitHubChanged` in `main.go`); the spawner instance itself is created once at startup. Agents stream stdout into `messages`; structured outputs (PRs opened, reviews posted) land in `artifacts` with a unique `is_primary` per conversation. Orphaned worktrees from crashed runs are cleaned on startup via `worktree.Cleanup()`.

A PR run's static `<task_context>` carries a **history skeleton** (`internal/prskeleton`) alongside the point-in-time event fields: the PR's commits, reviews, and lifecycle transitions, collapsed to a line budget in three tiers (full → runs folded to counts → landmarks retained with the gaps elided). It is fetched per run/step from two REST GETs — the PR object and `issues/{n}/timeline` — so the only capability required is `Get(ctx, path)` and it inherits whatever credential path the run already resolved. Best-effort: a fetch failure logs and yields an empty block, never a failed run. It renders **inside** the task context's untrusted-marker region, since commit subjects and PR titles are externally authored.

### Sandbox, isolation, and executor credentials (multi-mode)

Agent runs — delegated runs and curator turns — execute through `internal/agentproc.Run`, which spawns the Claude Code SDK as a `node` subprocess. In **multi mode on Linux** it wraps that subprocess in a gVisor (runsc) jail; the gate is `shouldSandbox()` = `runmode.Current()==ModeMulti && GOOS=="linux"`, so **local mode runs the same subprocess unsandboxed** — everything below about capabilities and isolation is multi-mode only. (The Haiku system jobs — scorer/classifier/profiler — bypass this path in multi mode: `internal/systemllm` makes direct, toolless LLM calls with no subprocess and no sandbox; local mode still runs them through the SDK subprocess.)

The executor is split three ways so that no process holds both a dangerous power and exposure to hostile input:

- **cap-broker** (`cmd/capbroker`, `internal/sandbox`) — the only holder of `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`. Builds the per-run netns/veth/iptables/cgroup, launches/supervises runsc, owns the OCI spec from a fixed template, and validates the narrow launch RPC (`ValidateLaunchParams`). Holds **no credentials**, parses **no agent output**.
- **orchestrator** — the main process, capabilities **dropped at exec** via `setpriv` (Go can't drop reliably in-process). Runs the dispatcher, holds credentials, and binds the per-run **llm/git/egress proxies** (`internal/agentproc/proxies.go`) + the **agenthost** unix socket (`cmd/exec/agenthost`) that parse hostile agent traffic — with zero capabilities.
- **sandbox** — the gVisor jail. **Property B**: no real credential ever enters its env; the agent gets a per-run proxy URL + a throwaway token, and the real key is injected host-side on the upstream hop. Per-run fail-closed egress allowlist.

**Executor credentials never come from the secret store.** On `TF_ROLE=executor` the secret store is disabled (`pgstore.NewWithoutSecrets`; `TF_SECRET_ENCRYPTION_KEY` is never loaded). Credentials arrive as **sealed per-claim bundles** — the control plane resolves and seals them (`internal/credprovision` → `internal/credseal`) to the active claim's published sidecar key, writes a `claim_credentials` row, and the executor unseals once at claim and threads the plaintext on ctx (`credbundle.FromContext`). One channel for both surfaces: delegated engagements and curator turns are claims alike. So executor-side code (proxies, agenthost, git) reads credentials from the **bundle**, never `stores.Secrets` — resolving from the disabled store is a recurring bug class. Local mode uses the live secret store directly (no bundle); there is no fused multi role that does — `TF_ROLE=all` refuses to boot in multi (TFAC-637), sandboxed runs REQUIRE a prebuilt per-run network + credential sidecar (`agentproc.Run` errors without one), and the former in-process proxy / clone-token / agenthost-over-stores paths are deleted. Full posture: `docs/security/security-overview.md` + `docs/security/privilege-separation.md`.

### Curator

`internal/curator` — an interactive, per-project agent the user chats with, distinct from the task→run delegation flow. Turns are serialized per session and execute through `agentproc.Run` (sandboxed like a delegated run in multi-mode), with tool access to a per-project **knowledge base** and pinned repo worktrees (shared read-only per `(org, repo)`, seeded on demand). State lives in the shared conversation model — one `conversations` row per (project, creator), `visibility='private'` (creator-scoped through the standard RLS arms), turns = an undelivered user `messages` row claimed and driven by an executor `claims` row — plus the knowledge-base files under `TF_STATE_ROOT`. Reset archives the conversation (`archived_at`); the next message mints a fresh one.

### Instance registry (fleet membership)

`internal/instance` + the `instances` table (`db.InstanceStore`) — every TF process's persistent identity and heartbeat, the substrate the horizontal-scaling epic's later phases (reaper, placement, fleet dashboard) read. The id is a file: `internal/instance.EnsureIdentity` mints (or re-reads) `<TF_STATE_ROOT>/instance-id` under an exclusive flock held for the process lifetime, so a restart keeps the same id and two processes pointed at one state root fail fast instead of silently sharing an identity. `app.New` resolves it first thing at boot, registers it (`Instances.Register` — an atomic upsert that bumps `boot_epoch` on every restart), and hands the (id, boot_epoch) pair to the spawner via `Spawner.SetExecutorID`, replacing the old per-boot random uuid that stamped the claim's executor identity. `Spawner.RunInstanceHeartbeat` renews the row every ~4s with the live capacity/admission snapshot (host memory headroom, the dispatch memory gate, semaphore occupancy) — moving state that used to live only on the process onto a row other instances (and eventually the fleet dashboard) can read. Every process registers, not just executors (deployment-wide visibility — build versions, health, the eventual lease holder); the row's `role` column carries this process's resolved `TF_ROLE` (`all`/`control`/`executor` — TFAC-582), stamped by `app.registerInstance` from `runmode.Role()`.

### Background-brain leader lease

`internal/lease` + the `leases` table (Postgres-only — see below) — the leader election that gates the background brain (pollers/tracker, the durable event-queue drain worker + sweeper, and — as of TFAC-583 — brain-gated `ExtensionAPI.OnReady` workers) so exactly one control pod runs it at a time under `TF_ROLE=control`. `lease.Manager.Run` drives acquire/renew off a Postgres row (`holder_id`, a `term` fencing token bumped on every acquisition, `acquired_at`/`renewed_at`); a holder self-demotes on its **own monotonic clock** once `TF_LEASE_DEMOTE_SEC` (default 15s) elapses since its last successful renewal, strictly before the `TF_LEASE_TTL_SEC` (default 20s) a successor needs to see elapsed before it may acquire — so a demoted holder is provably stopped before a new one starts. `internal/app`'s `startBrain`/`stopBrain` (`brain.go`) are the actual start/stop unit both the lease callbacks and (at `TF_ROLE=all`/local, which never elects — zero lease I/O) a direct `Run()` call drive. Non-leader callers of a background Manager's `Trigger(orgID)` or the poller's `PollSoon(source, orgID)` — config-save handlers on a standby, the delegation spawner's classifier wait on an executor — relay over the `tf_ctl` Postgres NOTIFY/LISTEN channel (`internal/ctlbus`) to whichever pod holds the lease; the relay is lossy by design (a dropped message just costs one deferred `system:poll:*`-driven pass). `GET /readyz`'s poller-alive hard check is lease-conditional: the holder is byte-identical to the original (TFAC-573) contract, a standby hard-checks only `db`+`migrations` and reports `poller_github`/`poller_jira` as the literal string `"standby"` (never 503, so an LB keeps every standby in rotation), and a `lease` field (`{name, holder_id, is_holder, term}`) appears on every control pod's response — `TF_ROLE=all`/local never wire `SetLeaseStatus`, so the field is omitted there, keeping that contract frozen. The org-scoped `poll_readiness` table (`db.PollReadinessStore`, both dialects) replaced two former in-memory, leader-coupled flags — the `/api/jira/stock` readiness gate and the one-shot "config took effect" toast — so any control pod's API reflects state the actual leader produced.

### AI scoring

`internal/ai/manager.go` + `internal/ai/runner.go` — a per-org `ai.Manager` owns lazy per-org `Runner`s, each with its own trigger channel and single-flight cycle gate, so a slow cycle on one tenant doesn't head-of-line-block scoring on others. `Manager.Trigger(orgID)` is idempotent during an active cycle (signals merge). Scoring does **not** block on repo profiling — there is no `ProfileGate`. Profiling (`internal/repoprofile`) is an independent `system:poll:` subscriber (a sibling per-org `repoprofile.Manager`, same shape as the scorer); the scorer uses whatever profile context exists and improves on the next cycle as profiles land (eventual consistency). Repo profiles have a 3-day TTL and regenerate per poll cycle (the `profiler` subscriber) or on an explicit re-profile (which forces past the TTL).

### Prompt ownership

Prompt text is organized by **what kind of artifact it is**, not by which package consumes it. Three kinds:

- **Agent framework prompts — `internal/agentprompt`, the only package that owns agent-facing text.** Who the agent is, what harness it is in, what it may do. `Build(Spec, Parts)` composes `blocks/` per a Go manifest (`manifest.go`) across four axes: **surface** (machinist | curator) × **runtime** (sdk | native) × **family** (claude) × **mode** (local | multi). Five rules keep that from becoming a combinatorial tree, and they are load-bearing: (1) nothing outside this package embeds agent prompt text; (2) each block file varies on **at most one** axis — a GPT set later is `identity/machinist.gpt.txt` plus a manifest arm, not a new tree; (3) `Build` is **byte-identical for a fixed `Spec`**, across runs and processes — that's the cacheable-prefix property, and per-run text arrives in `Parts` and is appended after; (4) `Mode` is a **parameter**, never a `runmode.Current()` read inside the package, so both variants are testable without env manipulation; (5) the manifest is Go, not a config format. The mode axis exists because the alternative is lying: local mode has no push gate (`gitproxy` is wired only for a sandboxed run), no per-run credential scoping on raw `git`, and no egress allowlist — and an agent that discovers a stated rule is false has reason to doubt the rest. Tests enforce all of it: determinism, mode divergence, a **shrinking** allowlist of blocks still carrying `{{placeholders}}`, and no orphaned block files.
- **Shipped content — `internal/promptseed`.** The seed rows a team gets in the `prompts` table. The `.txt` in the repo is a *seed*: after provision the row is the source of truth and the user may rewrite any of it in the UI. `promptseed.Prompts()` / `promptseed.Blueprints()` feed the bootstrap seeder and the boot-time drift sync; the sync walks *blueprints*, so a prompt with no wrapping blueprint is seeded once and then never drift-corrected. The curator's jira-formatting guidance is a seeded row for exactly this reason — shipping a default nobody can see or change is how guidance goes stale.
- **System job prompts — next to their consumer.** Toolless LLM calls TF makes for itself, closer to a SQL query than to agent instructions: `internal/ai/prompts/batch-prioritize*` (scoring), `internal/repoprofile/prompts/` (profiling), `internal/projectclassify/` (classification). No agent, no envelope, no tools, never user-visible.

`internal/ai` exports no prompt string, and neither `internal/delegate` nor `internal/curator` imports it. `ee/slack` contributes its verb docs inward via `agentprompt.RegisterToolsReference` at init — that registry is the whole reason it exists, since the ee-import-boundary guard means core can never import ee. Note `internal/server/prompts/` is a Go package of prompt *API primitives*, not prompt text.

### HTTP server

`internal/server/server.go` — plain `net/http` + `http.ServeMux` using Go 1.22+ pattern-based routing (`"POST /api/tasks/{id}/swipe"`). Each handler group lives in its own file (`tasks.go`, `settings.go`, `triggers_handler.go`, ...). The SPA is served from `embed.FS`; unknown paths fall through to `index.html` for client-side routing.

### Frontend

React 19 + Vite + TypeScript + Tailwind v4. Router routes live in `frontend/src/main.tsx`. All API calls go to `/api/*`; a long-lived websocket at `/api/ws` streams events (frontend listens via `hooks/useWebSocket`). `AuthGate` blocks the app until setup is complete.

## Conventions to know before editing

- **Schema: goose-managed forward migrations, fresh installs only.** The consolidated baseline (`202605130001`) is a hard reset — pre-baseline DBs are refused at boot via the brick check in `internal/db/migrations.go`. Operators run `triagefactory uninstall` (or `./scripts/clean-slate.sh` if working from source) and reinstall. New migrations land as `internal/db/migrations-sqlite/NNNNNNNNNNNN_description.sql` (12-digit `YYYYMMDDNNNN` version) with `-- +goose Up` / `-- +goose Down` markers. Down blocks are `SELECT 'down not supported';` no-ops. The brick check (`assertFreshOrCurrent`) gates entry to `goose.Up`: empty DB → proceed; `goose_db_version` contains the baseline (202605130001) → proceed; anything else → `ErrPreV1110Install`. Postgres migrations live in `internal/db/migrations-postgres/`; `db.Migrate(db, dialect)` routes to the matching tree. Postgres tests use the `internal/db/pgtest` harness — testcontainer with two connections (AdminDB superuser, AppDB authenticator+tf_app) — and skip cleanly when Docker isn't available. See `docs/for-agents/specs/sky-247-d3-multi-tenant-postgres-schema.html`.
- **Stores are dual-dialect behind one interface.** All persistence goes through the `db.Stores` interfaces (`internal/db`), implemented by **both** SQLite (`internal/db/sqlite`) and Postgres (`internal/db/postgres`) — a new store method is the interface + both implementations + a case in the `internal/db/dbtest` conformance suite (which runs against both). Each pod opens two pools: an **admin pool** (RLS-bypassed) and an **app pool** (RLS). Request handlers use the app pool under a per-request RLS context; JWT-less background jobs (pollers, scorer, delegation) use the `...System` method variants on the admin pool, with `org_id` bound by argument rather than RLS. The read-scoping standing rule above governs *what* those org-wide reads may return.
- **Events catalog** is a read-only system registry seeded from `domain.AllEventTypes()` via `db.SeedEventTypes`. New event types must be added there _and_ the events_catalog table will reject emissions of unregistered types (FK from `events.event_type`).
- **System triggers ship disabled.** They're reference examples — users opt in or replace them. See `internal/db/event_handlers_store.go` (`ShippedEventHandlers`).
- **Go module path:** `github.com/sky-ai-eng/triage-factory`. The GitHub org is `sky-ai-eng`.
- **Go version:** `go.mod` says 1.26.1, README says 1.23+; keep the floor modern but don't bump without reason.
- **User integration credentials never touch disk.** Per-user tokens that the running TF binary uses (GitHub PAT, Jira PAT, etc.) live in the OS keychain via `internal/auth`. Token fields in Settings show "leave blank to keep current" when a token is stored. This rule covers credentials the TF process reads at request time — it does NOT apply to multi-mode deployment secrets (DB passwords, GoTrue signing material, etc.), which live in the operator's `.env` like any compose deployment.
- **JWT verification.** `internal/auth/verify` wraps `MicahParks/keyfunc/v3` + `golang-jwt/jwt/v5` to verify GoTrue-signed RS256 access tokens against a remote JWKS. The **server auth path** is multi-mode only — local-mode boots without a GoTrue dependency and the request-handler middleware never constructs a Verifier. The `triagefactory jwk-init --verify` CLI smoke helper does construct a Verifier regardless of mode (operator-facing debug tool); that path is separate from the server. `triagefactory jwk-init [--write-env F]` generates the RS256 keypair GoTrue signs with — operator runs it once during install. See `docs/self-hosting/install.md`.
- **Runtime mode flag.** `TF_MODE=local|multi` is read once at startup by `internal/runmode` (called from `main()` before the argv-dispatch switch). Default is `local`. Downstream packages branch on `runmode.Current()`: `internal/db` picks SQLite vs Postgres, `internal/paths` (forthcoming with SKY-248 D4b) resolves state-root paths, future auth + sandbox tickets gate multi-only behavior. `runmode.LocalDefaultOrgID` is the synthetic org-context value local-mode callers pass everywhere a real `orgID` is expected.
- **Role flag.** `TF_ROLE=control|executor` is a **multi-mode-only input** naming which half of the split a process runs: `control` serves the API/WS and competes for the background-brain lease; `executor` runs the dispatcher + sandboxes and takes no inbound traffic. Multi requires it explicitly (unset, `all`, or a typo fails boot — every multi deployment is the control+executor split, so per-run credential isolation is structural, not a knob); **local mode ignores `TF_ROLE` entirely** — the single-process shape is gated on the mode, and `runmode.RoleAll` survives only as the internal name of local's resolved inventory (the everything-plan, the instance-registry stamp). Resolved via `internal/runmode`; the split is specified in `docs/for-agents/specs/horizontal-scaling/`.
- **Branch naming.** Use `aa/TFAC-<NNN>` (uppercase ticket ID, no trailing slug) for ticketed work — e.g., `aa/TFAC-327`, NOT `aa/tfac-327-foo` or `aa/TFAC-327-handler-sweep`. Linear holds the descriptive title; the branch name's job is just to point back to the ticket. For un-ticketed work, `aa/<short-kebab-slug>` is fine. `aa/` is the user's initials (Aidan Allchin); the `codex/...` prefix that exists in the repo comes from a different agent (OpenAI Codex) — don't copy it.
- **No ticket references in code comments.** Keep `TFAC-`/`SKY-` ids out of source comments — they belong in commit messages, PR bodies, and branch names only. Comments explain **why**; they never restate what the code does or assert what other code does.

## Reference docs

- `docs/concepts/tracked-events.md` — GitHub/Jira event taxonomy + snapshot field list.
- `docs/local-mode/` — local-mode usage: CLI flags, configuration, secret storage, headless, polling.
- `docs/self-hosting/` — multi-mode operator guides (install, scaling, SSO, monitoring).
- `docs/security/` — isolation tiers, security overview, privilege separation, seccomp, release verification.
- `docs/for-agents/auto-delegation-briefing.md` — briefing for delegated agents.
