# internal/egressrelay — the fetch-relay egress lane

This package is one of three sandbox egress lanes. `docs/security/sandbox-egress.md`
has the full picture; this file is the working guide for anyone adding or
changing an entry here.

| Lane | Mechanism | Lives in | Extend by |
| --- | --- | --- | --- |
| Tunnel | CONNECT + hostname allowlist, no TLS termination | `internal/egressproxy` | allowlist entry (data) |
| Fetch relay | host-side HTTP relay; redirects resolved on the host | **here** | catalog entry (data + tests) |
| Credential | per-run credential-injecting proxies | `internal/gitproxy`, `internal/llmproxy`, `internal/ghinjector` | its own design review |

## Decide the lane FIRST — probe, never assume (10 minutes)

The lane an ecosystem needs is an empirical property of how its registry
serves artifacts. Measure it against a real artifact URL:

```bash
curl -sI --max-redirs 0 <artifact URL> | grep -iE "^HTTP|^location"
```

- **200, or 3xx to a DEDICATED hostname** (`files.pythonhosted.org`,
  `release-assets.githubusercontent.com`, `static.crates.io`): **tunnel
  lane.** Add the hostname(s) to `egressproxy.DefaultRegistryHosts` — do
  NOT build a relay. Most ecosystems land here (npm, PyPI, Hackage, Maven
  Central all measured this way).
- **3xx into a SHARED multi-tenant storage namespace**
  (`storage.googleapis.com`, presigned S3, …): **this lane.** Such a
  hostname must never be allowlisted: the tunnel matches hostnames, not
  paths, so the entry would admit every tenant's bucket — a writable,
  credential-free, unattributable exfiltration channel for an agent whose
  job is reading hostile PR text.
- **The tool needs a credential injected**: credential lane. Stop here and
  read `docs/security/privilege-separation.md`.

Go is the worked example of the second case: `proxy.golang.org` serves
small zips inline but 302s large zips — and every toolchain — to
`storage.googleapis.com`. The split is size-dependent, so a small-module
smoke test passes while real builds fail; only a probe against a LARGE
artifact tells the truth.

## The integrity prerequisite (read before writing any code)

The relay is untrusted infrastructure ONLY because each ecosystem's client
verifies content itself — for Go, `go.sum` plus the checksum database,
which the relay carries but cannot forge. If the ecosystem's client does
**not** verify downloads against pinned hashes, a relay makes Triage
Factory a trusted supply-chain component — a strictly worse posture than
no support at all. Stop and open a design discussion instead of adding the
entry.

## Adding a catalog entry — contributor checklist

1. **Probe** (above). Put the raw output in your PR description.
2. **Integrity check** (above).
3. **Write the entry in `catalog.go`**: a `<Eco>() Config` route table, a
   `<Eco>Env(addr string) []string` pointing the tool at the relay, and an
   append in `Catalog()`. That is the only wiring — the per-run bring-up
   (`agentproc.startFetchRelaysForSandbox`) iterates `Catalog()` and needs
   no changes. If your ecosystem seems to need more than routes, a
   synthetic capability probe, and env pointing, that's a signal it may
   not fit this lane; discuss before coding.
4. **Tests**, mirroring the Go entry's:
   - redirect resolved host-side (the reason the lane exists),
   - a redirect to a denied target is refused with no body leaked,
   - escaped path preserved byte-identical,
   - upstream 404/410 passed through verbatim,
   - env shape: no `|` fallback separators, nothing that weakens the
     client's own verification (extend
     `TestCatalogEnv_NeverWeakensClientVerification` with your
     ecosystem's equivalents of `GOSUMDB=off`),
   - a live test behind `-tags live` against the real upstream, fetching
     an artifact LARGE enough to trip the redirect.
5. **Run** `go test ./internal/agentproc/ -run TestSandboxEnvAllowlistCovers`.
   It iterates `Catalog()`, so your env keys are checked automatically —
   when it fails, add the key to `allowedSandboxEnvKeys`
   (`internal/sandbox/launchspec_linux.go`) with a comment.
6. **Consider the agent prompt** (`internal/agentprompt`
   `blocks/guardrails/multi.txt`): if agents must not override your env
   var, say so there — stated as a mechanism, never a version, so it
   cannot drift.
7. **Add a row** to the inventory table in
   `docs/security/sandbox-egress.md`.

## Invariants — every entry, no exceptions

- Every dial goes through `egressproxy.VettedDialContext` — the initial
  request and every redirect hop. Never construct a bare `http.Client`
  here: a host-side fetcher acting on a sandbox's behalf is a confused
  deputy, and the vetted dial is the one gate between an upstream redirect
  and the cloud metadata endpoint.
- Fresh outbound request; no inbound header is ever forwarded.
- Status passthrough (404/410 verbatim); streaming, never buffering.
- GET/HEAD only. No caching (a cache is its own design, with eviction and
  poisoning questions this lane deliberately avoids). No credentials — a
  relay that needs one belongs in the credential lane.
- Bind refuses non-loopback without `AllowNonLoopback`.

## Never

- TLS termination / MITM of agent traffic, in any lane, for any reason.
- Allowlisting a shared storage namespace as the "simpler" alternative.
- Env that disables the client's own verification.
- A per-ecosystem fork of the engine — if `relay.go` can't express what
  you need as routes, the answer is a design conversation, not a copy.
