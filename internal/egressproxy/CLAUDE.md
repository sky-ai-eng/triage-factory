# internal/egressproxy — the tunnel egress lane

CONNECT-only forward proxy, deliberately without TLS termination: it sees
the destination hostname, decides allow/deny, and tunnels opaque bytes.
Content integrity is the package managers' own job (lockfile hashes,
go.sum) — that division is what keeps this proxy out of the trust chain.
It is one of three egress lanes; `docs/security/sandbox-egress.md` has the
full picture and `internal/egressrelay/CLAUDE.md` has the sibling lane's
guide.

## Adding an allowlist host (`DefaultRegistryHosts`)

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
- Exact hostnames only — no wildcards in v0. Per-profile allowlists
  (TFAC-408) replace this constant with customer data; the shape here is
  the floor for that design.

## Never

- `deniedRanges` (denylist.go) is operator-owned and compiled in so that
  no configuration knob can widen it. Nothing may bypass it, and nothing
  may re-implement its check: `VettedDialContext` / the internal
  `vettedDial` is the single resolve-then-check gate, shared with the
  fetch-relay lane precisely so there is one implementation to audit.
- No shared storage namespaces, ever, regardless of how convenient the
  entry would be — the tunnel matches hostnames, not paths, so such an
  entry admits every tenant of the storage service.
- No TLS termination. If a feature seems to need the proxy to see inside
  the stream, the answer is a different lane, not MITM.
