# Configuration (local mode)

Most of Triage Factory's settings live in the database and are edited in-app.
This covers where state is kept on disk, how credentials reach agents, the
Linux agent sandbox, and the clone protocol.

## Where state lives

Everything is under `~/.triagefactory/`:

| Path                                   | Contents                                                                                          |
| -------------------------------------- | ------------------------------------------------------------------------------------------------- |
| `triagefactory.db`                     | The SQLite database, settings included. You should never need to make changes directly in the db. |
| `secrets.enc`                          | Credentials when there is no OS keychain ([Secret storage](secret-storage.md)).                   |
| `bin/gh`                               | The `gh` release agents use.                                                                      |
| `blobs/`                               | Snapshots of parked runs' workspaces.                                                             |
| `teams/<team id>/kb/{private,shared}/` | Each team's knowledge base, as plain files you can edit in any editor.                            |

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
hooks, plugins, MCP servers, `CLAUDE.md`, skills, agents or commands. Proxy and
CA variables (`HTTPS_PROXY`, `NO_PROXY`, `NODE_EXTRA_CA_CERTS`) in
`settings.json` don't carry over either; set them in Triage Factory's own
environment.

## Agent sandbox (Linux)

On Linux, each delegated run executes inside a
[bubblewrap](https://github.com/containers/bubblewrap) mount and pid namespace.
A run sees its own worktree, the directories you granted it, and the Claude
Code credential file and its own transcript directory under `~/.claude`. Your
home directory, the Triage Factory database, other runs' worktrees and `/run`
are hidden, and a run sees only its own processes.

This prevents accidents (a confused or prompt-injected agent reading your
dotfiles, the database or another run's work). It is not a security boundary:
the agent still runs as you, shares your network, and could break out on
purpose.

What changes with the sandbox on:

- Each run's worktree is a full clone rather than a linked worktree, which
  costs clone time and disk.
- An agent can read and write the Claude Code credential its run uses, since a
  token refresh mid-run has to reach your own sessions.
- An auth helper passed on from your `settings.json` runs inside the sandbox,
  so one that needs a file in your home directory (its own script, `~/.aws`,
  `~/.config/gcloud`) fails. Keep the script outside your home directory, set
  the credential in Triage Factory's environment, or store a key in the app.

If Triage Factory can't build the sandbox it refuses to start, with a message
beginning `refusing to start: local agent runs are sandboxed with bubblewrap,
but …`. Install bubblewrap from your distribution
(`sudo apt install bubblewrap`; on Ubuntu 23.10 and later the distribution
package is what allows unprivileged user namespaces), or turn the sandbox off:

```bash
TF_LOCAL_SANDBOX=off triagefactory
```

With it off, agents run with your user's full access. Turn it off when Triage
Factory itself runs in a container (nested user namespaces are usually
blocked) or on a host without bubblewrap.

The sandbox is Linux-only. On macOS and Windows it is off, and setting
`TF_LOCAL_SANDBOX=on` there fails at startup.

## Clone protocol

The clone protocol in Settings decides how agents' Git reaches GitHub. The
GitHub API uses HTTPS either way.

- **HTTPS** (the default) authenticates with the GitHub credential you
  connected, through a per-run proxy. Pushes to a repository's base branch are
  checked against the team's policy, and pushes are recorded. A push the proxy
  can't authorize is refused; it never falls back to your SSH key or Git
  credential helper.
- **SSH** uses your own SSH agent and key, exactly as Git would from your
  shell. Use it only if your GitHub host restricts Git over HTTPS. The
  base-branch policy and push recording don't apply; a local pre-push hook
  still refuses base-branch pushes, but `git push --no-verify` skips it. If
  SSH fails (for example, an agent that lost its keys after a reboot), the
  operation fails; Triage Factory never switches to HTTPS on its own.

Neither is a security boundary in local mode: an agent runs as you and can
bypass these checks deliberately. GitHub branch protection is the server-side
guard.
