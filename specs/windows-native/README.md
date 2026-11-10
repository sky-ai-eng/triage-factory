# Windows-native local mode

Design spec for running local mode natively on Windows. For maintainers; users
read `docs/users/local/install.md`.

Ticket: TFAC-XXXX (placeholder in the commit titles; the maintainer assigns the
real id).

## Goals

`triagefactory.exe` (windows/amd64 and windows/arm64, `CGO_ENABLED=0`) installs,
starts, delegates, runs, steers, stops, resumes and cleans up an agent run in
local mode, with no Unix emulation beyond the Git for Windows bash that Claude
Code itself requires. Multi mode keeps compiling on Windows. Linux and macOS
behavior is unchanged: Windows code is selected at build time by file name, and
shared code gains a seam only where a platform pair plugs in.

## Non-goals

- Multi mode on Windows (gVisor, cap-broker, sidecar, Postgres roles).
- The gh binary channel on Windows.
- A local agent sandbox on Windows. The Linux one is bubblewrap.
- Authenticode signing, winget, Scoop. See [Maintainer decisions](#maintainer-decisions-needed).

## Prerequisites on the user's machine

Git for Windows (its bash is the only shell Claude Code uses on Windows), Node.js
18 or later, Claude Code. Documented in `docs/users/local/install.md`.

## Design by area

### Process tree (`internal/agentproc/proctree_windows.go`)

Cancelling the agent used to mean killing a process group. Windows has none that
reaches grandchildren, and the agent is a chain (node, claude, bash, then tools):
killing node alone leaves orphans holding handles on the run directory, so
cleanup fails.

Each agent process tree lives in a Job Object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. Cancel calls `TerminateJobObject`, then
waits up to 2s for the job's active process count to reach zero. The job handle
closes after `Wait`, and also when TF itself dies, which kills the tree. The
spawn seam is `execProc` (`proc.go`): attach happens right after `cmd.Start`.
`cmd.WaitDelay` is 3s on Windows so a grandchild holding the stdout pipe cannot
hang `Wait`. The Unix arm is the same two statements as before (`Setpgid`,
`Kill(-pid)`).

Known gap: `os/exec` has no suspended start, so a child spawned between `Start`
and attach escapes the job. A launcher shim (Volta, fnm) that spawns at once can
reach it. Accepted: node loads the SDK before it spawns anything.

### Arguments file (`argsfile_windows.go`, `wrapper.mjs`)

`CreateProcess` caps a command line at 32,767 characters. A run's arguments (the
system prompt, the tool allowlist, the tool schemas) are typically 22 to 40 KB
before Windows quoting. On Windows the wrapper gets `--args-file <path>`
instead: the arguments as a JSON array in an owner-only file under the state
root (not the run root, which the agent can write). The wrapper reads and
deletes it. Stale files older than an hour are pruned, since a crashed spawn
leaves prompt text behind. It is always on for Windows rather than past a
length threshold, because typical runs already exceed the limit and a threshold
would make the common path the untested one. stdin stays the interactive
control channel.

### The binary on the agent's PATH (`agentbin_windows.go`)

The agent runs commands in Git Bash, which treats a backslash as an escape, so
`C:\Users\me\triagefactory.exe exec ...` arrives mangled and a permission rule
naming that path never matches. Instead, the agent's PATH gets
`<StateRoot>\bin` first, where `triagefactory.exe` is a hardlink to the running
binary (a copy where links fail, compared by size and mtime; a stale file is
renamed aside, since Windows allows renaming an open image but not overwriting
it). The prompt keeps the bare name `triagefactory exec`, and the allowlist is
`Bash(triagefactory exec *)`. It is never the running binary's own folder: the
user owns that folder and may keep other programs in it. Paths shown to the
agent (run root, checkouts) are forward-slashed.

### Git Bash preflight (`gitbash_windows.go`)

Checked before the SDK install check, so a host with the SDK but no Git for
Windows fails early with a message naming the fix. Order: an explicit
`CLAUDE_CODE_GIT_BASH_PATH` (must exist; a bad value is an error, not a
fall-through), else derived from `git.exe` (`..\bin\bash.exe`, then
`..\..\bin\bash.exe`). It never searches PATH for `bash`, which on Windows is
often the WSL launcher. The resolved path is exported to the agent as
`CLAUDE_CODE_GIT_BASH_PATH`. `TF_CLAUDE_BINARY` on Windows must be a regular
`.exe` or `.js` file; there is no execute bit to check.

### Claude session directory (`internal/worktree/claude_session_windows.go`)

Claude Code keeps a run's transcript in `~/.claude/projects/<encoded cwd>`. On
Windows its encoding replaces every UTF-16 code unit outside `[A-Za-z0-9]` with
`-` and keeps case. TF's old encoder replaced only `/` and `.`, so resume and
cleanup missed the directory. For an encoded name over 200 characters Claude
Code truncates and appends a hash TF does not reproduce, so TF returns a typed
error (`ErrClaudeSessionNameTooLong`): crash-reclaim starts a fresh run,
message-resume fails with a session-lost error, and cleanup is a no-op, each
logged at WARN. The run root is under `os.TempDir()`, resolved through
`GetLongPathName` so the path TF passes as cwd matches what Claude Code
realpaths (an 8.3 short temp path is common).

### gitssh (`cmd/gitssh/exec_windows.go`)

`syscall.Exec` does not exist on Windows. The Windows arm starts ssh as a child
with inherited stdio and exits with its code (127 on start failure, as before).

### Instance lock (`internal/instance/lock_windows.go`)

`LockFileEx`, exclusive and fail-immediately, on one byte at offset 1 GiB.
`ERROR_LOCK_VIOLATION` maps to `ErrLocked`, and a second instance is refused.
Windows byte-range locks are mandatory and block reads through other handles, so
locking offset 0 would make the id file unreadable to anything but the holder.

### Keychain chunking (`internal/auth/keychain.go`)

go-keyring refuses a Windows Credential Manager value over 2560 bytes. Values
over the limit are split by bytes into `key#1` to `key#N`; the base entry holds
`tfchunks:v1:<N>:<sha256 of the whole value>`. Write order is chunks, then
base. Read reassembles the chunks and verifies the hash; a mismatch or a
missing chunk is an error. Delete removes the base and probes `key#1...` until one is not
found. Replacing a chunked value removes chunks beyond the new count. A failed
chunked write deletes the chunks it wrote and the stale base, so the key reads
as absent. One mutex covers put, get and delete. Values at or under the limit
are stored exactly as before, and other platforms set the limit to 0 (off).
The limit is a per-platform constant, and the tests run the logic on every OS.

### Git environment (`internal/githooks/gitenv_windows.go`, `internal/worktree/gitenv_windows.go`)

TF's own git calls and the agent's git get `core.longpaths=true`, an empty
`credential.helper` (resets the list so Git Credential Manager never opens a
window or stores a token), and `GCM_INTERACTIVE=never`. Both env builders use
the same `GIT_CONFIG_COUNT` layering. `gpg.ssh.program` is forward-slashed,
because git runs it through a shell. Hooks run under Git for Windows' sh; a
Windows-only test runs real `git commit -S` and `git push` through the hooks
against a local bare remote.

### Install and uninstall (`cmd/install/install_windows.go`, `cmd/uninstall/uninstall_windows.go`)

Windows has no unprivileged symlink, so install copies the binary to
`%LOCALAPPDATA%\Programs\triagefactory\triagefactory.exe` (temp file plus
rename; an existing copy is renamed to `.old` first), compares PATH entries
case-insensitively, and prints a PowerShell line that prepends the folder to the
user PATH. It does not change PATH itself. Replacing an existing destination
without `--force` is allowed only for a regular file named `triagefactory.exe`.
Re-running install after an upgrade is the upgrade path. Uninstall removes only
that file; a running copy is renamed to `.old` for the user to delete.

### Other

- State root stays `%USERPROFILE%\.triagefactory`, matching the `paths` package
  and the docs on other systems.
- The boot log says at INFO that Windows has no local sandbox.
  `TF_LOCAL_SANDBOX=on` already fails at boot on any non-Linux system.
- `isRunsBase` compares case-insensitively; the over-long path check in the
  scratch walk matches `ERROR_FILENAME_EXCED_RANGE`.
- `cmd/runsidecar` splits its listener fd check by platform; off Linux it
  reports an error and falls back to an ephemeral port, as a closed fd does.
- `filestore` rename retries briefly on a sharing violation.

### Line endings

`.gitattributes` pins `eol=lf` for text. `go:embed` bytes must match on every
platform, and the embedded hook scripts, `wrapper.mjs`, prompt blocks and seeds
break under CRLF. Tests assert every embedded text asset has no `\r`, so a
checkout with `autocrlf=true` cannot ship one.

### CI and release

`test.yml` gains a `test-windows` job (build, vet, gotestsum, no `-race`, since
the detector needs cgo). `goreleaser.yaml` builds windows amd64 and arm64 and
packs them as zips named `triagefactory_<version>_windows_<arch>.zip`; the brew
formula sees only darwin and linux. `scripts/lint.sh` cross-builds
`windows/amd64` and finds `goimports.exe`.

## Decisions and rationale

- **Job Object, not a process group.** The agent is a four-level chain, and only
  a job reaches every descendant. It also gives crash cleanup for free.
- **Args file, always on.** See above: a threshold would leave the common path
  untested.
- **Bare name on PATH, not an absolute path.** An absolute Windows path cannot
  survive Git Bash quoting or be matched by a permission rule. The cost is one
  hardlink per upgrade.
- **Closing the job after `Wait` also kills background processes the agent left
  running.** This differs from Unix, where the process group can outlive the
  run. On Windows it is the wanted cleanup.
- **No session-name hashing.** Reproducing Claude Code's hash would couple TF to
  an undocumented detail that is easy to get wrong. A typed error and a WARN
  say what happened, and the run degrades to a fresh start.
- **Session encoding changed on Windows only.** Linux keeps its `/` and `.`
  replacement so this change is byte-identical there, even though Claude Code
  probably maps all non-alphanumerics on Linux too (see follow-ups).
- **Bad `CLAUDE_CODE_GIT_BASH_PATH` is an error.** A silent fall-through to
  another bash hides the mistake.
- **Chunks carry a hash.** A partial or interleaved write is detected and
  surfaces as an error, not as a corrupt key.
- **State root unchanged.** Moving to `%LOCALAPPDATA%` would make Windows the one
  platform whose state is somewhere else, for no user benefit.
- **Skipped tests.** A Windows skip carries a one-line reason. Permission-bit
  tests skip (Windows has no POSIX modes), and Linux-only code (sandbox,
  cap-broker, local sandbox) is build-tagged out of the Windows test run.

## Risks

- **Bare-name allow rule.** Claude Code on Windows must match
  `Bash(triagefactory exec *)` against the command the model writes in Git Bash.
  Tests cover the rule text and the PATH; only a manual run with a real Claude
  Code confirms the match.
- **Job attach race** for launcher shims, as above.
- **Unsigned binary.** SmartScreen warns on first run. Antivirus products may
  also flag an unsigned Go binary that spawns processes.
- **Long paths.** TF covers Git with `core.longpaths`, but other tools need the
  Win32 `LongPathsEnabled` setting, and a long `TEMP` path disables session
  resume (200-character limit).
- **Unsandboxed runs.** Windows has no local sandbox, so a run has the user's
  full access. Documented, logged at boot, and not a regression relative to
  `TF_LOCAL_SANDBOX=off` elsewhere.
- **Windows CI cost.** The `test-windows` job is slower and costs more per minute
  than the Linux jobs, and was not run on GitHub Actions while writing this.
- **Tests.** Part of the suite assumes Unix (`sh`, `chmod`, symlinks). The ones
  that could be made portable were.

## Maintainer decisions needed

1. **Ticket id.** Replace `TFAC-XXXX` in the commit titles.
2. **Authenticode signing.** Whether to sign `triagefactory.exe`, with what
   certificate, and where the key lives. Signing alone may not remove the
   SmartScreen warning until the certificate has built reputation.
3. **Distribution.** winget, Scoop, both or neither. Both want a stable zip URL
   and a hash, which goreleaser already produces; each also needs a manifest or
   bucket repository to maintain.
4. **gh channel on Windows.** Whether to ship a pinned `gh` for Windows. Four
   changes: a `.exe` name in the binary path, the zip asset's `bin/gh.exe` path,
   replacing a running `gh.exe` (rename fails over a running executable), and
   the injector's self-signed certificate (Go on Windows ignores `SSL_CERT_FILE`).
   Until then runs use the scoped `exec gh` verbs.
5. **Windows CI.** Whether `test-windows` runs on every PR or on a schedule or
   label, given the cost.

## Follow-ups

- **Linux session encoding.** Claude Code likely replaces every non-alphanumeric
  character on Linux and macOS too, with the same 200-character rule. TF's
  encoder replaces only `/` and `.` there, so a path containing any other
  non-alphanumeric character (a space or an underscore, for example) can miss
  the directory. Verify against the installed Claude Code on Linux and fix in a
  separate change.
- **Local sandbox on Windows.** There is no bubblewrap equivalent. A Job Object
  limits resources, not file access; an AppContainer or a restricted token
  would be the starting point.
- **gh channel on Windows.** See decision 4.
- **Signing and distribution.** See decisions 2 and 3.
- **CA bundle paths on Windows.** `internal/cabundle` reads only the Linux CA
  bundle paths, so on Windows a consumer of `SSL_CERT_FILE` would trust only the
  injected leaf certificate and not the system roots. Moot while the gh channel
  is unavailable on Windows (decision 4); fix it before that channel is enabled.
- **Same-tick re-raise.** `NotificationUpsertOf` can report a re-raise in the
  same clock tick as an escalation when the clock is coarse, as the Windows
  clock is.
- **Go 1.27 `encoding/json` field names.** Under Go 1.27, `encoding/json` puts
  array and map segments in `UnmarshalTypeError.Field`, which breaks the field
  naming in `httpx`. This is not Windows-specific. The contributor has a patch
  available.
