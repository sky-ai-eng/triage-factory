# Running on a server (local mode)

Local mode runs fine on a server, VM or container with no desktop session. This
is still the single-user product; to serve an organization, see
[self-hosting](../self-hosting/install.md).

A server has no OS keychain and no browser. Credentials go in an encrypted file,
and you reach the UI from your own machine. Everything else is configured in the
app, as on a desktop.

1. **Install the binary** with Homebrew or a release tarball
   ([Install](install.md)). `$HOME` must be set; state goes in
   `~/.triagefactory/`.

2. **Create an encryption key** for the encrypted credential file
   ([Secret storage](secret-storage.md)):

   ```bash
   export TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
   ```

   Persist it wherever the process gets its environment (a systemd unit's
   `Environment=`, an env file, a shell profile). The server refuses to start
   without it, and losing or changing it means re-entering every credential.

3. **Start without a browser:**

   ```bash
   ./triagefactory --no-browser
   ```

4. **Reach the UI.** Keep the default loopback bind and tunnel to it:

   ```bash
   ssh -L 3000:localhost:3000 you@server   # then open http://localhost:3000
   ```

   To bind every interface instead, see [Running](running.md#launching). Open
   the UI by IP address in that case: local mode only answers requests
   addressed to `localhost`, an IP address, or the `--host` value.

5. **Configure in the browser**: connect GitHub, pick repositories, connect
   Jira or Linear, and choose how Claude authenticates. Everything you enter is
   stored in the encrypted file.

## Claude credentials

Either enter an Anthropic or Bedrock key in the app, or choose *Use system
Claude Code credentials* and export the variables Claude Code reads
(`ANTHROPIC_API_KEY`, or the Bedrock / Vertex variables) in the server
process's environment, the same way as `TF_SECRET_ENCRYPTION_KEY`. See
[Configuration](configuration.md#credentials).

## Cloning

Repositories clone over HTTPS with the GitHub credential you connected, which
needs nothing on the server. Switch the clone protocol to SSH in Settings only
if the server runs an SSH agent with a key loaded for your GitHub host; see
[Configuration](configuration.md#clone-protocol).
