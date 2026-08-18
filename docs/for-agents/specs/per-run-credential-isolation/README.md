# Per-run credential + parser isolation

Design of record for closing the last cross-tenant residual on a shared
executor: **no process holds more than one tenant's live credentials, and
each hostile-input parser holds only its own run's material.** Tracked as
**TFAC-620**. The tenancy analog of the privilege-separation epic
(**TFAC-600**): where privsep took *capabilities* out of the
credential-holding process, this takes *other tenants' credentials* out of it.

Status: **accepted design.** Builds on the sealed per-run credential bundles
(**TFAC-614**) — this epic changes *where* a bundle is unsealed and *who* holds
the plaintext, not the bundle format.

Scope note: multi-mode executors only. Local mode (Tier 4, one user, SQLite)
is single-tenant by construction — there is no co-located tenant to isolate
from — and every mechanism below is a no-op there. Local behavior does not
change.

---

## 0. The finding

Privilege separation (TFAC-600) split the executor into a **cap-broker**
(holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`, parses no hostile input, holds no
credentials) and an **orchestrator** (zero capabilities, holds credentials,
parses hostile input). That contains a *capability* escalation. It does **not**
contain a *cross-tenant* one.

On a shared executor the orchestrator runs, in **one address space**, for every
concurrent run at once:

- the per-run LLM / git / egress proxies — in-process `http.Server` goroutines
  (`internal/agentproc/proxies.go`), each holding **that run's real
  credentials** server-side;
- the per-run agenthost — an in-process unix-socket server
  (`cmd/exec/agenthost`) holding the full `db.Stores` handle (all orgs) and a
  GitHub/Jira credential resolver;
- the single **per-instance** X25519 unseal key (`Spawner.sealingKey`, minted
  once per boot) that opens **every** run's sealed bundle
  (`internal/delegate/awaiting_credentials.go`).

So an agent that achieves **code execution in the orchestrator** — via a
memory- or logic-safety bug in a proxy handler, the agenthost protocol, a
host-side `exec` verb, Go `net/http`, or any reachable dependency — reads
**all** co-located tenants' live credentials off one heap, and holds the one key
that decrypts every bundle. Goroutines share memory; there is no intra-process
boundary between one run's proxy and another's. Today the only thing preventing
this is **Go memory-safety + narrow protocols** — i.e. it is mitigated
*probabilistically*, not *structurally*.

## 1. Goal

Apply the power-vs-exposure separation to **tenancy**: no process holds more
than one tenant's live credentials, and each hostile-input parser holds only
its own run's material. A parser compromise then reaches one run's credentials
— which belong to the tenant that already owns them — never the tenant set.
Cross-tenant isolation becomes an **OS process boundary**, not a memory-safety
assumption.

## 2. What already prevents the non-RCE version (out of scope)

The non-RCE cross-tenant path — "route to another tenant's proxy and spend its
key" — is already closed three independent ways and is **not** what this epic
addresses:

- **Property B** — no credential ever enters the sandbox environment; the agent
  holds only a per-run placeholder token (`internal/sandbox`, `proxies.go`).
- **Fail-closed per-run egress isolation** — an agent can reach only its own
  gateway/proxies; a host-side veth ingress `DROP` backstops gVisor's netstack
  (`internal/sandbox/iptables_linux.go` `applyEgressPolicy`).
- **Per-run proxy token** — a sibling can't spend another run's key even if it
  reached the proxy (`proxies.go`).

This epic is **only** the RCE case: code execution *inside* the
credential-holding process.

## 3. Architecture: the per-run credential sidecar

For each run, a **capless per-run sidecar process** becomes the sole holder of
that one run's credentials.

- **Broker-spawned at a per-run uid.** The orchestrator is capless and cannot
  `setuid` a child; the cap-broker (root) already launches one per-run process
  today (the runsc supervisor) via its `runEntry` / `LaunchSupervised` /
  `WaitRun` / `KillRun` machinery (`cmd/capbroker/runlaunch_linux.go`,
  `internal/sandbox/launch_linux.go`). The sidecar reuses that path. Its uid is
  derived from the run's subnet index (the allocator already hands out a unique
  index per run, `internal/sandbox/subnet.go`), in a range distinct from the
  agent (10000) and orchestrator (10001) uids — so two sidecars cannot
  `ptrace`/read each other even beyond the address-space split.
- **It generates its own X25519 keypair** at startup and publishes only the
  public half; the brain seals *this run's* bundle to *this run's* pubkey. The
  private key is born in the sidecar and never exists in the orchestrator.
- **It holds the only copy of this run's decrypted credentials** and runs this
  run's proxies (LLM/git/egress) + agenthost. Binding the veth IP is capless
  once the broker has configured it; the agenthost socket grant is a capless
  `chgrp`. So the sidecar needs **zero capabilities**.
- **It reports back only non-secrets** — the proxy URLs and the per-run
  *placeholder* tokens (throwaway, not credentials) — for the orchestrator to
  stamp into the sandbox's OCI environment. Real keys never leave the sidecar.
- **Process exit is the eviction.** Today credentials linger in the
  orchestrator heap until GC after a reference drop, with no zeroing. A per-run
  process that dies at run-end frees its whole address space — a strictly
  stronger, claimable eviction guarantee.

**Result:** the orchestrator holds zero decrypted credentials and zero unseal
authority. An orchestrator RCE reaches no tenant's credentials. A sidecar RCE
reaches exactly one run's credentials. Cross-tenant reach is an OS boundary.

Footprint: one extra capless process per run — the same `triagefactory` binary
under a new subcommand (no new artifact; read-only code pages are shared
copy-on-write across all sidecars), so the marginal cost is mostly private heap
(order ~10 MB). The proxy/agenthost working memory *relocates* rather than
duplicates. The hard concurrency ceiling stays 256 (the subnet allocator).
Phase 4 benchmarks the real figure.

## 4. Settled decisions

1. **Per-run *process* + per-run uid, not a jail.** The sidecar is our
   memory-safe Go parsing narrow protocols — not the agent's arbitrary code
   (that is already jailed). A separate address space + distinct uid + no
   capabilities gives cross-tenant isolation; a second gVisor jail adds a runsc
   per run for no threat it closes.
2. **Per-run, not per-org.** The goal ("no process holds more than one tenant's
   creds") would also be met by one sidecar per co-located org. But every
   existing seam is per-run (subnet index, veth, agenthost socket, cleanup LIFO,
   the broker `runEntry`), so per-run maps 1:1 with no multiplexing, is strictly
   stronger, and costs only per-process heap. Per-org stays the lever if
   footprint ever binds.
3. **The keypair is born in the sidecar**, not minted by the orchestrator and
   passed down — that is what removes unseal authority from the orchestrator.
   Requires the `cred_request` / awaiting-credentials path to carry the per-run
   pubkey up to the brain before it seals.
4. **Reuse the broker's per-run supervision** (`runEntry`, `LaunchSupervised`,
   `WaitRun`/`KillRun`) rather than a new lifecycle. Orphan reap keys on the
   same `tf-<id>-<idx>` discipline as netns/subnet reaping.

## 5. What this does NOT close

Per-run isolation removes the cross-tenant *credential* reach via an
orchestrator / proxy / agenthost compromise. It does not remove the shared
**kernel/host**: a gVisor/runsc escape or a kernel exploit from the sandbox
still crosses runs. That residual is bounded by gVisor + the tailored seccomp
profile, and **dedicated executors** remain the escape hatch for tenants who
will not accept a shared kernel. This epic closes the credential-address-space
residual; it does not make a shared executor equivalent to a dedicated one.

## 6. Composition with the rest of the credential track

- **TFAC-614 (sealed bundles)** is the substrate. This epic changes the
  recipient key (per-instance → per-run) and the unseal location
  (orchestrator → sidecar); the bundle format and the delivery table
  (`claim_credentials`) are unchanged.
- **TFAC-609 (control-plane App-token minting)** and **TFAC-616 (Bedrock STS
  session creds)** govern what goes *into* the bundle. They are orthogonal to
  where it is unsealed — the sidecar consumes whatever the bundle carries.

---

## 7. Rollout

Each phase ships green. Because the whole credential-hardening track ships as
one release, no intermediate release exposes a partial state — phase ordering
is a PR-sequencing convenience, not a release boundary.

**Phase 0 — Agenthost credential path → bundle-aware.** Standalone correctness
fix on the current in-process architecture; depends on nothing (the TFAC-614
bundle already ships). The agenthost's `exec gh` / `exec jira` verbs resolve
credentials through `ghclient.NewResolver(stores.Secrets, …)` /
`jiraclient` — which read the App PEM / PAT / Jira token from `stores.Secrets`,
**disabled on executors** (`pgstore.NewWithoutSecrets`). So those verbs fail on
an executor today (the git *proxy* is already bundle-first; the agenthost verbs
were never converted, and the daemon's dispatch ctx is `context.Background()`,
so the bundle never crosses in). Fix: make the agenthost GitHub/Jira verbs read
from `credbundle.GitHubCreds.RepoTokens` / `credbundle.JiraCreds`. This
unblocks multi-mode PR-opening and makes Phase 3's relocation a pure
process-move. **Release-blocking for multi-mode independent of the rest of the
epic.** (S)

**Phase 1 — Per-run sidecar harness.** The broker spawns a capless per-run
process at a per-run uid, supervised through the existing `runEntry` machinery,
with lifecycle hooks at the `StartAgentHost` / `ConfigureProxies` seam
(`internal/agentproc/run.go`), teardown in the cleanup LIFO, and orphan reap in
`sandbox.ReapOrphans`. The process is an inert skeleton in this phase — no
behavior change — proving the hard infra (spawn, uid, reachability plumbing,
reap-on-every-exit-path) in isolation. (M)

**Phase 2 — Per-run keys + proxy relocation.** The sidecar mints its X25519
keypair and publishes the per-run pubkey; the brain
(`internal/credprovision`) seals to the per-run pubkey instead of
`instances.pubkey`; the sidecar unseals its own bundle and runs the LLM/git/
egress proxies, binding the veth IP and reporting URLs + placeholder tokens
back for OCI env injection. The orchestrator stops unsealing and stops holding
LLM/git credentials. Closes the orchestrator-RCE credential reach for the
proxy surface. (L)

**Phase 3 — Agenthost relocation + DB-scope narrowing.** Move the (now
bundle-fed, Phase 0) agenthost into the sidecar and replace its full `db.Stores`
handle with an **org-scoped** handle pinned to `info.OrgID` covering only the
verbs' real reach — reads: `runs`, `tasks`, `repo_profiles`,
`team_github_repos`, `run_worktrees`, `org_settings`, `artifacts`; writes:
`run_worktrees`, `artifacts`, `external_actions`, `entities` (plus the
`SyntheticClaimsWithTx` app-pool runner for manual-run writes). FS access is the
run's own root only. **`CallExtension`** is the exception: it dispatches into a
process-global EE registry whose handlers close over all-orgs `db.Stores` and
can reach the network, so it must be re-scoped (handlers take a per-run
org-scoped handle) or excluded from the sidecar and routed back — resolve in
this phase's design. (L)

**Phase 4 — Hardening, eviction proof, negative tests, docs.** Prove
process-exit credential eviction; add cross-sidecar negative tests (sidecar A
cannot read B's bundle row, unix socket, or process memory); benchmark the
per-run footprint; reconcile the operator/security docs. (M)

*Exit criteria: on a shared executor, an orchestrator compromise reaches zero
tenants' decrypted credentials and cannot unseal any bundle; a per-run sidecar
holds exactly one run's credentials and one org's scoped DB reach; killing a run
frees its credential address space; the `exec gh`/`exec jira` verbs work on
executors.*

---

## 8. Decision log

1. **Process vs jail** — process + per-run uid (§4.1). Reopens only if the
   sidecar ever runs untrusted code (it does not; it parses narrow protocols).
2. **Per-run vs per-org sidecar** — per-run (§4.2). Reopens on measured
   footprint pressure at high density; per-org is the pre-analyzed fallback.
3. **Unseal authority** — per-run keypair born in the sidecar (§4.3),
   superseding TFAC-614's per-instance sealing key for run credentials. The
   sealed-box primitive (`internal/credseal`) already accepts a per-run
   recipient; only the key-management plumbing changes.
4. **Phase 0 is a standalone bug fix** — the agenthost bundle-conversion lands
   independent of the sidecar because it is release-blocking for multi-mode
   PR-opening on its own.

## Related

- `docs/for-agents/specs/horizontal-scaling/` — the control/executor split this
  runs on; §4 (the executor fleet) is where sidecars live.
- `docs/security/security-overview.md` — the CISO-facing posture; §5 (credential
  handling) states the closed residual.
- `docs/security/privilege-separation.md` — the capability split this extends
  along the tenant axis.
- `docs/security/isolation-tiers.md` — dedicated executors, the escape hatch for
  the shared-kernel residual §5 does not close.
