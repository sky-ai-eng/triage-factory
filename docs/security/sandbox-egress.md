# Sandbox egress lanes

How a sandboxed agent run reaches the network, why there are exactly three
mechanisms, and how to add support for a new ecosystem. This is the
contributor-facing map; the per-lane working guides live next to the code
(`internal/egressproxy/CLAUDE.md`, `internal/egressrelay/CLAUDE.md`).

A delegated run executes in a gVisor jail whose L3 egress is pinned to the
listeners of its own credential sidecar: the jail's gateway drops every
connection except to the ports the sidecar reports as jail-facing
(`StartProxiesResult.JailEndpoints`), which the cell admits before the jail
starts (`RunNetwork.AdmitJailPorts`). The only way out is through those
host-side, per-run services; anything else the executor listens on, including
the sidecar's own LLM and API proxies, is closed to the jail. A new kind of
jail-facing listener is unreachable until it is added to `JailEndpoints`
(`internal/agentproc/proxies.go`, `cmd/runsidecar/runtime.go`); a new fetch
relay is added automatically, because the relays are started from the catalog.
Beneath every lane sits the operator-owned, compiled-in internal denylist
(`internal/egressproxy/denylist.go`): cloud metadata, private ranges, the
sandbox pool, sibling gateways. No lane, entry, or profile can widen it.

## The three lanes

| Lane | Mechanism | For | Add support by |
| --- | --- | --- | --- |
| **Tunnel** | CONNECT proxy + hostname allowlist; no TLS termination | Registries serving artifacts from their own or a dedicated hostname | Allowlist entry — `egressproxy.DefaultRegistryHosts` (per-profile data under TFAC-408) |
| **Fetch relay** | Host-side HTTP relay; redirects resolved on the host, every hop denylist-vetted | Registries that redirect artifacts into shared multi-tenant storage hostnames | Catalog entry — `internal/egressrelay/catalog.go` |
| **Credential** | Per-run credential-injecting proxies (git, `gh`) | Authenticated egress | Its own design; see `privilege-separation.md` |

The API proxies behind the `tfac exec` verbs (GitHub REST, Jira REST, Linear
GraphQL) are not lanes: the jail never reaches their upstreams, because a verb
rides the agenthost socket to the run's sidecar, whose daemon calls the proxy
that sidecar owns. Supporting a new verb-backed provider such as Linear adds no
allowlist entry.

Two constraints shape the whole design. The tunnel does not terminate TLS
today, so it can act only on hostnames — content integrity belongs to the package
managers' own verification (lockfile hashes, go.sum), never to us. And a
shared storage namespace (`storage.googleapis.com`, presigned S3) can
never be allowlisted: a hostname match admits every tenant's bucket, which
hands an agent that reads attacker-authored PR text a writable,
credential-free exfiltration channel. The fetch relay exists for exactly
the registries those two constraints would otherwise strand.

Acting only on hostnames leaves the tunnel two gaps it cannot close. It
cannot tell a read from a write on an allowlisted host, so `npm publish` to
`registry.npmjs.org` gets through. And it cannot see which name a client
asks a shared CDN edge for once the tunnel is open, so an allowlisted name
can front for another site on the same edge. TLS interception in the
sidecar closes both, under conditions designed in
`docs/for-agents/specs/sandbox-backends/` §6.1 (TFAC-1117).

## Which lane does an ecosystem need? Probe — never assume

```bash
curl -sI --max-redirs 0 <real artifact URL> | grep -iE "^HTTP|^location"
```

Measured behavior of the majors (2026-08):

| Ecosystem | Serving behavior | Lane |
| --- | --- | --- |
| npm | `registry.npmjs.org`, direct 200 | Tunnel (shipped) |
| PyPI | 302 → `files.pythonhosted.org` (dedicated) | Tunnel (shipped) |
| Go modules + toolchains | 302 → `storage.googleapis.com` for large zips; small zips inline | **Fetch relay (shipped: `go-modules`)** |
| Hackage | direct 200 | Tunnel, when a rootfs variant ships GHC |
| Maven Central | direct 200 | Tunnel, when a rootfs variant ships a JDK |
| GitHub release assets | 302 → `release-assets.githubusercontent.com` (dedicated) | Tunnel, if ever needed |
| crates.io, Hugging Face | unverified — probe before deciding | TBD (HF's presigned-storage redirects likely mean relay) |

Two rules ride along with the table. A tunnel entry also requires an
**in-sandbox consumer** — a toolchain in the rootfs that actually dials it
(so Hackage/Maven entries land together with their rootfs variants, not
before). And probe with a **large** artifact: the Go proxy's inline/redirect
split is size-dependent, so a small-file smoke test passes while real
builds fail — that false pass is how the gap shipped in the first place.

## The fetch relay in one paragraph

One generic engine (`internal/egressrelay/relay.go`): an ordered route
table mapping path prefixes to upstreams, fresh outbound requests (no
inbound header ever forwarded), redirects followed host-side with every
hop dialed through the shared denylist gate
(`egressproxy.VettedDialContext`), statuses passed through verbatim,
bodies streamed. Ecosystem knowledge is catalog data
(`catalog.go`): the Go entry is four routes (a synthesized sumdb
capability probe, the sumdb relay, a fence, the module-protocol
catch-all) plus one env entry (`GOPROXY=http://<relay>,direct`). The
relay holds no credentials and no cache, and it is safe as *untrusted*
infrastructure only because each relayed ecosystem's client verifies
content itself — an ecosystem whose client doesn't verify downloads needs
a design discussion, not a catalog entry.

TFAC-408's sandbox profiles are expected to serialize both tunnel entries
and relay catalog entries as per-profile egress rules; keeping them as
plain data is what makes that a serialization exercise rather than a
rewrite.

## Adding support — where to start

1. Probe the ecosystem (command above); paste the output in your PR.
2. Tunnel case → `internal/egressproxy/CLAUDE.md`.
3. Relay case → `internal/egressrelay/CLAUDE.md` (checklist: catalog
   entry, tests including the SSRF-refusal case and a `-tags live` fetch
   of a redirect-tripping artifact, broker env allowlist, docs row here).
4. Credentialed case → `privilege-separation.md`, and expect a security
   review rather than a checklist.

Any change in this area is security-sensitive. The invariants (vetted dial
on every hop, no header forwarding, no shared-storage allowlisting, no TLS
termination, no verification-weakening env) are enforced by tests where
possible — read the lane CLAUDE.md before assuming one is optional.
