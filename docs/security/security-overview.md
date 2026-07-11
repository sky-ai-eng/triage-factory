# Security overview

Triage Factory runs code authored by an AI agent that is, by construction,
acting on untrusted input (repository contents, issue text, tool output —
any of which can carry a prompt injection). Isolating that code is the
product's central security problem. This document states, precisely, what
privileges Triage Factory requires from its host, *why* each is required,
and what a compromise of each component would yield.

This is written for a reader evaluating whether to run Triage Factory in their
infrastructure. For how the shared-vs-dedicated-vs-self-host choice maps to your
trust requirement, see [isolation-tiers.md](isolation-tiers.md).

---

## 1. The core principle: the privilege belongs to the warden, not the prisoner

The instinct on reading "Triage Factory needs `CAP_SYS_ADMIN`" is that the
untrusted code runs with elevated privilege. However, the agent is the
**most confined** process in the system: it runs inside a gVisor sandbox, as a
non-root uid, with an empty capability set, a restricted seccomp policy, no
credentials in its environment, and a fail-closed egress firewall.

The capabilities exist so that Triage Factory can **construct that sandbox** —
create the network namespace, wire the virtual ethernet pair, write the
firewall rules, set the memory cgroup, and launch the gVisor runtime. This
is a standard sandboxing paradox, shared by GitLab runners, BuildKit,
Firecracker hosts, and Google's own GKE Sandbox (which also runs gVisor).

---

## 2. Threat model

Triage Factory's isolation model addresses four threats. For a security review,
these compose into two relevant adversaries:

**Adversary A — the agent** (a prompt-injected or otherwise-hostile agent
process inside the sandbox):

| | Threat | Defense |
| --- | --- | --- |
| **T1** | Credential exfiltration | Property B (§5): no credential ever enters the sandbox environment. |
| **T2** | In-run credential misuse | Strongly bounded: no credential is raw in the sandbox, and each is reachable only through a constrained interface (GitHub token injected only on git fetch/push to the authorized repo; Jira/GitHub API are fixed host-side `exec` verbs; LLM key is provider-only via the proxy). Residual: within its granted scope the agent still acts with the run's authority (push to the authorized repo, spend the org's LLM tokens), bounded by run wall-clock. |
| **T3** | RCE in the agent SDK escaping the SDK process | gVisor + in-sandbox hardening (non-root uid, empty caps, seccomp, no-new-privs). |
| **T4** | RCE escaping gVisor to the host kernel | gVisor's user-mode-kernel architecture. |

**Adversary B — Triage Factory itself, as third-party code.** A company with a
"no privileged third-party code" posture is not (only) worried about the agent;
they are worried that *our* binary holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`, and
that a bug in it — or a compromise of our supply chain — becomes their problem.
This is a separate threat, and §4 and §6 are the answer to it.

The two connect at one point: the agent's traffic is among the inputs our own
parsers (the proxies, the agent-host socket) handle, so a parser bug there is
where Adversary A could reach Adversary B — which is exactly why that parsing
runs on the unprivileged orchestrator, never the capability-holder (§4).

Local mode (single user, single machine, SQLite) collapses adversary A's
multi-tenant threats: it is single-tenant, and **the sandbox is skipped
entirely** (`shouldSandbox()` = `runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"`), so it takes none of
the host privileges below. Everything in this document about capabilities and
privilege separation concerns the multi-mode (self-host and SaaS) deployments.

---

## 3. What Triage Factory requires from the host

An executor (the process that runs sandboxes) needs a
**privileged-*capable*** container substrate or a plain VM/bare-metal host with
root.

We never ask for a blanket `--privileged`:

| Requirement | Why |
| --- | --- |
| `CAP_NET_ADMIN` | Per-run network namespace, veth pair, NAT (MASQUERADE), and the fail-closed egress allowlist (`internal/sandbox/netns_linux.go`, `iptables_linux.go`). |
| `CAP_SYS_ADMIN` | Namespace/mount setup for the gVisor OCI container and the netns; memory-cgroup setup. |
| `seccomp=unconfined` | Docker's default syscall filter blocks operations gVisor needs to build the far stricter sandbox, so we use a tailored profile (§6). |
| Private cgroup-v2 namespace | The per-run memory ceiling. |

What Triage Factory doesn't require:

- **not `--privileged`** — only the two capabilities above;
- **no host devices** (`/dev` is not exposed);
- **no host network namespace** (each run gets its own);
- **no host PID namespace**;
- **no host filesystem mounts** (the sandbox sees only its worktree);
- **no KVM / nested virtualization** — gVisor runs on the systrap platform.

The capability-holding component never listens on the network. The cap-broker's
only inputs are a local unix-socket RPC and the run I/O it passes through. It
has no routable port, no request parser, and no authentication surface. In the
all-in-one process (`TF_ROLE='all'`) the HTTP API is served by the unprivileged
orchestrator, not the cap-broker; in a fleet, executor-role pods take no inbound
traffic at all. The hostile-input surface is only things the process itself
reached out to or spawned.

---

## 4. Privilege separation: the trust-domain decomposition

There are two independent ways the system is decomposed:

- **Control vs. executor** is a *horizontal* split across machines — which host
  does which job (control serves the API, polls, and routes; executors run the
  sandboxes). It is about scale, and is specified in
  `docs/for-agents/specs/horizontal-scaling/`.
- **cap-broker vs. orchestrator vs. sandbox** is a *vertical* split within a
  single executor host — who is allowed to hold which dangerous thing. It is
  about blast radius, and is what this section is all about.

The governing rule of the vertical split is: **no component holds both a
dangerous power and exposure to the attacker.** For each component, the *power*
it holds and the *hostile input* it is exposed to never overlap:

| Component | Holds capabilities? | Holds credentials? | Parses hostile input? |
| --- | --- | --- | --- |
| **cap-broker** | **Yes** (the only holder) | No | **No** |
| **orchestrator** | No (dropped at exec) | Yes | Yes |
| **sandbox** (agent) | No | No | Yes |

- **cap-broker** — the only process that holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`. It
  builds the netns/veth/iptables/cgroup, launches and supervises the gVisor
  runtime, tears everything down, and reaps orphans at boot. It holds **no
  credentials**, binds **no proxies**, and **parses no agent output**. Its only
  input is a narrow, fixed RPC vocabulary from its parent plus, for the run's
  I/O, a socket file descriptor it passes straight through to the runtime and
  never reads — the run's bytes never enter the broker's address space (§4.2).
- **orchestrator** — today's main process, with its capabilities **dropped at
  exec** (not in-process — see the note below). It runs the dispatcher, resolves
  and holds credentials, binds the LLM/git/egress proxies (which parse hostile
  agent traffic), serves the agent-host socket, and drives the run. It holds
  every credential and **zero capabilities**.
- **sandbox** — the gVisor jail running the agent. No capabilities, no
  credentials (Property B).

### 4.1 Why this bounds a compromise

A vulnerability compromises a **running process** — its memory and its
kernel-granted privileges. Capabilities are per-process and enforced by the
kernel at syscall time.

- The orchestrator is the realistic target — it is the one parsing
  hostile input. An attacker would obtain only the credentials it holds, and
  nothing else. It dropped capabilities already, and the kernel denies the
  syscalls regardless of what code the attacker jumps to.
- There is no attacker-reachable path to the cap-broker. It parses no network,
  agent, or repository data; reaching it requires first owning the orchestrator
  and then finding a flaw in the narrow RPC between them.

The only component holding root-equivalent power is a few hundred lines that
construct sandboxes and never touch data from the network, an agent, or a
repository.

### 4.2 The runtime is held

The gVisor runtime (`runsc run`) is a blocking child that lives for the whole
run and must hold `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` for its duration (it joins the netns
and performs its own mounts). So the cap-broker supervises it rather than
"handing it off." This adds no exposure: a gVisor escape (T4) already means host
code execution regardless of which process is the runtime's parent. Crucially,
the run's I/O bytes **never enter the broker's address space** — the broker
passes the socket file descriptor to the runtime and closes its own copy, so
the kernel wires the runtime's stdio directly to the (unprivileged) orchestrator.
This is a structural property verifiable by inspection (no read call, descriptor
closed), not a behavioral promise. *(Validated against real gVisor: the runtime
faithfully proxies the newline-delimited-JSON agent protocol over a
socket-backed stdio while the supervising process reads nothing.)*

### 4.3 The broker's real attack surface is its RPC, and it is narrowed on purpose

The broker's exposure isn't stdio; it is the RPC from the orchestrator — the
process that *is* exposed to hostile input. A compromised orchestrator's
move is to abuse that RPC to regain capabilities. Therefore the broker's
protective value equals the narrowness and validation of that RPC.

> The broker **owns the OCI spec** from a fixed template (rootfs = the
> content-addressed rootfs it resolved, capabilities = empty, uid = 10000,
> seccomp, namespaces) and accepts over RPC only narrow, validated parameters —
> a run id, the netns/cgroup it created, an environment **allowlist**, numeric
> resource limits, and (self-host only) an additional permitted egress CIDR
> **validated against the immutable internal denylist** (cloud metadata endpoint
> `169.254.169.254`, the control-plane subnet, private/link-local ranges — see
> sandbox-fleet §3.1) before any iptables permit is written. It **never** execs
> an orchestrator-supplied `config.json`, command, or rootfs path.

A compromised orchestrator can inject an environment variable the *sandboxed*
(unprivileged) agent will see — harmless — but it can never make the broker run
arbitrary code with capabilities.

**Why this stays true as the code changes** — the narrowness is structural, not
a property someone must remember to preserve per change:

- **One unconditional choke point.** The broker is the only launch path, and
  every launch passes `ValidateLaunchParams` — every privileged run-tree
  chown/remove passes `validateRunTreeRoot` — before the broker builds a spec,
  execs anything, or touches a file. There is no second entry that skips the gate.
- **Allowlist, not denylist, so new inputs fail closed.** Rootfs is chosen by
  catalog *name* (unknown → rejected); the SDK path is resolved by the broker
  (the orchestrator's is discarded); the command's first two argv are pinned; env
  keys are an allowlist; mount options are the closed set `{ro, rw}`; rlimit types
  are allowlisted; the run-tree ops touch only trees owned by the sandbox or
  orchestrator uid, **never** root. A *new* dangerous value a future change might
  introduce is refused by default rather than accepted until someone blocks it.
- **The gate is fuzzed for "accepted ⇒ safe."** `internal/sandbox`'s
  `FuzzValidateLaunchParams` (plus the run-tree and egress fuzzers) feeds
  arbitrary RPC input to the validator and, for every input it *accepts*,
  independently asserts the invariants the broker then trusts — catalog rootfs,
  pinned entrypoint, allowlisted env/mounts, a run-bound netns, a non-denylisted
  egress CIDR. A change that widens an allowlist or drops a check surfaces as a
  failing test, not a shipped hole.

**The residuals are named, and none is a capability escalation.** Two are
documented in the validators themselves: the broker shape-checks a mount *source*
but does not yet pin it to the run's own paths, and it binds a run's netns by a
name derived from the run id rather than from broker-tracked state. Each lets a
*fully* compromised orchestrator reach a sibling run's data at the fixed sandbox
uid — a confidentiality/integrity residual bounded by host file permissions,
never a capability regain — and each is moot in practice, because an orchestrator
compromised enough to exploit it already holds every host-side credential those
paths would expose. Closing them is the broker-internal per-run-state evolution
the launch parameters already anticipate.

### 4.4 A note on dropping capabilities

Capabilities are dropped **at exec** (via a `setpriv`/`capsh`-style wrapper that
launches the orchestrator with an emptied capability set), not in-process.
Linux capabilities are per-thread, and the Go runtime spawns threads before
`main()` runs, so an in-process drop affects only one thread and silently leaves
others privileged — a control that *looks* applied but is not. The exec-time
drop is kernel-enforced from the first instruction. After an exec-time drop of
`CAP_NET_ADMIN`, a privileged network operation returns `EPERM`.

---

## 5. Credential handling

The credentials an executor holds — real LLM provider keys, GitHub tokens, the
database connection, object-store credentials — are held because the executor
needs them to do its job. Their safety rests on four things:

- **Property B**: no real credential ever enters the
  sandbox environment. The agent receives only a per-run proxy URL and a
  fresh-per-run token that authenticates it to *its own* proxy; the real key
  lives in the proxy on the host and is injected on the upstream hop. A
  jailbroken agent reading its own `/proc/self/environ` finds a throwaway token,
  not a credential.
- **Short-lived, narrowly-scoped GitHub auth**: git authentication uses GitHub
  **App installation tokens** — one-hour lifetime, scoped to a single
  installation — not a user PAT. These tokens are minted on the **control
  plane**; the executor holds only the hour-lived scoped token, never the App
  private key. A leaked token is minutes of single-org access.
- **Short-lived, scoped LLM credentials**: for the Bedrock route, an executor
  receives **control-plane-minted STS session credentials** — action-scoped to model
  invocation, expiring (≤1h), and optionally network-bound to the executor's
  egress — so the long-lived provider root never reaches an executor. The
  Anthropic-API route is bring-your-own-key: the key at risk is the customer's
  own, under their rotation control, hardened as a spend-capped, expiring,
  revocable credential.
- **Constrained interfaces**: each credential is usable only for its intended
  operation — the git proxy injects the token only on git fetch/push to
  authorized repos (any other path 403s, no CONNECT tunnel), and Jira/GitHub-API
  calls are fixed host-side `exec` verbs. The git or Jira token can't be
  repurposed for arbitrary requests.

A compromise of the proxy would still yield the credentials the proxy
holds. Any process that holds a key and serves untrusted callers can leak that
key if the process itself is compromised. What privilege separation guarantees
is that this compromise yields no capabilities: the proxy runs on the
orchestrator (unprivileged) side.

The *reach* of that compromise is deliberately bounded. An executor does not
load a secret-bag encryption key or resolve credentials from a shared store: it
receives only **sealed, per-run credential bundles** carrying the short-lived,
scoped material a run legitimately needs (control-plane-minted GitHub App
installation tokens, Bedrock STS session credentials). So a compromise of the
orchestrator or a proxy yields only the credentials for the runs currently
placed on that box — not an App private key, not the encryption key, not other
tenants' stored secrets. The remaining long-lived credential is the raw
Anthropic API key for orgs on that route — the customer's own, spend-capped,
expiring, and revocable.

---

## 6. Risk register

Each entry is a way an attacker reaches the privileged process, and how it is
mitigated.

**Vector 1 — our supply chain.** If the build pipeline or a dependency is
compromised, the executor is compromised. Irreducible for any privileged vendor
software; the defense is provenance.
- The Alpine rootfs tarball is sha256-pinned per arch and extracted
  as inert data; its code only ever runs *inside* sandboxes.
- The agent SDK's npm tree is pinned by an embedded lockfile with
  integrity hashes; `npm ci` refuses drift.
- `npm ci` runs with `--ignore-scripts`, so lifecycle scripts of
  the (pinned) tree never execute at install time — the most-abused
  supply-chain channel is closed.
- Releases are signed keylessly (cosign + GitHub OIDC) with a per-artifact SBOM
  and SLSA build-provenance attestation — see
  [verifying-releases.md](verifying-releases.md).

**Vector 2 — hostile input parsed by the privileged process.** The path that
skips the gVisor escape.
- The proxies and agent-host socket are memory-safe Go over narrow
  protocols; each sandbox authenticates to its own proxy with a per-run token.
- Executors accept **no inbound traffic** — the entire hostile-input
  surface is outbound-initiated.
- Privilege separation (§4): every hostile-input parser —
  proxies, agent-host socket, git, archive extraction — runs on the
  **unprivileged** orchestrator, so a parser flaw yields no capabilities.
- A **tailored seccomp profile** (`docker/seccomp-profile.json`)
  replaces `seccomp=unconfined`, turning the scariest line in the deployment
  manifest into an auditable allowlist.

**Vector 3 — the resident credentials.** (See §5.)
- Property B; App installation tokens (1h, single-installation); BYOK.
- App-token minting runs on the control plane — executors never hold the App
  private key, only the hour-lived scoped token.
- Executors receive only sealed, per-run credential bundles; the secret-bag
  encryption key never loads on an executor, so a compromise reaches only the
  runs placed there, never the whole tenant secret set.
- LLM credentials are short-lived where the provider allows it: Bedrock uses
  control-plane-minted STS session credentials (action-scoped, optionally network-bound
  to the executor's egress); the Anthropic-API route stays bring-your-own-key,
  hardened with spend caps, key expiry, and programmatic revocation.
- A least-privilege `tf_system` database role for executors (no superuser DSN on
  the most-exposed machine class).

**Vector 4 — a compromised orchestrator abusing the broker RPC (resource
exhaustion, not a capability leak).** Even fully deprivileged, a compromised
orchestrator can call the broker's RPC as fast as it likes, and the broker
faithfully executes well-formed requests. It cannot regain capabilities (§4) —
but it can consume host resources: most concretely the fixed **256-slot**
per-run subnet pool (`internal/sandbox/subnet.go`, a `/16`→`/24` allocator), the
privileged netns/veth/iptables/cgroup setup and supervised runtime each
`LaunchRun` costs.
- The broker caps in-flight `LaunchRun`s per orchestrator instance
  (one orchestrator maps to one broker) at the subnet-pool size, releasing the
  slot when the run is reaped, so a runaway caller degrades to queueing rather
  than host exhaustion. This is denial-of-service resistance, **not** a
  capability boundary — called out so the two are not conflated.

---

## 7. Deployment guidance for security teams

The capability requirements do **not** force a privileged workload into a
Kubernetes cluster. The recommended topology keeps the privileged part out of
the cluster entirely:

- **Executors as dedicated VMs (or bare metal), outside the cluster.** They
  accept no inbound traffic and dial out to Postgres to claim work — exactly the
  pattern enterprise teams already run for GitHub Actions / GitLab / Buildkite
  runners. Root on a VM that was provisioned for this one purpose has a blast
  radius of that one machine.
- **Control plane as ordinary web-service pods.** The API/websocket/polling tier
  is a normal web service. It carries the sandbox capabilities only to jail its
  own low-volume system jobs (e.g. curator chat sessions); delegated agent runs
  land on executors, not here.
- **If executors must run in Kubernetes**, they run as privileged pods on a
  dedicated, tainted node pool with the cloud metadata endpoint blocked, a
  minimal node role, and no other tenants' pods scheduled there. This is the
  fallback, not the lead: a privileged pod is a standing admission-policy
  exception, and root on a k8s node reaches node credentials, the cluster API,
  the node's cloud identity, and the pod network — which is why "privileged on a
  VM" and "privileged on a shared k8s node" are genuinely different risks.

What Triage Factory deliberately does **not** do is run each sandbox as a pod
(`runtimeClass: gvisor`). A run is not a pod: one executor hosts ~a hundred
sandboxes sharing a warm rootfs page-cache, a subnet pool, and per-run proxies;
run-per-pod forfeits all three and trades a ~200 ms sandbox spawn for
pod-startup seconds. gVisor is *permitted* as an executor substrate where a team
already runs self-managed k8s; it is never *required*, and never the design
basis.

---

## Related

- `docs/for-agents/specs/horizontal-scaling/` — the control/executor split; §2.1 on the
  k8s posture, §6.4 on the sandbox-fleet interplay.
- `docs/for-agents/specs/sandbox-fleet/` — customizable sandboxes; §4's rootfs
  build is subject to the §4.3 broker rule here.
- `docs/security/isolation-tiers.md` — the tier ladder deployments slot into.
- `docs/for-agents/specs/sky-254-runsc-validation/` — the validated egress/proxy
  mechanics, and Property A (the agent cannot read its own env/memory/FDs).
