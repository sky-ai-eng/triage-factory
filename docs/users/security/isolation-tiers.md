# Isolation tiers

How Triage Factory separates tenants, and which deployment shape fits a given
trust requirement.

## Org and deployment are different boundaries

- **The org is the isolation unit.** Within a deployment, Postgres row-level
  security (RLS) partitions every org's rows, each org's secrets are encrypted
  in the application and stored per org, and every agent run is isolated in
  its own gVisor sandbox with credentials built for that run. Every signup gets
  its own org, so customers never share one.
- **The deployment is the trust unit.** It is the Postgres cluster, the
  executor fleet, the proxies, and whoever operates them. The tiers below
  differ in what is shared at this level and who runs it.

Both hold at once: your org is logically isolated, and in a shared deployment
it runs on infrastructure shared with other orgs. The logical isolation is the
same in every tier.

## The tiers

Higher number, stronger isolation.

| Tier | Shape | Shared | Operator | Residual blast radius |
| --- | --- | --- | --- | --- |
| **4** | **Local**: one user's binary on their own machine | Nothing | You | One engineer's own GitHub access |
| **3** | **Self-hosted**: your own deployment, one or many orgs | Server, database and secrets, across your own orgs only | You | Your own infrastructure and staff |
| **2.5** | **Managed dedicated**: single-tenant infrastructure SkyAI operates for you (not offered yet) | Nothing across customers | SkyAI | Your instance and SkyAI's operators |
| **2** | **Shared hosted**: your org on `app.triagefactory.com` | Server, database, secrets table and executor fleet, across all customers | SkyAI | Shared infrastructure, SkyAI's operators, and other tenants at the infrastructure layer |
| **1** | **Shared org**: several parties in one org | Everything, including the GitHub App | Varies | Everything. Not offered. |

A self-hosted deployment with several orgs (for example, one locked-down org
apart from the rest of the company) is still Tier 3.

## What Tier 2 shares

Always on, per org:

- RLS limits every query to the org's own rows.
- Secrets (GitHub App keys, tokens, LLM credentials) are encrypted with
  AES-256-GCM using `TF_SECRET_ENCRYPTION_KEY`, which lives in the
  deployment's environment, never in Postgres. The ciphertext sits in one
  `public.org_secrets` table behind RLS on the org (and on the user, for
  per-user secrets). Each ciphertext is bound to its own org, user and key
  name, so a blob copied into another row by a direct database write fails to
  decrypt.
- Each agent run is in its own gVisor sandbox and cannot read another org's
  worktree or the host's secrets.

Not zero: an RLS policy bug, a gVisor escape, or a compromise of SkyAI's
operators reaches across tenants, because the orgs share one database and one
executor fleet. Only a dedicated deployment (Tier 2.5 or 3) removes that.

Tier 2 is the same trust class as hosting code on github.com, which is also
shared and operated by a third party. Code kept on an isolated GitHub
Enterprise Server because shared third-party infrastructure is off the table
belongs on Tier 3 or 4; `app.triagefactory.com` usually cannot reach it
anyway.

## Choosing a tier

| Requirement | Tier |
| --- | --- |
| Individual or small team, code on github.com | 2 |
| No build tooling on shared SaaS, code reachable from the internet | 3, or 2.5 once offered |
| Source on an isolated GitHub Enterprise Server | 3 |
| One engineer, nothing shared | 4 |
| Code that must not share infrastructure with the rest of the company's Triage Factory use | A separate deployment (3), not a separate org in a shared one |

A separate org is the logical boundary. For code nothing else may touch, use
a separate deployment.

## Related

- [Security overview](security-overview.md)
- [Self-hosting](../self-hosting/install.md)
- [`specs/multi-tenant-architecture.html`](../../../specs/multi-tenant-architecture.html):
  the original multi-tenant design; not kept current
