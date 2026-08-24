# Configuration (local mode)

Settings are stored in the SQLite DB at `~/.triagefactory/triagefactory.db`
(table `settings`, single row holding a YAML blob) and edited exclusively via the
Settings page. The poll interval defaults to 5 minutes for both GitHub and Jira;
configurable values are 30s, 1m, 2m, 5m. Maintain this configuration through the
UI, not direct DB access.

## Jira setup

Jira setup in Settings goes connect → watch → map:

1. Enter your Jira URL and Personal Access Token, click **Connect**. Credentials
   are validated and stored immediately.
2. The card expands to reveal the project picker, the poll interval, and the
   per-project status mapping. The picker lists the projects your Jira
   credential can see, read live from Jira rather than from a stored copy.
3. **Watch** a project in one click. A watched project is tracked but not yet
   polled: it shows as *Statuses not mapped* until you map its workflow — which
   of its statuses count as pickup, as in-progress, and as done. Mapping is
   per-project, because each project's workflow scheme has its own statuses.
4. Only a fully mapped project is polled. Watching one without mapping it is a
   valid saved state, so **Save** is blocked only by a half-finished rule (one
   with statuses picked but no write target), never by an unmapped project.

## Credentials

All credentials (GitHub PAT, Jira PAT, the Anthropic key, GitHub App private
keys) are stored outside the database — in the OS keychain on desktop, or an
encrypted file on headless installs. Token fields in Settings show "leave blank
to keep current" when a token is already stored.

Where exactly they land, and how the headless encrypted-file backend works, is
covered in [Secret storage](secret-storage.md).

## Knowledge base

The Knowledge page is your team's knowledge base — the notes, conventions and
runbooks every delegated run reads before it starts. It is stored as plain files
under the state root, in two folders:

```
~/.triagefactory/teams/<teamID>/kb/private/…    # your team's own
~/.triagefactory/teams/<teamID>/kb/shared/…     # published to the organization
```

**The folder is the visibility.** There is no per-file setting anywhere;
publishing a document moves it from `private/` to `shared/`, and the page's
publish verb is exactly that move. At N=1 the distinction costs nothing and
still matters: it is the same layout a hosted deployment uses, so a knowledge
base written here means the same thing if the org ever grows.

Under each root the layout is an ordinary folder tree. Because these are plain
files on your own machine — no object store, no database — you can edit them
with your usual editor and the page picks the change up on its next read. Names
are validated the same way the upload route validates them: no traversal, no
path separators inside a name, no dot files.

Before every delegated run, the whole knowledge base is copied into that run's
tree under `_tfac/knowledge/`, and a manifest of what landed — the folder tree
with a one-line summary per file — rides the run's opening context. The copy is
read-only from the agent's point of view: it is rebuilt on the next run, so
anything an agent leaves there is discarded.

## Agent sandbox (Linux)

On Linux, delegated agent runs execute inside a
[bubblewrap](https://github.com/containers/bubblewrap) mount namespace by
default. Inside it a run sees its own worktree, its own gh config, TF's git
hooks and toolchain, and any directory you explicitly granted the run — and
nothing else. Your home directory, the TF database and clone cache, every
concurrent run's worktree, and the OS keychain's session sockets are all
replaced by empty directories.

This is **courtesy isolation, not a security boundary**. The agent runs as you,
shares your network, and could break out on purpose the way any process running
as you could. What it prevents is the accident: a wandering or prompt-injected
agent reading another run's work, your dotfiles, or the database, all of which
were one `cat` away before.

Two consequences worth knowing:

- Each run's worktree becomes a self-contained clone rather than a zero-copy
  linked worktree, because the shared bare clone cache isn't visible inside the
  namespace. That costs clone time and disk.
- The agent can still read `~/.claude` — its own transcripts live there, and so
  does the Claude Code credential the run authenticates with.

`TF_LOCAL_SANDBOX=off` turns it off, and agents then run with your full user's
powers, exactly as they did before. Set it if TF itself runs inside a container
(nested unprivileged user namespaces are usually blocked), on a host without
bubblewrap, or when you want the zero-copy worktree performance back.

Bubblewrap is required when the sandbox is on: if TF cannot build a namespace
at startup it **refuses to boot** rather than silently running agents
unsandboxed. Install it (`sudo apt install bubblewrap`, or your distro's
equivalent — the distro package is what carries the AppArmor profile that
permits unprivileged user namespaces on Ubuntu 23.10 and later) or set
`TF_LOCAL_SANDBOX=off`.

The sandbox is Linux-only today. On macOS and Windows the setting resolves to
off; an explicit `TF_LOCAL_SANDBOX=on` there is a boot error rather than a
no-op, since a security setting that is quietly ignored is worse than one that
is refused. macOS support is planned on `sandbox-exec`, which is a different
enough mechanism — it filters paths where bubblewrap replaces them — to be its
own piece of work rather than a port.

## Base-branch pushes

Team Settings → **Team defaults** → *Pushes to the base branch* controls whether
a delegated agent may push straight to a repository's base or default branch —
`main`, `master`, or whatever the repository records, plus any base branch you
configured for the repo. The default is **Never**: agents push their own branch
and open a pull request. **Only runs a human started** allows it for a run you
dispatched yourself while still refusing it on runs a trigger fired, and
**Always** allows it everywhere — the right setting for trunk-based repos, docs
and config repos, and generated-file bots.

The setting is per team, not per repository or per prompt, and only a team admin
can change it. That is deliberate: a task's text comes from pull-request bodies,
issue comments and labels, so anyone who can comment on an issue in a tracked
repository could otherwise talk a run into pushing to `main`.

In local mode TF routes the managed Git path through a per-run loopback proxy.
That proxy authenticates with the GitHub identity configured in TF, checks the
team's tracked/materialized repository set and base-branch policy, and records
the actual upstream result. It refuses a push when it cannot authorize the
repository or ref instead of falling back to your SSH key or credential helper.

This is still a **safety and identity guarantee for ordinary behavior, not a
security boundary**: a local-mode agent runs as you with unrestricted shell
access and can deliberately bypass process-scoped routing. Multi mode's sandbox
is the containment boundary; GitHub branch protection remains the authoritative
server-side guard in either mode.
