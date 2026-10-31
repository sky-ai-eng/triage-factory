# Sandbox egress lanes

How a sandboxed run reaches the network, and how to add support for a new
package ecosystem. The working guide for each lane is next to its code:
`internal/proxy/egressproxy/CLAUDE.md` (tunnel) and `internal/proxy/egressrelay/CLAUDE.md`
(fetch relay). The design of record is
[`specs/sandbox-backends/`](../../specs/sandbox-backends/README.md) §6.1.

A run's sandbox can open connections only to the jail-facing listeners of its
own credential sidecar. Everything else, including the sidecar's LLM and API
proxies, is closed to it. A new kind of jail-facing listener is unreachable
until it is added to `JailEndpoints` (`internal/agentproc/proxies.go`,
`cmd/runsidecar/runtime.go`); a new fetch relay is added automatically from
the catalog. Under every lane sits the compiled-in internal denylist
(`internal/proxy/egressproxy/denylist.go`: cloud metadata, private ranges, the
sandbox pool, sibling gateways), which nothing can widen.

## The three lanes

| Lane | Mechanism | For | Add support by |
| --- | --- | --- | --- |
| **Tunnel** | CONNECT proxy with a hostname allowlist. TLS is terminated under a CA minted for each engagement, and registries are read-only (GET and HEAD, plus POSTs a registry lists as reads). | Registries that serve artifacts from their own or a dedicated hostname | An allowlist entry in `egressproxy.DefaultRegistries` |
| **Fetch relay** | Host-side HTTP relay that follows redirects itself and checks every hop against the denylist | Registries that redirect artifacts into shared multi-tenant storage | A catalog entry in `internal/proxy/egressrelay/catalog.go` |
| **Credential** | Per-run proxies that attach a credential (git, `gh`) | Authenticated egress | Its own design and a security review; see [Privilege separation](../users/security/privilege-separation.md) |

The API proxies behind `tfac exec` verbs (GitHub, Jira, Linear) are not lanes:
a verb goes over the agenthost socket to the run's sidecar, which calls the
upstream itself. A new verb-backed provider needs no allowlist entry.

Two constraints shape every lane:

- **Never allowlist shared storage** (`storage.googleapis.com`, presigned S3).
  The allowlist matches hostnames, so the entry would admit every tenant's
  bucket and give an agent reading attacker-written text a way to upload
  data. The fetch relay exists for registries that redirect there.
- **Never change the bytes.** Content integrity rests on each package
  manager's own checks (lockfile hashes, `go.sum` and the checksum database),
  which a lane must pass through untouched. An ecosystem without such a check
  needs a design discussion, not a catalog entry.

## Which lane does an ecosystem need? Probe it

```bash
curl -sI --max-redirs 0 <real artifact URL> | grep -iE "^HTTP|^location"
```

Probe with a **large** artifact. The Go module proxy serves small zips inline
and redirects large ones to shared storage, so a small-file test passes while
real builds fail.

Measured behavior (2026-08):

| Ecosystem | Serving behavior | Lane |
| --- | --- | --- |
| npm | `registry.npmjs.org`, direct 200 | Tunnel (shipped; audit POSTs listed as reads) |
| PyPI | 302 → `files.pythonhosted.org` (dedicated) | Tunnel (shipped; pip runs from a venv) |
| Go modules and toolchains | 302 → `storage.googleapis.com` for large zips; small zips inline | Fetch relay (shipped: `go-modules`) |
| Hackage | direct 200 | Tunnel, when a rootfs variant ships GHC |
| Maven Central | direct 200 | Tunnel, when a rootfs variant ships a JDK |
| GitHub release assets | 302 → `release-assets.githubusercontent.com` (dedicated) | Tunnel, if needed |
| crates.io, Hugging Face | Not probed yet | Undecided (Hugging Face's presigned-storage redirects suggest the relay) |

A tunnel entry also needs:

- **A consumer in the sandbox**: a toolchain in the rootfs that actually uses
  it. Hackage and Maven entries land with the rootfs variants that ship GHC
  and a JDK, not before.
- **Trust in the run's CA**: the toolchain must read the run's CA bundle
  through a variable set in `cell.JailEnv`, or every fetch fails
  verification.

## Adding support

1. Probe the ecosystem with the command above and paste the output in your PR.
2. Tunnel: follow `internal/proxy/egressproxy/CLAUDE.md`.
3. Fetch relay: follow `internal/proxy/egressrelay/CLAUDE.md` (catalog entry; tests
   including the SSRF-refusal case and a `-tags live` fetch of an artifact
   that redirects; the broker env allowlist; a row in the table above).
4. Credentialed egress: expect a security review, not a checklist.

The invariants (a denylist-checked dial on every hop, no request headers
forwarded by the relay, no shared-storage allowlisting, bodies passed through
unchanged and uncached, upstream TLS built in one place and never skipped, no
environment variable that weakens verification) are enforced by tests and
`scripts/lint.sh` where possible. Read the lane's `CLAUDE.md` before treating
any of them as optional.
