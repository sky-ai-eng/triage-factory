# Privilege separation — the cap-broker / orchestrator split

Design of record for shrinking the code that holds root-equivalent Linux
capabilities on an executor from "the whole binary" to "a small, data-blind
helper." The executor process today holds the capabilities **and** the
credentials **and** the hostile-input parsers in one address space; this epic
splits them so a compromise of the exposed part yields no capabilities.

Status: **accepted design, validated empirically 2026-07-08** (§3). The
customer/CISO-facing companion is `docs/sandbox-security-architecture.md`; this
spec is the engineering design-of-record the child tickets cite. Builds on the
gVisor sandbox (`internal/sandbox`, SKY-254), the per-run proxies (SKY-395), and
composes with — does not replace — the horizontal-scaling fleet split
(`docs/specs/horizontal-scaling/`, TFAC-71).

Scope: multi-mode only. Local mode skips the sandbox entirely
(`shouldSandbox()` = `runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"`, `internal/agentproc/sandbox_integration.go`),
so it holds no capabilities and this epic is a no-op there (§7).

---

## 1. Problem and goal

A security team on a "no privileged third-party code" posture treats *our*
binary as the risk: it holds `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`, and a bug in it —
or a supply-chain compromise — becomes their host compromise. Today an executor
is a single process (verified: one goroutine tree per run does queue dispatch,
netns/iptables setup, credential resolution, and proxy serving —
`internal/delegate/dispatch.go`, `internal/agentproc/run.go`,
`internal/sandbox/sandbox_linux.go`). One exploit of the hostile-input parser
yields capabilities **and** credentials **and** the GitHub App private key at
once.

Goal: the only component holding capabilities is a few hundred lines that
construct sandboxes and never touch data from the network, an agent, or a
repository.

---

## 2. Topology

Two independent decompositions; do not conflate them:

- **control vs. executor** — horizontal split across hosts (which machine does
  which job). Scale. `docs/specs/horizontal-scaling/`.
- **cap-broker vs. orchestrator vs. sandbox** — vertical split *within one
  executor host* (who holds which dangerous thing). Blast radius. **This spec.**

Governing rule: **no component holds both a dangerous power and exposure to the
attacker.**

| Component | Capabilities | Credentials | Parses hostile input | Lifetime |
| --- | --- | --- | --- | --- |
| **cap-broker** | **Yes (only holder)** | No | **No** | long-lived helper |
| **orchestrator** | No (dropped at exec) | Yes | Yes | today's main process |
| **sandbox** (agent) | No | No | Yes (is the source) | per run |

- **cap-broker** — re-exec of the same binary as a `cap-broker` subcommand.
  Owns all privileged operations (§5): netns/veth/iptables/cgroup setup,
  execs+supervises the gVisor runtime, teardown, boot orphan reap, curated
  rootfs bake. Holds no credentials, binds no proxies, reads no agent output.
- **orchestrator** — today's process with capabilities dropped at exec (§3.3).
  Dispatcher, credential resolution, the LLM/git/egress proxies (hostile-input
  facing), the agent-host socket, the live driver.
- **sandbox** — unchanged; the gVisor jail.

---

## 3. Validated constraints (empirical, 2026-07-08)

Experiments run against real `runsc` (release-20260511) in a `--privileged`
container. Harnesses preserved under the epic's working notes.

### 3.1 The runtime is held by the cap-holder, not handed off

`runsc run` is a single **blocking** child that lives for the whole agent run
and needs `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` for its full duration (setns into the pre-made
netns + gVisor's own mounts). So the cap-broker must **exec and supervise** the
runtime; there is no "build a sandbox and return an unprivileged handle." This
adds no exposure: a gVisor escape (T4) means host code execution regardless of
the runtime's parent.

### 3.2 Stdio crosses as a passed-through socket fd (DECISION: socket-by-path)

The orchestrator (the live driver) must read/write the agent's
newline-delimited-JSON stream, but the broker execs the runtime. Resolution:
the orchestrator listens on a per-run unix socket; the broker dials it, hands
the connection's fd to the runtime as stdin+stdout, and **closes its own copy**.
The kernel then wires the runtime's stdio directly to the orchestrator; the
bytes never enter the broker's address space (a structural property — no read
call, fd closed — not a behavioral promise).

**Validated:** real `runsc run` faithfully proxied NDJSON both directions over
socket-backed stdio while the supervising process read nothing. This reuses the
`cmd/exec/agenthost/` socket pattern and needs **no** `SCM_RIGHTS` fd-passing
(the codebase has none today; not introduced).

### 3.3 Capabilities are dropped at exec, never in-process (DECISION)

Linux capabilities are per-thread and the Go runtime spawns threads before
`main()`, so an in-process `capset` affects one thread and silently leaves
others privileged — a control that looks applied but is not. Drop at exec via a
`setpriv`/`capsh`-style wrapper in the container entrypoint: it holds caps only
long enough to spawn the cap-broker, then execs the orchestrator with an emptied
capability set the kernel enforces from the first instruction.

**Validated:** after an exec-time drop of `CAP_NET_ADMIN`, `ip link add` returns
`EPERM` and the cap is absent from the bounding set.

---

## 4. The broker's contract

The broker's real attack surface is the RPC from the (hostile-input-exposed)
orchestrator, so the contract is deliberately narrow:

> The broker **owns the OCI spec** from a fixed template (rootfs = the
> content-addressed rootfs it resolved; capabilities = empty; uid/gid = 10000;
> seccomp; namespaces) and accepts over RPC only narrow, validated parameters.
> It **never** execs an orchestrator-supplied `config.json`, command, or rootfs
> path.

RPC vocabulary (fixed, versioned; length-prefixed JSON per `agenthost` precedent):

| Method | Params (all validated) | Returns |
| --- | --- | --- |
| `SetupNetwork` | run id | subnet, `HostIP`, netns path (broker-created) |
| `LaunchRun` | run id, rootfs selector (name→hash, from broker catalog), env **allowlist**, memory limit, rlimits, stdio socket path, self-host-only extra egress CIDR | (supervised to completion) |
| `Teardown` | run id | ok |
| `EnsureRootfs` | rootfs selector (curated catalog only in v1) | hash |

Params that are **data** (env values, numeric limits, a curated rootfs name, a
CIDR validated against the immutable internal denylist) are safe; a path, a
command, or a whole spec is never accepted. The self-host-only egress CIDR is
validated against the denylist (cloud metadata endpoint `169.254.169.254`, the
control-plane subnet, private/link-local ranges — sandbox-fleet §3.1) before any
iptables permit is written; "validated" means *safe*, not merely *well-formed*.
A compromised orchestrator can inject an env var the *unprivileged* agent sees
— harmless — but cannot make the broker run arbitrary code with capabilities.

Handoff artifacts (from the privileged-op audit): netns crosses **by path**
(reachable in the shared mount ns), the cgroup fd stays **broker-internal**
(needed only at the broker's own `clone3`), credentials **never** cross.

**Abuse resistance (DoS, not a capability boundary).** The RPC guards against
capability *escalation*, not resource *exhaustion*: a compromised orchestrator
can still spam well-formed `LaunchRun`s and exhaust the 256-slot subnet pool
(`internal/sandbox/subnet.go`) or the privileged setup each launch costs. PS-P3
adds a per-orchestrator-instance cap on in-flight `LaunchRun`s (one orchestrator
maps to one broker) plus release enforcement, so a runaway caller degrades to
queueing rather than host exhaustion. This is denial-of-service resistance, not a
capability boundary — see `docs/sandbox-security-architecture.md` §6 vector 4.

---

## 5. Privileged-operation inventory (what moves to the broker)

From the audit of `internal/sandbox` (all Linux-only):

- **Network** (`netns_linux.go`, `iptables_linux.go`): `ip netns/link/addr/route`,
  `iptables` MASQUERADE + egress allowlist, `ip_forward` procfs write. CAP_NET_ADMIN
  (+CAP_SYS_ADMIN for the netns mount).
- **Cgroup** (`cgroup_linux.go`): possible `mount` remount of cgroup2, mkdir +
  `memory.max`/`memory.swap.max` writes, the run cgroup fd. CAP_SYS_ADMIN.
- **Runtime** (`runsc.go`, `sandbox_linux.go`): `runsc run` exec + supervise;
  child `clone3`'d into the cgroup via `CgroupFD`. CAP_SYS_ADMIN+CAP_NET_ADMIN, whole run.
- **Rootfs bake** (`rootfs_linux.go`): `chroot`+`apk` for curated variants.
  CAP_SYS_CHROOT. Curated content only in the broker (§8).
- **Teardown / boot reap** (`reaper*.go`, `Close`): `iptables -D`,
  `ip link/netns delete`, cgroup `rmdir`. Reconstructible from durable
  identifiers (the reaper already proves a *different* process can tear down).

A **second** privileged caller exists outside `internal/sandbox`:
`internal/delegate/capture_isolated_linux.go` uses `CLONE_NEWNET` (CAP_SYS_ADMIN) for
screenshot capture — it must also route through the broker (phase PS-P5).

Stays on the orchestrator (unprivileged): the proxies
(`internal/agentproc/proxies.go`), credential resolution
(`internal/agentproc/credentials.go`, `internal/githubapp`), the agent-host
socket (`cmd/exec/agenthost/`), the live driver, OCI-spec *data* assembly.

---

## 6. Precedents to reuse (do not invent)

- **Subcommand wiring:** add `cap-broker` in `cli.go`'s `dispatchCLI` switch,
  **off** the agent `exec` allowlist (like `hook` / `snapshot-capture`).
- **Self re-exec:** `internal/delegate/capture_isolated_linux.go` already
  re-execs `os.Executable()` as a child with a modified privilege set.
- **Unix socket + protocol + lifecycle:** `cmd/exec/agenthost/` (Start / chmod /
  chown / bind-mount; length-prefixed JSON `protocol.go`; version handshake;
  one-shot conn; `_linux`/`_other` split).
- **Capability policy expression:** `internal/sandbox/spec.go` empty cap sets +
  uid 10000.

Genuinely new, budget for it: exec-time capability drop (no in-process
precedent), the broker supervision loop, the socket-stdio wiring for `runsc`.

---

## 7. Local mode / dialect impact

Local mode (`TF_MODE=local`) skips the sandbox, so it holds no capabilities and
never spawns a broker — the orchestrator is exactly today's process, unchanged.
No SQLite/Postgres schema change in the core split (the hardening track's
`tf_system` role touches Postgres roles only). Every phase must state "local
mode: unchanged."

---

## 8. TFAC-408 interplay

The sandbox-fleet epic customizes three dimensions; each lands on the safe side
of the boundary by construction:

- **Egress** — orchestrator-side (the gating proxy). Zero broker contact, except
  the self-host-only raw-L3-to-private permit, a validated CIDR param to
  `SetupNetwork`.
- **Image/rootfs** — the name→recipe→hash design *is* the broker contract: the
  orchestrator passes a selector; the broker resolves against a catalog it owns
  and mounts by hash, read-only. v1's curated catalog is pre-baked into the
  shipped image.
- **Resources** — validated numeric rlimits in the broker-owned spec.

Forward constraint this epic imposes on 408: once **org-authored recipes** ship
(408 level-2, deferred), the rootfs build executes customer-influenced package
scripts and must run in an isolated/unprivileged builder producing an immutable
hash-addressed image — **never** `apk add <customer input>` in the broker. See
`docs/sandbox-security-architecture.md` §4.3 and sandbox-fleet §4.

---

## 9. Phase plan (children)

Each PR independently mergeable; the split defaults **off** until PS-P4.

**Core split (sequential):**
- **PS-P0** — Extract every privileged sandbox op behind a Go interface;
  in-process implementation; zero behavior change. (Foundation.)
- **PS-P1** — `cap-broker` subcommand + socket RPC protocol + client; broker runs
  the P0 implementation; flag-gated, default off; both paths live.
- **PS-P2** — runsc stdio boundary: socket-by-path passthrough (broker
  execs+supervises the runtime; orchestrator drives via the socket).
- **PS-P3** — broker owns the OCI spec; narrow validated RPC params (§4).
- **PS-P4** — flip default on + exec-time capability drop on the orchestrator.
  Landed with an audit addendum: the §5 audit covered sandbox-infrastructure
  syscalls but missed the file-ownership ops the drop also takes away —
  worktree chown to the sandbox uid (CAP_CHOWN, agentproc), run-tree removal
  at teardown (unlinking through sandbox-owned modes), the capture child's
  setuid (CAP_SETUID, not just its CLONE_NEWNET), and the agenthost socket
  chown + its /run/tf directory write. P4 therefore also brokered the
  run-tree lifecycle (ChownRunTree / RemoveRunTree / CaptureRunDelta, with
  path-shape + tree-ownership validation at the RPC boundary), absorbed
  PS-P5, handed /run/tf to the orchestrator's uid at broker boot, and made
  the agenthost socket grant an owner-legal chgrp via the image's
  tf-sandbox group instead of a chown.
- **PS-P5** — ~~route `capture_isolated` through the broker~~ folded into
  PS-P4 (see above): the capture's *setuid* half broke the moment P4's drop
  landed, not just its netns half, so it could not wait.

**Hardening track (parallel, independent of the split):**
- **PS-H1** — `npm ci --ignore-scripts`.
- **PS-H2** — tailored seccomp profile replacing `seccomp=unconfined`.
- **PS-H3** — control-plane GitHub App-token minting (executors never hold the
  App private key).

Related, tracked elsewhere: least-privilege `tf_system` DB role for executors
(horizontal-scaling §4.4); signed releases + SBOM (TFOPS).

---

## 10. Boundaries

- No rootless/unprivileged-runsc rewrite (research item, worse CISO posture on
  kernels that distrust unprivileged userns).
- No per-sandbox-as-pod / `runtimeClass: gvisor` model (a run is not a pod).
- Container cap **grant** is unchanged — privsep shrinks who *wields* caps, not
  the container ceiling; the "no privileged pod in-cluster" objection is answered
  by topology (`docs/sandbox-security-architecture.md` §7), not this epic.
