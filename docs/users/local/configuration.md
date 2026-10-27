# Configuration (local mode)

Settings live in the database and are edited in the app's Settings pages. This
page covers what the app doesn't: where state is kept on disk, how credentials
reach agents, the Linux agent sandbox, and the clone protocol.

## Where state lives

Everything is under `~/.triagefactory/`:

| Path | Contents |
|---|---|
| `triagefactory.db` | The SQLite database, settings included. Change settings in the app, not in the database. |
| `secrets.enc` | Credentials, when there is no OS keychain ([Secret storage](secret-storage.md)). |
| `bin/gh` | The `gh` release agents use ([Running](running.md#the-agents-gh)). |
| `blobs/` | Snapshots of parked runs' workspaces. |
| `teams/<team id>/kb/{private,shared}/` | Each team's knowledge base, as plain files you can edit in any editor. |

`triagefactory uninstall` deletes all of it.

## Credentials

GitHub, Jira and Linear credentials are entered in the app and stored in the OS
keychain or the encrypted file. Local mode never reads them from environment
variables.

Claude is the exception. Under **Claude credentials**:

- **Use system Claude Code credentials** (the default) stores nothing. The
  Claude Code process an agent runs in authenticates from the environment it
  inherits from Triage Factory: a Claude subscription login, an exported
  `ANTHROPIC_API_KEY`, or the Bedrock / Vertex variables Claude Code reads.
- **A stored Anthropic or Bedrock key** replaces all of that. The inherited
  credential variables are removed from the agent's environment, so a stray
  shell variable can't override the stored key.

Runs don't load your own Claude Code settings. Claude Code starts with project
settings only, so `~/.claude/settings.json` adds no permission rules, hooks,
plugins, MCP servers or `env` to a run, and your user `CLAUDE.md`, skills,
agents and commands don't load.

With *Use system Claude Code credentials*, Triage Factory does pass on the
authentication parts of `~/.claude/settings.json`, and nothing else:

- `apiKeyHelper`, `proxyAuthHelper`, `awsAuthRefresh`, `awsCredentialExport`
  and `gcpAuthRefresh`.
- From its `env`, the credential variables only: the `ANTHROPIC_*` key, token,
  base URL and model variables, Claude Code's OAuth token, the
  `CLAUDE_CODE_USE_*` provider switches, each provider's endpoint, project and
  region variables, and the AWS and Google credential chains. These take
  precedence over the same variables in Triage Factory's environment.

Proxy and CA settings in that `env` (`HTTPS_PROXY`, `NO_PROXY`,
`NODE_EXTRA_CA_CERTS`) are not passed on. Set them in Triage Factory's own
environment instead. With a stored key, nothing from the file is passed on.

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
