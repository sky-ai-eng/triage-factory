# Running on a server (local mode)

Local mode runs on a server, VM or container with no desktop login. This is
**single-user local mode** — *not* the multi-tenant deployment in
[self-hosting](../self-hosting/install.md), which is the only thing that needs
Postgres, GoTrue, or Docker.

A server changes two things: there is no OS keychain to hold credentials, and
there is no browser on the machine. The first is handled by an encrypted file,
the second by reaching the UI from your own machine. Everything else is
configured in the UI exactly as on a desktop. GitHub and Jira credentials can
only be entered there; local mode does not read them from environment variables.
Claude is the one credential that can come from the environment, covered
[below](#claude-credentials).

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
   without it when the encrypted file is in use, and losing or changing it means
   re-entering your credentials.

3. **Run without a browser:**

   ```bash
   ./triagefactory --no-browser
   ```

4. **Reach the UI.** Keep the default loopback bind and tunnel to it from your
   own machine:

   ```bash
   ssh -L 3000:localhost:3000 you@server   # then open http://localhost:3000
   ```

   Alternatively, bind every interface with `--host 0.0.0.0`. Local mode has no
   authentication — every request is treated as the owner — so the server
   refuses a non-loopback bind unless you also set `TF_ALLOW_PUBLIC_LOCAL=true`.
   Only do that on a network where everyone who can reach the port is you.

5. **Configure in the browser.** Start your factory, then connect GitHub (host
   and token), pick your repos, connect Jira if you use it, choose how Claude
   authenticates, and bind your own identity. Everything you enter is stored in
   the encrypted file and survives restarts.

## Claude credentials

Either enter an Anthropic or Bedrock key under **Claude credentials**, or choose
*Use system Claude Code credentials* and give the server process the credentials
Claude Code reads itself: export `ANTHROPIC_API_KEY` (or the Bedrock / Vertex
variables) where the process gets its environment, as with
`TF_SECRET_ENCRYPTION_KEY`. Triage Factory passes that environment through to
the agent untouched and never reads the variables itself; see
[configuration](configuration.md#credentials) for how the two options interact.

## Cloning

Repositories clone over **HTTPS** by default, authenticated with the GitHub token
you connected, which works on a server with no SSH agent. Switch the clone
protocol to SSH in Settings only if the server has an SSH agent with a loaded key
for your GitHub host; SSH clones authenticate through that agent, not the token,
and base-branch protection and push recording do not apply on that path (see
[configuration](configuration.md)).
