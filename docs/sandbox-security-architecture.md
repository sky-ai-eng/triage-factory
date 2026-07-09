# Sandbox security architecture

Triage Factory runs code authored by an AI agent that is, by construction,
acting on untrusted input (repository contents, issue text, tool output —
any of which can carry a prompt injection). Isolating that code is the
product's central security problem. This document states, precisely, what
privileges Triage Factory requires from its host, *why* each is required,
what a compromise of each component would yield, and where the boundaries
are being tightened.

It is written for a reader evaluating whether to run Triage Factory in their
infrastructure — including a security team that has to sign off on it.

> **Status.** This document describes the **target** architecture and is
> explicit throughout about what is shipped today versus in progress. The
> privilege-separation design in §4 is an accepted design under active
> implementation (the "privsep" epic); §8 is the honest current-state table.
> Do not treat unshipped items as shipped in a customer conversation.

---

## 1. The core principle: the privilege belongs to the warden, not the prisoner

The instinct on reading "Triage Factory needs `CAP_SYS_ADMIN`" is that the
untrusted code runs with elevated privilege. The opposite is true. The agent —
the untrusted party — is the **most confined** process in the system: it runs
inside a gVisor sandbox, as a non-root uid, with an empty capability set, a
restricted seccomp policy, no credentials in its environment, and a
fail-closed egress firewall.

The capabilities exist so that Triage Factory can **construct that cell** —
create the network namespace, wire the virtual ethernet pair, write the
firewall rules, set the memory cgroup, and launch the gVisor runtime. Building
a jail requires privileged operations; running the prisoner inside it does not.

This is the standard sandboxing paradox, shared by GitLab runners, BuildKit,
Firecracker hosts, and Google's own GKE Sandbox (which also runs gVisor). The
attack chain a security review should actually weigh is: compromise the agent →
**escape gVisor** (a user-mode-kernel escape — the same bet Google makes for
GKE Sandbox and App Engine) → *only then* hold the capabilities of the
Triage Factory process. gVisor is the load-bearing boundary, and it is designed
to be the hard one.

---

## 2. Threat model

Triage Factory's sandbox package (`internal/sandbox/doc.go`) enumerates four
threats. The critical framing for a security review is that there are **two
distinct adversaries**, and the architecture must answer both:

**Adversary A — the agent** (a prompt-injected or otherwise-hostile agent
process inside the sandbox):

| | Threat | Defense |
| --- | --- | --- |
| **T1** | Credential exfiltration | Property B (§5): no credential ever enters the sandbox environment. |
| **T2** | In-run credential misuse | Bounded by run wall-clock + the short-lived, single-installation GitHub token; the upstream LLM key is long-lived (BYOK, customer-rotated), so this is **partial coverage in v1** (§5). |
| **T3** | RCE in the agent SDK escaping the SDK process | gVisor + in-sandbox hardening (non-root uid, empty caps, seccomp, no-new-privs). |
| **T4** | RCE escaping gVisor to the host kernel | gVisor's user-mode-kernel architecture — the reason gVisor is used at all. |

**Adversary B — Triage Factory itself, as third-party code.** A security team
on a "no privileged third-party code" posture is not (only) worried about the
agent; they are worried that *our* binary holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`, and
that a bug in it — or a compromise of our supply chain — becomes their problem.
This is a legitimate and separate threat, and §4 and §6 are the answer to it.

Local mode (single user, single machine, SQLite) collapses adversary A's
multi-tenant threats: it is single-tenant, and **the sandbox is skipped
entirely** (`shouldSandbox()` = `runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"`), so it takes none of
the host privileges below. Everything in this document about capabilities and
privilege separation concerns the multi-mode (self-host and SaaS) deployments.

---

## 3. What Triage Factory requires from the host

An executor (the process that runs sandboxes) needs a
**privileged-*capable*** container substrate or a plain VM/bare-metal host with
root. Precisely — and note this is **not** `--privileged`:

| Requirement | Why |
| --- | --- |
| `CAP_NET_ADMIN` | Per-run network namespace, veth pair, NAT (MASQUERADE), and the fail-closed egress allowlist (`internal/sandbox/netns_linux.go`, `iptables_linux.go`). |
| `CAP_SYS_ADMIN` | Namespace/mount setup for the gVisor OCI container and the netns; memory-cgroup setup. |
| `seccomp=unconfined` | Docker's default syscall filter blocks operations gVisor needs to build its own, far stricter, sandbox. (A **tailored** profile replaces this — §6/§8.) |
| Private cgroup-v2 namespace | The per-run memory ceiling. |

What Triage Factory **explicitly does not take**, and a reviewer can verify:

- **not `--privileged`** — only the two capabilities above;
- **no host devices** (`/dev` is not exposed);
- **no host network namespace** (each run gets its own);
- **no host PID namespace**;
- **no host filesystem mounts** (the sandbox sees only its worktree);
- **no KVM / nested virtualization** — gVisor runs on the systrap platform.

And a property that matters as much as the capability list: **executors accept
no inbound network traffic.** An executor's only network activity is *outbound*
— to Postgres (claims, heartbeats), and the agent's proxied egress. There is no
listening port on a routable interface, no request parser exposed to the
network, no authentication surface. The entire hostile-input surface is things
the executor itself reached out to or spawned.

---

## 4. Privilege separation: the trust-domain decomposition

*(Target architecture — the privsep epic. Current state is a single process;
see §8.)*

There are two independent ways the system is decomposed, and conflating them
causes confusion, so name both:

- **Control vs. executor** is a *horizontal* split across machines — which host
  does which job (the "brain": API, polling, routing; vs. the "muscle":
  running sandboxes). It is about scale. It is specified in
  `docs/specs/horizontal-scaling/`.
- **cap-broker vs. orchestrator vs. sandbox** is a *vertical* split **within a
  single executor host** — who is allowed to hold which dangerous thing. It is
  about blast radius. That is this section.

The governing rule of the vertical split is: **no component holds both a
dangerous power and exposure to the attacker.** For each component, watch two
properties — the *power* it holds and the *hostile input* it is exposed to —
and note they never overlap:

| Component | Holds capabilities? | Holds credentials? | Parses hostile input? |
| --- | --- | --- | --- |
| **cap-broker** | **Yes** (the only holder) | No | **No** |
| **orchestrator** | No (dropped at exec) | Yes | Yes |
| **sandbox** (agent) | No | No | Yes (is the source) |

- **cap-broker** — the only process that holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`. It
  builds the netns/veth/iptables/cgroup, launches and supervises the gVisor
  runtime, tears everything down, and reaps orphans at boot. It holds **no
  credentials**, binds **no proxies**, and **reads no agent output**. Its only
  input is a narrow, fixed RPC vocabulary from its parent plus, for the run's
  I/O, a socket file descriptor it passes straight through to the runtime and
  never reads.
- **orchestrator** — today's main process, with its capabilities **dropped at
  exec** (not in-process — see the note below). It runs the dispatcher, resolves
  and holds credentials, binds the LLM/git/egress proxies (which parse hostile
  agent traffic), serves the agent-host socket, and drives the run. It holds
  every credential and **zero capabilities**.
- **sandbox** — the gVisor jail running the agent. No capabilities, no
  credentials (Property B).

### 4.1 Why this bounds a compromise

A vulnerability compromises a **running process** — its memory and its
kernel-granted privileges — not a *binary*. The same binary running as three
processes is three separate address spaces and three separate capability sets;
capabilities are per-process and enforced by the kernel at syscall time, not by
which code is present in the binary.

- Compromise the **orchestrator** (the realistic target — it is the one parsing
  hostile input): you obtain the credentials it legitimately holds, and nothing
  else. No capabilities — it dropped them, and the kernel denies the syscalls
  regardless of what code the attacker jumps to.
- Compromise the **cap-broker**: there is no attacker-reachable path to it. It
  parses no network, agent, or repository data; reaching it requires first
  owning the orchestrator and then finding a flaw in the narrow RPC between
  them.

The sentence this earns: *the only component holding root-equivalent power is a
few hundred lines that construct sandboxes and never touch data from the
network, an agent, or a repository.*

### 4.2 The runtime is held, and that is correct

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

The broker's exposure is **not** stdio; it is the RPC from the orchestrator —
the process that *is* exposed to hostile input. A compromised orchestrator's
move is to abuse that RPC to regain capabilities. Therefore the broker's
protective value equals the narrowness and validation of that RPC, and the
design rule is hard:

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

### 4.4 A note on dropping capabilities

Capabilities are dropped **at exec** (via a `setpriv`/`capsh`-style wrapper that
launches the orchestrator with an emptied capability set), **not** in-process.
Linux capabilities are per-thread, and the Go runtime spawns threads before
`main()` runs, so an in-process drop affects only one thread and silently leaves
others privileged — a control that *looks* applied but is not. The exec-time
drop is kernel-enforced from the first instruction. *(Validated: after an
exec-time drop of `CAP_NET_ADMIN`, a privileged network operation returns
`EPERM`.)*

---

## 5. Credential handling

The credentials an executor holds — real LLM provider keys, GitHub tokens, the
database connection, object-store credentials — are held because the executor
legitimately needs them to do its job. Their safety rests on three things:

- **Property B** (`internal/sandbox/doc.go`): no real credential ever enters the
  sandbox environment. The agent receives only a per-run proxy URL and a
  fresh-per-run token that authenticates it to *its own* proxy; the real key
  lives in the proxy on the host and is injected on the upstream hop. A
  jailbroken agent reading its own `/proc/self/environ` finds a throwaway token,
  not a credential.
- **Short-lived, narrowly-scoped GitHub auth**: git authentication uses GitHub
  **App installation tokens** — one-hour lifetime, scoped to a single
  installation — not a user PAT. A leaked token is minutes of single-org access.
  *(Hardening in progress: minting moves to the control plane so the executor
  holds only hour-lived tokens, never the App private key — §8.)*
- **Bring-your-own-key**: the LLM key at risk in a SaaS deployment is the
  customer's own, under the customer's rotation control — Triage Factory does
  not resell inference.

The honest residual: a compromise of the **proxy** yields the credentials the
proxy holds. This is inherent to the proxy pattern — any process that holds a
key and serves untrusted callers can leak that key if the process itself is
compromised — and it is the same exposure a credential-proxy sidecar has in any
architecture. What privilege separation guarantees is that this compromise
yields **no capabilities**: the proxy runs on the orchestrator (unprivileged)
side. The 408 sandbox-fleet spec's rule — "credentials and egress policy stay
per-run in the proxies, never baked into an image" — is the same invariant
stated from the customization side.

---

## 6. Risk register

Each entry is a way an attacker reaches the privileged process, with mitigations
marked **[shipped]** or **[planned]**.

**Vector 1 — our supply chain.** If the build pipeline or a dependency is
compromised, the executor is compromised. Irreducible for any privileged vendor
software; the defense is provenance.
- **[shipped]** The Alpine rootfs tarball is sha256-pinned per arch and extracted
  as inert data; its code only ever runs *inside* sandboxes.
- **[shipped]** The agent SDK's npm tree is pinned by an embedded lockfile with
  integrity hashes; `npm ci` refuses drift.
- **[planned]** `npm ci` currently runs **without `--ignore-scripts`**, so
  lifecycle scripts of the (pinned) tree execute at install time in the process's
  context. Adding `--ignore-scripts` closes the most-abused supply-chain channel.
- **[planned]** Signed releases + SBOM + build attestation.

**Vector 2 — hostile input parsed by the privileged process.** The path that
skips the gVisor escape.
- **[shipped]** The proxies and agent-host socket are memory-safe Go over narrow
  protocols; each sandbox authenticates to its own proxy with a per-run token.
- **[shipped]** Executors accept **no inbound traffic** — the entire hostile-input
  surface is outbound-initiated.
- **[planned]** Privilege separation (§4) moves every hostile-input parser —
  proxies, agent-host socket, git, archive extraction — onto the **unprivileged**
  orchestrator, so a parser flaw yields no capabilities.
- **[planned]** A **tailored seccomp profile** replaces `seccomp=unconfined`,
  turning the scariest line in the deployment manifest into an auditable
  allowlist.

**Vector 3 — the resident credentials.** (See §5.)
- **[shipped]** Property B; App installation tokens (1h, single-installation);
  BYOK.
- **[planned]** Move App-token minting to the control plane (executors never hold
  the App private key).
- **[planned]** A least-privilege `tf_system` database role for executors (no
  superuser DSN on the most-exposed machine class).

**Vector 4 — a compromised orchestrator abusing the broker RPC (resource
exhaustion, not a capability leak).** Even fully deprivileged, a compromised
orchestrator can call the broker's RPC as fast as it likes, and the broker
faithfully executes well-formed requests. It cannot regain capabilities (§4) —
but it can consume host resources: most concretely the fixed **256-slot**
per-run subnet pool (`internal/sandbox/subnet.go`, a `/16`→`/24` allocator), plus
the privileged netns/veth/iptables/cgroup setup and supervised runtime each
`LaunchRun` costs.
- **[planned]** The broker caps in-flight `LaunchRun`s per orchestrator instance
  (one orchestrator maps to one broker) and enforces release, so a runaway caller
  degrades to queueing rather than host exhaustion. This is denial-of-service
  resistance, **not** a capability boundary — called out so the two are not
  conflated.

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
- **Control plane as ordinary, unprivileged pods.** The API/websocket/polling
  tier is a normal web service. *(It retains capabilities today only because it
  still jails its own low-volume system jobs; a planned job-class split makes it
  fully unprivileged and cluster-native — see `docs/specs/horizontal-scaling/`
  §2.1.)*
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

## 8. Current state vs. roadmap

**Shipped today:**
- gVisor isolation of every multi-mode run (T3/T4).
- Property B — no credential in the sandbox environment (T1).
- Fail-closed egress default-DROP; per-run netns, veth, subnet.
- Per-run memory ceiling via cgroup-v2.
- GitHub App installation tokens (short-lived, single-installation).
- Executors accept no inbound traffic.
- Local mode takes none of the host privileges (sandbox skipped).

**In progress — privilege separation (the privsep epic):**
- Extract privileged operations behind a broker interface (refactor, no
  behavior change).
- The `cap-broker` process + narrow RPC + socket-fd stdio boundary.
- Exec-time capability drop on the orchestrator.
- Route the isolated-capture path through the broker.

**In progress — hardening (parallel track):**
- `npm ci --ignore-scripts`.
- Tailored seccomp profile replacing `seccomp=unconfined`.
- Control-plane GitHub App-token minting (executors never hold the App key).
- Least-privilege `tf_system` database role for executors.
- Signed releases + SBOM.

Until the privsep epic lands, an executor is a **single process** holding the
capabilities, the credentials, and the hostile-input parsers together. The
mitigations above (memory-safe Go, no inbound traffic, Property B, gVisor) bound
that today; privilege separation is what shrinks the privileged, credential-
holding surface to a small, auditable component.

---

## Related

- `internal/sandbox/doc.go` — Property B and the T1–T4 threat model in code
  (Property A — the agent cannot read its own env/memory/FDs — is defined in the
  sky-254 validation doc below).
- `docs/specs/horizontal-scaling/` — the control/executor split; §2.1 on the
  k8s posture, §6.4 on the sandbox-fleet interplay.
- `docs/specs/sandbox-fleet/` — TFAC-408 customizable sandboxes; §4's rootfs
  build is subject to the §4.3 broker rule here.
- `docs/isolation-tiers.md` — the tier ladder deployments slot into.
- `docs/specs/sky-254-runsc-validation/` — the gVisor threat model and the
  validated egress/proxy mechanics.
