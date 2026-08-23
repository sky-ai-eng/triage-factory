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
| **T2** | In-run credential misuse | Strongly bounded: no credential is raw in the sandbox, and each is reachable only through a constrained interface (GitHub token injected only on git fetch/push to the authorized repo; Jira/GitHub API are fixed host-side `exec` verbs; the real-`gh` channel injects one repo-set-scoped token at a proxy that audits every write it forwards and refuses two families outright — submitting a review and creating a repository, in every spelling including a raw `curl`; LLM key is provider-only via the proxy, and under the native runtime the sandbox is given no LLM channel at all). Residual: within its granted scope, and outside those refused families, the agent still acts with the run's authority (push to the authorized repo, comment and label on the repos it tracks, merge a pull request it was told to — subject only to the delegated runtime's advisory merge question, which does not bind a non-`bash` client — spend the org's LLM tokens), bounded by run wall-clock. |
| **T3** | RCE in the in-jail process (the native runtime's resident tool host) escaping that process | gVisor + in-sandbox hardening (non-root uid, empty caps, seccomp, no-new-privs). |
| **T4** | RCE escaping gVisor to the host kernel | gVisor's user-mode-kernel architecture. |

**Adversary B — Triage Factory itself, as third-party code.** A company with a
"no privileged third-party code" posture is not (only) worried about the agent;
they are worried that *our* binary holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`, and
that a bug in it — or a compromise of our supply chain — becomes their problem.
This is a separate threat, and §4 and §6 are the answer to it.

The two connect at one point: the agent's traffic is among the inputs our own
parsers (the proxies, the agent-host socket) handle, so a parser bug there is
where Adversary A could reach Adversary B — which is exactly why that parsing
runs on an unprivileged process, never the capability-holder (§4): in a
multi-mode executor it is the run's own capless credential sidecar, holding only
that one run's material; on the all/local path it is the orchestrator.

Local mode (single user, single machine, SQLite) collapses adversary A's
multi-tenant threats: it is single-tenant, and **the gVisor sandbox is skipped
entirely** (`shouldSandbox()` = `runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"`), so it takes none of
the host privileges below. Everything in this document about capabilities and
privilege separation concerns the multi-mode (self-host and SaaS) deployments.

On Linux, local runs do get a **mount namespace** by default (bubblewrap;
`TF_LOCAL_SANDBOX`, see [Configuration](../local-mode/configuration.md)). It is
courtesy isolation and deliberately not a boundary — same uid, shared network,
shared `/proc` — so nothing in this document rests on it. What it buys is that a
confused or prompt-injected agent cannot read a sibling run's worktree, the TF
database, or the operator's home directory without deliberately breaking out,
and that the OS keychain is out of reach from inside. Everything a compromised
local agent could do to the machine before, it can still do if it tries.

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
has no routable port, no request parser, and no authentication surface. Multi
mode is always the control+executor split (`TF_ROLE=all` refuses to boot
there), and only executors run a broker — executor pods take no inbound
traffic at all, while the control pod serves the HTTP API with no broker and
no sandbox capabilities in its container. The hostile-input surface is only
things the process itself reached out to or spawned.

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
| **orchestrator** | No (dropped at exec) | Its own control-plane creds; **no per-run agent credential** | Its own control-plane inputs; **not** a run's agent output |
| **credential sidecar** (one per run) | No (capless) | Yes — one run's material only | Yes — that one run's agent traffic, inbound and (at the `gh` injector) outbound |
| **sandbox** (agent) | No | No | Yes |

- **cap-broker** — the only process that holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`. It
  builds the netns/veth/iptables/cgroup, launches and supervises the gVisor
  runtime, tears everything down, and reaps orphans at boot. It holds **no
  credentials**, binds **no proxies**, and **parses no agent output**. Its only
  input is a narrow, fixed RPC vocabulary from its parent plus, for the run's
  I/O, a socket file descriptor it passes straight through to the runtime and
  never reads — the run's bytes never enter the broker's address space (§4.2).
- **orchestrator** — today's main process, with its capabilities **dropped at
  exec** (not in-process — see the note below). It runs the dispatcher and drives
  the run. The credential-holding parts — the LLM/git/egress proxies (which parse
  hostile agent traffic) and the agent-host socket — run in a **capless per-run
  child process**, one per run, so a run's credentials are process-isolated per
  run rather than shared across the box (§5); the orchestrator itself keeps only a
  capless op server (`RelayServer`) that answers that child's narrow, validated
  policy/DB/audit relays — no credential-bearing op. It holds **zero
  capabilities**.
- **credential sidecar** — a capless per-run child holding only *one* run's
  credentials, unsealed with a key it generates for itself. No capabilities, one
  tenant's material, exposed only to that run's own traffic, freed when the run's
  process exits (§5). Its exposure runs both ways: besides the upstream
  responses its proxies parse, the `gh` injector reads the body of an outbound
  GraphQL request to learn which mutation it performs. That read is what makes
  a write policy possible at all against a protocol that names the act only in
  the body, and it is narrow by construction — one endpoint, a size cap, a
  grammar that collects top-level field names and skips everything beneath them
  uninterpreted, parse-fail refuses the request, and nothing forwarded is
  altered. §4.1 covers what a fault in it would and would not yield.
- **sandbox** — the gVisor jail running the agent. No capabilities, no
  credentials (Property B). What it is *pointed at* narrows further with the
  runtime: a jail whose agent loop runs outside it — the native runtime, where
  the loop lives in the executor process and only tool execution is jailed — is
  built with no LLM proxy address and no placeholder to present at one, so it
  has no provider channel to reach for at all rather than a channel carrying a
  per-run placeholder.

### 4.1 Why this bounds a compromise

A vulnerability compromises a **running process** — its memory and its
kernel-granted privileges. Capabilities are per-process and enforced by the
kernel at syscall time.

- The orchestrator and each run's credential sidecar are the realistic targets —
  the processes parsing hostile input. Neither holds capabilities (the
  orchestrator dropped them at exec; the sidecar's setuid-from-root exec cleared
  them), and the kernel denies the syscalls regardless of what code the attacker
  jumps to. The per-run agent credentials live only in the run's own sidecar, so
  compromising the orchestrator/dispatcher core yields **no** agent credentials,
  and compromising a single run's sidecar yields only **that run's** material —
  never the co-located set (§5).
- Both are Go, so the realistic fault in a parser here is a panic or a
  misclassification rather than memory corruption, and both are contained. Every
  goroutine in the sidecar's own packages either recovers or documents why it
  cannot panic (the rule is stated in `cmd/runsidecar`'s package doc), so a
  panic costs the request or the connection, not the process holding the run's
  credentials. A misclassification fails closed where it matters: the `gh`
  channel's write gate refuses any request whose act it could not name, so a
  document the reader cannot parse is refused rather than forwarded.
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
The runtime proxies the newline-delimited-JSON agent protocol over a
socket-backed stdio while the supervising process reads nothing.

### 4.3 The broker's real attack surface is its narrow RPC

The broker's exposure isn't stdio; it is the RPC from the orchestrator,
which *is* exposed to hostile input. A compromised orchestrator's move
is to abuse that RPC to regain capabilities. Therefore the broker's
protective value equals the narrowness and validation of that RPC.

> The broker **owns the OCI spec** from a fixed template (rootfs = the
> content-addressed rootfs it resolved, capabilities = empty, uid = 10000,
> seccomp, namespaces) and accepts over RPC only narrow, validated parameters —
> a run id, the netns/cgroup it created, an environment allowlist, numeric
> resource limits, and (self-host only) an additional permitted egress CIDR
> validated against the immutable internal denylist (cloud metadata endpoint
> `169.254.169.254`, the control-plane subnet, private/link-local ranges — see
> [sandbox-fleet](../for-agents/specs/sandbox-fleet/README.md) §3.1) before any iptables permit is written. It **never** execs
> an orchestrator-supplied `config.json`, command, or rootfs path.

A compromised orchestrator can inject an environment variable the *sandboxed*
(unprivileged) agent will see — harmless — but it can never make the broker run
arbitrary code with capabilities.

Why this stays true as the code changes:

- **One unconditional choke point.** The broker is the only launch path, and
  every launch passes `ValidateLaunchParams` — every privileged run-tree
  chown/remove passes `validateRunTreeRoot` — before the broker builds a spec,
  execs anything, or touches a file.
- **Allowlist, not denylist, so new inputs fail closed.** A *new* dangerous value
  a future change might introduce is refused by default:
    - rootfs is chosen by catalog *name* (unknown → rejected);
    - the command's first two argv are pinned to the tool-host binary + its
      `serve` verb, and that binary's mount source is validated against the
      broker's own resolution, never the orchestrator's;
    - env keys are an allowlist, and mount options the closed set `{ro, rw}`;
    - rlimit types are allowlisted;
    - the run-tree ops touch only trees owned by the sandbox or orchestrator uid,
      **never** root.
- **The gate is fuzzed for "accepted ⇒ safe."** `internal/sandbox`'s
  `FuzzValidateLaunchParams` (plus the run-tree and egress fuzzers) feeds
  arbitrary RPC input to the validator and, for every input it *accepts*,
  independently asserts the invariants the broker then trusts — catalog rootfs,
  pinned entrypoint, allowlisted env/mounts, a run-bound netns, a non-denylisted
  egress CIDR. A change that widens an allowlist or drops a check surfaces as a
  failing test.

**None of this is a capability escalation, and the data-plane sources are pinned
too.** The broker pins every bind-mount *source* to the run's own tree and binds
each run's netns from broker-tracked per-run state, so a compromised orchestrator
cannot point a mount or a namespace at a sibling run's data — the checks enforce
one consistent per-run scope, not merely a well-formed shape. This is internal
consistency, not tenant authentication: a credential-free broker cannot
authenticate an org, so the guarantee is that every path a run touches shares its
own tree's scope. The privileged run-tree chown/remove path resolves through
`openat2(RESOLVE_NO_SYMLINKS)`, so the kernel refuses a swapped-symlink component
rather than a prior check merely catching it.

### 4.4 A note on dropping capabilities

Capabilities are dropped by a small `setpriv` wrapper **before the orchestrator
binary even loads** — not from inside the running process, where Go's threading
model makes an in-process drop unreliable (it would silently leave some threads
privileged). So the orchestrator starts with an empty, kernel-enforced capability
set from its first instruction: any privileged operation it attempts is refused
by the kernel.

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
  App installation tokens — one-hour lifetime, scoped to a single
  installation — not a user PAT. These tokens are minted on the control
  plane; the executor holds only the hour-lived scoped token, never the App
  private key. A leaked token is minutes of single-org access, scoped to a few
  repos at most.
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
  repurposed for arbitrary requests. On a push the proxy also enforces a
  per-ref allowlist: a run may update only the branch its own worktree is on,
  and a repository's base/default branch only if the team's base-branch push
  policy permits it (default: it does not). That policy is team-grained and
  team-admin-only — a task's text is externally authored, so it must never be
  able to authorize its own base-branch push. Local mode enforces the same
  policy at TF's `pre-push` hook, where it is a guard against a mistaken agent
  rather than a boundary: a local-mode agent runs as the operator and can skip
  client hooks.

A compromise of a run's proxy still yields the credentials that proxy
holds — any process that holds a key and serves untrusted callers can leak it if
the process itself is compromised. Two things bound that leak: privilege
separation guarantees it yields **no capabilities** (the proxy is capless), and
per-run isolation guarantees it yields **no other run's credentials** — each
run's proxies and agent-host run in their own **capless per-run process** holding
only that one run's material.

The *reach* is bounded by construction. An executor loads no secret-bag
encryption key and resolves nothing from a shared store: each run receives only a
**sealed, per-run credential bundle** carrying the short-lived, scoped material it
legitimately needs (control-plane-minted GitHub App installation tokens, Bedrock
STS session credentials), unsealed inside that run's own process with a key the
process generates for itself — so the dispatcher core holds no decrypted
credentials and cannot open any run's bundle. A compromise therefore reaches
**one run's** credentials, not the co-located set — never an App private key,
never the encryption key (which never touch an executor), never another tenant's
material — and when the run ends its process exits and its credentials go with
its address space. The remaining long-lived credential is the raw Anthropic API
key for orgs on that route — the customer's own, spend-capped, expiring, and
revocable.

---

## 6. Risk register

**Vector 1 — our supply chain.** If the build pipeline or a dependency is
compromised, the executor is compromised.
- The Alpine rootfs tarball is sha256-pinned per arch and extracted
  as inert data; its code only ever runs *inside* sandboxes.
- The agent SDK's npm tree is pinned by an embedded lockfile with
  integrity hashes; `npm ci` refuses drift.
- `npm ci` runs with `--ignore-scripts`, so lifecycle scripts of
  the (pinned) tree never execute at install time.
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
  proxies, agent-host socket, git, archive extraction — runs **capless**, and the
  credential-holding ones (proxies, agent-host) run in a **per-run** process, so a
  parser flaw yields neither capabilities nor another run's credentials.
- A **tailored seccomp profile** (`docker/seccomp-profile.json`)
  replaces `seccomp=unconfined` in the deployment manifest.

**Vector 3 — the resident credentials.** (See §5.)
- Property B; App installation tokens (1h, single-installation); BYOK.
- App-token minting runs on the control plane — executors never hold the App
  private key, only the hour-lived scoped token.
- Executors receive only sealed, per-run credential bundles, unsealed inside each
  run's own process; the secret-bag encryption key never loads on an executor, so
  a compromise reaches only **that one run's** bundle, never the whole tenant
  secret set.
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
  than host exhaustion. This is really denial-of-service resistance,
  not a capability boundary.

---

## 7. Deployment guidance

The *most* secure deployment pattern is as follows, and is particularly relevant
when k8s is involved. The capability requirements allow for privileged workloads
to remain out of Kubernetes clusters entirely:

- **Executors as dedicated VMs (or bare metal), outside the cluster.** They
  accept no inbound traffic and dial out to Postgres to claim work — exactly the
  pattern used for GitHub Actions / GitLab / Buildkite runners.
- **Control plane as ordinary, fully-unprivileged pods.** The API/websocket/polling
  tier is a normal web service that holds **no** sandbox capabilities: every
  sandboxed workload — delegated runs — runs on executors. The control
  plane's own background LLM work (task scoring, project
  classification, repo profiling) is deliberately toolless — prompt in, JSON out,
  no filesystem, no tool loop, no subprocess — so there is nothing to jail: those
  are direct API calls from the Go process. The control service carries no
  sandbox capabilities: `docker-compose.control.yml` clears the caps + seccomp
  and the control pod spawns no cap-broker (the entrypoint and the Go role-gate
  both skip it), so nothing privileged is present to misuse.
- **If executors must run in Kubernetes**, they run as privileged pods on a
  dedicated, tainted node pool with the cloud metadata endpoint blocked, a
  minimal node role, and no other tenants' pods scheduled there. We do not
  recommend this: a privileged pod is a standing admission-policy
  exception, and root on a k8s node reaches node credentials, the cluster API,
  the node's cloud identity, and the pod network — which is why "privileged on a
  VM" and "privileged on a shared k8s node" are genuinely different risks.

What Triage Factory deliberately does **not** do is run each sandbox as a pod
(`runtimeClass: gvisor`). A run is not a pod: one executor hosts ~a hundred
sandboxes sharing a warm rootfs page-cache, a subnet pool, and per-run proxies;
run-per-pod forfeits all three and trades a ~200 ms sandbox spawn for
pod-startup seconds.

---

## Related

- `docs/for-agents/specs/horizontal-scaling/` — the control/executor split; §2.1 on the
  k8s posture, §6.4 on the sandbox-fleet interplay.
- `docs/for-agents/specs/sandbox-fleet/` — customizable sandboxes; §4's rootfs
  build is subject to the §4.3 broker rule here.
- `docs/security/isolation-tiers.md` — the tier ladder deployments slot into.
- `docs/for-agents/specs/sky-254-runsc-validation/` — the validated egress/proxy
  mechanics, and Property A (the agent cannot read its own env/memory/FDs).
