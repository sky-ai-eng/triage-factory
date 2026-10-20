# Sandbox backends — one cell contract, more than one implementation

Design of record for building a delegated run's **cell** through an interface
that more than one isolation technology can implement. The gVisor stack in
`internal/sandbox` is backend 1. Backend 2 is Firecracker microVMs, built and
destroyed by TF's own cap-broker on the executor's host, the way it drives
`runsc` today. A deployment operator chooses which backend each executor uses.

Status: **accepted design; step 1 (the interface, gVisor behind it, and the
conformance suite) is implemented.** Builds on the sandbox-fleet profiles spec
(`docs/for-agents/specs/sandbox-fleet/`, TFAC-408) and the placement design in
`docs/for-agents/specs/horizontal-scaling/` §6.

This is for multi-tenant mode only. Local mode runs the SDK runtime with no sidecar
and no cell, and stays out of this interface.

---

## 1. Why more than one backend

Triage Factory's value is the triage engine, not the sandboxing. The gVisor
cell runs on any Linux host with the capabilities a privileged container
grants, and it stays the default: it needs no `/dev/kvm`, so it runs on
ordinary cloud VMs and locked-down hosts where a microVM cannot
(`docs/benchmarks/gvisor-cpu-tax.md` rules a microVM out as the default for
that reason). A deployment that has KVM hosts and wants what a VM gives an
agent (root, a container runtime, near-native CPU on build and test bursts,
memory snapshots) should be able to use them without TF choosing for it. The
two run side by side: a few executors on KVM hosts for the profiles that need
a VM, and a gVisor fleet for the rest (§7, §12.1).

Measured on one host (§12.6), a Firecracker cell starts as fast as a gVisor
one and runs a `go test` burst at native wall time on 1.19 times native CPU,
where gVisor under systrap costs about 14 times native CPU on syscall-heavy
work (`docs/benchmarks/gvisor-cpu-tax.md`). In exchange an idle cell holds
about 55 MB of host memory, against gVisor's 24 MB.

The cost of two backends is that they drift apart. Two implementations of one
security property eventually disagree, and a bug then exists on one backend
only. Everything in this spec is arranged to keep that from happening, by the
same method TF already uses for its two database dialects: one interface
(`db.Stores` there, `cell.Backend` here) and one conformance suite that every
implementation must pass (`internal/db/dbtest` there,
`internal/cell/celltest` here).

### 1.1 Why TF drives Firecracker itself

Perplexity's SPACE (`github.com/perplexityai/SPACE`) was considered as
backend 2. It is not a library around Firecracker but an orchestration
platform: an API that records desired state in its own Postgres, controllers
that reconcile it, agents on every node, a data plane that carries process
streams to clients, template builders and object storage, deployed through
Kubernetes or a direct adapter. TF already does each of those jobs. The
instance registry and placement decide which executor takes a run
(horizontal-scaling §6), claims and leases track it, the cap-broker builds the
cell, and the sidecar enforces egress. SPACE under TF would mean:

- two schedulers, SPACE's choosing the node a VM lands on and TF's choosing
  the executor that claims the run, with nothing keeping them in agreement;
- a network policy TF does not write, on the path where invariant 2 (§5) has
  to hold;
- an HTTP data plane between the executor and the tool host, where gVisor has
  a unix socket;
- either a SPACE control plane on every KVM host, or VMs on a different
  machine from their executor, whose admission would then measure a machine
  the VMs do not use.

Measuring SPACE on one KVM host found more:

- SPACE's per-sandbox nftables drop guest connections to every RFC 1918 range
  before tenant policy is read, so a guest reaches a sidecar on the executor
  only through SPACE's data plane, which a hard-coded per-sandbox limiter
  caps at about 55 MB/s. Its egress policy names addresses, never ports, so
  it could not admit one sidecar port anyway.
- The bidirectional stream the tool host needs is an Incubating API, and a
  call sent to a paused sandbox closes it.
- Its public API cannot place a sandbox on a chosen node, so the same-host
  layout (§12.1) is not available through it.
- An idle sandbox costs about 104 MB of host memory, against gVisor's ~24 MB,
  and bring-up takes a p50 of 1.3 s, against gVisor's ~171–292 ms.
- SPACE's stats report neither the guest's memory nor its OOM kills.

Firecracker is the part that runs one VM: one process per microVM, configured
over a unix-socket API, with `jailer` to confine that process on the host.
Everything above one VM is already TF's, so TF drives Firecracker directly.
SPACE is an Apache-2.0 implementation of agent sandboxes on Firecracker, so
TF reads it and takes code from it (§12.5) without running any of its
services.

## 2. Vocabulary

- **Cell** — one engagement's isolated environment taken as a unit: its
  network, its credential sidecar, its run-tree grants and its jail. The word
  has meant this in `internal/sandbox` and `cmd/capbroker` since the privsep
  split; the interface adopts it.
- **Jail** — the isolated process at the centre of a cell. For a native
  engagement it runs the resident tool host (`tf-harness-tools serve`). On
  gVisor it is the `runsc` sandbox; on Firecracker it is the microVM: the VMM
  and the guest it runs.
- **VMM** — the `firecracker` process that runs one microVM.
- **Guest** — the kernel and userspace inside a microVM.
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
| The cell's network: netns, veth, the gateway rules | `internal/sandbox` `RunNetwork`, through the cap-broker |
| Profiles, admission, memory budgets | sandbox-fleet spec, horizontal-scaling §6.4 |

A backend owns only what differs between isolation technologies:

| Backend-specific | gVisor | Firecracker |
| --- | --- | --- |
| Starting and destroying the jail, and reaping orphans at boot | `runsc` at the jail uid, through the cap-broker | the VMM under `jailer` at the jail uid, through the cap-broker, with a tap device in the cell's netns for the guest's network card; orphans found by their jailer directories |
| Putting the run tree in front of the jail | `ChownRunTree` / `ParkRunTree` hand-off of a host directory | a disk image built from the host tree at launch, copied back out at `Stop`, with host writes pushed in between (§12.3) |
| Delivering TF's inputs (binaries, trust file, skills, memory) | bind mounts | read-only disk images built at launch |
| Starting the tool host and returning a connection to it | runsc with a bind-mounted unix socket the in-jail host dials | the guest's init starts the tool host, which dials the executor over vsock; the exec verbs use a second vsock port to the sidecar |
| Freezing and thawing the jail | `runsc pause` / `resume` | the VMM's pause and resume |
| Keeping a parked jail's memory | not supported | a Firecracker snapshot on the executor's disk (§8) |
| Measuring what the jail cost | the per-run memory cgroup | the cgroup `jailer` puts the VMM in, plus out-of-memory kills the guest reports |

§12 maps each contract member and lists what each row still has to prove.

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
`sandbox.TrustedSkillsDestination` / `TrustedMemoryDestination`. `LaunchSpec`
names the inputs by their host source; the backend decides how they arrive.
Firecracker has no shared-filesystem device, so its backend attaches them as
read-only disk images mounted at the same paths (§12.3).

## 5. Invariants every backend holds

These are the properties the conformance suite (§9) checks. A backend that
fails one does not meet the contract.

1. **Property B.** No real credential appears anywhere in the jail: its
   environment, its argv, or any file it can read. The jail holds
   placeholders that only the run's own sidecar honors.
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
6. **The jail is not root** unless the engagement's profile requires the
   `root` capability and its backend declares it (§7). gVisor never declares
   it.
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
- **A backend brings no egress controls of its own.** A Firecracker guest's
  only network device is a tap in the cell's netns, so its traffic leaves
  through the same veth and the same host rules as a gVisor jail's. A backend
  that came with its own egress gateway would configure it to admit only the
  run's sidecar, as a backstop and never as the policy, so that one profile's
  egress rules never live in two implementations.
- **The sidecar authenticates its caller with a per-run token on every
  backend.** Every listener that holds a credential already does: the LLM,
  git and egress proxies, the gh injector and the API proxies. The fetch
  relays hold none and take no token (`internal/egressrelay/relay.go`). On
  gVisor and on Firecracker, network position isolates cells: each veth leads
  to one sidecar, and the jail is admitted only to its sidecar's ports. A
  backend whose jails reached their sidecars over a shared network would have
  only the token, so the relays would gain one before such a backend shipped.

### 6.1 TLS interception in the sidecar

TFAC-1117. The tunnel lane passes TLS through opaquely, so it acts only on
the hostname in the `CONNECT` line. That leaves two gaps:

- **Writes to allowlisted registries.** `npm publish` is a PUT to
  `registry.npmjs.org`, which is allowlisted. An agent given an attacker's npm
  token in hostile PR text can publish the repository as a public package.
- **Fronting through shared CDN edges.** An allowlisted name can resolve to an
  edge that also serves other sites, and the client inside the tunnel can
  handshake for one of them. The red team of Perplexity's SPACE project showed
  it against PyPI's Fastly edge.

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
4. **Registries are read-only:** GET and HEAD, plus POST to the paths a
   registry serves reads on, listed per registry as data and probed like any
   allowlist entry. For npm that is the audit endpoints `npm install` calls by
   default (`/-/npm/v1/security/advisories/bulk`, and
   `/-/npm/v1/security/audits/quick` it falls back to). With `Host` pinned
   that body reaches only the registry's own advisory service. Every other
   POST, and every PUT, PATCH and DELETE, is refused. This closes the publish
   path.
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
  gVisor declares none. Firecracker declares `root` and `memory_checkpoint`,
  and `container_runtime` when its guest image ships one.
- **Profiles declare requirements.** A profile whose requirements no backend
  the deployment declares can meet is refused at save (422 `INVALID_FIELD`).
- **A cell grants what the profile requires, and no more.** A profile that
  does not require `root` runs its agent unprivileged on Firecracker too.
  Otherwise a conversation whose first claim landed on a VM would come to rely
  on root, then lose it when a later claim resumed on gVisor. With the grant
  tied to the requirement, every claim of a conversation gives the agent the
  same environment, and the eligibility term below makes sure the backend can,
  until a person moves the conversation to another profile (sandbox-fleet
  §6).
  `container_runtime` implies `root` inside the guest, because a user who can
  drive the container runtime is root there anyway. `memory_checkpoint` is
  invisible to the agent: it is just a cache the backend uses when it parks a 
  jail (§8).
- **Three records, one per question.** What an executor can build goes on its
  `instances` row with its backend name, written at each boot. What a run
  needs is in the copy of its profile frozen when the conversation was minted,
  which the conversation references (sandbox-fleet §6), so editing a profile
  does not change work already queued or parked. What an engagement
  got is an execution manifest on the claim, next to
  `claims.source_manifest`: the backend and the capabilities granted. It is
  per claim because one conversation's claims can land on different backends
  (§8) or run under different profile copies after a move, and the
  `instances` row holds only the current boot's values.
- **Placement matches them.** A run's requirements become an eligibility term
  on the claim, the layer horizontal-scaling §6.4 already defines
  ("eligibility is a constraint"): an aged run never spills to an executor
  that cannot run it, the enqueue-time stamp ranks only eligible executors,
  and a run no live executor can serve fails fast with a `failure_kind`.
  Mixed fleets then work without branching code.
- **The agent learns from `<run_context>`, then from the conversation.**
  `<run_context>` is composed once, by the claim that launches the
  conversation, and every later claim sends the same bytes
  (`conversations.system_block`). It renders that claim's execution manifest
  as one line per granted capability. After a move to another profile, the
  next claim injects a message into the conversation naming the capabilities
  gained and lost, as the workspace-restored notice is injected
  (`injection:executor-changed`, `internal/agentloop/repair.go`). The message
  is always sent, because the turns above it were taken in the old
  environment. The first claim after the move that finds the provider's cache
  cold also recomposes `<run_context>` for the new profile, since a cold cache
  re-reads the whole prompt anyway. Warmth is derived the way compaction
  derives it (`warmCacheLive`: the newest assistant message's age against the
  cache TTL), and a wrong guess costs one uncached read, never correctness.
  The `agentprompt` blocks stay byte-identical across backends.

None of §7 is built in step 1: nothing consumes a capability yet, and the
interface carries no capability method until the first consumer lands.
In fact, we'll probably wait to ship this until we at least scaffold the
sandbox profiles.

## 8. Snapshots and resume need no backend ratchet

Unlike `conversations.runtime`, the backend is not stamped on a conversation.
The durable record of a parked conversation is TF's run-tree capture, in the
one capture format every backend produces. A backend's own memory checkpoint
is a cache for a warm resume on the same host and is discarded otherwise. A
parked conversation therefore resumes on any backend its profile is eligible
for.

A cold resume restores what the capture carries and nothing else: each
checkout as a git delta (its HEAD, a bundle of its local commits, and a patch
of every uncommitted change `git add -A` would stage, so gitignored files are
left out), plus the run root's `_tfac` scratch (`internal/worktree/snapshot.go`,
`internal/worktree/scratch.go`). A service's state survives a cold resume only
if it is in that set. A database whose data is in a container runtime's
volume, outside the run tree, does not, and neither does one whose data
directory is a gitignored path in a checkout. Capturing service volumes is not
designed. It would need a capture format of its own, because a data directory
is large binary state that a git patch carries badly, and a running
database's files are at best crash-consistent. A warm resume keeps everything,
since the guest's memory and disks come back as they were. An engagement that
starts in a rebuilt tree after earlier work is told so by an injected notice
(`injection:executor-changed`).

Anything a backend keeps for a conversation beyond the capture is a cache: no
resume may depend on it. That rule is what keeps a move to another profile
(sandbox-fleet §6) possible. The run tree comes back from the capture on any
backend. A move discards every cache the new profile cannot use, and always
the memory snapshot, since a restore needs the same VM configuration.
Whatever the old profile granted and the new one does not, such as a
container runtime's images and volumes, is lost with those caches.

On Firecracker the checkpoint is a snapshot: the guest's memory and the VM's
device state, taken with the VM paused, kept together with the run-tree disk
image, which Firecracker leaves to its caller. Placement already sends a
resume to the machine that holds it: a resume is stamped with the executor of
the conversation's newest claim (horizontal-scaling §6.2), which on the
same-host layout (§12.1) is the host with the snapshot. If that executor is
dead or draining, the run goes to another eligible executor and restores cold
from the capture. A snapshot is used only when all of these hold, and is
deleted otherwise:

- **Nothing has written the run tree since it was taken.** A task's
  conversations share one run tree, so another conversation may have run in
  between; the tree's latest capture must be the one taken at that park.
- **The host still runs the Firecracker and kernel it was taken on.**
  Firecracker restores a snapshot only onto identical software and hardware.
- **The conversation still references the profile copy it was taken
  under.** A move to another profile deletes it.
- **It has not been restored before.** A snapshot is restored at most once,
  never into a second VM, so the duplicated guest state Firecracker's
  documentation warns about (random-number state, tokens, identifiers) cannot
  arise. A warm pool booted from one base snapshot would raise it, and none is
  planned.

A restore starts the VMM under a fresh `jailer` directory holding the same
images and snapshot files, and the guest resumes where it stopped, with two
exceptions the backend handles. Firecracker resets every vsock connection on
restore, so the tool host redials and the executor accepts on the new
directory's socket. And the guest's clock did not move while the VM was down,
so the init steps it from the host before the next tool call; otherwise TLS
validity checks, git timestamps and token expiry in the guest are off by the
downtime.

Snapshots stay on the executor's disk, owned by the broker, and are never
uploaded. They hold what the guest held: the run's code and its placeholders,
never a real credential (invariant 1). A snapshot's memory file is the
guest's whole configured memory, fully allocated, so the disk budget they
count against is in configured memory, not memory touched. They are evicted
oldest first; an evicted snapshot costs a cold resume and nothing else.

The run-tree image keeps Firecracker's default drive cache mode, which does
not pass the guest's flushes through to the host's disk. Everything durable
leaves the VM in a capture or the copy-out at `Stop` (§12.3), so a host crash
loses only the image and the snapshots, which are caches.

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
in-process stand-in. The Firecracker backend wires it the same way from
`internal/cell/firecracker`.

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
none of the hosts the suite has run on did. When §7 lands, the suite gains a
case per capability, run on the backends that declare it.

Two cases hold a VM backend to the hard parts of its design: `RunTreeAtWork`
reads the jail's write back after `Stop`, which fails unless `Stop` copies the
tree out (§12.3), and the network cases run against the tap with the host's
rules unchanged.

Every network negative is paired with a positive control (the jail reaches
its own sidecar), so a broken probe cannot pass as a closed door. Probes run
`nc -z` inside the jail through the tool host's `bash` tool; `nc` is in
busybox, so any Alpine-based rootfs or guest image has it.

Prerequisites follow the existing sandbox integration tests: root (or the
broker's capabilities), `ip`, `iptables`, a built rootfs, and the tool host
binary at `sandbox.TrustedToolHostBinaryPath()`, plus `runsc` for gVisor, and
`/dev/kvm`, the `firecracker` and `jailer` binaries and the guest kernel for
Firecracker. A missing prerequisite skips the suite with the reason, the way
`pgtest` skips without Docker. Run it with:

```bash
go test -tags integration ./internal/cell/gvisor/
```

`scripts/test-sandbox-linux.sh` runs it alongside the sandbox integration
suite.

## 10. Order of work

1. **Extract the interface with gVisor behind it, and the suite.** *Done.*
2. **TLS interception in the sidecar** (§6.1, TFAC-1117).
3. **The Firecracker backend**, built against the suite. The spike items
   that could run without it pass (§12.6); the rest run against it. It
   isolates cells by network position as gVisor does, so it does not wait for
   a token on the fetch relays (§6).
4. **Capabilities, profile requirements and claim eligibility** (§7). Until
   this lands a Firecracker executor runs whatever it claims as a gVisor one
   would, unprivileged: a VM pool already gives build bursts near-native CPU,
   but grants nothing a profile could ask for.
5. **Destination-scoped credential injection** (sandbox-fleet §7), on the
   termination step 2 builds.

## 11. What step 1 leaves gVisor-only

Step 1 moves the cell lifecycle behind the interface and nothing else. These
paths still reach `internal/sandbox` directly, and each becomes an interface
member when the Firecracker backend needs it:

- **Run-tree residency.** The run tree is a host directory built by
  `internal/worktree` and handed to the jail, and host code writes into it
  while the jail runs. A VM backend copies it in at launch and out at `Stop`,
  and pushes host writes in between (§12.3). `HandOffRunTree(root)` is the
  seam for the copy in and `Stop` already precedes every read of the tree,
  so only the push becomes a new contract member.
- **Capture.** Checkpoints and endings capture the run tree with a
  broker-launched capture jail reading the host directory
  (`sandbox.CaptureRunTree`, `internal/delegate/capture_jail.go`). On
  Firecracker the capture jail is a capture microVM reading the run-tree image
  (§12.3). This becomes `Cell.CaptureTree`.
- **Staging grants.** Skills and memory staging directories are granted to
  the jail identity (`RunNetwork.GrantStaging`) inside the gVisor launch. On
  Firecracker they become read-only input images.
- **Resource sampling.** The sandbox-stats sampler reads each live jail's
  cgroup by container id (`agentproc.LiveJails`, `sandbox.SampleRunCgroup`).
  This becomes `Jail.Sample`. `jailer` puts the VMM in a cgroup too, so the
  cgroup read carries over.
- **The tool-host launch code** (TFAC-1119). `agentproc.LaunchToolHost` stays
  in `internal/agentproc` and is reached only through the gVisor backend. It
  shares env assembly and mount helpers with the rest of that package, so
  moving it into `internal/cell/gvisor` is a later cleanup with no
  behavioural content.
- **Sidecar identity.** The sidecar's uid derives from the cell's subnet
  index (`sandbox.SidecarUID`). The Firecracker backend builds its network
  through `RunNetwork` and so has a subnet index, and its uid bands derive as
  gVisor's do. Only a backend without a subnet index would need its own
  allocator.

## 12. The Firecracker backend

### 12.1 Where it runs

An executor on the Firecracker backend runs on the KVM host whose VMs it
manages, with its cap-broker and its sidecars, as a gVisor executor runs
beside its jails. One host runs many microVMs, one VMM process each, and an
executor holds many cells as it does on gVisor. That keeps:

- **admission accurate**, because the executor's memory heartbeat and
  dispatch gate measure the machine the VMs use;
- **sidecar traffic on the machine**, because the guest reaches its sidecar
  over a veth, as a gVisor jail does;
- **warm resume reachable**, because the executor a resume is stamped for is
  the one holding the snapshot (§8);
- **one unit of scale**: a deployment grows by adding executors, on KVM hosts
  or not, and nothing new joins the fleet for placement or autoscaling to
  learn.

A deployment that wants both runs two groups of the same executor role: a few
on KVM hosts on the Firecracker backend, and a gVisor fleet elsewhere, drawing
from one queue under §7's eligibility.

KVM hosts are bare metal, metal cloud instances, or VMs with nested
virtualization. Fly Machines do not expose `/dev/kvm`, so a deployment on Fly
runs its Firecracker executors elsewhere. Each host runs cgroup v2 and carries
the `firecracker` and `jailer` binaries and a guest kernel, all pinned by
version and checksum.

### 12.2 The contract, member by member

| Contract member | Firecracker | Open question, and the spike's answer |
| --- | --- | --- |
| `Provision` | `RunNetwork` as on gVisor (netns, veth, gateway rules, IPv6 off), plus a tap device in the netns bridged to the veth's inner end, so the guest's network card carries the jail IP. The broker launches the sidecar at its uid as today | Does the bridge pass the network conformance cases with the host rules unchanged? **Yes.** With `SetupRunNetwork` and `AdmitJailPorts` unchanged, the guest reaches only its admitted sidecar port, before and after it changes its own address and MAC as root, IPv6 included |
| `ProxyHost` | The veth's host-side IP | — |
| `HandOffRunTree` | Builds the run-tree image from the host tree (§12.3) | Cost on a large tree: **1.5 s** for `mkfs.ext4 -d` at the jail uid on 48,744 entries and 469 MB, 0.13 s on a fresh checkout, with owners, modes, symlinks and hard links kept |
| `Launch` | The broker runs `jailer`, which chroots the VMM, gives it private mount and pid namespaces, joins it to the cell's netns, puts it in a cgroup and drops it to the jail uid and gid. The VM's configuration comes from a fixed template, as the OCI spec does on gVisor: the guest kernel, the read-only rootfs image, the read-only input images, the run-tree image, one network card on the tap, one vsock device, memory and vCPUs from the profile's class, and Firecracker's metadata service off | Bring-up latency against gVisor's: **the same.** `jailer` exec to the tool host's first answer p50 154 ms, p95 181 ms; with the cell built first, as gVisor's spawn counts it, p50 282 ms against gVisor's ~171–292 ms |
| `Connect` | The tool host dials the host over vsock. Firecracker hands a guest's connection to port N to the unix socket `<vsock path>_N`, on which the executor listens inside the jailer directory | Round trip per tool call against the unix socket: **0.04 ms p50, 0.06 ms p95** for a no-op call, the same as a unix socket on the host |
| Exec verbs (`/run/tf.sock`) | A relay in the guest listens on `/run/tf.sock` and dials a second vsock port, whose host socket is the sidecar's agenthost socket | — |
| `Freeze` / `Thaw` | `Freeze` asks the init to freeze `/work`, then pauses the VMM over its API socket, which only the broker uses; `Thaw` resumes the VMM, then unfreezes `/work`. If the init does not confirm, the VMM is paused anyway, so every process is still held still, and a capture reads what the guest last flushed; only that run's capture is affected | Measured: freezing `/work` takes 51–57 ms and the pause about 1 ms; a tool call sent while paused waits and is answered after `Thaw` |
| `Stop` | Stop the VM, wait for the VMM to exit, then copy the run tree out (§12.3) | Cost of the copy-out: **2.2 s** for the large tree whole and 0.33–0.40 s for changed paths only, after a capture-VM boot of about 0.15 s; 0.23–0.31 s and 22 ms on a fresh checkout |
| `Close` | Delete the jailer directory and the images; network teardown as on gVisor | — |
| `OOMKilled`, actuals | The actuals come from the VMM's cgroup: CPU, and a `memory.peak` that is the most guest memory the VM touched, which is what it held on the host. The cgroup's limit sits above guest memory plus VMM overhead, so the guest's own OOM killer acts first and the cgroup records nothing. The init reports the guest's `/proc/vmstat` `oom_kill` count over vsock; `OOMKilled` is that count, or the cgroup's own `oom_kill`, which means the host killed the VMM and the cell is gone | Answered: an OOM inside the guest leaves the cgroup's events at zero |
| `ReapOrphans` | Kill VMMs and delete jailer directories the broker's ledger does not hold; reap netns as on gVisor | — |

A guest reaches the host over vsock only at sockets that exist under its own
jailer directory, so the host listeners it can reach are the two TF puts
there: the executor's tool-host listener and the sidecar's agenthost socket.
A gVisor jail reaches the same pair through its bind-mounted sockets.

Bridged frames do not pass through the netns's iptables unless
`br_netfilter` is loaded, so the in-netns OUTPUT allowlist does not apply to
the guest. The host-side drops on the veth are what confine it, as they are
for gVisor's netstack (`internal/sandbox/iptables_linux.go`).

### 12.3 The run tree

Firecracker has no shared-filesystem device: it emulates network, block,
vsock and balloon devices and does not implement virtio-fs. A host directory
cannot be the guest's `/work`, so the tree crosses the boundary at fixed
points:

- **In, at launch.** `mkfs.ext4 -d` makes an image from the host tree, run
  at the jail uid, so it needs no privilege and its files keep the jail uid,
  which is also the agent's uid in the guest. It keeps modes, symlinks, hard
  links and mtimes, and takes 1.5 s for a 469 MB tree of 48,744 entries,
  about what a tar stream into a blank image takes without needing a VM up
  first. It is attached writable at `/work`, and its size is the run's disk
  quota.
- **Out, at `Stop` and at every capture.** `Stop` copies the tree into the
  host tree once the VMM has exited. That is what invariant 5 requires, and it
  leaves host code that reads a run tree between engagements unchanged. A
  capture (§11) streams the capture manifest out the same way. Neither mounts
  the image on the host: a filesystem image the guest has written is
  attacker-shaped input to the host kernel's ext4 driver. A capture microVM
  reads it instead, a short-lived VM that runs only TF's capture code with
  the run-tree image attached. It streams the tree or the manifest over vsock
  to an unprivileged receiver at the jail uid, which treats the stream as
  hostile input the way the capture jail treats the tree today. It is the
  gVisor capture jail's role in a VM, and it reads an image that is not
  changing: after `Stop` the guest is gone, and before a checkpoint `Freeze`
  has frozen `/work` (`fsfreeze`, which also flushes the guest's page cache)
  and paused the VM. A frozen image is attached read-only. After an unclean
  stop the image's journal needs replaying, which a read-only attach cannot
  do, so the capture microVM gets that image writable; it is the image's only
  user by then, and the tree reads as of the guest's last journal commit.
  Capture code that cannot mount `/work` fails rather than streaming an empty
  tree. To copy out only what changed, the host sends its listing of the tree
  as handed in, and the capture microVM sends the entries that differ and the
  paths deleted.
- **Host writes during an engagement.** `workspace add` builds a checkout on
  the host, in the live run tree, and relies on the jail seeing it appear
  under `/work` (`cmd/exec/agenthost/workspace.go`). On Firecracker it would
  not. After building the checkout, the sidecar asks the executor to push the
  new subtree into the jail, and the executor sends a tar of the host-built
  checkout over vsock to a receiver in the tool host. On gVisor the push does
  nothing, so the agenthost code is the same on both backends. Any other host
  code that reads or writes a live run tree moves to an engagement boundary or
  goes through the same push; the step that builds the backend lists them.

### 12.4 The guest

- **Kernel.** Built from Firecracker's recommended guest configuration and
  pinned by version and checksum. This is the guest-kernel supply chain
  `docs/benchmarks/gvisor-cpu-tax.md` counts against a microVM: a guest-kernel
  vulnerability needs a TF release.
- **Rootfs.** The apk package set and cache key of gVisor's rootfs
  (`rootfsCacheKey`), packed into an ext4 image and attached read-only, so
  the rule in horizontal-scaling §6.4 for sharing a baked rootfs across
  tenants holds. Writable paths outside `/work` are tmpfs and are discarded
  at `Stop`, as a gVisor jail's are. A guest with a container runtime gets a
  scratch disk image for the runtime's images and volumes instead, since on
  tmpfs they would count against guest memory; it is discarded at `Stop` the
  same way (§8).
- **Init.** A small TF-owned PID 1. It mounts the images at the §4.1 paths,
  configures the network card from the kernel command line, sets a hostname
  that `/etc/hosts` resolves, starts the exec-verb relay, starts the tool host
  at the agent's uid and restarts its connection when vsock resets, freezes
  `/work` on request, steps the clock from the host after a restore (§8), and
  reports out-of-memory kills. Nothing else runs in the guest unless the agent
  starts it. The hostname matters because a cell has no reachable resolver:
  musl waits out its whole 5 s timeout on a name it cannot resolve locally,
  and git looks up the hostname to build a default identity.
- **Root.** When the profile requires `root` (§7), the tool host runs as uid
  0, and the guest kernel is the agent's boundary, with the VMM and `jailer`
  behind it. Property B is unaffected, because nothing real is in the guest
  to read. A root agent can rewrite the init and the tool host and forge
  anything they report, so the host treats everything from the guest as
  hostile: tool results, the pushed and captured trees, out-of-memory
  reports. It can also change its own address; the host rules match on the
  veth, not on an address, so that reaches nothing more.

### 12.5 What TF takes from SPACE

SPACE is Apache-2.0, copyright Perplexity AI. The license lets TF copy and
modify its code under TF's own license, provided TF meets its section 4: a
copy of the Apache license ships with the code taken, each copied file keeps
its copyright and attribution notices, each copied file TF changes says that
it was changed, and SPACE's `NOTICE` file comes along if the project ever adds
one. Code taken sits next to a copy of the Apache license and is reviewed like
TF's own.

SPACE is written in Rust. Guest-side code can come across close to as is,
since the tool host is Rust too; host-side code is a reference for the Go
broker rather than something to copy. Places to start:

- `templates/firecracker/` (`kernel`, `recipes`, `seed`): the guest kernel
  and image build.
- `docs/architecture.md`, and the node agents (`sandbox-manager`,
  `node-manager`) and worker services it names as managing VMs, networking
  and volumes: how it drives Firecracker on a host.
- `docs/base-snapshots.md` and `docs/warm-pools.md`: its snapshot handling,
  to compare against §8's rules.
- `docs/egress-profiles.md`: its egress design, to compare against §6.

TF takes none of its services: the API and its Postgres, the controllers, the
data plane, the builders and object storage, the Kubernetes adapter, the
egress gateway and the credential manager each do a job a TF component
already does (§1.1).

### 12.6 Spike results

Measured on one KVM host, with Firecracker v1.17.0 under `jailer` and TF's
own cell network code.

1. **Tool-call round trip over vsock**, against a budget set before
   measuring of p50 ≤ 5 ms and p95 ≤ 25 ms per no-op call. **Pass:** p50
   0.04 ms and p95 0.06 ms, the same as a unix socket on the host.
2. **The cell network.** **Pass:** a guest on a tap bridged into a cell that
   `SetupRunNetwork` and `AdmitJailPorts` built, unchanged, reaches only its
   admitted sidecar port, before and after it changes its own address and MAC
   as root. Cloud metadata, the internet, the executor's other listeners, the
   other cell and IPv6 all stay closed.
3. **The run tree in and out**, for a fresh checkout and a ~470 MB tree with
   dependencies installed. **Pass:** in by `mkfs.ext4 -d` in 0.13 s and
   1.5 s; out through a capture microVM in 0.2–0.3 s and 2.2 s for the whole
   tree, or 22 ms and 0.3–0.4 s for changed paths only, after a capture-VM
   boot of about 0.15 s. Contents, modes, symlinks, hard links, mtimes and
   ownership match both ways.
4. **`workspace add`** pushed into a live guest. **Not run:** needs the
   backend.
5. **A `git push`** from the guest through the run's sidecar, with push
   policy enforced. **Not run:** needs the backend.
6. **A snapshot** of a guest with a database running inside, restored on the
   same host. **Pass:** a 1 GiB guest snapshots in about 0.6 s and restores in
   30–39 ms with its rows and processes intact. The tool host reconnects and
   the clock needs a step (§8).
7. **Idle memory per cell.** About 55 MB per idle 512 MB VM, of which the
   guest has touched 52 MB: 2.3 times gVisor's ~24 MB per idle jail
   (`docs/benchmarks/sandbox-bench.md`), before the ~24 MB sidecar every
   backend pays.
8. **Bring-up latency.** **Pass:** p50 154 ms from `jailer` to the tool
   host's first answer, and 282 ms including the cell, against gVisor's spawn
   p50 of ~171–292 ms.
9. **A build burst.** **Pass:** `go test` at native wall time on 1.19 times
   native host CPU, and a frontend build the same as native.
10. **One week** of TF's own delegations on one KVM host. **Not run:** needs
    the backend.
11. **The conformance suite** against the Firecracker backend, unmodified.
    **Not run:** needs the backend.

## Related

- `docs/for-agents/specs/sandbox-fleet/` — profiles; §3 and §7 are the egress
  and credential design §6 here implements.
- `docs/for-agents/specs/horizontal-scaling/` §6.2 and §6.4 — the resume
  stamp, and eligibility, admission and preference on the claim.
- `docs/security/privilege-separation.md` — the four-process split every
  backend preserves.
- `docs/security/sandbox-egress.md` — the egress lanes; §6.1 changes the
  tunnel lane.
- `docs/benchmarks/gvisor-cpu-tax.md` — why a VM backend is attractive for
  build and test bursts, and why gVisor stays the default.
- `docs/benchmarks/sandbox-bench.md` — gVisor's idle memory and spawn
  latency, the baselines §12.6 measures against.
- Firecracker's own documentation (`github.com/firecracker-microvm/firecracker`,
  `docs/`): `jailer.md`, `vsock.md` and `snapshotting/snapshot-support.md`
  for the parts §12 relies on.
- SPACE (`github.com/perplexityai/SPACE`) — §1.1 for why it is not a
  dependency, §12.5 for what TF takes from it.
