# internal/egressproxy — the tunnel egress lane

A CONNECT proxy that intercepts what it admits. It sees the destination
hostname and decides allow/deny; for an admitted host it verifies the
registry's certificate itself, terminates the client's TLS with a leaf from
a CA minted for the engagement, and relays each request the policy admits
with the response copied back verbatim. Registries are read-only through it:
GET and HEAD, plus POST to the paths a registry lists as reads. It is one of
three egress lanes; `docs/security/sandbox-egress.md` has the full picture
and `internal/egressrelay/CLAUDE.md` has the sibling lane's guide. The
design of record, with every condition this package implements, is
`docs/for-agents/specs/sandbox-backends/` §6.1.

## Adding an allowlist host (`DefaultRegistries`)

- **Probe first**: `curl -sI --max-redirs 0 <artifact URL>`. An entry only
  works if artifacts are served from the hostname itself or a DEDICATED
  companion hostname (add both). If artifacts redirect into a shared
  storage namespace (`storage.googleapis.com`, presigned S3), an entry
  here does NOT work and must not be added — that ecosystem belongs to
  the fetch-relay lane (`internal/egressrelay`).
- **Every entry needs an in-sandbox consumer**: a toolchain actually
  installed in the rootfs (`internal/sandbox` rootfs.go `apkPackages`)
  that dials it. Name the consumer in the comment. Entries without one
  are dead reach waiting to be abused, and get removed.
- **Its toolchain must trust the run's CA.** Every admitted host is
  intercepted, so a client that ignores the trust variables in
  `agentproc.caBundleEnv` fails with certificate errors. Run
  `TestIntegration_EgressIntercept` (`internal/agentproc`, root and runsc)
  with the new toolchain added to its script; when it fails, add that
  client's variable to `caBundleEnv` and to the broker's
  `allowedSandboxEnvKeys`.
- **A read POST path is probed like a host.** List one in
  `ReadPOSTPaths` only after capturing a real client sending it (the
  denial log names the method and path of every refused request) and
  confirming the registry answers a read there and stores nothing.
- Exact hostnames only — no wildcards. Per-profile allowlists (TFAC-408)
  replace this constant with customer data; the shape here is the floor
  for that design.

## Never

- `deniedRanges` (denylist.go) is operator-owned and compiled in so that
  no configuration knob can widen it. Nothing may bypass it, and nothing
  may re-implement its check: `VettedDialContext` / the internal
  `vettedDial` is the single resolve-then-check gate, shared with the
  fetch-relay lane precisely so there is one implementation to audit.
  The interceptor's redials go through it too.
- No shared storage namespaces, ever, regardless of how convenient the
  entry would be — the allowlist matches hostnames, so such an entry
  admits every tenant of the storage service.
- No `tls.Config` outside `tlsconfig.go`, and no `InsecureSkipVerify`
  anywhere; `scripts/lint.sh` refuses both. The upstream configuration
  takes nothing from `Config`: one dial verifies for every toolchain in
  the jail, and each of them then sees a valid certificate, so a weakened
  check would weaken all of them silently.
- Nothing that rewrites or caches a body. The relay copies the upstream
  body verbatim; a hook that could edit it, or a cache that could serve
  one run's response to another, needs its own design.
- No fallback to an opaque tunnel. A host that fails verification, a
  client whose TLS server name or `Host` differs from the CONNECT host,
  and every refused request get an explicit error and a recorded denial.
- Never intercept GitHub or the LLM provider. They are not on the
  allowlist, and the CA's name constraints cover only the hosts that are.
- The CA's private key never leaves this process's memory: not the sealed
  bundle, not the run tree, not a workspace snapshot. Only its certificate
  goes into the jail's CA bundle.
