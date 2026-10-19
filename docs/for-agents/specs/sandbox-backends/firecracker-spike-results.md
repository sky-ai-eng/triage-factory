# Firecracker spike: results

Measurements of what the Firecracker backend in §13 would cost Triage Factory,
with TF driving Firecracker itself under `jailer`, taken on one KVM host. They
answer the open questions in §13.2 and the spike items in §13.6 that do not
need the backend to exist. The harness, `cmd/firecracker-spike`, is on branch
`aa/firecracker-spike` (commit `1794aea`) rather than in this tree; its package
comment lists the subcommands, and every number below came from one of them.

## Summary

| Question | Answer |
| --- | --- |
| Tool-call round trip over vsock (budget p50 ≤ 5 ms, p95 ≤ 25 ms) | **Pass.** No-op p50 0.041–0.042 ms, p95 0.056–0.057 ms, the same as a unix socket on the host (p50 0.033–0.063 ms); gVisor under `runsc do` p50 0.14–0.45 ms. Mixed calls p95 0.55–1.58 ms |
| Does the bridged tap pass the network cases with TF's host rules unchanged? | **Pass.** TF's own `SetupRunNetwork` and `AdmitJailPorts` built the cells. Only the admitted sidecar port connected, before and after the guest took another address and MAC as root; nothing else on the executor, the other cell, the metadata address, the internet or IPv6 answered; the host rules did not change |
| Run tree in | `mkfs.ext4 -d` as the jail uid: 0.13 s for a 90 MB checkout, 1.48 s for the 469 MB, 48,744-entry tree with dependencies. A tar stream over vsock into a blank image: 0.19–0.22 s and 1.62–1.71 s, plus a VM boot |
| Run tree out | Whole tree through a capture VM: 0.23–0.31 s and 2.24–2.25 s, after a 0.12–0.18 s capture-VM boot. Changed paths only (10 modified, 1 deleted): 22 ms and 0.33–0.40 s. Contents, modes, symlinks, executable bits, hard links, mtimes and ownership match both ways |
| Freeze for a capture | `FIFREEZE` on `/work` 51–57 ms, then a VM pause of 0.6–1.5 ms. A capture VM read the frozen image consistently while the first VM stayed paused. A tool call sent while paused waits and is answered 3–4 ms after the resume. After an unclean stop the image can only be captured attached writable, so the journal can replay |
| Snapshot with a database inside (§13.6 item 6) | **Pass.** Pause under 1 ms; full snapshot of a 1 GiB guest 0.57–0.61 s, 1 GiB of memory plus 25 KB of state; restore 30–39 ms from `jailer` to a running guest; rows intact and still growing, writer at the same pid. The vsock connection does not survive, so the tool host redials and answers 75–80 ms after the resume, and the guest clock is behind by the downtime |
| Idle memory per VM (§13.6 item 7) | 55 MB RSS per idle 512 MB guest (52 MB of it guest memory the guest has touched, 3 MB the VMM's own), 49–53 MB of host used memory per VM at 10 and 50. That is 2.3 times gVisor's ~24 MB per idle jail, before the ~24 MB sidecar every backend pays. Idle CPU is 0.11% of a core per VM |
| Bring-up latency (§13.6 item 8) | `jailer` exec to the tool host's first answer: p50 154 ms, p95 181 ms. With the cell built first, as gVisor's spawn figure counts it: p50 282 ms, p95 330 ms, against gVisor's spawn p50 of ~171–292 ms |
| Build burst (§13.6 item 9) | `go test` over 100 packages: 26.0 s wall on both sides, 65.6–66.0 s of host CPU in the guest against 55.2–55.4 s natively (1.19 times). Frontend build: 1.8 s against 1.7–1.9 s, the same CPU within noise |
| How does an OOM inside the guest surface? | With the VMM's cgroup above guest memory, the guest's OOM killer kills the process (exit 137 to the bash tool), the guest's `/proc/vmstat` `oom_kill` and kernel log record it, and the host cgroup records nothing. With the cgroup below guest memory, the host kills the VMM and the whole cell is gone |

## Machine and versions

| | |
| --- | --- |
| CPU | Intel Core i9-14900KF, 24 cores (8 performance, 16 efficiency), 32 threads, VT-x |
| Memory | 64 GB (61 GiB usable) |
| Host kernel | 7.0.0-38-generic, Ubuntu 26.04.1 LTS, cgroup v2 |
| Disk | NVMe SSD, ext4 |
| Where the spike ran | A privileged container (Ubuntu 26.04) with its own network, mount and cgroup namespaces and `/dev/kvm`. Its network namespace stands in for the executor's root namespace, and its cgroup subtree for the executor's, so nothing the spike built touched the host's own rules or cgroups |
| Firecracker | v1.17.0, `firecracker` and `jailer` from the GitHub release tarball, whose sha256 matched the release's published checksum (`06094a11…b558`) |
| Guest kernel | Linux 6.1.188 from Firecracker's CI artifacts (`firecracker-ci/20261016-5732b8331e1e-0/x86_64/vmlinux-6.1.188` and its `.config`), sha256 `c1628885…1e92`. vsock, virtio-blk, ext4, cgroup v2 memory, IPv6, the balloon with free page reporting, and KSM are built in |
| Guest root | TF's sandbox rootfs as `ensureRootfs` builds it (Alpine 3.21.7 with `rootfsCacheKey`'s apk set and pnpm 11.9.0), plus the tool host, this harness as the guest helper, and `tfinit.sh` as PID 1. `image` packs it with `mkfs.ext4 -d` into an 811 MiB image (598 MiB of files), attached read-only |
| Tool host | `tf-harness-tools` from `harness/`, built for `x86_64-unknown-linux-musl`, with `serve --connect vsock:CID:PORT` added: a vsock dial beside the unix one, about a hundred lines with a parser test, on branch `aa/firecracker-spike` (commit `c2debf3`) |
| gVisor | `runsc` release-20260511.0 (systrap), for the round-trip column only |
| Tools | iptables 1.8.11 (nf_tables), iproute2 6.19.0, e2fsprogs 1.47.2, Go 1.27.2 |

The host was not otherwise idle. A TF development stack, an idle
Kubernetes-in-Docker cluster and about 40 idle Postgres containers ran
throughout. Memory figures are deltas or per-process reads, and the build burst
pinned both sides to the same CPUs.

Every VM below runs under `jailer` as in production: its own chroot with the
kernel and images hard-linked in, private mount and pid namespaces, a cgroup of
its own under one parent, the jail's uid and gid (a cell's `JailUID` when it
has a cell), the release's default seccomp filter, and `--config-file` for its
configuration. The guest's tool host dials vsock port 1024; the harness listens
on `<chroot>/v.sock_1024` and drives the connection with
`agentloop.NewToolHost`.

## 1. Tool-call round trip

**Method.** The harness boots a VM (2 vCPU, 1 GiB, a blank 256 MiB `/work`),
accepts the tool host's vsock dial, and drives it with `agentloop.NewToolHost`.
After seeding a small tree and 20 warm-up calls it times 1,000 no-op calls
(`read` of a one-line file) and a fixed sequence of 300 mixed calls, which
stands in for §13.6's replayed run: `read` 35%, `bash` echo 25%, `grep` 15%,
`write` of 200 to 2,200 bytes 15%, `ls` 10%. The native floor is the same
static tool host on the host, dialing a unix socket the harness listens on. The
gVisor column runs that tool host under `runsc do` (systrap, `--host-uds=open`)
dialing the same kind of socket; it is not TF's production jail spec. Three
runs each, a fresh VM or process per run.

**Raw (ms).**

| Transport | No-op p50 | No-op p95 | No-op p99 | Mixed p50 | Mixed p95 | `bash` p50 | `read` p50 (mixed) |
| --- | --- | --- | --- | --- | --- | --- | --- |
| vsock into the guest | 0.041–0.042 | 0.056–0.057 | 0.071–0.113 | 0.10–0.24 | 0.55–1.58 | 0.50–0.78 | 0.07–0.13 |
| Unix socket on the host (floor) | 0.033–0.063 | 0.072–0.119 | 0.089–0.220 | 0.29–0.34 | 2.65–2.92 | 1.87–2.70 | 0.16–0.24 |
| gVisor (`runsc do`) | 0.14–0.45 | 0.21–0.91 | 0.37–1.38 | 0.77–1.34 | 9.5–13.8 | 3.8–5.7 | 0.32–0.57 |

**Verdict.** **Pass**, by two orders of magnitude: p50 0.04 ms and p95 0.06 ms
against a budget of 5 ms and 25 ms. vsock adds nothing measurable over a unix
socket on the host. Mixed calls ran faster in the guest than on the host: every
tool but the no-op read took two to four times as long on the host (`bash`
1.9–2.7 ms against 0.5–0.8 ms). Neither side was pinned to CPUs, and the spike
did not look into why; it does not change the verdict.

## 2. The cell network

**Method.** Two cells built by TF's own code: `sandbox.SetupRunNetwork` (netns,
veth, gateway address, MASQUERADE, the in-netns OUTPUT allowlist, the host-side
DROPs on the veth, IPv6 off on the host end) and `RunNetwork.AdmitJailPorts`
for one sidecar port X in each. Inside each netns, and only there: the jail
address comes off the veth's inner end, a bridge joins that end to a tap owned
by the jail uid, and the guest takes the jail address on its network card.
`jailer --netns` puts the VMM in the netns. On the executor side, listeners on
gateway 1 at X (admitted) and Y (not admitted), on `0.0.0.0:Z` and `[::]:Z+1`,
and on gateway 2 at X; a second guest in cell 2 runs a listener on port 9000.
The guest-side probe is a TCP connect with a 3 s timeout, run as root through
the tool host, and a connection counts only if the listener's greeting comes
back. Three rounds: as launched; after the guest moved to another address in
its subnet and another MAC; after it took cell 2's jail address, routed at cell
2's gateway, with a third MAC.

**Host rules TF installed**, five per cell, addresses and interface names
replaced (the same ten lines, unchanged, after every round; none left after
teardown):

```
*filter, at the head of INPUT and FORWARD, per cell:
-A INPUT -d <gw>/32 -i <veth> -p tcp -m tcp --dport <X> -j ACCEPT
-A INPUT -d <gw>/32 -i <veth> -j DROP
-A INPUT ! -d <gw>/32 -i <veth> -j DROP
-A FORWARD ! -d <gw>/32 -i <veth> -j DROP
*nat:
-A POSTROUTING -s <subnet> -o <uplink> -j MASQUERADE
```

**Raw.**

| Probe from the guest in cell 1 | As launched | New address in its subnet, new MAC | Cell 2's address and gateway, new MAC |
| --- | --- | --- | --- |
| Its gateway, admitted port X | connected | connected | timeout (expected either way: replies route to cell 2's veth) |
| Its gateway, port Y not admitted | timeout | timeout | timeout |
| Its gateway, port Z of a `0.0.0.0` listener | timeout | timeout | timeout |
| The executor's uplink address, port Z | timeout | timeout | timeout |
| 169.254.169.254:80 | timeout | timeout | timeout |
| A public address, port 443 | timeout | timeout | timeout |
| 8.8.8.8:53 | timeout | timeout | timeout |
| Cell 2's gateway, its admitted port X | timeout | timeout | timeout |
| Cell 2's guest, port 9000 | timeout | timeout | refused (the guest's own address now) |
| A public IPv6 address | network unreachable | network unreachable | network unreachable |
| IPv6 all-nodes ping on the link | only the guest itself answers | no IPv6 address left | no IPv6 address left |

From cell 2's guest, its own admitted port connected and cell 1's did not.
Every timeout is 3 s with nothing arriving: the listeners counted only the two
admitted connections from cell 1 and the one from cell 2.

**Verdict.** **Pass.** It covers `OnlyTheSidecarIsReachable` (the admitted port
connects; a gateway port not admitted and a listener on every interface do
not), `InternalDenylistUnreachable` (metadata, a public address, public DNS,
and the executor's own uplink address are all closed), and `CellsAreIsolated`
in both directions, with IPv6 closed: the guest has a link-local address, but
nothing else on the link answers on IPv6, because the host end of the veth has
IPv6 off. The host rules needed no change from what TF installs for gVisor. The
rules match on the veth, so an address or MAC the guest chooses reaches nothing
new. Bridged frames do not pass through iptables here (`br_netfilter` is not
loaded), so the in-netns OUTPUT allowlist does not apply to the guest; the
host-side DROPs on the veth are what stop it, which is also the case for
gVisor's netstack (`iptables_linux.go`). Each cell took 80–134 ms to build,
TF's part and the bridge and tap together.

## 3. The run tree in and out

**Trees.** Small: a fresh clone of this repository, 3,714 entries, 86 MB in
3,440 files, 32 executables, no symlinks. Large: the same after `pnpm install`
in `frontend/`, 48,744 entries, 469 MB in 43,172 files, 1,422 symlinks, 199
executables and 4,427 hard-linked files (pnpm's). The trees are copied and
handed to a jail uid (`chown -R`, as `HandOffRunTree` does) before anything is
timed. The large tree is about half the suggested 1 GB.

**Method.**

- In, populated image: `mkfs.ext4 -d` from the tree, run as the jail uid with
  `setpriv`, into an image sized at 1.5 times the tree's disk usage plus 128
  MiB (264 MiB and 971 MiB).
- In, tar stream: a blank image of the same size (`mkfs.ext4`), a VM booted
  with it at `/work`, and the tree sent as a tar over vsock to the guest
  helper, which extracts it as root with numeric owners and modes, syncs and
  answers. Timed from the guest's dial to its answer; the boot before the dial
  is listed separately.
- Out: a VM booted on the populated image modifies 10 files, deletes 1, and
  shuts down cleanly (sync, remount `/work` read-only, reboot). A capture VM
  then boots with the image attached read-only, streams a tar of `/work` over
  vsock, computes the image's digests and reboots. The host extracts as it
  receives. For changed paths only, the host first sends its listing of the
  tree as handed in (path, type, mode, owner, size, mtime, link target), and
  the capture VM sends what differs plus the list of deleted paths, which the
  host applies to its copy.
- Verification: a digest per property over every entry (sha256 of content or
  link target, mode bits, uid:gid, mtime), computed by the same code on the
  host and in the guest, compared after every direction.

**Raw (3 runs each).**

| Method | Small | Large |
| --- | --- | --- |
| In: `mkfs.ext4 -d` as the jail uid | 131–137 ms | 1,483–1,488 ms |
| In: `mkfs.ext4` of a blank image | 5–10 ms | 6–14 ms |
| In: tar over vsock, extracted and synced in the guest | 188–223 ms (86 MB) | 1,621–1,707 ms (456 MB; hard links are not resent) |
| Boot before the stream starts (`jailer` to the guest's dial) | 137–165 ms | 158–190 ms |
| Out: whole tree through a capture VM | 227–307 ms (89 MB) | 2,235–2,251 ms (493 MB) |
| Out: changed paths only | 22–23 ms (103 KB) | 332–397 ms (103 KB) |
| Capture VM boot before the stream | 118–175 ms | 118–157 ms |

The changed-paths capture of the large tree spends most of its time in the
guest walking 48,744 entries and decoding the host's listing, not sending.

**Verification.** Every comparison matches, on both trees: the guest's view
after either way in against the source; the capture VM's view against the
working VM's own digest before it shut down; and the host's copy against the
capture VM's view, for the whole tree and for changed paths. Content, mode bits
(including executable bits and read-only files), symlinks and their targets,
hard links, mtimes and numeric ownership all survive. Files in the guest are
owned by the jail uid and gid they had on the host (40002:40002 here), by both
methods, so the agent runs at the uid that owns its tree.

**Freeze.** A VM on each tree's image runs a writer appending a numbered line
to `/work` every 10 ms; the harness freezes `/work`, pauses the VM, captures
the image with a second VM, then resumes and thaws the first. Raw:

| Step (3 runs) | Small | Large |
| --- | --- | --- |
| `FIFREEZE` on `/work` (the ioctl, in the guest) | 51–56 ms | 51–57 ms |
| The same through a tool call | 82–93 ms | 74–87 ms |
| VM pause | 0.6–1.5 ms | 0.9–1.1 ms |
| Capture VM, whole tree, while the first VM is paused | 219–254 ms | 2,384–2,599 ms |
| VM resume | 0.2–0.9 ms | 0.6–0.8 ms |
| `FITHAW` through a tool call | 4–12 ms | 4–16 ms |

The capture VM mounted the frozen image read-only without journal recovery, its
digest matched the host's copy, and the writer's file in the capture is a clean
run of 95–97 complete lines with no partial line. After the thaw the writer
carried on from the next line.

A tool call sent while the VM was paused got no answer for the 2 s the pause
lasted and was answered 3.3–3.6 ms after the resume, on the same connection.
Nothing fails while frozen; calls wait.

**After an unclean stop.** When the VMM is killed while the guest is writing,
the image's journal needs recovery. A capture VM given the image read-only
cannot mount it (`EXT4-fs (vdb): write access unavailable, cannot proceed`),
and the spike's init carried on with an empty `/work`, so the capture streamed
nothing. Attached writable, the same capture VM's kernel replays the journal,
mounts `/work` read-only, and the capture matches the image on both trees. It
holds the tree as of the guest's last journal commit, here the freeze: the
writer's file is a clean run of 96 lines, and the lines written in the second
between the thaw and the kill were lost, as they would be on a machine that
lost power.

**Verdict.** Both ways in work and are lossless. `mkfs.ext4 -d` needs no
privilege, keeps the host tree's numeric owners, and takes about as long as the
tar stream without needing a VM up first, so it is the right `HandOffRunTree`.
Copy-out through a capture VM costs a VM boot (about 0.15 s) plus 2.2 s for the
large tree whole or 0.33–0.40 s for changed paths. A SPACE spike on the same
host and trees took 7.0–7.6 s to stream the large tree in through SPACE's data
plane and 2.3–3.6 s to stream it out.

## 4. and 5. Not run

`workspace add` pushed into a live guest and a `git push` through the run's
sidecar need the backend and its sidecar wiring to exist.

## 6. Snapshot with a database inside

**Method.** A 2-vCPU, 1 GiB guest runs a SQLite writer inserting a timestamped
row every 20 ms (WAL, one transaction per row) beside the tool host, with a
marker file on `/work`. The harness pauses the VM, takes a full snapshot, kills
the VMM with SIGKILL, starts a fresh `jailer` with a new chroot holding hard
links to the same images and snapshot files, and loads the snapshot with
`resume_vm`. Three runs.

**Raw.**

| Step | Result |
| --- | --- |
| Pause | 0.3–0.9 ms |
| Full snapshot | 568–607 ms; memory file 1,073,741,824 bytes, fully allocated; VM state 25,481 bytes |
| Restore, `jailer` exec to a resumed guest | 30–39 ms, of which the load call is 6.7–7.4 ms |
| Resume to the tool host's dial | 63–67 ms |
| Resume to its first answer | 75–80 ms |
| Restored VMM RSS | 38 MB at first answer: guest memory loads on demand from the file |
| Rows | 96 before the pause, 101 at the first query after, 149–150 a second later; `max(seq)` equals the count; `integrity_check` ok |
| Writer | same pid, running |
| Tool host | a new process: the old one exited when its connection reset, and the init started another, which dialed the new chroot's socket |
| Marker file | intact |
| Guest clock | 1.1–1.2 s behind the host after restore, the time the VM was down; the largest gap between rows is 30 ms because the guest saw no time pass |

**Verdict.** **Pass**, with two things the backend has to handle. Firecracker
resets every vsock connection on restore, so the tool host must redial (the
spike's init restarts it; a production init would reconnect in place), and the
executor accepts on the new chroot's socket. And the guest's wall clock does
not move while the VM is down, so the init has to step it on resume (from the
host over vsock), or TLS validity checks, git timestamps and token expiry in
the guest are off by the downtime.

## 7. Idle memory and density

**Method.** N VMs, each in a TF cell of its own, 1 vCPU and 512 MB, the tool
host connected and idle, 30 s after the last boot. Host used memory is
`MemTotal − MemAvailable`. Per VMM: RSS and PSS from `/proc/<pid>/smaps`, and
the RSS of the anonymous mapping that is guest memory, which is what the guest
has touched. The guest's own view is its `MemTotal − MemAvailable`.

**Raw.**

| N | Host used delta | Per VM | VMM RSS | Guest memory touched | VMM outside guest memory | VMM cgroup `memory.current` | Guest's own used | Idle CPU per VM |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | within noise | — | 54.9 MB | 51.9 MB | 3.0 MB | 57.4 MB | 25.6 MB | 0.13% |
| 10 | 533 MB | 53.3 MB | 54.9 MB | 51.9 MB | 3.0 MB | 57.5 MB | 25.5 MB | 0.12% |
| 50 | 2,467 MB | 49.3 MB | 55.0 MB | 51.9 MB | 3.0 MB | 57.5 MB | 25.5 MB | 0.11% |

PSS equals RSS: nothing is shared between VMMs (jailer copies the binary into
each chroot, and guest memory is private). The 50 VMs and their cells came up
in 0.9 s, ten at a time, boot p50 163 ms and max 234 ms. Building the 50 cells
alone added about 60 MB.

An idle guest touches 52 MB of its 512 MB: the kernel image (about 20 MB of
code and data), the kernel's own reserved structures, slab and a few MB of page
cache. Its own used figure is 25 MB. A balloon with free page reporting
(`free_page_reporting`, at the default and at order 0) changed nothing, because
almost none of that memory is free in the guest.

**Verdict.** About 55 MB per idle VM against gVisor's ~24 MB per idle jail, 2.3
times as much, before the ~24 MB sidecar every backend pays. Guest memory is
overcommitted, not reserved, so a 512 MB guest that stays idle costs 55 MB.
Idle CPU is 0.0011 cores per VM against gVisor's 0.026 cores per idle jail
under systrap. The levers left are a smaller guest kernel and same-page merging
across guests (the kernel's text is the same in every one); neither was
measured. A SPACE spike on the same host measured 104 MB per idle sandbox: its
Firecracker process held 98 MB with SPACE's Ubuntu guest, and its per-sandbox
gateway and host services about 6 MB more by PSS.

## 8. Bring-up latency

**Method.** 20 sequential bring-ups of a 2-vCPU, 1 GiB VM, each into a fresh TF
cell. Before the clock starts, the harness builds the cell, makes a blank 64
MiB `/work` image, and builds the chroot; each of those is timed on its own.
The clock runs from the `jailer` exec to the tool host's dial and then to its
first answered `bash` call. The guest's `/proc/uptime` at that answer splits
the boot into the part before the guest kernel starts and the part after.

**Raw (ms).**

| Phase | p50 | p95 | min | max |
| --- | --- | --- | --- | --- |
| Before `jailer`: the cell, the image and the chroot | 132 | 149 | 64 | 168 |
| of which TF's `SetupRunNetwork` | 98 | 114 | 50 | 131 |
| of which the bridge and the tap | 26 | 28 | 9 | 29 |
| of which the blank image | 8 | 9 | 5 | 9 |
| `jailer` exec to the guest kernel starting | 74 | 93 | 46 | 95 |
| Guest kernel start to the first answer (guest uptime, 10 ms resolution) | 80 | 100 | 80 | 140 |
| `jailer` exec to the tool host's dial | 152 | 179 | 125 | 214 |
| Dial to the first answer | 1.9 | 2.4 | 1.7 | 2.9 |
| **`jailer` exec to the first answer** | **154** | **181** | 126 | 217 |
| Cell setup through the first answer | 282 | 330 | 223 | 340 |

A run with the serial port removed from the guest (`8250.nr_uarts=0`) was the
same within noise (p50 152 ms, p95 182 ms). In item 7, 50 VMs booted ten at a
time had a boot p50 of 163 ms.

**Verdict.** The same as gVisor. gVisor's spawn, the network plus the jail, has
a p50 of ~171–292 ms (`docs/benchmarks/sandbox-bench.md`); the Firecracker
equivalent, the cell through the tool host's first answer, has a p50 of 282 ms
and a p95 of 330 ms, a third of it TF's own network setup. The VM itself is up
and answering 154 ms after `jailer` starts.

## 9. A build burst

**Method.** This repository with its Go dependencies vendored and
`frontend/node_modules` installed for musl by the guest root's pnpm 11.9.0,
with Go 1.27.2 and the guest root's Node 22.23.2. The native side is a chroot
of a writable copy of the guest's root with the same tree bind-mounted at
`/work`, so both sides run the same binaries on the same files and differ only
in the VM. The workload is `go test -count=1` over 100 packages under
`internal/` (those whose tests do not need Docker), then `pnpm run build` in
`frontend/` (`tsc -b && vite build`). Each side is driven through the tool
host's bash tool, with one warm-up run before two measured runs; the guest's
image was made from the tree after the native runs, so its caches started warm
too. Native ran pinned to one thread on each of the 8 performance cores; the
guest had 8 vCPUs and 8 GiB with the VMM pinned to the same 8 CPUs. Host CPU is
the cgroup's `usage_usec`: the VMM's, which holds everything the guest does,
and on the native side a cgroup the chrooted tool host was started in.

**Raw.**

| Workload | Native wall | Native CPU | Guest wall | Guest host CPU |
| --- | --- | --- | --- | --- |
| `go test`, 100 packages | 25.98–26.00 s | 55.2–55.4 s | 25.98–26.01 s | 65.6–66.0 s |
| Frontend build | 1.67–1.92 s | 3.9–5.2 s | 1.80–1.81 s | 4.8–4.9 s |

On both sides 89 packages pass, 9 have no tests, and the same 2 fail for
reasons of the environment: `internal/agentloop` wants a cargo-built tool host
the tree does not have, and two `internal/sandbox` probes parse a child's
output that a dependency's warning under Go 1.27 pollutes.

The first guest run of this workload stalled for minutes in
`internal/worktree`. One of its tests took 10.1 s in the guest against 0.29 s
natively, two `git reset --hard` calls taking 5.0 s each. musl's resolver waits
out its whole 5 s timeout when no nameserver can be reached, git resolves the
hostname to build a default identity for a reflog entry, and the guest's
hostname was unset. The spike's init now sets a hostname that `/etc/hosts`
carries; the numbers above are with it. Natively the chroot's resolver answered
at once.

A guest `fsync` takes 0.06 ms against 0.81 ms natively on this disk (p50 of
300). Firecracker's default drive cache mode does not pass the guest's flushes
to the host, so data the guest has synced is in the host's page cache, not on
disk, until the host writes it back.

**Verdict.** A build burst costs the same wall time as native and 1.19 times
the host CPU for `go test`, against the ~14 times gVisor shows under systrap
(`docs/benchmarks/gvisor-cpu-tax.md`); the frontend build is the same within
run-to-run noise.

## OOM behaviour

**Method.** A 1-vCPU, 512 MB guest, the VMM's cgroup limited by `jailer`
(`memory.max`, `memory.swap.max=0`), and the guest helper allocating and
touching 32 MB every 100 ms until it is killed, run through the bash tool.

**Raw.**

| | VMM cgroup above guest memory (640 MB) | VMM cgroup below guest memory (384 MB) |
| --- | --- | --- |
| What happened | the guest's OOM killer killed the allocator at 448 MB held; the bash tool returned exit 137 | the host's OOM killer killed the VMM after 1.2 s; the tool-host connection closed (EOF) |
| Guest `/proc/vmstat` `oom_kill` | 1 | — (the guest is gone) |
| Guest kernel log | `fcspike invoked oom-killer`, `Out of memory: Killed process … (fcspike)`, with `global_oom` | — |
| Tool host afterwards | answers | gone with the VM |
| Host cgroup `memory.peak` | 510.8 MB | 384.0 MB |
| Host cgroup `memory.events` | `oom 0 oom_kill 0` | `max 36 oom 1 oom_kill 1` |

**Verdict.** With the limit above guest memory plus the VMM's overhead, as
§13.2 sets it, the guest's OOM killer acts first and the host sees no OOM at
all: the cgroup's events are zero, and its `memory.peak` reads the guest's
whole memory, not the workload's peak. So `OOMKilled` cannot come from the
VMM's cgroup as it does on gVisor. The guest reports it: its `oom_kill` counter
and kernel log carry each kill, and the init can send the counter over vsock. A
limit below guest memory turns a workload OOM into the loss of the whole cell,
which the VMM's cgroup does record.

## Not run

- **4. `workspace add`** and **5. `git push` through the sidecar**: need the
  backend and its sidecar wiring.
- **10. One week of delegations** and **11. the conformance suite**: need the
  backend.

## What this changes in §13

Nothing measured argues against the design. These are what the backend has to
do that §13 does not yet say:

- **`OOMKilled` comes from the guest** (§13.2). With the VMM's cgroup above
  guest memory, an OOM inside the guest leaves the cgroup's events at zero, and
  the cgroup's `memory.peak` is the most guest memory ever touched, not the
  workload's peak. The init reports the guest's `oom_kill` count over vsock;
  `OOMKilled` is that count or the cgroup's own `oom_kill`, which means the VMM
  itself was killed and the cell is gone.
- **The capture VM attaches the image writable after an unclean stop** (§13.3).
  A VM that died while writing leaves a journal to replay, and a read-only
  attach cannot mount it. Attached writable, the capture VM's kernel replays
  the journal and the tree reads as of the guest's last commit. The capture
  code fails when `/work` does not mount rather than streaming an empty tree,
  as the spike's init did.
- **Restore redials and resets the clock** (§8, §13.4). Firecracker resets
  every vsock connection on snapshot restore, so the tool host reconnects and
  the executor accepts on the restored VM's socket. The guest's wall clock
  stands still while the VM is down, so the init steps it from the host at
  resume.
- **The init gives the guest a hostname `/etc/hosts` resolves** (§13.4). A cell
  has no reachable resolver, and musl spends its whole 5 s timeout on every
  lookup of a name it cannot resolve locally; git looks the hostname up to
  build a default identity, which turned a 0.3 s test into a 10 s one.
- **The drive cache mode is a decision** (§13.2 `Launch`). Firecracker's
  default ignores the guest's flushes, so a guest `fsync` is cheap but not on
  disk. That is fine for a tree copied out at `Stop`; a parked run that must
  survive a host crash with its image needs `Writeback`.
- **`HandOffRunTree` is `mkfs.ext4 -d` at the jail uid** (§13.3). It needs no
  privilege beyond reading the tree, keeps owners, modes, symlinks and hard
  links, and builds the large tree's image in 1.5 s, about the time the tar
  stream takes without needing a VM up first.
- **The tool host's vsock dial** (§13.2 `Connect`) exists as
  `serve --connect vsock:CID:PORT` on branch `aa/firecracker-spike` (commit
  `c2debf3`).

The measured costs to weigh: about 55 MB per idle VM against gVisor's 24 MB, in
exchange for native wall time and 1.19 times native CPU on a build burst
against gVisor's ~14 times, bring-up equal to gVisor's, and memory snapshots
that restore in about 35 ms.
