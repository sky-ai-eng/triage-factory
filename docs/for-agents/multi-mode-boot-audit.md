# Triage Factory Multi-Mode Boot Audit

**Scope:** every remaining `runmode.LocalDefault*` reference (and a few implicit single-org assumptions) reached during boot. Findings ordered by execution phase.

**Context:** PR #226 (SKY-339) landed multi-mode boot wiring. `TF_MODE=multi` now opens admin + authenticator Postgres pools, constructs `postgres.New(admin, app)` as the CuratorStore implementation, boots the GoTrue verifier + session store, and reaches `ListenAndServe` without fataling. Three call sites were explicitly gated to local-only in that PR: `db.BootstrapLocalAgent`, the `worktree.SetOnCloneResult` callback, and `skills.ImportAll`. This audit covers everything else.

## Phase 1: main() startup sequence (pre-server-construction)

### 1. Cleanup of orphaned worktrees reads taken-over runs from the local sentinel org only
- **Where:** `main.go:559` — `stores.AgentRuns.ListTakenOverIDsSystem(ctx, runmode.LocalDefaultOrg)`
- **Local purpose:** Cleanup of worktrees from crashed runs; preserves `~/.claude/projects/` JSONLs for any run still flagged `taken_over` so the user can `triagefactory resume`. Runs **unconditionally** in both modes.
- **Multi-mode:** Wrong shape. In multi mode the lookup returns nothing (no runs ever exist under the synthetic org), and `worktree.CleanupWithOptions` proceeds with an empty preserve set — so taken-over runs in **real** tenant orgs would have their `~/.claude/projects/` entries nuked. Fix: gate this whole block on `ModeLocal` (the `resume` CLI is local-only anyway per the comment on `cmd/resume/resume.go:62`), or iterate `stores.Orgs.ListActiveSystem` and union the preserve sets.

### 2. Local GitHub-identity bootstrap — REMOVED (SKY-458)
- **Was:** `bootstrapLocalGitHubIdentity`, which derived the local user's
  `user_github_identities` row from the configured org PAT at boot.
- **Removed** because it conflated access (the org PAT, PAT_1) with identity
  (the user's `@login`, PAT_2). Per-user GitHub identity is now captured only
  through its own surface — the setup wizard's User step / the Connect gate page
  (`internal/server/github_connect.go`) — in both modes, never derived from the
  org credential. The local user binds their login through that step like any
  other user; nothing happens silently at boot.

### 3. Local Jira-identity capture — same as above
- **Where:** `internal/server/jira_connect.go` → `handleJiraIdentityPAT` (the boot-time `bootstrapLocalJiraIdentity` was removed by SKY-461).
- **Local purpose:** The per-user Jira PAT bind flow validates the token, stores it host-scoped, and derives the user's host-scoped identity into `user_jira_identities` (SKY-397) — like the GitHub bind in #2, never derived from the org credential at boot. Nothing happens silently at boot.
- **Multi-mode:** Request-scoped capture under the caller's claims; the identity row is RLS-gated self-only. DONE.

### 4. `seedDefaultPrompts` iterates a single hardcoded `(LocalDefaultOrg, LocalDefaultTeamID)` pair
- **Where:** `seed.go:48` — `orgs := []orgTeam{{runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID}}` (called unconditionally at `main.go:586`)
- **Local purpose:** Seeds the six shipped system prompts + their team-scoped event handlers (rules + triggers) into the one local team. The TODO at `seed.go:43` calls this out: "when OrgStore lands, replace this hard-coded slice with `stores.Orgs.ListAll(ctx) × per-org teams`."
- **Multi-mode:** Three options, in increasing complexity:
  - **Cheapest:** gate the whole call on `runmode.ModeLocal` — shipped prompts/handlers materialize via the org-create + team-create flows (SKY-257 D14) instead. The sentinel team has no FK target in multi-mode Postgres, so the current call would error at the `team_id` FK regardless.
  - **Right:** iterate `stores.Orgs.ListActiveSystem` × per-org `stores.Teams.List` and re-seed on every boot (the inserts are `INSERT OR IGNORE` so it's safe). But that overlaps with the team-create handler's responsibility.
  - Recommend the gate.

### 5. `db.BootstrapLocalAgent` is correctly gated
- **Where:** `main.go:607-611`
- **Local purpose:** Synthesizes the local agent row + team_agents row.
- **Multi-mode:** Already gated. DONE (per PR #226).

### 6. `skills.ImportAll` — boot path gated; the HTTP route needed its own gate (TFAC-109)
- **Where:** boot: `internal/app/startup.go` `importLocalSkills` (moved out of main.go; double-gated on `a.local()` + the sentinel org existing). HTTP: `POST /api/skills/import` → `internal/server/skills_handler.go`.
- **Local purpose:** Auto-imports SKILL.md prompts as `imported`-source rows.
- **Multi-mode:** The boot call was always gated, but this item originally examined only that call site — the HTTP route was reachable in multi with no mode check and returned its failures as a 200 with a swallowed errors array. TFAC-109 501-gated the route (the scan reads the orchestrator's own `~/.claude/skills`, which is not a tenant concept) and added the multi-mode replacement: `POST /api/skills/upload` imports pasted SKILL.md content as a claims-bound, org/team-scoped prompt. DONE.

### 7. `worktree.SetOnCloneResult` is correctly gated
- **Where:** `main.go:645-702`
- **Local purpose:** Stamps `repo_profiles.clone_status` + broadcasts WS updates when bare clones succeed/fail.
- **Multi-mode:** Already gated per PR #226 (no live clone-status surfacing in multi for now). DONE.

## Phase 2: server-construction-time wiring

### 8. `LifetimeDistinctCounter.Hydrate` aggregates events across **all** orgs into one process-wide counter
- **Where:** `main.go:712-717` + `internal/db/lifetime_counter.go`
- **Local purpose:** Powers the factory snapshot's "lifetime distinct entities per station" stat — runs an unqualified `SELECT DISTINCT event_type, entity_id FROM events WHERE entity_id IS NOT NULL` once, then folds in deltas via `SetOnEventRecorded`.
- **Multi-mode:** This is a **cross-tenant leak**. The counter has no `orgID` dimension; in multi mode it merges events from every tenant into one map and serves them via `srv.SetLifetimeCounter` → factory_handler. Two real options:
  - Drop the orgID-blind hydrate + hook; recompute per-org on demand (cheap given the partial index).
  - Add an orgID dimension throughout (map `orgID → map[event_type]int`), hydrate one row per org, and have `Snapshot(orgID)` return the per-tenant slice.
  - Either way: this is **not safe to ship to multi-tenant** in its current shape.

### 9. `CancelOrphanedNonTerminalCuratorRequests` cancels every tenant's queued/running curator rows
- **Where:** `main.go:884` → `internal/db/curator.go:223`
- **Local purpose:** Startup recovery; cancels stranded curator turns so the user re-sends rather than getting a delayed mystery reply.
- **Multi-mode:** Runs an unqualified `UPDATE curator_requests SET status='cancelled' WHERE status IN ('queued', 'running')` — process-wide. In a hosted deploy that restarts (e.g. rolling deploy of the binary), this nukes *every* tenant's in-flight chat. Arguably correct — a binary restart genuinely kills every per-project goroutine in the process — but it's worth deciding explicitly. If you accept it as correct, leave a comment; if not, scope it per-org-of-this-pod or rework to soft-cancel only orgs whose live agent goroutine had a known PID.

### 10. Curator's `materializePinnedRepos` reads `repo_profiles` under the sentinel org
- **Where:** `internal/curator/repos.go:39` — `repos.Get(ctx, runmode.LocalDefaultOrgID, slug)`
- **Local purpose:** Per-dispatch refresh of pinned-repo worktrees inside each curator chat.
- **Multi-mode:** Per-dispatch, not strictly startup — but the curator runtime is constructed at startup (`main.go:889`) and the path is reached on every chat send. In multi mode this reads the wrong org's profile and would always return nil (no profiles in the synthetic org), silently dropping every pinned repo from every multi-mode chat. Fix: thread the project's owning orgID through `materializePinnedRepos` (the caller already has `orgID` from the curator request).

## Phase 3: poller wiring + initial start

### 11. `pollerMgr.RestartAll(ctx, orgID)` runs with the local-sentinel orgID in both modes
- **Where:** `main.go:1106` (`orgID := runmode.LocalDefaultOrgID`) → `main.go:1124` / `main.go:1130` → `internal/poller/manager.go:99` `RestartAll`
- **Local purpose:** Picks up the configured GitHub PAT + Jira PAT from the keychain at boot. `orgID` is the *credential-resolution scope*, not the polling scope — the cycle inside iterates `stores.Orgs.ListActiveSystem` itself.
- **Multi-mode:** The "initial start with current credentials" block (`main.go:1100-1131`) reads `config.Load()` + `integrations.Load(ctx, ..., LocalDefaultOrgID)` at startup, then constructs a GitHub client from those creds and hands it to `spawner.UpdateCredentials` / `srv.SetGitHubClient` — i.e. **one process-global GitHub client built from one tenant's PAT**. That's fundamentally wrong shape for multi-mode (every org has its own PAT / GitHub App). Fix: gate the whole "initial start with current credentials" block on `runmode.ModeLocal`. Multi-mode pollers boot empty and resolve per-org credentials on each cycle (which is also where the GitHub App work in SKY-263 will land).

### 12. `bootstrapBareClones` uses the sentinel org to enumerate repos
- **Where:** `main.go:134-149` (`bootstrapBareClones`), called from `main.go:1014` + `main.go:1126`
- **Local purpose:** After profiling completes, ensures every configured repo has a bare clone on disk so first-delegation latency is bounded.
- **Multi-mode:** Hardcoded `runmode.LocalDefaultOrgID` on the `repos.ListSystem` call. Same shape as #11 — the bare-clone bootstrap should iterate `stores.Orgs.ListActiveSystem` and union the targets, OR gate the whole thing on local-mode and let multi-mode pay lazy-clone latency on first delegation per repo (also acceptable; bootstrap is `best-effort` per its docstring).

### 13. `poller.Manager.localGitHubUserID` is a package-level var seeded from the sentinel
- **Where:** `internal/poller/manager.go:26` — `var localGitHubUserID = runmode.LocalDefaultUserID`
- **Local purpose:** Resolves "the user's GitHub login" for predicate-matching ("PR review requested from me"). The poller acts as the lone local user; the user's `users.github_username` is the predicate input.
- **Multi-mode:** `runGitHubCycle` reads `m.users.GetGitHubUsernameSystem(ctx, localGitHubUserID)` for **every** real-tenant org — always against the sentinel user, which has no FK target in multi-mode `public.users`. The lookup returns empty → the org is silently skipped at `manager.go:273` (`if username == "" { continue }`). Net effect: in multi mode the GitHub poller iterates every org but never refreshes any of them. Fix is non-trivial (per-org credential resolution is deferred per the docstring at line 24); for now, this poll loop should be **gated off entirely** in multi until SKY-263 / SKY-265 lands. The current behavior (loop runs to no effect every cycle) is worse than just skipping the start in multi.

### 14. `srv.SetOnGitHubChanged` / `SetOnJiraChanged` closures call `pollerMgr.RestartAll` with the request orgID
- **Where:** `main.go:983-1029` / `main.go:1032-1046`
- **Local purpose:** Settings handler invokes these when the user saves new GitHub/Jira config.
- **Multi-mode:** These callbacks already take the orgID from the closure call site (settings handler threads it through). Shape-correct for multi — the issue is downstream: the callback then calls `spawner.UpdateCredentials` and `srv.SetGitHubClient` with a single-tenant client (same #11 issue). The callback wiring itself is fine; the **single-client model** behind it is the multi-mode footgun. Per-org client/spawner is a separate (large) refactor.

## Phase 4: background subscribers + delegation surface

### 15. Curator runtime `New` is constructed at startup with empty model
- **Where:** `main.go:889` — `curator.New(database, stores, wsHub, "")`
- **Local purpose:** Construction is mode-agnostic; `curator.UpdateCredentials` is what flips on the model after `config.Load`.
- **Multi-mode:** Construction itself is fine. The path that matters is per-dispatch (covered in #10 above). No startup-time fix here.

### 16. Spawner created once with single-process credentials
- **Where:** `main.go:857-863` (`spawner := delegate.NewSpawner(...)` + `spawner.SetStores(stores)`)
- **Local purpose:** One spawner instance per process; credentials hot-swapped via `UpdateCredentials` on config change.
- **Multi-mode:** Construction is fine, but the credential model is fundamentally single-tenant (#11 / #14). Delegated runs spawned per-request will hot-swap to the wrong tenant's PAT under contention. The spawner *itself* doesn't need gating — the multi-tenant credential model behind it is what needs the rework.

### 17. Event bus router does not pin a sentinel at construction, BUT the `handlerTeamID` fallback does
- **Where:** `internal/routing/router.go:390-396` — `handlerTeamID` returns `runmode.LocalDefaultTeamID` when the matched handler has no team_id
- **Local purpose:** Defensive fallback if a pre-SKY-295 handler row survived migration. Logged loudly.
- **Multi-mode:** In steady state this branch is unreachable (every handler is team-scoped post-SKY-295). If it IS reached in multi mode, the fallback writes a task with `team_id = local sentinel`, which will FK-fail in Postgres. The log line is enough — the fallback should be deleted in multi mode (let the FK fail loudly rather than mint a wrong-team task). Same shape at `router.go:450` (the `task.TeamID == ""` branch in `tryAutoDelegate`).

### 18. Router inline close-check reads the local user's Jira identity
- **Where:** `internal/routing/close_checks.go` — `r.users.GetJiraIdentitySystem(ctx, runmode.LocalDefaultUserID, jiraHost)` inside `closeCheckJiraReassigned`, where `jiraHost` comes from `r.orgs.GetSettingsSystem(...).JiraBaseURL` (host-scoped identity, SKY-397).
- **Local purpose:** Inline auto-close: when a Jira issue is reassigned away from "me," close active assigned-to-me tasks. "Me" is the local user.
- **Multi-mode:** Already acknowledged in the docstring ("no single 'the user' whose Jira identity counts ... current sentinel-keyed read keeps local-mode behavior intact and over-closes in multi-mode"). Documented as acceptable. Leave as-is.

## Phase 5: CLI surface (delegated-agent entry points)

### 19. `runident.ResolveRunIdentity` always returns `runmode.LocalDefaultOrg` as the run's orgID
- **Where:** `cmd/exec/runident/runident.go:109` — `orgID := runmode.LocalDefaultOrg`
- **Local purpose:** Every `triagefactory exec ...` invocation by a delegated agent resolves its (orgID, userID, runID) via this helper. Local mode collapses to the sentinel.
- **Multi-mode:** TODO at line 106 calls this out — "SKY-269 will replace runmode.LocalDefaultOrg with the run's real org_id." Until then, **every multi-mode delegated agent's `triagefactory exec ...` call writes against the wrong org**. The fix is local: `orgID = run.OrgID` from the `agent_runs` row, which we already have in scope. Should land before any multi-mode delegation can fire.

### 20. `cmd/exec/exec.go` credential resolver hardcodes the sentinel
- **Where:** `cmd/exec/exec.go:105` — `orgID := runmode.LocalDefaultOrgID` (with the comment "Local mode collapses to runmode.LocalDefaultOrgID, but the resolver shape stays multi-mode ready")
- **Local purpose:** When `ResolveRunIdentity` errors (e.g. `--help` invocation without a real run), defaults to sentinel for credential lookup.
- **Multi-mode:** Coupled to #19 — once `runident` returns the real orgID, this site stays correct because it reads `ident.OrgID` on success. The only remaining concern is the error fallback: in multi mode an exec invocation without a valid run shouldn't fall back to the sentinel (which has no creds anyway) — it should refuse. Minor; the actual bug surface is #19.

### 21. `cmd/resume/resume.go` reads taken-over runs from the sentinel org
- **Where:** `cmd/resume/resume.go:69`
- **Local purpose:** `triagefactory resume` lists taken-over runs to pick one for `claude --resume`.
- **Multi-mode:** Per the TODO at line 62, `resume` is local-only by nature (cd's into a worktree on the operator's machine). Should `os.Exit` early in multi mode with a clear message ("resume is local-mode only").

## Phase 6: SQLite store internal sentinels (defense-in-depth, not boot-path)

### 22. `internal/db/sqlite/agentrun.go:46` defaults `CreatorUserID` to the sentinel
- **Where:** SQLite store `Create` defaults manual-run `CreatorUserID` to `runmode.LocalDefaultUserID` when caller leaves it empty.
- **Local purpose:** Convenience for SQLite-only paths where the caller (pre-D-Claims test fixtures, spawner without explicit user) doesn't thread a user id.
- **Multi-mode:** Only reachable via the SQLite store, which only runs in local mode (the postgres store is constructed in `case ModeMulti`). No multi-mode impact. Belongs to the "SQLite-only defaults" set — fine as-is.

### 23. `internal/db/sqlite/event_handlers.go:248,265` hardcodes team/user sentinels in `Create`
- **Where:** SQLite `Create` for user-source event_handlers.
- **Local purpose:** Local SQLite has one team + one user; hardcoded.
- **Multi-mode:** Sqlite-impl-only. Fine.

### 24. `internal/db/sqlite/projects.go:66`, `prompts.go:226,239`, `agents.go:76` all reference sentinels
- **Multi-mode:** All SQLite-only. Fine.

## Per-request handler sites (not boot, but reached the moment the server accepts traffic)

These are NOT startup, but worth flagging since they fire immediately after `ListenAndServe`:

### 25. `internal/server/stock.go:464,522` hardcode `LocalDefaultTeamID` on `Tasks.FindOrCreate`
- Per-request Jira-stock claim/queue handler. In multi-mode this would bind every claimed/queued task to the sentinel team (FK-fail in Postgres).

### 26. `internal/server/factory_delegate.go:173` hardcodes `LocalDefaultTeamID` on `Tasks.FindOrCreate`
- Per-request factory-drop handler. Same FK-fail in multi-mode Postgres.

### 27. `internal/server/projects.go:131` hardcodes `LocalDefaultTeamID` on `Projects.Create`
- Per-request project-create handler. Same FK-fail in multi-mode Postgres.

### 28. `internal/server/server.go:179` hardcodes `LocalDefaultTeamID` on `TeamAgents.GetForTeam`
- Per-request agent-enabled gate in `agentEnabledForOrg`. Wrong team in multi-mode.

### 29. `internal/projectbundle` — ported to the store layer (TFAC-109)
- The sentinel hardcoding this item originally flagged was fixed by an earlier identity-threading pass (real orgID/teamID/userID parameters). TFAC-109 then completed the port: import/export/preview run every DB read/write through claims-bound `WithTx` store methods (the raw `?`-placeholder SQL — which in multi mode executed on the ADMIN pool, RLS bypassed — is deleted, along with the org-blind package-level curator helpers it shared with the curator HTTP handlers), and session transcripts resolve mode-aware via `worktree.ClaudeProjectDir` (sandboxed runs write them under the run's org-scoped directory, not the orchestrator's `$HOME`). Both endpoints now work in multi mode; the interim 501 gates are lifted. DONE.

### 31. `GET /api/projects/{id}/export` + `/export/preview` were never gated (found + fixed by TFAC-109)
- The original audit caught the import gate but not its export siblings: both endpoints were reachable in multi mode, 500ing on the un-elevated app pool — and, once past that, would have silently exported transcript-less bundles because `appendSessionArtifacts` looked under the orchestrator's `$HOME` while the sandboxed curator writes its transcript under the project KB dir (`HOME=/work`). Fixed by the same TFAC-109 port as #29; a transcript that exists but is unreadable now surfaces as a preview/manifest warning instead of a silent omission.

### 32. `skills.ScanAgentTools` merged orchestrator-host agent files into every tenant's `--allowedTools` (found + fixed by TFAC-109)
- `internal/skills/agents.go` reads the process's own `~/.claude/agents/*.md`; its only caller (`delegate/prompt.go` `cachedAgentTools`) merged the declared tools into every delegated run's `--allowedTools` in BOTH modes, unconditionally. Live behavior, not a fail-closed gap: host-level files silently widened every org's tool allowlist. Now gated to local mode.

### 33. Delegate resume/snapshot resolved session transcripts from the orchestrator's `$HOME` (found + fixed by TFAC-109)
- `sessionTranscriptExists`, `readSessionTranscript`/`restoreSessionTranscript`, and `RemoveClaudeProjectDir` all reconstructed the transcript path from host HOME + encoded host cwd — the direct-run layout. In sandboxed multi mode they silently missed (crash-reclaim never actually resumed; taken-over-run snapshots carried no transcript). All now route through the mode-aware `worktree.ClaudeProjectDir`.

### 30. `internal/agentmeta/footer.go:68,111` reads agent run + token totals from sentinel org
- Per-request review/PR submit footer rendering, called from `cmd/exec` subcommands. Coupled to #19 — fixing `runident` to thread the real orgID lets this be passed through cleanly.

## Test files

189 references to `runmode.LocalDefault*` in `internal/db/_test.go` files alone, 654 total. Almost all are test-fixture identity values — fine as-is. Two patterns are worth a brief mention:

- `internal/db/dbtest/{task,prompt,conformance}.go` (lines 15-26 in each) explicitly note that SQLite returns `LocalDefaultOrg` while Postgres returns a fresh org UUID — these are the conformance harnesses, the dual-shape is intentional.
- `internal/runmode/test_helper.go` provides `SetForTest` — the canonical way for tests to flip mode. Working as designed.
- No load-bearing test is using the sentinels in a way that masks a multi-mode bug.

## Overall pattern

The findings fall into three groups:

1. **"Should be mode-gated off entirely in multi"** — bulk of the boot-path issues. Examples: #1 (taken-over preserve set), #4 (`seedDefaultPrompts`), #11 / #12 (initial credential load + bare-clone bootstrap), #13 (GitHub poller's borrowed-user model), #21 (`triagefactory resume`). These exist to make local-mode boot smooth and have no analog in multi — the corresponding multi-mode work is done lazily per-request (orgs are bootstrapped via SKY-257 D14, credentials per-org per SKY-263, etc.). The cheap, correct move is `if runmode.Current() == ModeLocal { ... }` around each block; the expensive-but-right move is rewriting them to iterate `stores.Orgs.ListActiveSystem`.

2. **"Cross-tenant leak — must fix before any multi-mode tenant exists"** — only one but it's load-bearing: #8 (`LifetimeDistinctCounter` aggregates every tenant's events into one process-wide map served to the factory snapshot endpoint). #9 (curator-orphan sweep) is in this family but less severe.

3. **"Per-request site hardcoding the local team/user sentinel"** — #10, #19, #20, #25-30. These aren't startup-time but they're reached the moment a request lands. In Postgres they'll FK-fail (the sentinel team/user have no FK target in `public.teams` / `public.users`). The fix shape is universal: thread the real `team_id` / `user_id` from request context / from the resolved run. #19 (`runident`) is the single highest-leverage one — it gates every `triagefactory exec` subcommand's identity resolution.

**Recommended ordering for a follow-up PR:** fix #19 first (unblocks all of #25-30 by making the multi-mode identity threadable), then #8 (cross-tenant leak), then the mode-gate sweep (#1, #4, #11, #12, #13, #21).
