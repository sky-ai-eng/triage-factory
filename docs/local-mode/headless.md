# Running headless (local mode)

Local mode runs fine on a headless server (no desktop, no browser, no OS
keychain). This is **single-user local mode** — *not* the multi-tenant deployment
in [self-hosting](../self-hosting/install.md), which is the only thing that needs
Postgres, GoTrue, or Docker. There are two ways to set it up: run it and reach
the UI once over a tunnel, or provision everything from environment variables
(zero-touch — see below).

1. **Install the binary** (Homebrew or `go build` — see the
   [README](../../README.md)). `$HOME` must be set; state lands in
   `~/.triagefactory/`.

2. **Generate a secret key.** With no keychain reachable, secrets are kept in an
   encrypted file (see [Secret storage](secret-storage.md)), which requires
   `TF_SECRET_ENCRYPTION_KEY`:

   ```bash
   export TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
   ```

   Persist it where the process reads its environment (a systemd unit's
   `Environment=`, an `.env`, your shell profile). The server refuses to start
   headless without it, and losing or changing it means re-entering your
   credentials.

3. **Run without a browser**, reachable from where you'll use it:

   ```bash
   ./triagefactory --no-browser --host 0.0.0.0
   ```

   `--host 0.0.0.0` exposes the API on all interfaces. The HTTP API is
   unauthenticated, so only do that on a trusted network — otherwise keep the
   default loopback bind and reach it over an SSH tunnel:

   ```bash
   ssh -L 3000:localhost:3000 you@server   # then open http://localhost:3000
   ```

4. **Finish setup in the browser** (the tunneled or exposed URL) — paste your
   GitHub / Jira / Anthropic credentials, pick your repos, and bind your
   identity. Everything you enter persists (encrypted) across restarts.

## Zero-touch provisioning (`TF_HEADLESS`)

To skip the browser entirely — for a reproducible container, a CI runner, or any
unattended deploy — set `TF_HEADLESS=1`. On first start (local mode only) the
server provisions itself from the environment: it creates the workspace, tracks
the listed repos, optionally configures Jira (Data Center), and binds your
identity — landing directly on the app with no setup wizard.

This is **not Linux-specific** — it works the same on macOS (a laptop, a mac CI
runner). On any machine with a working OS keychain, secrets go to the keychain as
usual, so `TF_SECRET_ENCRYPTION_KEY` (step 2 above) isn't needed — that key is
only for the keychain-less file backend. The rest of the flow is identical.

| Variable | Purpose |
| --- | --- |
| `TF_HEADLESS` | Any non-empty value enables env-driven provisioning. |
| `TRIAGE_FACTORY_GITHUB_URL` | GitHub host (e.g. `https://github.com`). |
| `TRIAGE_FACTORY_GITHUB_BOT_PAT` | The org/bot access token the factory polls and runs autonomous work with. |
| `TRIAGE_FACTORY_GITHUB_USER_PAT` | *Your* identity token. Required for a no-browser boot — without it you'll be asked to connect your GitHub identity in the UI. Usually the same value as the bot PAT for a solo operator. |
| `TRIAGE_FACTORY_REPOS` | Comma-separated `owner/repo` list to track. Without at least one, the factory has nothing to poll. |
| `TRIAGE_FACTORY_CLONE_PROTOCOL` | `https` (default) or `ssh` — how repos are cloned to the box. Optional. |
| `TRIAGE_FACTORY_JIRA_URL` | Jira (Data Center) host. Optional. |
| `TRIAGE_FACTORY_JIRA_BOT_PAT` | Jira service token. Optional. |
| `TRIAGE_FACTORY_JIRA_USER_PAT` | Your Jira identity token. Required whenever Jira is configured. |
| `TRIAGE_FACTORY_JIRA_PROJECTS` | Comma-separated project keys (e.g. `SKY,TFAC`). |
| `TRIAGE_FACTORY_JIRA_PICKUP_STATUSES` | Comma-separated statuses that mean "ready to pick up". |
| `TRIAGE_FACTORY_JIRA_INPROGRESS_STATUS` | The single in-progress status. |
| `TRIAGE_FACTORY_JIRA_INREVIEW_STATUS` | The single in-review status — the one that names work awaiting human review. Optional. |
| `TRIAGE_FACTORY_JIRA_DONE_STATUS` | The single done status. |
| `ANTHROPIC_API_KEY` | Claude credential — local mode inherits it from the environment for scoring and delegation. |

The Jira variables apply one global status mapping to every tracked project. Jira
here is optional and Data Center only; Jira Cloud onboarding stays in the UI.

Headless defaults to **HTTPS** cloning, authenticated with the bot PAT — a
headless box usually has no SSH agent, so HTTPS is the credential-bearing path
that works out of the box (private repos included). Set
`TRIAGE_FACTORY_CLONE_PROTOCOL=ssh` only if the box has an SSH agent with a loaded
key for your GitHub host; SSH clones authenticate through that agent, not the PAT,
and base-branch protection and push recording do not apply on that path (see
[configuration](configuration.md)).

Provisioning is a **one-time seed**: it runs only on the first start and never
overwrites anything you later change in the UI, so editing repos or statuses in
the app sticks across restarts. If `TF_HEADLESS` is unset but the seed variables
are present, they're ignored with a warning. Missing or invalid GitHub
credentials skip the whole bootstrap; an incomplete Jira block skips just Jira —
both log a warning rather than failing the server.

> **Note:** `TRIAGE_FACTORY_GITHUB_BOT_PAT` / `TRIAGE_FACTORY_JIRA_BOT_PAT` were
> previously `TRIAGE_FACTORY_GITHUB_PAT` / `TRIAGE_FACTORY_JIRA_PAT`. The `BOT`
> names make the access-vs-identity split explicit (paired with the `USER_PAT`
> identity vars). Update any existing env files on upgrade.
