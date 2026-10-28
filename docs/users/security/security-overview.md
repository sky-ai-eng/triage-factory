# Security overview

Triage Factory runs code written by an AI agent that acts on untrusted input:
repository contents, issue text and tool output, any of which can carry a
prompt injection. This page states what Triage Factory requires from its host,
why, and what a compromise of each component yields. It is written for someone
deciding whether to run Triage Factory on their infrastructure. For choosing a
deployment shape, see [Isolation tiers](isolation-tiers.md).

Everything below about capabilities and privilege separation concerns multi
mode (self-host and SaaS). **Local mode** is single-tenant and does not use
the gVisor sandbox, so it takes none of the host privileges below. On Linux, a
local run gets a bubblewrap mount and pid namespace by default
(`TF_LOCAL_SANDBOX`, see [local configuration](../local/configuration.md)).
That is isolation for convenience, not a boundary: the run has the operator's
uid and shares the network. It stops a confused agent from reading a sibling
run's worktree, the TF database or the operator's home directory by path, and
from changing the Claude Code configuration under `~/.claude`. It does not
make the keychain unreachable, and a local agent that sets out to escape it
can.

## 1. The capabilities build the sandbox; the agent never holds them

Triage Factory needs `CAP_SYS_ADMIN`, which reads as if untrusted code runs
privileged. It does not. The agent is the most confined process in the system:
it runs in a gVisor sandbox, as a non-root uid, with an empty capability set, a
seccomp policy, no credentials in its environment, and a fail-closed egress
firewall. The capabilities exist so that Triage Factory can build that sandbox:
the network namespace, the veth pair, the firewall rules, the memory cgroup,
and the gVisor runtime. GitLab runners, BuildKit and GKE Sandbox (also gVisor)
have the same shape.

## 2. Threat model

**Adversary A: the agent**, prompt-injected or otherwise hostile, inside the
sandbox.

| | Threat | Defense |
| --- | --- | --- |
| **T1** | Credential exfiltration | No credential enters the sandbox (Property B, §5). |
| **T2** | Misusing credentials during the run | Each credential is reachable only through a fixed interface: the GitHub token is attached only to git fetch/push for the authorized repository; Jira and GitHub API calls are host-side `exec` verbs; the `gh` channel injects one repository-scoped token at a proxy that audits every write and refuses submitting a review, creating a repository and opening a pull request in every spelling, including raw `curl` (each has its own `exec` verb); the LLM key is reachable only through the provider proxy, and under the native runtime the sandbox has no LLM channel at all. **Residual:** within its granted scope the agent acts with the run's authority — push to the authorized repository, comment and label on tracked repositories, merge a pull request it was told to, spend the org's LLM tokens — for the run's wall-clock time. |
| **T3** | Code execution in the in-sandbox tool host escaping that process | gVisor plus in-sandbox hardening: non-root uid, empty capabilities, seccomp, no-new-privs. |
| **T4** | Code execution escaping gVisor to the host kernel | gVisor's user-mode kernel. |

**Adversary B: Triage Factory itself, as third-party code.** An organization
that does not run privileged third-party code worries that a bug in Triage
Factory, or a compromise of its supply chain, lands on a process holding
`CAP_SYS_ADMIN`/`CAP_NET_ADMIN`. §4 and §6 answer that.

The two adversaries meet at one point: the agent's traffic is parsed by
Triage Factory's own proxies. That parsing runs in the run's credential
sidecar, a process with no capabilities that holds only that one run's
credentials, never in the process that holds capabilities.

## 3. What Triage Factory requires from the host

An executor (the process that runs sandboxes) needs a container runtime that
can grant capabilities, or a VM or bare-metal host with root. It never needs
`--privileged`.

| Requirement | Why |
| --- | --- |
| `CAP_NET_ADMIN` | Per-run network namespace, veth pair, NAT, and the fail-closed egress allowlist. |
| `CAP_SYS_ADMIN` | Namespace and mount setup for the gVisor container and the network namespace; memory-cgroup setup. |
| `CAP_SYS_RESOURCE` | Placing each sandbox's runtime below its guest processes in the kernel's OOM order, so a guest that hits its memory ceiling loses a process rather than its sandbox. The executor refuses to start without it. |
| Tailored seccomp profile | Docker's default profile blocks a syscall gVisor needs to build its sandbox. `docker/seccomp-profile.json` is Docker's default allowlist with that one syscall added and others removed; see [Seccomp profile](seccomp-profile.md). |
| Private cgroup v2 namespace | Per-run memory ceilings (`cgroup: private` in compose, Docker's default on cgroup v2 hosts). Without one, ceilings are off and the executor logs a warning. |

It does not require host devices, the host network namespace, the host PID
namespace, host filesystem mounts beyond each run's own tree, or KVM (gVisor
runs on the systrap platform).

The capability-holding process never listens on the network. Its only input is
a host-local unix socket from its parent. Multi mode always splits control and
executor processes: executors run the broker and accept no inbound traffic;
control pods serve the HTTP API and hold no sandbox capabilities.

## 4. Privilege separation

Two independent splits:

- **Control vs. executor**, across machines, is about scale: control serves the
  API, polls and routes; executors run sandboxes.
- **Cap-broker vs. orchestrator vs. sidecar vs. sandbox**, within one executor,
  is about blast radius, and is the subject of this section.

The rule: **no process holds both a dangerous power and exposure to hostile
input.**

| Process | Capabilities | Credentials | Parses hostile input |
| --- | --- | --- | --- |
| **cap-broker** | **Yes**, the only holder | None | **No** |
| **orchestrator** | None (dropped before the binary loads) | Its own control-plane credentials; no per-run agent credential | Its own control-plane inputs; not the agent's output |
| **credential sidecar** (one per run) | None | One run's credentials only | That run's agent traffic |
| **sandbox** (the agent) | None | None | Yes |

- **cap-broker** builds and tears down each run's network, cgroup and gVisor
  runtime, and supervises the runtime. It holds no credentials, runs no
  proxies and reads none of the run's bytes: it passes the run's socket to the
  runtime and closes its own copy.
- **orchestrator** is the main process. A `setpriv` wrapper drops its
  capabilities and clears its bounding set before the binary loads, so the
  kernel refuses any privileged call it makes.
- **credential sidecar** runs at a per-run uid, holds one run's credentials,
  unsealed with a key it generates itself, and serves that run's proxies and
  `exec`-verb socket. It exits with the run.
- **sandbox** runs the agent with no capabilities and no credentials.

[Privilege separation](privilege-separation.md) shows how to check each of
these on a running executor.

### What a compromise yields

- **Orchestrator:** no capabilities and no per-run agent credentials.
- **One run's sidecar:** that run's credentials and nothing else. It has no
  capabilities, and its per-run uid stops it from reading or tracing any other
  run's sidecar.
- **Cap-broker:** there is no direct path to it. It parses no network, agent
  or repository data; reaching it means first owning the orchestrator and then
  abusing the broker's narrow RPC (below).
- **Sandbox escape (T4):** host code execution, whichever process launched the
  runtime.

The proxies and sockets that parse hostile input are Go, so the realistic
fault is a panic or a misclassification rather than memory corruption. A panic
in the sidecar costs the request, not the process. The `gh` write gate refuses
any request whose action it cannot identify.

### The broker's RPC is its attack surface

A compromised orchestrator's best move is to abuse the broker's RPC to get
capabilities back. The broker therefore owns the sandbox's OCI spec, built
from a fixed template, and accepts only validated parameters: a run id, the
network namespace and cgroup it created itself, the jail identity derived from
that namespace, an allowlisted environment, numeric limits, and (self-host
only) an extra egress CIDR checked against the internal denylist (cloud
metadata, the control-plane subnet, private and link-local ranges). It never
runs an orchestrator-supplied config, command or rootfs path.

Every check is an allowlist, so a new kind of value fails closed: the rootfs
is chosen by catalog name, the entrypoint is pinned, environment keys and
mount options are allowlisted, and bind-mount sources are pinned to the run's
own tree. Privileged ownership changes and removals resolve paths with
`openat2(RESOLVE_NO_SYMLINKS)` and act only on trees already owned by the
orchestrator or a run's identity, never root. To check: `FuzzValidateLaunchParams` and the run-tree and egress
fuzzers in `internal/sandbox` assert, for every input the validator accepts,
the invariants the broker relies on.

The most a compromised orchestrator can do through the RPC is inject an
environment variable the sandboxed, unprivileged agent sees, or exhaust
resources (§6, vector 4).

## 5. Credential handling

- **No credential enters the sandbox (Property B).** The agent gets a per-run
  proxy URL and a throwaway token that authenticates it to its own proxy. The
  real credential is attached on the upstream hop. An agent reading its own
  `/proc/self/environ` finds the throwaway token.
- **GitHub uses App installation tokens:** one hour, one installation, minted
  on the control plane. Executors never hold the App private key.
- **Bedrock uses STS session credentials** minted on the control plane:
  limited to model invocation, at most one hour, and optionally bound to the
  executor's egress. The Anthropic API route uses the customer's own key, which
  they can spend-cap, expire and revoke.
- **Each credential works for one operation.** The git proxy attaches the
  token only to fetch and push for authorized repositories (anything else gets
  a 403). On push it allows only the run's own branch, plus the base branch
  when the team's base-branch policy permits it (off by default; team admins
  only, because a task's text is externally authored). Local mode enforces the
  same policy in a `pre-push` hook, which a local agent running as the
  operator can skip.
- **Executors get sealed per-run bundles.** An executor loads no
  secret-encryption key and reads no shared secret store. Each run receives a
  bundle sealed to its sidecar, so a compromise reaches one run's short-lived
  credentials, never an App private key, the encryption key, or another
  tenant's material.

Residual: a compromised sidecar leaks the credentials that sidecar holds.

## 6. Risk register

**Vector 1: supply chain.**
- The Alpine rootfs tarball is sha256-pinned per architecture and only ever
  runs inside sandboxes.
- The agent SDK's npm tree is pinned by an embedded lockfile with integrity
  hashes and installed with `npm ci --ignore-scripts`.
- Releases are signed keylessly (cosign and GitHub OIDC) with an SBOM and SLSA
  provenance; see [Verifying releases](verifying-releases.md).

**Vector 2: hostile input reaching a privileged process.**
- Every parser of hostile input runs without capabilities, and the ones that
  hold credentials run per run.
- Executors accept no inbound traffic.
- The executor container runs under the tailored seccomp profile, not
  `unconfined`.

**Vector 3: credentials resident on an executor.** See §5. Executors also
connect to Postgres as `tf_system`, a role with no DDL and a narrow grant list,
instead of a superuser.

**Vector 4: a compromised orchestrator exhausting the broker.** It cannot
regain capabilities, but it can request well-formed sandboxes as fast as it
likes, and each one costs one of 256 per-run subnets. The broker caps
in-flight launches at the subnet pool size, so a runaway caller queues rather
than exhausting the host. This is denial-of-service resistance, not a
capability boundary.

## 7. Deployment guidance

- **Run executors on dedicated VMs or bare metal, outside Kubernetes.** They
  accept no inbound traffic and dial out to Postgres for work, like CI
  runners. Run the executor in a container with a private cgroup namespace;
  directly on the host it runs without per-run memory ceilings.
- **Run the control plane as ordinary unprivileged pods.** In the bundled
  `docker-compose.yml` the control service has no added capabilities and no
  custom seccomp profile, and spawns no cap-broker. Its background LLM work
  (scoring, repo profiling) is toolless API calls from the Go process, so
  there is nothing to sandbox.
- **If executors must run in Kubernetes,** use privileged pods on a dedicated,
  tainted node pool with the cloud metadata endpoint blocked, a minimal node
  role, and no other workloads. We do not recommend it: root on a Kubernetes
  node reaches node credentials, the cluster API, the node's cloud identity
  and the pod network.

Triage Factory does not run each sandbox as its own pod (`runtimeClass:
gvisor`). One executor hosts around a hundred sandboxes sharing a warm rootfs
page cache, a subnet pool and per-run proxies; a pod per run loses all three
and turns a ~200 ms sandbox spawn into seconds.

## Related

- [Isolation tiers](isolation-tiers.md)
- [Privilege separation](privilege-separation.md)
- [`specs/horizontal-scaling/`](../../../specs/horizontal-scaling/README.md):
  the control/executor split
- [`specs/sandbox-fleet/`](../../../specs/sandbox-fleet/README.md):
  customizable sandboxes and the egress denylist
- [`specs/sky-254-runsc-validation/`](../../../specs/sky-254-runsc-validation/README.md):
  the validated egress and proxy mechanics
