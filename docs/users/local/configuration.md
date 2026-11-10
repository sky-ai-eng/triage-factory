# Configuration (local mode)

Most of Triage Factory's settings live in the database and are edited in-app.
This covers where state is kept on disk, how credentials reach agents, the
agent sandbox (Linux), what Windows does differently, and the clone protocol.

## Where state lives

Everything is under `~/.triagefactory/` (on Windows,
`%USERPROFILE%\.triagefactory\`):

| Path                                   | Contents                                                                                          |
| -------------------------------------- | ------------------------------------------------------------------------------------------------- |
| `triagefactory.db`                     | The SQLite database, settings included. You should never need to make changes directly in the db. |
| `secrets.enc`                          | Credentials when there is no OS keychain ([Secret storage](secret-storage.md)).                   |
| `bin/gh`                               | The `gh` release agents use (macOS and Linux only).                                               |
| `bin/triagefactory.exe`                | Windows only. The copy of the binary agents run as `triagefactory`. Triage Factory refreshes it.  |
| `blobs/`                               | Snapshots of parked runs' workspaces.                                                             |
| `teams/<team id>/kb/{private,shared}/` | The team's knowledge base, as plain files you can edit in any editor.                             |

`triagefactory uninstall` deletes all of it.

## Credentials

GitHub, Jira, and Linear credentials are entered in the app and stored in the OS
keychain or the encrypted file. Local mode never reads them from environment
variables.

The exception is Claude Code. Delegated agents can use your own Claude login,
making use of your existing subscription. Choose under **Claude credentials**:

- **Use system Claude Code credentials** (the default): agents authenticate
  like `claude` does, from the environment Triage Factory was started with — a
  subscription login, `ANTHROPIC_API_KEY`, or Claude Code's Bedrock or Vertex
  variables. The auth helpers and credential variables in
  `~/.claude/settings.json` apply too.
- **A stored Anthropic or Bedrock key**: agents use only that key. Credentials
  from the environment and `settings.json` are ignored.

Nothing else from your Claude Code setup reaches a run: no permission rules,
hooks, plugins, MCP servers (your claude.ai connectors included), `CLAUDE.md`,
skills, agents or commands. Proxy and CA variables (`HTTPS_PROXY`, `NO_PROXY`,
`NODE_EXTRA_CA_CERTS`) in `settings.json` don't carry over either; set them in
Triage Factory's own environment.

The one exception is the team setting **claude.ai connectors**, off by default,
which lets runs on system Claude Code credentials use your connectors.

## Agent sandbox

### Linux

On Linux, each run executes in a
[bubblewrap](https://github.com/containers/bubblewrap) namespace that hides your
home directory, Triage Factory's database, and other runs' worktrees. This only
guards against accidents, not attacks. Even with the sandbox on, the agent still
runs as you and shares your network.

Auth helpers from `~/.claude/settings.json` run inside the sandbox, so one that
reads files in your home directory (`~/.aws`, `~/.config/gcloud`, its own
script) fails. Move the script out of your home directory, set the credential in
Triage Factory's environment, or store a key in the app.

If Triage Factory can't build the sandbox, it refuses to start with
`refusing to start: local agent runs are sandboxed with bubblewrap, but …`.
Install bubblewrap from your distribution (`sudo apt install bubblewrap`; on
Ubuntu 23.10 and later it must be the distribution's package), or turn the
sandbox off, for example when Triage Factory itself runs in a container:

```bash
TF_LOCAL_SANDBOX=off triagefactory
```

### Windows

There is no agent sandbox on Windows. Each run is a plain child process that
runs as you and can read whatever you can read: your home folder, the database,
other runs' folders, the network, and your stored credentials (in Windows
Credential Manager, or `secrets.enc` when there is no keychain). Triage Factory
logs a warning, `local agent sandbox not available on Windows`, at startup. Setting
`TF_LOCAL_SANDBOX=on` fails at startup with
`TF_LOCAL_SANDBOX=on cannot be honored on windows`; leave it unset or set it to
`off`.

Windows has two other differences that you can see:

- Agents don't get a real `gh` binary. The pinned `gh` that macOS and Linux runs
  use isn't published for Windows, so a run does its GitHub work with the
  scoped `triagefactory exec gh …` commands instead. They use your stored GitHub
  credential and record each action. Nothing needs configuring, and a run isn't
  blocked by it.
- Agents run shell commands in Git Bash (see
  [Install](install.md#prerequisites)), and Triage Factory sets
  `core.longpaths` for their Git, so deep checkouts work.

## Clone protocol

The clone protocol in Settings decides how agents' Git reaches GitHub. The
GitHub API uses HTTPS either way.

- **HTTPS** (the default) authenticates with the GitHub credential you
  connected, through a per-run proxy. Pushes to a repository's base branch are
  checked against the team's policy, and pushes are recorded. We intentionally
  refuse pushes the proxy can't authorize with no fallback.
- **SSH** uses your own SSH agent and key, exactly as Git would from your
  shell. Use it only if your GitHub host restricts Git over HTTPS. The
  base-branch policy and push recording don't apply; a local pre-push hook
  still refuses base-branch pushes, but `git push --no-verify` skips it. If
  SSH fails (for example, an agent that lost its keys after a reboot), the
  operation fails; Triage Factory will never switch to HTTPS on its own.

Again, neither is a security boundary in local mode: an agent runs as you and
can bypass these checks deliberately. GitHub branch protection is the
server-side guard.

## Git LFS

Checkouts Triage Factory makes leave files stored in Git LFS as pointer files,
whatever your own `git lfs install` configured, so a run never downloads LFS
content it didn't ask for. An agent fetches the files it needs with
`git lfs pull --include=<path>`, and a push uploads new LFS objects, both
through the run's proxy. Both need `git-lfs` installed on the machine Triage
Factory runs on; without it, agents see only the pointers.
