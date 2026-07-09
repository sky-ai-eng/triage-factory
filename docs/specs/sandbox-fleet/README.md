# Sandbox fleet — configurable sandbox profiles

Spec for making the agent sandbox a **configurable fleet** instead of a single
fixed shape: an org admin defines named **sandbox profiles** (what's baked in,
where the sandbox may reach out, how big it is); team admins choose which
profiles their teams may use; and event-handler rules bind a profile to a job so
the right sandbox spins up automatically for the right task.

This generalizes the Playwright/Chromium work
(`docs/specs/playwright-chromium-sandbox/`) — a "browser" profile becomes *one
instance* of this model rather than a special case.

Status: **proposal**. No code written yet. Tracked as **TFAC-408** (post-v1;
deliberately not under the v1 multi-tenant epic TFAC-51). Parent context: the
SKY-254 sandbox epic (`internal/sandbox`).

Scope note: this targets the **multi modes only** (self-host multi and shared
SaaS). Local mode (Tier 4, N=1) has one user, one trust boundary, and no
profile fleet — it stays as-is. Throughout, "self-host" means Tier 3 and "shared
SaaS" means Tier 2 in `docs/isolation-tiers.md`.

---

## 1. The model

A **sandbox profile** is a named, org-owned object with four dimensions:

| Dimension | What it controls | Enforced by |
| --- | --- | --- |
| **Image** | What's baked into the rootfs (toolchain, Chromium, extra packages) | rootfs variant (§4) |
| **Egress** | What the sandbox may reach out and hit | the host forward proxy (§3) |
| **Resources** | RAM/CPU/FD/process limits, and at scale, *where* the run is scheduled | in-sandbox rlimits + Machine/cgroup sizing (§5) |
| **Proxied credentials** | Customer secrets the sandbox's tools may *use* without the agent seeing them | **deferred — sidebar §7**, not v1 |

Authoring + selection flow:

- **Org admin** defines the profiles available in the org.
- **Team admin** chooses which of those profiles their team may use.
- **Rule author** (a team member) binds a profile to an event-handler rule, so a
  matching event spawns a run in that profile.

Three of the four dimensions collapse onto two existing host-side components: the
**rootfs builder** (`internal/sandbox/rootfs*.go`) and the **forward proxy**
(`internal/llmproxy`, `internal/gitproxy`, wired via the `ConfigureProxies`
callback in `internal/sandbox/sandbox.go`). The fourth (credentials) is the one
unsolved piece and is explicitly out of v1 — see §7.

---

## 2. Why this is mostly tier-agnostic, and the one knob that isn't

The fleet concept itself — named profiles, admin authoring, team selection,
rule binding — has **no multitenancy implications**. It does not touch the
isolation primitives (gVisor still contains the process; Postgres RLS still
partitions the data; a profile cannot make org A read org B). Build it
first-party across both multi modes.

The **only** tier-sensitive dimension is the *content of the egress policy*,
and the sensitivity is narrow and specific. It is **not** "org A reaches org B's
data" — that stays impossible. It is that on **shared** infrastructure, "what the
sandbox may reach" includes things that belong to the operator and to other
tenants by virtue of network position:

- the cloud's link-local **metadata endpoint** (can hand out the *host's* cloud
  identity — a shared-host compromise),
- the operator's **internal network** — on the shared Fly fleet the Machines sit
  on Fly's private network (the SKY-254 validation README notes the internal
  `fdaa::3` resolver on it); that network is where the shared Postgres, the
  Vault, and **every other tenant's Machine** live,
- the operator's **shared egress identity** (NAT source IP) — traffic leaves with
  the Machine's address, an identity shared across all tenants on that host.

Today's egress default-DROP (`applyEgressPolicy`, SKY-395) is what keeps a
sandbox off all of that. A customer-tunable egress policy is, by construction, a
customer-tunable hole in that wall — and on shared infra the wall protects
everyone, so one org's hole (or one org-admin mistake/compromise) widens the
**shared** blast radius. On self-host the identical hole only exposes the
customer's *own* network, which they already own and trust.

The reconciliation (§3) is that the proxy makes broad egress safe on **all**
tiers *for public destinations*, and the only thing that stays self-host/dedicated
is reaching a genuinely **private** network — which is decided by physics
(shared SaaS has no route to a private VPC regardless of config), not by policy.

---

## 3. The forward proxy is the load-bearing safety mechanism

All sandbox egress already flows through host-side proxies on the run's gateway
IP (`sb.HostIP`), bound by the `ConfigureProxies` callback after the netns is up
(`sandbox_linux.go` step 9.5). The fleet feature extends that proxy to be the
single place that **gates destinations**, and (later, §7) **injects credentials**
and **audits**.

### 3.1 Why "intercept everything" is necessary but not sufficient

Routing all egress through a host proxy is the right design, but interception
alone does **not** make "allow anything" safe — because the proxy runs on the
host, which has *more* network reach than the sandbox. A naively-permissive proxy
is a confused deputy: an injected agent asks it to connect to an internal address,
and the proxy — sitting on the privileged host — dutifully does so and hands back
the result. The sandbox borrows the host's reach. (Classic SSRF.)

So the proxy must enforce a destination policy. The safe shape is **deny-internal,
allow-configured-public**:

- An **operator-owned, customer-immutable denylist** of internal destinations the
  proxy will *never* connect to no matter what any profile configures: the cloud
  metadata endpoint, all private/link-local ranges, the operator's own
  network, the sandbox subnet pool (`10.42.0.0/16`), other tenants' gateways.
- Above that line, a **per-profile allowlist** of public destinations the org
  admin configures.

With the denylist in place, "allow basically anything public" is safe **even on
shared SaaS**, because the only thing that's ever dangerous on shared infra —
reaching the internal network — is structurally blocked at the proxy and the
customer cannot widen it.

### 3.2 Two capability levels of the proxy

- **Gating destinations is cheap.** Even for encrypted (HTTPS) traffic, the proxy
  sees the *destination* from the connection setup and can block denylisted
  targets **without decrypting anything**. This is all v1 needs.
- **Injecting credentials is expensive** (requires terminating TLS so the proxy
  can modify the request, i.e. a trusted proxy cert inside the sandbox). This is
  the deferred §7 work; v1 does not do it.

So v1's proxy is a **gating** proxy: it decides *whether* a connection is allowed,
not *what's in it*.

### 3.3 The public-vs-private split (and why VPC is not a prerequisite)

The line that's tier-gated is **public door vs no public door**, not "VPC vs not":

- **Public endpoint** (e.g. a staging API reachable over the internet, gated by a
  token): the proxy connects to it — it isn't on the internal denylist — so this
  works on **shared SaaS**. A customer does *not* need to self-host to point a
  sandbox at their public staging environment.
- **Genuinely private network, no public route** (e.g. a k8s stack on a private
  VPC): nothing off that network can reach it, the shared SaaS infra included.
  This is **self-host/dedicated by physics** — the same correlation
  `docs/isolation-tiers.md` already draws (the strict-isolation need and the
  network-unreachability go together).

### 3.4 The egress power-dial per tier

| Egress flavor | Self-host multi (T3) | Dedicated (T2.5) | Shared SaaS (T2) |
| --- | --- | --- | --- |
| Proxy-only (today's default) | ✅ | ✅ | ✅ |
| Public-destination allowlist via gating proxy (deny-internal enforced) | ✅ | ✅ | ✅ |
| Reach a genuinely private network | ✅ | ✅ | ❌ (no route exists) |

### 3.5 Implementation sketch (gating proxy + denylist)

- The gating proxy lives where the LLM/git proxies already live — bound on
  `sb.HostIP`, configured per-run via `ConfigureProxies`. The sandbox's tools are
  pointed at it (proxy env / git `http.proxy`, the existing mechanism).
- The egress allowlist + the immutable internal denylist are passed into the
  proxy at construction from the resolved profile.
- The **host-side L3 backstop stays unchanged**. `applyEgressPolicy`'s two layers
  (in-netns `OUTPUT` allow + host-side veth `FORWARD`/`INPUT` DROP) continue to
  pin raw egress to the gateway IP only, so the *only* way out remains the proxy.
  We do **not** punch raw CIDR holes in the L3 layer for the public-allowlist
  case — the proxy is the allowlist enforcement point, the L3 layer just keeps
  everything funneled through it. (Raw-L3-to-private, the self-host case, is the
  one variant that adds an L3 permit, and it's gated to self-host.)

### 3.6 Open security-review items (hard gates before shared-SaaS ship)

The denylist is a denylist, and denylists fail by *incompleteness*. Each of these
must be handled and independently reviewed/tested before the public-allowlist
flavor is enabled on shared infra:

- **DNS rebinding** — the proxy MUST resolve the destination itself and re-check
  the **resolved IP** against the denylist at connect time. Never trust the
  hostname alone; never let the sandbox side resolve.
- **Redirects** — follow-and-recheck; a redirect to an internal URL must be
  re-validated, not blindly followed.
- **Alternate encodings / IPv6** — block the metadata and private ranges in all
  their forms (IPv6, IPv4-mapped, alternate notations), not just the canonical
  decimal.
- **Egress network path** — ideally the proxy's own outbound path cannot see the
  internal network at all (defense in depth behind the denylist).

These are explicit gates, not afterthoughts. The pattern is standard SSRF
defense; the value is entirely in getting these details right.

---

## 4. Image dimension (rootfs variants)

Already specced in `docs/specs/playwright-chromium-sandbox/`. The fleet
generalizes it: each profile names a rootfs variant; `apkPackages` becomes a
function of variant, and `rootfsCacheKey()` already keys on the package set so
each variant caches to its own `rootfs-<key>` dir
(`internal/paths/paths.go:174`) — no collision. The "browser" variant (Chromium +
fonts + Playwright) is the first worked example; "base" is today's toolchain.

Cost note from that doc: a browser variant adds ~400–700 MB; pre-bake variants
into the published image (`docker/Dockerfile`) so an autoscaled Machine doesn't
pay a cold extract on first use.

Keying (settled 2026-07-07, jointly with the TFAC-71 design): **two-layer,
name → recipe → hash.** A profile binds a *named* catalog entry; the entry is
a recipe (base + package set) resolving to a `rootfsCacheKey` content hash;
executors bake, cache, and share by **hash** (org-blind — see
`docs/specs/horizontal-scaling/` §6.4 for the cross-tenant invariant), while
authoring and rule-binding speak **names**. Editing a recipe under a name
mints a new hash, so the physical layer never holds ambiguous bytes. The v1
catalog is curated ("base", "browser"); org-authored entries are later catalog
rows with authorship — same mechanism, more authors.

**Privilege-separation constraint (privsep epic).** Building a rootfs runs
`apk add`, which executes each package's install scripts as root; the current
builder does this via `chroot` (`internal/sandbox/rootfs_linux.go`), and a
chroot is not a security boundary. For the **curated** v1 catalog this is
acceptable — the packages are vetted by us and signature-verified against the
pinned Alpine repo, the same trust surface as today's base rootfs, and the
variants are pre-baked into the shipped image regardless. But once
**org-authored recipes** ship (the later catalog rows above), the package set is
customer-controlled, so the build *executes customer-influenced code*. That
build must **not** run in the capability-holding `cap-broker` process: it runs in
an isolated/unprivileged builder that emits an immutable, content-addressed
image, and the broker only ever *mounts the result read-only by verified hash* —
never `apk add <customer input>` with host capabilities. This keeps the broker's
invariant intact (it resolves a name → hash and mounts; it never execs
orchestrator- or customer-supplied content). See
`docs/sandbox-security-architecture.md`.

---

## 5. Resources dimension + horizontal scaling

Resource limits live in two places:

- **In-sandbox** rlimits (`RLIMIT_NOFILE`, `RLIMIT_NPROC`) in `spec.go` — set
  per profile (a browser profile needs more than a CI-triage profile).
- **Around the sandbox** — the cgroup/Machine sizing the run lands in.

At horizontal scale the profile becomes the **scheduling unit**: a heavy/browser
profile routes to a large-Machine pool, a cheap profile to small ones. That same
sizing is the natural **metering/billing hook** — "this profile's runs cost this
much and go there." On shared SaaS, larger/browser profiles are the upcharge
lever; on self-host, the operator sizes their own pools. The fleet-side
mechanics — budget-based admission, the eligibility-vs-affinity split in the
run claim, warm-variant tracking, and the cross-org recipe-sharing invariant —
are specified in `docs/specs/horizontal-scaling/` §6.4.

---

## 6. Authoring, scoping, and rule binding

- **Storage + scoping.** Profiles are org-owned. Per the multi-mode read-scoping
  standing rule (CLAUDE.md), every profile read must be team/org-scoped under RLS
  — a profile list keyed on `org_id` alone is a cross-team leak once it carries
  egress config. Team availability is a team-scoped join (which profiles a team
  admin has enabled). Local mode is N=1 and unscoped.
- **Rule binding.** The router (`internal/routing/router.go`) already matches
  events to `task_rules` and fires `prompt_triggers` for auto-delegation. The
  binding adds a profile reference to the trigger/rule, which the delegation
  spawner (`internal/delegate/spawner.go`) resolves to `sandbox.Config` (image
  variant, rlimits, the proxy's allowlist/denylist) + the Machine class. Keep
  selection **explicit and data-driven** — a CI-triage rule must never
  accidentally spin a heavy browser sandbox.
- **Defaults.** A built-in "base" profile (today's behavior) is the default when a
  rule names none, so existing rules keep working unchanged.

---

## 7. Sidebar (deferred, NOT v1): proxied credentials

Org admins will eventually want a profile to let the sandbox's tools *use* a
customer secret (e.g. a staging API token) **without** the agent being able to
read it. This is deferred because it's the one piece we can't fully sort out yet;
it's recorded here so the v1 proxy is built without foreclosing it.

The reason it's hard: for our own proxies we wrote both ends and know exactly
where a credential goes. For an arbitrary customer secret to an arbitrary
service, the proxy doesn't know which requests should carry it, where it goes, or
whether the request is even shaped so we can add it. There are three outcomes,
and they form a ladder:

1. **Destination-scoped injection at the proxy** (Property-B-preserving). Profile
   config: "for traffic to `staging.example`, attach this token as header X." The
   real secret lives in the proxy; the agent sees only a placeholder + the proxy
   endpoint. Requires the proxy to **terminate TLS** (a trusted proxy cert in the
   sandbox) so it can modify the request — the §3.2 "expensive" capability.
   Covers the common "HTTPS API + token in a header" case.
2. **Verbatim injection** (the escape hatch — risk-gated). For non-HTTP-shaped
   secrets (a DB password used over a raw protocol), the only way the tool gets it
   is the real value in the env, which the agent can read. Acceptable for a
   self-hoster using a low-privilege cred (own blast radius); dangerous on shared
   SaaS (a leaked cred could be sent to any allowed public destination). If
   offered, it's itself a tier/risk-gated profile option.
3. **The hard boundary.** Any credential the agent's own process must actively use
   in a way we can't sit in the middle of is, by necessity, reachable by the
   agent. Property B (`internal/sandbox/doc.go`) holds only for credentials we can
   keep in the proxy and inject on a channel we control. This is the edge of the
   model, not a bug to fix.

v1 decision: **gating proxy only (§3).** No credential injection, no TLS
termination, no verbatim-env option. Revisit as a follow-up once the gating
proxy + denylist are shipped and reviewed.

---

## 8. v1 scope vs later

**v1 (this spec):**
- Profile object with three live dimensions: image, egress (gating proxy +
  immutable internal denylist + per-profile public allowlist), resources.
- Org-admin authoring, team-admin availability, rule→profile binding, "base"
  default.
- Self-host-only raw-L3-to-private variant for the private-network case.
- The §3.6 SSRF-bypass review as a hard gate before the public-allowlist flavor
  ships on shared SaaS.

**Later:**
- Proxied credentials (§7), starting with destination-scoped header injection.
- Richer scheduling/metering on the resources dimension.
- External-URL rendering for the browser profile (would route through the same
  gating proxy — `docs/specs/playwright-chromium-sandbox/` §3 flags it).

---

## Related

- `docs/specs/horizontal-scaling/` — §6.4: how profiles interact with the
  executor fleet (eligibility vs affinity in the claim, budget admission,
  cross-org variant sharing).
- `docs/specs/playwright-chromium-sandbox/` — the browser profile; the first
  worked instance of the image dimension.
- `docs/isolation-tiers.md` — the tier ladder the egress power-dial and the
  public-vs-private split slot into.
- `docs/specs/sky-254-runsc-validation/` — the sandbox + the proxy/egress
  mechanics (`ConfigureProxies`, `applyEgressPolicy`) this builds on.
- `internal/sandbox/doc.go` — the Property-A/B credential invariants §7 lives
  inside.
- `docs/sandbox-security-architecture.md` — the sandbox threat model, the exact
  host capability requirements, and the privilege-separation (`cap-broker`)
  design whose "resolve name → mount by hash, never exec supplied content" rule
  the image dimension's build step must respect (§4).
