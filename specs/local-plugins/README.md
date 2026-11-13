# Local plugins: requirements and design

A team can name Claude Code plugin directories that load into every local-mode delegated run. The plugin's hooks, skills and agents join the run. Nothing else from the operator's `~/.claude` does.

Status: Rev 5. Plan review round 3 returned APPROVE-WITH-CHANGES, and those changes are applied (section 13). Phase 1 probes run and absorbed (section 13, Rev 5). Phase 1 in build. Spike done (section 2).
Scope note: local mode, delegated runs only. Multi mode runs the native Go loop (`internal/agentloop`) and has no Claude Code to load a plugin into. One-shot paths (scorer, classifier, profiler, and system jobs with no Cwd) never get plugins.

---

## 0. Why

Local runs deliberately load project settings only (`internal/agentproc/argv.go:20-30`). That keeps the operator's permission rules, `env` and hooks out of every run. It is the right default. It also means an operator who relies on a guard plugin, such as one whose `PreToolUse` hooks refuse a force push to a protected branch, gets none of that protection in a delegated run. They get no signal that it is missing, either. `docs/users/local/configuration.md:38-41` already says plugins do not reach a run.

The ask is narrow: let a team admin opt specific plugin directories into runs, explicitly, the same way `claude_ai_connectors_enabled` opted connectors in (commit bace8316). It does not reopen user settings.

## 1. Goals and non-goals

Goals:

1. A team setting lists zero or more local plugin directories. Each one loads into every local delegated run, and every resume, for that team through the Agent SDK `plugins` option.
2. Each entry can carry a small map of non-secret environment variables, with one TF-supplied placeholder for a per-run directory. This lets a plugin keep per-run state instead of sharing state across concurrent runs in the operator's home.
3. **For a required entry, the run makes no tool call unless the plugin loaded with no load errors.** "Loaded" is not "functional". A hook that crashes, times out or exits with a non-blocking code still lets the call through, by Claude Code's hook contract. TF cannot detect that, and the spec does not claim to (section 8).
4. The run page shows which plugins loaded, with versions, for each run segment.
5. On Linux, the bubblewrap sandbox binds each plugin directory read-only and strictly. The home-dir mask stays in place.
6. Off by default. With the list empty, argv, env and the mount plan are byte-identical to today.

Non-goals:

- Loading the operator's user settings, `enabledPlugins` or marketplaces. Entries are directories, not marketplace names.
- Plugin MCP servers. Every entry is passed with `skipMcpDiscovery: true`. This stops TF from launching a plugin's declared MCP servers. It does not stop plugin hook code from opening network connections (section 8).
- Multi mode. The PATCH refuses the field there (section 6). This is new behavior: the connectors PATCH has no such refusal.
- A secret channel. Plugin env is stored in plain text in `team_settings` and is shown only to team admins (section 6).
- Changing permission mode. A command-heavy plugin still needs the team's auto-mode opt-in (`internal/delegate/permissions.go:148-166`). See Q2.

## 2. Spike findings

The spike was run 2026-11-10 against `@anthropic-ai/claude-agent-sdk` 0.3.296, the version `install.go:21` pins. It called `query()` directly with the options TF uses: `settingSources: ["project"]`, `permissionMode: "default"`, cwd set to a run root, and the repo in a subfolder at `<runRoot>/acme/widget`. A real guard plugin was used. The prompt asked for `git -C acme/widget push --force origin main` against an empty local bare remote.

| Arm | `init.plugins` | Result |
|---|---|---|
| no `plugins` option | built-ins only | the command ran (git failed on the empty remote) |
| `plugins: [{type:"local", path, skipMcpDiscovery:true}]` | plugin listed with version and a backslash Windows path | the plugin's `PreToolUse` hook denied the call, and the reason came back as an `is_error` tool_result |

What the spike established:

- `settingSources: ["project"]` does not stop an explicitly passed plugin from loading. Its hooks run.
- The deny comes back as an ordinary `is_error` tool_result. `parseToolResult` (`stream.go:480-499`) already turns that into a failed row with an ERR pane. `hook_started` and `hook_response` system messages are emitted only for `SessionStart`, and `stream.go:287-299` drops them.
- `init.plugins[].path` is the CLI's normalised form of the path, not the string passed in. Path matching must not compare strings (section 4.2).
- The plugin wrote state into the operator's home: a session lock and a trace store. A finished run's lock made the next run see a live "peer session" and deny on it. Concurrent runs share that state unless the plugin is pointed at a per-run directory (goal 2).
- The plugin's `SessionStart` hooks took about 19 seconds before the first turn.

Not covered by the spike: the TF binary end to end, the Linux sandbox (home masked, so hook interpreters under `$HOME` may be missing; see 4.4), Windows path forms through the TF binary, and skills or agents from the plugin. Only hooks were exercised.

## 3. Settled decisions

- **D1. Directories, not marketplace references.** One absolute path per entry, with `.claude-plugin/plugin.json` inside it. TF does not install, update or fetch plugins. This is a new trust grant: plugin hooks are code that runs on every tool call. No existing TF setting grants that, and section 8 states the consequences.
- **D2. Team setting, team admin, local only.** Storage and PATCH semantics follow `claude_ai_connectors_enabled`, and so does the multi-mode early return in `resolveSDKTeamOptions`. In local mode the team admin is the operator, so there is no escalation across users.
- **D3. MCP discovery is always skipped.** It is not configurable in v1.
- **D4. Fail closed, at both points where the setting could silently vanish.**
  - (a) If the team settings read fails, the spawn fails with `team_settings_unreadable`. Today the read at `permissions.go:153-160` logs "using defaults" (`:158`) and continues. Only a confirmed absent row means `[]`, and the store already returns defaults for no row (`sqlite/teams.go:72-77`). The connectors and auto-mode defaults are safe to fall back to. An empty guard list is not.
  - A task with no team (`teamID == ""`, `delegate/run.go:525`) has no team setting to read, so it runs with no plugins. That is logged at info on spawn. Team-less runs are rare, since `task.TeamID` is set for any task that reaches a run. The docs state this.
  - (b) A required entry that did not load, or loaded with a `plugin_errors` entry, latches the run into deny-all (section 4.2).
  - A non-required entry that fails logs a warning, and the run continues.
- **D5. Env values are literal strings plus one placeholder.** `{run_state_dir}` expands to `<runRoot>/_tfac/plugins/<index>/`, which TF creates at mode 0700 before spawn. No other expansion is done, and no shell is involved.
- **D6. One shared denylist, enforced at PATCH and at spawn.** `agentproc.PluginEnvKeyAllowed(key) (ok bool, reason string)` is exported once and called by both the settings handler and the spawn path. At spawn, any key that collides with a key TF sets for that run is dropped and logged. The set TF uses is dynamic (`GIT_CONFIG_*`, `BUN_JSC_useJIT`, `ExtraEnv`), so the PATCH cannot see all of it.
- **D7. Tool grants are explicit per entry.** `grant_tools` items are `Agent` or `Skill`. The default is empty. Per-skill grants (`Skill(<name>)`) are dropped from v1: the Phase 1 probe (section 13, Rev 5) showed that SDK 0.3.296 does not refuse a skill outside a `Skill(<name>)` allow rule. The same probe showed that `Skill` and `Agent` run with **no** allow rule at all under `permissionMode: "default"`, so in v1 both grants are recorded and passed through as extras but do not change what the run can do. Loading a plugin makes all of its skills and agents callable. Section 8 states this. A deny rule does work (`disallowedTools: ["Skill(<name>)"]` refused that skill and allowed its sibling), so a per-skill deny list is the v2 mechanism if one is wanted.
- **D8. Pass and bind the resolved path.** Preflight runs `EvalSymlinks` on each path. The resolved path is what goes to `--plugin-dir`, the sandbox bind and the required check, so all three name the same directory.

## 4. Design

### 4.1 Setting to spawn

```
team_settings.local_plugins (JSON list)
  -> domain.TeamSettings.LocalPlugins []LocalPlugin{Path, Required, Env map[string]string, GrantTools []string}
  -> resolveSDKTeamOptions (permissions.go:133-166): multi returns early; read error -> spawn error (D4a)
  -> RunOptions.Plugins (run.go:59), wired at run.go:577-581 AND resume.go:469-473
  -> preflight: EvalSymlinks; require a dir with .claude-plugin/plugin.json; refuse paths under
     RunRoot, any AddDir, or worktree.RunsBase(); mkdir {run_state_dir} 0700
  -> BuildArgs: one --plugin-dir <resolved> per entry, plus --plugin-required <resolved> per required entry
     (argv.go, beside --add-dir at :121-126)
  -> env: plugin env prepended to the run's added env, before opts.ExtraEnv and hookEnv (hookEnv assigned at run.go:701; mergeEnv at :768),
     never in parentEnv; collisions dropped and logged (D6)
  -> tools: GrantTools merged as extras at run.go:589 and resume.go:488
  -> sandbox: Spec.Plugins (new field), strict --ro-bind, checked by Preflight (args_linux.go:195)
  -> wrapper.mjs parseArgs: --plugin-dir and --plugin-required collected;
       opts.plugins = dirs.map(path => ({type:"local", path, skipMcpDiscovery:true}))
  -> wrapper on system/init: required check, latch, emit control "plugins_loaded" (4.2)
  -> Go stream.go: parse control "plugins_loaded", persist per segment (section 5), log, metric
```

`--plugin-dir` is emitted by `BuildArgs`, not `run.go:678`, because it carries a value and repeats. That mirrors `--add-dir` (`wrapper.mjs:112-113, :178`). One-shot paths build `RunOptions` without `Plugins`, which is nil, so they emit nothing.

### 4.2 Required-plugin check (in the wrapper)

The check runs in `wrapper.mjs`, the process that already owns TF's `PreToolUse` hook (`onToolStarted`, `:462`). A Go-side kill after init would race the model's first tool call. A latch in the wrapper does not.

`onToolStarted` is the only gate that sees every call. `canUseTool` is wired only when `permissionPrompts` is set (`:294`), and allowlisted calls never reach it. So the latch lives in `onToolStarted`, and `canUseTool` checks it as a second line where it exists.

1. **The latch.** A `pluginGate` latch starts closed whenever any `--plugin-required` was passed. While it is closed, `onToolStarted` returns `permissionDecision: "deny"` with the reason "required plugin check pending" for every call, main thread and subagent alike.
   - The latch test is the **first** statement in `onToolStarted`. It comes before `if (closing) return {}` (`:463`) and before the `input?.agent_id` branch. Today `closing` makes the hook allow everything, so a latch placed after it would open itself on shutdown.
   - `canUseTool`, where wired, also tests the latch first.
2. **Evaluating init.** On every `system/init`, the wrapper reads `init.plugins[]` and `init.plugin_errors[]` (`sdk.d.ts` ~6016-6023). A re-emitted init is evaluated again, and a failure on any evaluation closes the latch for good.
3. **Matching.** A configured entry matches a loaded row by file identity: `fs.statSync(p, {bigint: true})` on both paths, then compare `dev` and `ino`. Bigint is required because NTFS file IDs exceed 2^53. A path that cannot be stat'ed is a non-match. The match is never a string compare, because of Windows case, 8.3 names, and macOS `/var` vs `/private/var`. `fs.realpathSync.native` is used only for the text in the log and the record. Unverified: ReFS Dev Drive uses 128-bit file IDs, which libuv may truncate. This is part of Q4.
4. **Tying errors.** A `plugin_errors` entry is tied to a configured entry in any of these cases:
   - (a) The error's `plugin` tag, with any `@<source>` suffix removed, equals the `name` of the `init.plugins` row the entry matched, or the entry's directory name. This covers a plugin that loaded but lost a component, such as `hook-load-failed` for its `hooks.json`. That is exactly the case a guard must not pass. The directory name is required: Claude Code tags a component error with the plugin's directory name (`<dir>@inline`), not its manifest name (Rev 5).
   - (b) The error's `path` matches the entry by file identity.
   - (c) The error carries the positional `inline[N]` tag for the entry's index. That covers an entry that failed before it had a name.
   - (d) Fail closed: an `@inline` or `inline[N]` error that ties to no entry by (a)-(c) is tied to every required entry. Every directory the wrapper passes is an inline entry, so such an error belongs to one of them.
5. **Passing.** A required entry passes only when it matched a loaded row and no error is tied to it.
6. **Init deadline.** If no init arrives within 60 seconds, the check fails. Without a deadline a closed latch would deny forever while the model spins.
   - The timer starts when the wrapper receives its first `user_message` control line, not at spawn. A live run can sit idle before its first input.
   - Verified in Phase 1 (section 13, Rev 5): in streaming-input mode the CLI emits init only after the first user message arrives, so the timer starts there. Plugin `SessionStart` hooks start at spawn, before init.
   - 60 seconds is three times the spike's 19-second SessionStart. `TF_PLUGIN_INIT_TIMEOUT`, in seconds, overrides it, and is documented in `docs/users/local/tuning.md` beside `TF_CLAUDE_BINARY`.
7. **On failure,** the latch stays closed, and the wrapper:
   - writes the control line `{"type":"control","subtype":"plugins_loaded","ok":false,"failed":[{path,reason}]}` with a new `emitThen(line, cb)` variant of `emit()`. Today `emit()` (`wrapper.mjs` ~:200) calls `process.stdout.write` with no callback;
   - calls `process.exit(3)` in that callback, with a 2-second `setTimeout` fallback in case the callback never fires.

   It does **not** call `q.interrupt()`. That returns a Promise that resolves only when the CLI acknowledges (`sdk.d.ts` ~2879), which could hang during SessionStart. It does **not** use `closeInput()` either. That only ends the input iterable, it sets `closing` (`:489`), and it leaves the in-flight turn running.

   The latch stays closed until the process dies, so any call in that window is denied. Process death abandons pending batches and tool calls, which Go's `abandonToolBatches` and `cancelToolCalls` defers already handle. Exit code 3 is new. Go reads only "non-zero with no result" (`live.go:518`, `run.go:638`), and the wrapper's existing codes are 1 and 2.
8. **On success,** the latch opens and the wrapper emits `{"type":"control","subtype":"plugins_loaded","ok":true,"plugins":[{name,version,path}]}`.
9. **No required entries.** The latch is open from the start, and `plugins_loaded` is still emitted.

Go side:
- `controlLine` gains `OK *bool`, `Plugins []LoadedPlugin` and `Failed []PluginFailure`.
- `parseControlLine` decodes them, and `consumeStreamInteractive` gains a `plugins_loaded` case. Today unknown subtypes are ignored, because the `live.go` switch has no default.
- `ok:false` sets a `pluginFailed` flag on the `LiveRun` and is stored on the run's claim.
- `readLoop` sets `termErr` to `ErrPluginNotLoaded` whenever that flag is set, whether or not a `result` was seen. Today `termErr` is set only when `result == nil` (`live.go:515-528`), which would let a failure on a re-emitted init, after an earlier turn produced a result, be recorded as `completed`.
- The run fails with outcome `failed` and error `plugin_not_loaded: <paths>` on its `claims` row (section 5).

Because the latch denies tool calls from the start, the agent's first turn can produce text but cannot act until the check passes. A `TF_CLAUDE_BINARY` override that rejects `--plugin-dir` or ignores `skipMcpDiscovery` also fails this check, so it is covered.

### 4.3 Validation (PATCH and spawn)

Validated at PATCH (400 with a field error) and again at spawn, through the shared functions in D6:

- `Path` is absolute after `Clean`, exists, is a directory, and contains `.claude-plugin/plugin.json`. Windows drive paths are accepted on Windows.
- The list holds at most 8 entries, and no resolved path appears twice.
- Each `GrantTools` item is exactly `Agent` or `Skill` (D7). `Skill(<name>)` is refused with a message saying per-skill grants are not supported.
- Each env key matches `^[A-Z][A-Z0-9_]{0,63}$`. The value is at most 1024 bytes, contains no NUL, and the only placeholder allowed is `{run_state_dir}`.
- Env keys are refused by family:

| Family | Keys refused |
|---|---|
| TF and model | `TRIAGE_FACTORY_*`, `TF_*`, `ANTHROPIC_*`, `BUN_*`, `CLAUDECODE`, and **every** `CLAUDE_*` key, except keys under an operator-allowed prefix (below) |
| Git and forge | `GH_*`, `GITHUB_*`, `GIT_*`, `SSH_*` |
| Loaders, interpreters and shells | `NODE_*`, `LD_*`, `DYLD_*`, `PYTHON*`, `PERL*`, `RUBY*`, `JAVA_*`, `_JAVA_*`, `LUA_*`, `GLIBC_*`, `BASH_ENV`, `ENV`, `SHELL`, `ZDOTDIR`, `SHELLOPTS`, `PS4`, `IFS` |
| Package managers | `NPM_CONFIG_*`, `PIP_*`, `UV_*`, `CARGO_*`, `GOPROXY`, `GOFLAGS`, `GOPRIVATE`, `GONOSUMDB`, `GONOSUMCHECK`, `GOSUMDB`, `GOINSECURE`, `GOTOOLCHAIN` |
| Model provider | every key the credential resolver strips from an inherited env (`isCredentialEnvKey`: `CLOUD_ML_REGION`, `GCLOUD_PROJECT`, `VERTEX_REGION_*`, ...) |
| Network and trust | `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY` (any case), `SSL_*`, `CURL_*`, `REQUESTS_CA_BUNDLE`, `SSLKEYLOGFILE` |
| Identity and paths | `HOME`, `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `XDG_*`, `PATH`, `PATHEXT`, `COMSPEC`, `TMPDIR`, `TEMP`, `TMP`, `AWS_*`, `AZURE_*`, `GOOGLE_*` |
| Substring | any key containing `TOKEN`, `SECRET`, `PASSWORD`, `CREDENTIAL`, `_KEY` or `AUTH` |

- In multi mode, any non-empty `local_plugins` is refused with "local plugins apply to local mode only".

**Allowed prefixes.**
- Claude Code reads many `CLAUDE_*` keys, and the set grows with each SDK release. Examples: `CLAUDE_SECURESTORAGE_CONFIG_DIR`, which relocates the credential store, and `CLAUDE_AI_ORIGIN` and `CLAUDE_BRIDGE_*`, which redirect endpoints. So the whole prefix is refused by default.
- Some plugins name their own keys `CLAUDE_<PLUGIN>_*`. The operator, not a team admin, may allow specific prefixes with the deployment setting `TF_PLUGIN_ENV_ALLOW_PREFIXES`, a comma list such as `CLAUDE_LOOP_HARNESS_`.
- Each prefix must start with `CLAUDE_`, end in `_`, and have at least two segments after `CLAUDE_`.
- **The default is the safety net.** Refusing the whole prefix is the protection. Only the operator can allow a prefix, so the exposure is bounded to what the operator chose. The guard below is secondary.
- **Guard in CI.** A dedicated CI job runs `npm ci` from the embedded lockfile (`internal/agentproc/package-lock.json`), so the SDK is present. The SDK is not vendored; `install.go` installs it at first use.
  - The job then extracts every `CLAUDE_[A-Z0-9_]+` literal from the SDK's `.mjs` files and, with `strings`, from the platform CLI binary, where most reads live.
  - It hard-fails when a documented or tested allowed prefix matches one of those keys. It never skips.
  - An SDK bump that starts reading a key under an allowed prefix therefore fails CI.
- **Guard at startup.** If the SDK is installed, the same scan runs against the configured prefixes, and any matching prefix is refused with an error log. If the SDK is not installed yet, the scan runs after `install.go` completes, before the first spawn. It never treats "could not scan" as "no match".

### 4.4 Sandbox (Linux)

- Add a dedicated `Spec.Plugins []string`, separate from `Spec.ReadOnly`, which binds with `--ro-bind-try` (`args_linux.go:135`) and would silently mount an empty directory over a vanished path.
- `Spec.Plugins` is bound with strict `--ro-bind`, after the home mask, and `Preflight` (`args_linux.go:195`) checks that each one exists. It also joins the absolute-path check at `args_linux.go:39`.
- `directArgv` (`internal/agentproc/localsandbox.go:25`) folds `RunOptions.Plugins` into `Spec.Plugins`, so the granted list and the bound list come from one slice.
- `{run_state_dir}` lives under `RunRoot`, which is already bound read-write. The home mask (`args_linux.go:107-108`) is unchanged.

The home mask may hide what a hook needs: an interpreter or virtualenv under `$HOME`, or a tool on a `$HOME` PATH entry. A hook that cannot start is a non-blocking error, so the guard fails open on that call (goal 3). v1 does not bind more of `$HOME`. The Phase 1 exit criteria require the fixture to run under bwrap with a hook that uses the system interpreter, and the docs tell plugin authors that a hook must not depend on `$HOME`.

On macOS and Windows there is no sandbox (`args_other.go:14-25`).

### 4.5 Tools

`BuildAllowedToolsFor` (`allowlist.go:327-331`) includes neither `Skill` nor `Agent`. Each entry's `GrantTools` goes through the existing extras merge at `run.go:589` and `resume.go:488`. What each grant widens:

- **`Skill`.** No grant is needed today: under `permissionMode: "default"` the Skill tool runs with no allow rule (D7), so this grant is a pass-through with no effect in v1. The tool loads the plugin's skills as instructions. A skill's `allowed-tools` frontmatter pre-approves those tools for the turns where the skill is active (`sdk.d.ts` ~7359), bypassing TF's allowlist for those tools.
- **`Agent`.** No grant is needed today either (D7). The tool lets the run start subagents, including the plugin's own agents. TF's `onToolStarted` already denies subagents the `mcp__tf__*` tools (`wrapper.mjs:455-475`). Subagents otherwise inherit the run's allowlist and permission mode.
- **Plugin hooks, with no grant needed.** A plugin `PreToolUse` hook that returns `allow` skips the `canUseTool` prompt for that call, even under `permissionMode: "default"`. That is inherent in loading hooks, and it is stated in section 8.

## 5. Data

SQLite migration `internal/db/migrations-sqlite/202611100001_team_local_plugins.sql`. Use the next free number at build time; the latest today is `202611090001`.

```sql
-- +goose Up
ALTER TABLE team_settings ADD COLUMN local_plugins TEXT NOT NULL DEFAULT '[]';
-- +goose Down
SELECT 'down not supported';
```

Postgres: add `local_plugins jsonb NOT NULL DEFAULT '[]'::jsonb` to the `team_settings` block of `202605130001_pg_baseline.sql`, the same in-place edit bace8316 made. The JSON shape is `[{"path":"...","required":true,"env":{"K":"v"},"grant_tools":["Skill"]}]`.

Store changes, both stores:
- `teamSettingsColumns` and `scanTeamSettings`;
- the positional upsert in `UpdateSettings` (`sqlite/teams.go` ~600-640, `postgres/teams.go` ~708);
- `DefaultTeamSettingsFor` (`domain/settings.go:717`);
- `dbtest/settings_conformance.go`.

Run record: a column `plugins_loaded` on `claims`, the per-engagement execution row (`migrations-sqlite/202607230001_conversations_refactor.sql:252`). A resume gets its own claims row (`resume.go:304`), so this is per segment with no new table.
- It holds TEXT JSON on SQLite and jsonb on PG, contains the 4.2 `plugins_loaded` payload, and defaults to NULL (no plugins configured).
- It is written through the claims store, in the same migration.
- A failed check also sets the existing `claims.outcome` and `claims.error` (`plugin_not_loaded: <paths>`), so the failure shows in every view that already reads claims.

JSON tags:
- `domain.TeamSettings` has no JSON tags, so its wire keys are PascalCase, for example `ClaudeAIConnectorsEnabled` (`teamConfig.ts:503`).
- `LocalPlugin` gets explicit snake_case tags (`path`, `required`, `env`, `grant_tools`), so the stored column JSON, the PATCH body and the GET item shape are one shape.
- The top-level field stays `LocalPlugins` on the wire, matching its siblings.

## 6. API

| Route | Gate | Change |
|---|---|---|
| `GET /api/teams/{team_id}/settings` | team member | team admins get `LocalPlugins` in full; other members get `LocalPlugins: null` and `local_plugin_count`, because paths and env values are operator detail |
| `PATCH /api/teams/{team_id}/settings` | team admin, team not archived | `local_plugins`: `null` resets to `[]`; a list replaces the whole list; validation per 4.3; refused in multi mode (new behavior). The read-back goes to an admin and is not redacted |
| `GET /api/agent/conversations/{conversationID}` | unchanged gate | adds `plugin_segments`: one entry per engagement that had plugins, oldest first (`claim_id`, `claimed_at`, `released_at`, `outcome`, `error`, `plugins_loaded`). No existing route served claim rows, so this is new. A reader who is not an admin of the conversation's team gets it redacted: no paths, failure reasons reduced to "did not load", and a `plugin_not_loaded` error reduced to its code |

Implementation notes:
- **Redaction.**
  - `teamSettingsResponse` holds `domain.TeamSettings` as a named field with tag `team_settings` (`settings_handlers.go:271`), and uses snake_case tags. The count goes on `teamSettingsResponse` as `local_plugin_count`, not on the domain struct.
  - Redaction happens in the GET handler only, after `readTeamSettings`. That helper is shared with the PATCH read-back, so redacting inside it would hide the result from the admin who just saved.
- **Run-page endpoint.** Pinned in Phase 1: `plugin_segments` on the conversation resource (table above), backed by `ConversationStore.ListClaimsForConversation`.
- **PATCH reader.** `httpx/patch.go` has readers for scalars and `[]string` only. Add a generic `PatchJSON[T]` reader for a list of objects. Its null and absent semantics match `PatchStrings` (`settings_handlers.go:641`).

## 7. UI

The local-only section of `TeamSettings.tsx`, behind `isLocal` in `teamConfig.ts`, gets "Plugins loaded into runs":

- One row per entry, showing:
  - the path and a Required toggle;
  - tool grants, as an Agent checkbox and a Skill checkbox (D7: neither changes behavior in v1, and the copy says so);
  - an env key/value editor with a hint for `{run_state_dir}`.
- One line of copy: "These plugins run as you, inside every delegated run, see the same untrusted PR and issue text the agent does, and can approve tool calls."
- A warning when the team is on `default` permission mode (Q2).
- The run page header shows "Plugins: name version, ..." from `PluginsLoaded` for each segment, a warning chip for a non-required entry that did not load, and the failure reason for `plugin_not_loaded`.

## 8. Security posture

What this opens:

- **Code execution as the operator on every tool call.** Operator-chosen code runs with the operator's uid on every tool call of every run for that team, and it reads the same untrusted input the agent reads.
- **The run's network and credentials.** Plugin code runs with the run's environment, which includes the resolved credentials TF injects. The sandbox has no `--unshare-net` (`args_linux.go:66`), so the plugin has whatever network the run has. `skipMcpDiscovery` stops TF from launching declared MCP servers. It does not stop hook code from opening connections.
- **Plugins can approve calls.** A plugin hook can allow a call that `canUseTool` would have prompted for. A plugin skill's `allowed-tools` pre-approves tools while it is active (4.5).
- **Plugin instructions.** Plugin skills and agents become instructions for the run. Loading a plugin makes all of its skills and agents callable, whatever `grant_tools` says: under `permissionMode: "default"` neither the Skill nor the Agent tool needs an allow rule (D7).
- **Tamper by a prompt-injected agent.**
  - On macOS and Windows the plugin directory is writable by the agent: `Write` and `Edit` are not path-scoped, and `rm`, `mv`, `cp`, `tee` and `sed` are allowlisted. An injected agent can rewrite `hooks.json` and disarm the guard for every later run.
  - On Linux the strict read-only bind prevents this inside the sandbox.
  - The per-run `{run_state_dir}` is agent-writable on every platform, so the agent can alter the guard's own state for that run.
  - Mitigation in v1: preflight refuses plugin paths under the run root, an add-dir or the runs base; the UI and `configuration.md` state the residual risk; plugin authors are told to treat run state as untrusted. A tamper check, such as hashing the plugin tree at spawn and comparing it to the previous spawn, is Q5.

What it does not open:

- No user settings, permission rules or `env` from `~/.claude/settings.json`. The argv.go:20-30 rationale stands.
- No TF-launched plugin MCP servers.
- No new credential channel *into* TF: credential-shaped keys and TF-set keys cannot be set or overridden through plugin env.
- Nothing in multi mode or in one-shot paths.

`docs/users/local/configuration.md:38-44` gets this as a second documented exception, next to connectors, with the residual risks above stated plainly.

## 9. Observability

- At spawn, one info log line per run with the team, plugin count, resolved paths, the required flags and granted tools. Env keys are listed, values are not.
- When `plugins_loaded` arrives, one log line with `ok`, the plugins with versions, `failed`, and the time from spawn to init. Time to init is the plugin's SessionStart cost; the spike saw about 19s.
- Counters `local_plugin_spawns_total{team}`, `local_plugin_not_loaded_total{team}` and `local_plugin_env_key_dropped_total{team}`, plus a histogram `local_plugin_init_seconds`. Use the existing metrics registry.

## 10. Testing

- `argv_test.go`:
  - an empty list emits no `--plugin-dir` or `--plugin-required`;
  - N entries emit N flags in order, and only required ones emit `--plugin-required`;
  - empty paths are dropped;
  - one-shot `RunOptions` emit none.
- `wrapper_permission_test.go`:
  - Extend `optionsEchoStubSDK` to echo `plugins`. Assert `{type:"local", path, skipMcpDiscovery:true}` per flag, and no `plugins` key when there is no flag.
  - The latch: the stub SDK emits an init without the required path, so a tool call is denied, `plugins_loaded ok:false` is emitted, and the process exits with code 3.
  - The latch is not opened by shutdown: with the latch closed, `closeInput()` runs, and a later tool call is still denied.
  - The latch is checked for a subagent call (`agent_id` set) and for an allowlisted call when `permissionPrompts` is off.
  - The latch with a `plugin_errors` entry tied by plugin name with no `path` (`hook-load-failed`), one tied by `path`, and one tied by `inline[N]`.
  - Init with a matching path in a different case or through a symlink opens the latch, using the bigint stat comparison.
  - With no init before the deadline, the run fails with `plugin_not_loaded`.
  - A second init that drops the plugin closes the latch.
- `live_test.go`:
  - a `plugins_loaded ok:false` control line makes `consumeStreamInteractive` return `ErrPluginNotLoaded`, and the claim records it;
  - the same with an earlier `result` already seen still yields `ErrPluginNotLoaded`, not `completed`.
- Wrapper failure path: `emitThen` writes the line before exit 3; with the stdout callback stubbed never to fire, the 2-second fallback exits.
- SDK key scan (4.3): the CI job installs the SDK from the lockfile, scans the `.mjs` files and the CLI binary, and hard-fails on a matching allowed prefix. The startup check refuses a matching prefix and defers the scan until after install.
- Validation: `Skill(<name>)` in `grant_tools` is refused at PATCH with the per-skill message (D7). Init timing in streaming-input mode was recorded by the Phase 1 probe (4.2 step 6).
- `localsandbox/args_linux_test.go`:
  - `Spec.Plugins` emits strict `--ro-bind` after the masks;
  - `Preflight` fails on a missing plugin path;
  - `TestArgs_MountPlan` is unchanged with an empty list.
- `permissions_test.go`:
  - a read error fails the spawn (D4a);
  - an absent row gives `[]`;
  - multi returns no plugins;
  - `GrantTools` become extras only for the entries that set them.
- `settings_handler_test.go`:
  - each 4.3 rejection, including each denylist family;
  - the null reset and the admin gate;
  - the multi-mode refusal;
  - GET redaction for non-admins.
- Env merge test: a plugin key that collides with a TF-set key is dropped at spawn, and TF's value reaches the child.
- `dbtest/settings_conformance.go`: round trip and default `[]` on both stores; `plugins_loaded` per segment.
- `teamConfig.test.ts`: load and save of the list, the dirty check, and the default-mode warning.
- Fixture `internal/agentproc/testdata/denyplugin/`: a minimal plugin whose `PreToolUse` hook, using the system interpreter only, denies any Bash command containing `tf-plugin-probe-deny`. An e2e test, gated like the existing real-Claude tests, delegates a run that tries that command and asserts an `is_error` row carrying the plugin's reason. It runs once unsandboxed and once under bwrap on Linux.

## 11. Rollout

Phase 1, backend, spawn and resume (M). This covers:
- the migration, domain, stores and PATCH or GET changes;
- `RunOptions`, wired at both the run and the resume sites;
- `BuildArgs`, the wrapper latch, env, preflight and `Spec.Plugins`;
- the `plugins_loaded` record, logs and metrics.

Exit criteria: all section 10 unit tests pass, and the fixture e2e denies both unsandboxed and under bwrap.

Phase 2, UI and docs (S). This covers the settings section, the run-page plugin line and `configuration.md`. Exit criteria: the frontend tests pass, and a human configures the fixture plugin through the UI and sees the deny in a run.

Resume semantics, settled here and not deferred:
- A resume reads the current team setting, not the one at first spawn, and records its own `plugins_loaded` segment.
- Each resume, including the per-message respawn, pays the plugin's SessionStart cost again. The time-to-init histogram makes that visible.
- If a crash-reclaim resume finds the run root already removed, `{run_state_dir}` is recreated empty. The plugin's per-run state resets, and the spawn log says so.

There is no migration for consumers. The change is additive and not breaking: default `[]`, and argv, env and mounts are unchanged when empty.

## 12. Open questions

- **Q1. Upstream appetite.** Does the maintainer accept a trust grant beyond connectors? If not, the fallback is a local patch carried on a fork. The design does not change.
- **Q2. Permission mode.** A command-heavy plugin stalls on the 150s browser prompt (`activity.go:56`) unless the team is on auto mode. The proposal is a UI warning only, with no coupling.
- **Q3. Repo root for plugins.** The cwd is the run root and the repo is a subfolder. Plugins that find "the repository" from cwd will see a directory that is not a git repo. Should TF export a read-only `TRIAGE_FACTORY_REPO_DIR` for single-repo runs? Proposed: yes.
- **Q4. Windows.** The Windows port's `--args-file` route must carry `--plugin-dir` and `--plugin-required`. ACLs on `{run_state_dir}` and the same-file check need confirming on NTFS.
- **Q5. Tamper detection on macOS and Windows.** Should spawn hash the plugin tree, compare it with the hash from the last spawn, and fail a required entry whose hash changed with no admin action? Proposed for v2.
- **Q6. SDK initialize path.** Is `--await-initialize` with `initializationResult().plugins_applied` (`sdk.d.ts` ~2052, ~4577) a stronger signal than `init.plugins` for the latch? Check that during Phase 1.

## 13. Decision log

- 2026-11-10, Rev 1: drafted from the spike in section 2.
- 2026-11-10, Rev 2: plan review 1 returned REJECT with 5 blockers. All absorbed:
  - B1, fail-open on a settings read error: now D4a.
  - B2, a Go-side required check would race the first tool call and match paths by string: now the wrapper latch with realpath and `plugin_errors` (4.2), and goal 3 is reworded.
  - B3, the sandbox would bind with `-try` and preflight did not check plugin paths: now `Spec.Plugins` with a strict bind and a bwrap exit criterion (4.4).
  - B4, the env denylist missed loader, network and `CLAUDE_*` keys and could not be checked statically: now the families table, one shared function, and drop-at-spawn (D6, 4.3).
  - B5, the security section made false statements: section 8 rewritten.
  - RECs absorbed: the run record (5), resume settled and the Phase 3 contradiction removed (11), resolved path everywhere (D8), the API detail (6), one-shot scope and the `TF_CLAUDE_BINARY` case (scope note, 4.2), observability (9).
  - Citations corrected: `stream.go:480-499`, `localsandbox.go:25`, migration number. The `~/.claude/agents` analogy was dropped from D1.
- 2026-11-10, Rev 3: plan review 2 returned REJECT with 3 blockers, all written by the Rev 2 absorption. All absorbed:
  - N1, shutdown opened the latch: the latch is now first in `onToolStarted`, and the failure path is interrupt then exit 3, not `closeInput`. The Go control-line plumbing is named (4.2).
  - N2, a hook-load error on a loaded guard did not tie to it: errors are now also tied by plugin name (4.2 step 4a).
  - N3, the narrowed `CLAUDE_` rule let auth and endpoint keys through: the whole prefix is now refused, with operator-allowed prefixes and an SDK key-scan guard (4.3).
  - RECs absorbed: bigint file identity (4.2 step 3), the init deadline (step 6), `claims` as the run record (5), JSON tags and GET-only redaction (5, 6), per-skill grants (D7), team-less tasks (D4).
  - Citations corrected: `permissions.go:153-160`, `run.go:701`, `settings_handlers.go:641`, `args_linux.go:39`.
- 2026-11-10, Rev 4: plan review 3 returned APPROVE-WITH-CHANGES with 0 blockers. Changes applied:
  - the failure path uses `emitThen` plus exit 3, with a fallback and no `interrupt()`;
  - the key-scan guard is a CI job with the SDK installed, scanning the CLI binary too, and the startup check never treats "could not scan" as a pass;
  - the init deadline starts at the first input, and init timing is verified in Phase 1;
  - `pluginFailed` overrides an earlier result;
  - `Skill(<name>)` is marked unverified, with an e2e gate and a fallback;
  - the UI can express named-skill grants;
  - `local_plugin_count` is snake_case.
  - Accepted as open: the run-page claims route is named by the builder.
- 2026-11-10, Rev 5: Phase 1 probes against SDK 0.3.296 (`settingSources: ["project"]`, `permissionMode: "default"`, no `canUseTool`).
  - Init timing in streaming-input mode: init follows the first user message. With no plugin and an 8 s hold, the first user message went at 9.47 s and `system/init` arrived at 9.94 s. With the guard plugin and a 25 s hold, its `SessionStart` `hook_response` arrived at 4-5 s, before any input, the first user message went at 26.0 s, and `system/init` arrived at 34.5 s. 4.2 step 6 keeps the timer at the first `user_message`. `plugin_errors` is absent, not `[]`, on a clean load.
  - `Skill(<name>)` as an allow rule is not honoured: with `allowedTools: ["Skill(skillprobe:alpha)"]`, skill beta ran. With `allowedTools: []`, both a skill and the Agent tool ran. `disallowedTools: ["Skill(skillprobe:beta)"]` refused beta and allowed alpha; `disallowedTools: ["Skill"]` removed the tool. Per-skill grants are dropped (D7, 4.3), `grant_tools` is `Agent` or `Skill`, and both are no-ops in v1 (4.5, 8). The UI's named-skill input is dropped (7). The D7 e2e becomes a PATCH validation test (10).
  - Q6 (`--await-initialize`) was not probed.
  - Review of the build (security and ops, both APPROVE with 0 blockers) added: the loader, shell, package-manager and model-provider families to 4.3, since a plugin's env reaches the whole run process and not only its hooks; redaction of `plugin_segments` for readers who are not team admins (6); the wrapper's exit status 3 read as `plugin_not_loaded` when the report line was lost; a refusal of a symlink anywhere on a run state directory's path, since the run root is agent-writable; and a fix to the SDK key scan, which could drop a key that straddled its carry boundary.
  - Found while building, against the real CLI: a plugin whose `hooks.json` does not parse still gets its `init.plugins` row (manifest name `denyplugin`), and its error is tagged `brokenplugin@inline`, the directory name. Tying by manifest name alone let it pass as loaded, so 4.2 step 4a also ties by directory name, and a new step 4d ties any unattributed inline error to every required entry. With the fix the run fails with `plugin_not_loaded` and the wrapper exits 3 before any tool call.

## Appendix A. First consumer: claude-loop-harness

This shows the motivating plugin as an example of an entry. It is not part of TF's contract.

```json
[{
  "path": "/home/me/plugins/claude-loop-harness",
  "required": true,
  "grant_tools": ["Skill", "Agent"],
  "env": {
    "CLAUDE_LOOP_HARNESS_SESSIONS_ROOT": "{run_state_dir}sessions",
    "CLAUDE_LOOP_HARNESS_STATE_ROOT": "{run_state_dir}state",
    "CLAUDE_LOOP_HARNESS_TRACE_HOME": "{run_state_dir}trace"
  }
}]
```

These keys pass section 4.3 only when the operator sets `TF_PLUGIN_ENV_ALLOW_PREFIXES=CLAUDE_LOOP_HARNESS_`. The SDK key-scan guard confirms that the pinned SDK reads no key under that prefix.

The per-run sessions root stops concurrent runs counting each other as live peers. That is right here because TF gives each run its own worktree and branch. The per-run state and trace roots keep breaker and trace state out of the operator's home.

## Related

- `internal/agentproc/argv.go:20-30`, why user settings are excluded
- commit bace8316, `claude_ai_connectors_enabled`, the template for this setting
- `specs/per-run-credential-isolation/`, credential handling the env denylist protects
- `docs/users/local/configuration.md`, the documented exceptions
