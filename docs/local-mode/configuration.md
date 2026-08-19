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
