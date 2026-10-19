# Sandbox backends — one cell contract, more than one implementation

Design of record for building a delegated run's **cell** through an interface
that more than one isolation technology can implement. The gVisor stack in
`internal/sandbox` is backend 1. A Firecracker-based backend built on
Perplexity's SPACE (`github.com/perplexityai/SPACE`) is the planned backend 2.
A deployment operator chooses which one its executors use.

Status: **accepted design; step 1 (the interface, gVisor behind it, and the
conformance suite) is implemented.** Builds on the sandbox-fleet profiles spec
(`docs/for-agents/specs/sandbox-fleet/`, TFAC-408) and the placement design in
`docs/for-agents/specs/horizontal-scaling/` §6.4.

Scope note: multi mode only. Local mode runs the SDK runtime under bubblewrap
(`internal/agentproc/localsandbox`) with no sidecar and no cell, and stays out
of this interface. Putting it behind the contract would add a fourth
implementation of nothing.

---

## 1. Why more than one backend

Triage Factory sells the triage engine, not the isolation substrate. The gVisor
cell runs on any Linux host with the capabilities a privileged container
grants, and it is the right default for a single-machine or few-machine
deployment. A deployment that already runs microVM infrastructure, or that
wants what a VM gives an agent (root, a container runtime, near-native CPU on
build and test bursts, memory snapshots), should be able to use it without TF
choosing for it.

The cost of two backends is that they drift apart. Two implementations of one
security property eventually disagree, and a bug then exists on one backend
only. Everything in this spec is arranged to keep that from happening, by the
same method TF already uses for its two database dialects: one interface
(`db.Stores` there, `cell.Backend` here) and one conformance suite that every
implementation must pass (`internal/db/dbtest` there,
`internal/cell/celltest` here).

## 2. Vocabulary

- **Cell** — one engagement's isolated environment taken as a unit: its
  network, its credential sidecar, its run-tree grants and its jail. The word
  has meant this in `internal/sandbox` and `cmd/capbroker` since the privsep
  split; the interface adopts it.
- **Jail** — the isolated process at the centre of a cell. For a native
  engagement it runs the resident tool host (`tf-harness-tools serve`).
- **Backend** — the code that builds and reclaims cells. One per executor
  process.

## 3. What is shared and what is the backend's

Everything that is the product is shared and identical on every backend:

| Shared | Where it lives |
| --- | --- |
| The native agent loop | `internal/agentloop`, in the executor process |
| The tool host binary and its frame protocol | `harness/tf-harness-tools`, `agentloop.ToolHost` |
| The credential sidecar and every policy it enforces: git push policy, the gh channel's refusals and audit, the exec verbs, the LLM/git/egress proxies | `cmd/runsidecar`, `internal/gitproxy`, `internal/ghinjector`, `cmd/exec/agenthost` |
| Credential provisioning and sealing | `internal/credprovision`, `internal/credseal` |
| Run-tree contents: checkouts, knowledge, memory, skills | `internal/worktree`, `internal/delegate` |
| The workspace capture format | `internal/worktree` capture manifest |
| Profiles, admission, memory budgets | sandbox-fleet spec, horizontal-scaling §6.4 |

A backend owns only what differs between isolation technologies:

| Backend-specific | gVisor today |
| --- | --- |
| Building and destroying a cell, and reaping orphans at boot | netns + veth + iptables, the per-run uid bands, runsc, all through the cap-broker |
| Where the sidecar's proxies listen, reachable from the host and the jail | the veth's host-side IP |
| Putting the run tree in front of the jail | `ChownRunTree` / `ParkRunTree` hand-off of a host directory |
| Starting the tool host and returning a connection to it | runsc with a bind-mounted unix socket the in-jail host dials |
| Freezing and thawing the jail | `runsc pause` / `resume` |
| Measuring what the jail cost | the per-run memory cgroup |

## 4. The interface (`internal/cell`)

```go
type Backend interface {
	Name() string
	Provision(ctx context.Context, spec ProvisionSpec) (Cell, error)
	ReapOrphans(ctx context.Context) error
}

type Cell interface {
	ProxyHost() string
	Sidecar() Sidecar
	HandOffRunTree(ctx context.Context, root string) error
	Launch(ctx context.Context, spec LaunchSpec) (Jail, error)
	Close() error
}

type Jail interface {
	Connect(ctx context.Context, timeout time.Duration) (net.Conn, error)
	Stop() error
	Exited() bool
	Freeze(ctx context.Context) error
	Thaw(ctx context.Context) error
	OOMKilled() bool
	Close() error
}
```

The method docs in `internal/cell/cell.go` are the contract. The points a
second implementation is most likely to get wrong:

- **`Provision` stops before credentials.** It builds the network and starts
  the sidecar *process*. Provisioning the sidecar (publishing its key,
  relaying the sealed bundle, starting proxies) is backend-neutral protocol
  over `Sidecar().Supervision()`, done by `agentproc.BringUpRunSidecar`, and
  it binds the proxies on `ProxyHost()`.
- **`ProvisionSpec.Step`** lets the caller bracket each internal bring-up step
  with its own stall watchdog. A backend names its steps; gVisor's are
  `sidecar_network`, `sidecar_sockets` and `sidecar_launch`.
- **`Close` order.** The caller stops the jail, closes it, shuts the sidecar
  protocol down (relay stop, audit drain), then calls `Cell.Close`, which
  kills the sidecar process and only then releases the cell's identity. No
  identity a cell held (a uid, an address, a file grant) may reach another
  engagement while anything of this one still carries it.
- **`Stop` versus `Close`.** `Stop` returns once no process of the jail is
  running; `Jail.Close` makes no such promise. An ending that is about to
  read the run tree, and a teardown about to close the cell, call `Stop`
  first: "the kill was sent" is not "nothing is running". On gVisor a cell
  closed under a jail that is still exiting hands its subnet index, and with
  it the next engagement's deterministic container id, to a successor while
  the broker is still cleaning up the predecessor.
- **A jail lives no longer than the context `Launch` was given.** Cancelling
  it kills the jail. The delegate passes the engagement's context, so a
  stopped engagement takes its jail with it; a caller that launches under a
  short bring-up timeout kills the jail when the timeout's cancel runs.
- **`Freeze` that cannot freeze is an error.** A checkpoint that went ahead on
  a jail it believed frozen would read a moving tree.
- **`LaunchSpec.SidecarEndpoints` are the only host addresses the jail can
  reach.** They are the sidecar's jail-facing listeners as the sidecar
  reported them after `StartProxies`. A backend opens exactly these before the
  jail starts and nothing else on the host, the sidecar's host-only listeners
  (the LLM proxy, the API proxies) included.

### 4.1 The jail filesystem contract

Env assembly is shared, so every backend presents the same paths inside the
jail: the run tree at `/work`, TF's binaries at the fixed destinations in
`internal/sandbox/spec.go` (`/opt/tf/bin/...`), the exec-verb socket at
`agenthost.DefaultSocketPath`, staged skills and memory at
`sandbox.TrustedSkillsDestination` / `TrustedMemoryDestination`. A backend
that cannot bind-mount (Firecracker has no shared-filesystem device) copies
or streams those inputs to the same paths. `LaunchSpec` names the inputs by
their host source; the backend decides how they arrive.

## 5. Invariants every backend holds

These are the properties the conformance suite (§9) checks. A backend that
fails one does not meet the contract.

1. **Property B.** No real credential appears anywhere in the jail: its
   environment, its argv, or any file it can read. The jail holds
   placeholders that only the run's own sidecar honours.
2. **The run's sidecar is the jail's only way out.** Every connection the
   jail can complete goes to its own sidecar's proxies. Raw egress to any
   other address fails.
3. **The internal denylist is unreachable** from the jail, directly or
   through the sidecar: cloud metadata, private and link-local ranges, the
   sandbox pool, other cells' sidecars (`internal/egressproxy/denylist.go`).
4. **Cells are isolated from each other.** One jail cannot reach another
   cell's sidecar or read another cell's run tree.
5. **The run tree is the jail's at `/work`.** Writes from the jail land in the
   run tree the caller handed off, and the caller can read them back once the
   jail has stopped.
6. **The jail is not root** unless the backend declares the `root`
   capability (§7). gVisor never does.
7. **Freeze holds every process still; Stop leaves none running.**
8. **Memory ceilings are enforced** where the host allows them, and an OOM
   kill is attributed (`OOMKilled`).
9. **Teardown releases everything.** After a contract-order teardown, a new
   cell for the same conversation provisions and launches cleanly, and the
   boot-time reap runs without error.

## 6. Policy stays in TF on every backend

TF's sidecar is the jail's only way out on every backend (invariant 2), and
the egress policy is enforced once, in TF code:

- **Interception and credential injection are written once, in the sidecar**
  (§6.1). Header-scoped credential injection (sandbox-fleet §7) rides on the
  same termination. gVisor needs interception regardless, so a second backend
  adds no extra enforcement code.
- **A backend's own egress controls are a backstop, never the policy.**
  SPACE ships its own gateway with interception and credential rules. Under
  TF it is configured to admit only the run's sidecar. Using SPACE's gateway
  for SPACE cells and TF's for gVisor cells would put one profile's egress
  rules into two implementations with different rule formats (SPACE accepts
  IPs, CIDRs and wildcard domains; TF's allowlist is exact hostnames).
- **The sidecar authenticates its caller with a per-run token on every
  backend.** Every listener that holds a credential already does: the LLM,
  git and egress proxies, the gh injector and the API proxies. The fetch
  relays hold none and take no token (`internal/egressrelay/relay.go`). On
  gVisor network position isolates cells, since each veth leads to one sidecar
  and the jail is admitted only to its sidecar's ports. On a backend where the
  sidecar is reached over a shared network the token is the only isolation,
  so the relays gain one with that backend.

### 6.1 TLS interception in the sidecar

TFAC-1117. The tunnel lane passes TLS through opaquely, so it acts only on
the hostname in the `CONNECT` line. That leaves two gaps:

- **Writes to allowlisted registries.** `npm publish` is a PUT to
  `registry.npmjs.org`, which is allowlisted. An agent given an attacker's npm
  token in hostile PR text can publish the repository as a public package.
- **Fronting through shared CDN edges.** An allowlisted name can resolve to an
  edge that also serves other sites, and the client inside the tunnel can
  handshake for one of them. SPACE's red team showed it against PyPI's Fastly
  edge.

Closing either means terminating TLS, which reverses the rule in
`internal/egressproxy/CLAUDE.md` and `internal/egressrelay/CLAUDE.md` that no
lane terminates the agent's TLS. That rule kept TF out of the content trust
chain: with TLS end to end, the proxy cannot change what a package manager
downloads. Interception gives that up for the hosts it covers, so it is built
only under these conditions:

1. **A per-engagement CA.** It is generated in the sidecar's memory for each
   engagement and never written to the sealed bundle, the run tree or a
   workspace snapshot. Only its certificate reaches the jail, appended to the
   system roots in the trust file the gh channel already uses
   (`internal/ghinjector/cert.go`), never substituted for them. That file sits
   outside the run tree and is rewritten at every engagement, so a resumed run
   trusts a fresh CA. The CA is valid for hours, not years, and is
   name-constrained to exactly the hosts the tunnel admits. GitHub and the LLM
   provider are never intercepted; they keep their explicit endpoints.
2. **Every host the tunnel admits is intercepted.** A host left opaque keeps
   both gaps.
3. **Pure pass-through.** After the policy decision the upstream body is
   copied verbatim, and nothing in the interceptor can rewrite a body. No
   cache, so one bad response cannot reach another run or a later fetch.
4. **Registries are read-only:** GET and HEAD. This closes the publish path.
5. **One request shape.** Upgrade and a nested `CONNECT` are refused, and so
   are a request with ambiguous framing (both `Content-Length` and
   `Transfer-Encoding`, or conflicting lengths), a GET or HEAD with a body,
   and method-override headers (`X-HTTP-Method-Override` and its variants).
   Read-only means nothing if a GET can be turned into something else.
6. **One hostname end to end.** The leaf the jail sees is minted for the
   `CONNECT` host only, so a client whose SNI differs fails its handshake.
   Every request's `Host` (`:authority` on HTTP/2) must equal the `CONNECT`
   host or the request is refused, because a CDN edge routes on that header
   even when the SNI is right. The upstream `ServerName` is the same host.
   This closes fronting at the TLS layer and the HTTP layer.
7. **Upstream dials go through the tunnel's gate:** `VettedDialContext`
   (resolve on the host, check every address against the internal denylist,
   dial the vetted IP) and the `CONNECT` lane's port rule, 443 only. No second
   resolver or dialer, so DNS rebinding behind an allowlisted name gets no
   further than it does today.
8. **Upstream TLS comes from one constructor with no settings:** system
   roots, Go's default versions and ciphers, nothing read from config or the
   environment. A guard in `scripts/lint.sh` refuses `InsecureSkipVerify` or a
   hand-built `tls.Config` in the interceptor's package, and tests prove that
   a self-signed, expired or wrong-host certificate, and a valid certificate
   for a different allowlisted host, are all refused. Today each client
   verifies its registry itself and a mistake fails loudly; afterwards one
   dial verifies for every toolchain, and a mistake there would weaken all of
   them while each client still saw a valid certificate.
9. **Failures are loud.** An upstream verification failure, and every
   refusal above, is an explicit error to the jail and a recorded denial,
   never a fallback to tunnelling.
10. **Every toolchain the rootfs ships trusts the CA**, each through the
    variable it reads (`SSL_CERT_FILE` for Go, `NODE_EXTRA_CA_CERTS` for Node
    and npm, pip's own, and so on). The list is fixed by a rootfs integration
    test that fetches through the interceptor with each toolchain in
    `internal/sandbox` `apkPackages` (Go, Node and npm, Python and pip, curl),
    so a toolchain added to the rootfs without its variable fails the test.
    The guardrail prompt forbids disabling verification. A toolchain left out
    fails with certificate errors, and an agent's fix for those
    (`strict-ssl=false`, `--trusted-host`) ends up committed to the customer's
    repository.

**Accepted residual.** For npm and pip, a new dependency's bytes now pass
through TF with only TLS behind them. That stays contained. An install outside
TF re-fetches from the real registry and checks the lockfile's hash (`npm ci`
fails with `EINTEGRITY`, pip's hash-checking mode with a mismatch), so a forged
package is caught at the first such install, and an unpinned one was never
recorded. Go is covered even inside the run, because the checksum database's
key ships with the toolchain. What remains is build output committed to a
repository, such as a `dist/` bundle, which can embed a forged dependency that
only a CI job rebuilding it would notice. That is accepted.

GET paths and query strings still carry whatever the jail puts in them. With
`Host` pinned they reach only the registry itself, and the one thing a
registry publishes about reads, the download counts of a package an attacker
owns, is a channel measured in bits per day. That is accepted too, as it is
for the tunnel today.

Two mitigations were considered and not taken. A second-opinion lockfile
check from the control plane would only repeat what the first install outside
TF already does. A separate interceptor process would keep this parsing away
from the run's credentials, but the sidecar already terminates TLS from the
jail (the gh injector) and parses the jail's HTTP in plaintext (the git proxy,
the fetch relays) in that same process, so interception adds more of the
parsing it already does rather than a new kind.

The step that builds this rewrites the two package guides, the relay lane's
integrity prerequisite and `docs/security/sandbox-egress.md` to match. Until
then they describe the code as it is.

## 7. Capability differences are data

Backends differ in what an agent can do inside a cell. Those differences are
declared and matched, never branched on:

- **Backends declare capabilities.** Initial vocabulary: `root` (the agent
  runs as uid 0 inside its jail), `container_runtime` (it can run containers),
  `memory_checkpoint` (a parked jail can be suspended with its memory).
  gVisor declares none.
- **Profiles declare requirements.** A profile whose requirements no backend
  the deployment declares can meet is refused at save (422 `INVALID_FIELD`).
- **Placement matches them.** Each executor records its backend and
  capabilities on its `instances` row. A profile's requirements become an
  eligibility term on the claim — the eligibility layer horizontal-scaling
  §6.4 already defines ("eligibility is a constraint"). Mixed fleets then
  work without branching code.
- **The agent learns from `<run_context>`.** An execution manifest is stamped
  on the claim the way `claims.source_manifest` is, and rendered as one line
  per capability. The `agentprompt` blocks stay byte-identical across
  backends.

None of §7 is built in step 1: nothing consumes a capability yet, and the
interface carries no capability method until the first consumer lands.

## 8. Snapshots and resume need no backend ratchet

Unlike `conversations.runtime`, the backend is not stamped on a conversation.
The durable record of a parked conversation is TF's run-tree capture, in the
one capture format every backend produces. A backend's own memory checkpoint
(SPACE's suspend) is a cache for a warm resume on the same deployment and is
discarded otherwise. A parked conversation therefore resumes on any backend
its profile is eligible for. Service state (a database running inside the
cell) does not survive a resume onto another backend, which matches the
fleet spec's position that services are rebuilt from seed data per
engagement.

## 9. The conformance suite (`internal/cell/celltest`)

`celltest.Run(t, harness)` drives a backend through `cell.Backend` and the
tool host exactly as a native engagement does — provision, bring the sidecar
up with a sealed test bundle, hand off a run tree, launch the tool host,
connect, and issue tool calls over the frame protocol — and asserts the §5
invariants from inside the jail. A backend wires it from its own package:

```go
//go:build linux && integration
func TestConformance(t *testing.T) { celltest.Run(t, harnessForThisBackend(t)) }
```

The gVisor wiring is `internal/cell/gvisor/conformance_linux_test.go`. It
starts a real cap-broker and launches real sidecars, exactly as an executor
does, so the suite exercises the privilege-separated path rather than an
in-process stand-in.

The cases, one per invariant except 8:

| Case | Invariant |
| --- | --- |
| `PropertyB` | 1 — four canaries (LLM key, PAT, gh CLI token, repo token) absent from the jail's env, `/proc/1/environ`, `/proc/1/cmdline`, and every file under `/work /run /opt /etc /tmp /root /home /usr/local/bin`; the sidecar's placeholders present as the positive control |
| `OnlyTheSidecarIsReachable` | 2 — the jail reaches every endpoint its sidecar reported, and does not reach the sidecar's LLM or GitHub API proxy or a listener the executor binds on all interfaces |
| `InternalDenylistUnreachable` | 3 — cloud metadata, the internet directly, and every executor address are closed |
| `CellsAreIsolated` | 4 — two live cells; neither jail reaches the other's sidecar or sees the other's run tree |
| `RunTreeAtWork` | 5 — the caller's seed is at `/work`; the jail's write is in the caller's tree after `Stop` |
| `JailIsNotRoot` | 6 |
| `FreezeHoldsTheJailStill`, `StopLeavesNothingRunning` | 7 |
| `TeardownReleasesTheCell` | 9 — two engagements of one conversation back to back, then a reap |

Invariant 8 stays covered by `internal/sandbox`'s own integration tests for
now: a memory case needs a host that grants a private cgroup namespace, and
none of the hosts the suite has run on did.

Every network negative is paired with a positive control (the jail reaches
its own sidecar), so a broken probe cannot pass as a closed door. Probes run
`nc -z` inside the jail through the tool host's `bash` tool; `nc` is in
busybox, so any Alpine-based rootfs has it.

Prerequisites follow the existing sandbox integration tests: root (or the
broker's capabilities), `runsc`, `ip`, `iptables`, a built rootfs, and the
tool host binary at `sandbox.TrustedToolHostBinaryPath()`. A missing
prerequisite skips the suite with the reason, the way `pgtest` skips without
Docker. Run it with:

```bash
go test -tags integration ./internal/cell/gvisor/
```

`scripts/test-sandbox-linux.sh` runs it alongside the sandbox integration
suite.

## 10. Order of work

1. **Extract the interface with gVisor behind it, and the suite.** *Done.*
   The extraction changes no behaviour. The suite passes on gVisor; it found
   two defects, both fixed in this step (§11).
2. **TLS interception in the sidecar** (§6.1, TFAC-1117).
3. **The SPACE backend**, built against the suite, with a per-run token on
   the fetch relays (§6). Starts with the spike in §13.
4. **Destination-scoped credential injection** (sandbox-fleet §7), on the
   termination step 2 builds.
5. **Capabilities, profile requirements and claim eligibility** (§7).

## 11. What the suite found on gVisor

**Every port of the gateway was open to the jail.** The host-side veth rule
(`applyEgressPolicy` layer 2) dropped jail traffic whose destination was not
the cell's gateway IP, but accepted every *port* on that IP. Anything the
executor bound on all interfaces was reachable from every jail at
`<gateway>:<port>`. In multi mode that included the `/metrics` listener, which
binds `:9464` by default (`internal/telemetry`) and is unauthenticated, and
the sidecar's own LLM and API proxies, which no jail is pointed at. The
executor's healthz listener is loopback-only and was unaffected.

It is closed by admitting only the sidecar's jail-facing ports:

- Layer 2 drops every packet from the jail, including those to the gateway
  IP. A cell whose ports were never admitted reaches nothing on the host.
- The sidecar reports the listeners it points the jail at
  (`StartProxiesResult.JailEndpoints`): the egress proxy, each fetch relay,
  the git proxy, and the gh injector or shared origin.
- `Cell.Launch` admits those ports before the jail starts. On gVisor that is
  `RunNetwork.AdmitJailPorts`, a broker RPC (protocol version 4) that installs
  one ACCEPT per port at the head of the veth's INPUT chain. The broker derives
  the veth and gateway from (conversation, subnet index), refuses a veth that
  does not exist, and caps a cell at `sandbox.MaxJailPorts`. The rules are
  recorded with the network's state, removed at teardown, and matched by the
  boot-time reaper.

Giving the sidecar its own network namespace, holding the veth's host end so
the jail's only neighbour is a namespace in which only the sidecar listens,
would close the same hole structurally. It is a larger privsep change and was
not needed for this fix.

**Broker run registry.** `waitRun` (`cmd/capbroker/runlaunch_linux.go`)
deleted `s.runs[containerID]` without checking the entry was still the one it
waited on. Container ids are deterministic per (conversation, subnet index),
so a successor launched under the same id before the predecessor's `WaitRun`
returned lost its registry entry ("unknown run"). Production did not reach it,
because every ending stops the jail and `Jail.Close` waits for the reap when
it records actuals, but the suite hit it once while its teardown skipped
`Stop`. `forgetRun` now deletes the entry only if it is still the one waited
on.

## 12. What step 1 leaves gVisor-only

Step 1 moves the cell lifecycle behind the interface and nothing else. These
paths still reach `internal/sandbox` directly, and each becomes an interface
member when the SPACE backend needs it:

- **Run-tree residency.** The run tree is a host directory built by
  `internal/worktree` and handed to the jail. Firecracker has no shared
  filesystem, so a VM backend needs the tree copied in at launch and out at
  capture. `HandOffRunTree(root)` is the seam where that sync goes.
- **Capture.** Checkpoints and endings capture the run tree with a
  broker-launched capture jail reading the host directory
  (`sandbox.CaptureRunTree`, `internal/delegate/capture_jail.go`). A VM
  backend captures inside the guest and streams the same manifest out. This
  becomes `Cell.CaptureTree`.
- **Staging grants.** Skills and memory staging directories are granted to
  the jail identity (`RunNetwork.GrantStaging`) inside the gVisor launch.
- **Resource sampling.** The sandbox-stats sampler reads each live jail's
  cgroup by container id (`agentproc.LiveJails`, `sandbox.SampleRunCgroup`).
  This becomes `Jail.Sample`.
- **The tool-host launch code.** `agentproc.LaunchToolHost` stays in
  `internal/agentproc` and is reached only through the gVisor backend. It
  shares env assembly and mount helpers with the rest of that package, so
  moving it into `internal/cell/gvisor` is a later cleanup with no
  behavioural content.
- **Sidecar identity.** The sidecar's uid derives from the cell's subnet
  index (`sandbox.SidecarUID`). A backend with no subnet index needs its own
  allocator for the sidecar uid band.

## 13. The SPACE backend: shape and open questions

SPACE runs each sandbox in a Firecracker microVM with root inside, controlled
over an HTTP API (create, process sessions with attached streams, file
read/write, port and protocol sessions, pause/suspend/checkpoint/restore). It
needs KVM, btrfs and its own Postgres, controllers and object storage. Fly
Machines do not expose `/dev/kvm`, so executors on a SPACE backend run on
bare metal, metal cloud instances, or a provider that operates SPACE.

The mapping, and what has to be proven before building it:

| Contract member | SPACE mapping | Open question |
| --- | --- | --- |
| `Provision` | Launch the sidecar on the executor (broker, per-run uid); create the SPACE sandbox with a network policy admitting only that sidecar | Does SPACE's platform guardrail deny private ranges regardless of policy? If so, the sidecar is reached over the tool host's stream instead (next row) |
| `ProxyHost` | An executor address the guest can reach, or a guest-local port the tool host forwards over its stream | Throughput of the data plane for package downloads |
| `HandOffRunTree` | Copy the host tree into the guest (file API or a tar stream through a process session) | Cost for a large checkout; delta sync on a warm resume |
| `Launch` / `Connect` | Start `tf-harness-tools serve` as a long-lived process session; the executor attaches to its stdio stream | Per-tool-call round trip versus the unix socket, measured on a recorded real run against a budget set first |
| Exec verbs (`/run/tf.sock`) | Carried over the tool host's stream to the executor, which forwards to the sidecar | None structural; a framing change in the tool host |
| `Freeze` / `Thaw` | SPACE pause / wake | — |
| `Stop` / `Close` | Kill and delete the sandbox | — |
| `OOMKilled`, actuals | SPACE stats | Whether peak memory is reported |
| `ReapOrphans` | List sandboxes labelled with this executor's instance id and delete them | — |

Spike items, each with a pass/fail result written back here:

1. Tool host as a SPACE process session: per-call latency on a replayed run.
2. A `git push` from the guest through the run's sidecar, with push policy
   still enforced.
3. A parked conversation suspended with a database running inside, resumed
   on another node.
4. Idle memory per cell versus gVisor's (~0.5 GB planning budget).
5. One week of TF's own delegations on one bare-metal SPACE host.
6. The conformance suite passing against the SPACE backend unmodified.

## Related

- `docs/for-agents/specs/sandbox-fleet/` — profiles; §3 and §7 are the egress
  and credential design §6 here implements.
- `docs/for-agents/specs/horizontal-scaling/` §6.4 — eligibility, admission
  and preference on the claim.
- `docs/security/privilege-separation.md` — the four-process split every
  backend preserves.
- `docs/security/sandbox-egress.md` — the egress lanes; §6.1 changes the
  tunnel lane.
- `docs/benchmarks/gvisor-cpu-tax.md` — why a VM backend is attractive for
  build and test bursts, and why gVisor stays the portable default.
