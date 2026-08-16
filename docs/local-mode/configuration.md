# Configuration (local mode)

Settings are stored in the SQLite DB at `~/.triagefactory/triagefactory.db`
(table `settings`, single row holding a YAML blob) and edited exclusively via the
Settings page. The poll interval defaults to 5 minutes for both GitHub and Jira;
configurable values are 30s, 1m, 2m, 5m. Maintain this configuration through the
UI, not direct DB access.

## Jira setup

Jira uses a two-stage flow in Settings:

1. Enter your Jira URL and Personal Access Token, click **Connect**. Credentials
   are validated and stored immediately.
2. The card expands to reveal project selection, poll interval, and status
   configuration. Statuses are fetched automatically from your Jira instance.
3. **Save** is disabled until you've configured projects, pickup statuses, and an
   in-progress status.

## Credentials

All credentials (GitHub PAT, Jira PAT, the Anthropic key, GitHub App private
keys) are stored outside the database — in the OS keychain on desktop, or an
encrypted file on headless installs. Token fields in Settings show "leave blank
to keep current" when a token is already stored.

Where exactly they land, and how the headless encrypted-file backend works, is
covered in [Secret storage](secret-storage.md).

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

In local mode this is enforced by TF's `pre-push` git hook, which the agent's
git runs under. Treat it as a **safety guard against mistakes, not a security
boundary**: a local-mode agent runs as you, with unrestricted shell access, so
`git push --no-verify` skips the hook entirely and nothing here can stop an
agent that is actively trying to get around it. What it does reliably stop is
the far more common case — an agent that never considered the rule and ran the
obvious command. Branch protection on GitHub is what actually enforces this;
this setting is the local-first line of defence in front of it.

The guard also fails open on purpose: if the hook cannot work out the policy —
no run context, an unreadable database — the push proceeds rather than being
blocked by an unrelated outage.
