# Isolation tiers

How Triage Factory isolates tenants, and how to choose a deployment shape for a given trust requirement. Security-reviewer-facing.

## The two boundaries (read this first)

The single most common confusion: conflating the **org** boundary with the **deployment** boundary. They are different things, and most "is this safe enough?" questions are really about one or the other.

- **Org = the isolation unit.** Your tenant. Within a deployment, every org's data is partitioned by Postgres row-level security (RLS), its secrets sit in one shared Vault namespaced + access-gated per-org, and every agent run is kernel-isolated with gVisor and handed credentials constructed from scratch (no host credential bleed). You are never in a shared org with another customer — every signup gets its own org by default (one org / one team / one user at N=1, expandable to many teams and users).
- **Deployment = the trust unit.** The Postgres cluster, the sandbox host fleet, the proxies, and the operator's access. Whether this is shared across customers — and _who operates it_ — is what the tiers below distinguish.

These coexist without contradiction: **you have your own logically-isolated org, AND (in a shared deployment) your org runs on physical infrastructure shared with other orgs.** The first is always true. The second varies by tier.

A useful pair of one-liners:

> The org is the isolation unit. The deployment is the trust unit.

## The two axes

Every deployment shape is a point on two axes:

1. **What's shared** — from "an App key" (worst) through "server / DB / Vault" to "nothing."
2. **Who operates the shared thing** — you, or a third party (SkyAI).

Logical isolation strength (RLS + per-org Vault + gVisor) is constant across tiers — it's always on. The tiers differ in what _physical_ infrastructure is shared and who runs it.

## The tiers

Higher tier number = stronger isolation.

| Tier    | Shape                                                                                           | What's shared                                            | Operator          | Residual blast radius                                                                                       |
| ------- | ----------------------------------------------------------------------------------------------- | -------------------------------------------------------- | ----------------- | ----------------------------------------------------------------------------------------------------------- |
| **4**   | **Local + PAT** — per-user binary on the user's own machine                                     | Nothing                                                  | You (your laptop) | One engineer's own GitHub access. No shared App, no shared server.                                          |
| **3**   | **Self-hosted** — your own TF deployment (one or many orgs inside)                              | Server / DB / Vault, across _your_ orgs only             | **You**           | Your own infrastructure. Insider risk is your own people, same as any internal system.                      |
| **2.5** | **Managed dedicated** — SkyAI operates, but on single-tenant infrastructure provisioned for you | Nothing cross-customer; SkyAI operates                   | SkyAI             | Your dedicated instance + SkyAI ops. No other tenants in the blast radius. _(Future offering — not built.)_ |
| **2**   | **Shared hosted** — `app.triagefactory.com`; your own org among all customers' orgs             | Server / DB / Vault / sandbox host, across _all_ tenants | SkyAI             | Shared infrastructure + SkyAI ops + every other tenant (at the infra layer only — see below).               |
| **1**   | **Shared org** — multiple parties inside one org sharing an App                                 | The App key too                                          | varies            | Widest. **Anti-pattern; not offered.**                                                                      |

Notes:

- **Tier 3 covers both single-org and multi-org self-hosted.** A customer running one deployment with several orgs inside it (e.g. a locked-down org separated from the rest of the company) is still Tier 3 — they operate it, nothing is shared with other parties. The org is still the isolation unit within their own deployment.
- **Tier 1 is what we deliberately do NOT do.** Personal-org-per-user exists precisely so customers are never co-located in one org. It's listed only to name the anti-pattern.

## What `app.triagefactory.com` (Tier 2) shares

**Strong, always-on logical isolation per org:**

- RLS partitions every row by org. An org's queries can only read its own rows.
- **One shared Vault gated by `SECURITY DEFINER` functions.** Secrets (GitHub App keys, PATs, LLM credentials) live in a single `supabase_vault` store, namespaced by secret name (`org/<org_uuid>/<key>`). The only path in for the app role is three `SECURITY DEFINER` functions (`vault_{put,get,delete}_org_secret`) that raise `42501` unless the requested `org_id` matches `tf.current_org_id()` (read from the request's verified JWT claim). The app role has no direct grant on the `vault` schema, so the gate can't be bypassed. The `org_id` a caller passes is _not_ trusted — the JWT claim is the authority, the same anchor every RLS policy keys on.
- gVisor kernel-isolates every agent run; an agent can't read another org's worktree or the host's secrets.
- Sandboxed agents get credentials built from scratch — no host credential bleed into the sandbox.

**Residual, infra-layer blast radius (not zero):**

A compromise at the _infrastructure_ layer — an RLS-policy bug, a gVisor escape, a Vault scoping error, or a SkyAI ops/insider compromise — has a reach that spans tenants, because the orgs share one Postgres cluster and one sandbox host fleet under SkyAI's operation. This is the inherent property of shared third-party infrastructure, and no tenancy model removes it. Only a dedicated deployment (Tier 2.5 or 3) does.

For the overwhelming majority of code, Tier 2 is the right call — it's the **same trust class the customer already accepts by hosting their code on github.com**, itself a shared, third-party-operated, multi-tenant system. If they trust GitHub's tenant isolation for their source, TF-hosted is a comparable bet on the same source.

The narrow exception is code that _isn't even on standard github.com_ — source kept on an isolated-network enterprise GitHub Server precisely because shared third-party infra is off the table. That code's owners draw the line at Tier 2 for _everything_ in their toolchain, and they should self-host TF (Tier 3) or stay local (Tier 4) — the same boundary they already drew with GitHub.

## How the segments self-select

The customers for whom Tier 2 is unacceptable overlap heavily with the customers who _can't reach_ `app.triagefactory.com` anyway:

- Maximally-isolated source (defense, regulated, platform-critical) tends to live on isolated-network GitHub Enterprise Server. `app.triagefactory.com` is network-unreachable from there regardless of how strong the isolation is.
- So the isolation requirement and the network unreachability correlate. The hosted option naturally self-selects to the population for whom Tier 2 is fine.

It's a correlation, not a law — a cloud-reachable-but-security-strict customer (source on Enterprise Cloud, but a policy that says no to shared SaaS for build tooling) exists. For them: self-host (Tier 3), managed dedicated (Tier 2.5, when offered), or local (Tier 4).

## How this compares to peers

The **hosted + self-host** split mirrors the dev-tools companies with the most security-conscious customer bases:

| Company                 | Broad-market hosted                                                                                     | High-isolation option                                       |
| ----------------------- | ------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- |
| GitHub                  | github.com + Enterprise Cloud — Tier 2, multi-tenant + enterprise features (SAML, audit, IP allowlists) | Enterprise Server — self-hosted, customer-operated (Tier 3) |
| GitLab                  | GitLab.com SaaS (Tier 2)                                                                                | Self-managed (Tier 3)                                       |
| HashiCorp               | HCP (Tier 2)                                                                                            | Self-managed (Tier 3)                                       |
| Linear / Notion / Figma | SaaS only (Tier 2)                                                                                      | _(none — that segment isn't served)_                        |

Tier 2.5 (vendor-operated, single-tenant dedicated) is offered by some vendors (e.g. Sourcegraph managed instances, Auth0 private cloud) and serves "we want isolation but won't operate it ourselves." GitHub doesn't do this — Enterprise Cloud is shared, Enterprise Server is customer-run — so it's not required to match the peer model, but it's a known premium lever.

## Choosing a tier

| If the requirement is…                                                       | Use                                                                                                                                                                                               |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Individual / small team, code already on github.com                          | Tier 2 (`app.triagefactory.com`) — sign up, get your own org                                                                                                                                      |
| "We won't put build tooling on shared SaaS" but code is cloud-reachable      | Tier 3 self-host, or Tier 2.5 managed dedicated (when offered)                                                                                                                                    |
| Source on isolated-network enterprise GitHub                                 | Tier 3 self-host (the only network-reachable option)                                                                                                                                              |
| One engineer, maximum isolation, no shared anything                          | Tier 4 local + PAT — the most isolated shape on the ladder                                                                                                                                        |
| Sensitive code that must not commingle with the rest of a company's TF usage | A separate org is the _logical_ unit; a separate deployment (Tier 3) is the _physical_ one. For code that "nothing can touch," use a separate deployment, not a separate org inside a shared one. |

The last row is the important nuance: **the org is the isolation unit, but for code that cannot share physical infrastructure, the answer is a separate deployment — not a separate org inside someone else's deployment.**

## Related

- `docs/for-agents/multi-tenant-architecture.html` — the full multi-tenant design (RLS, Vault, gVisor, the v1/v2/v3 scope).
- `docs/self-hosting/install.md` — operator install flow for a Tier 3 self-hosted deployment.
