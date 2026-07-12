# Per-user integration identity

How TF stores "who this user is on the providers it acts against" — GitHub
(`user_github_identities`, SKY-396) and Jira (`user_jira_identities`, SKY-397)
today, Linear as a planned sibling (SKY-398). This is the *integration-identity*
layer; it is not login.

## Two layers TF must not conflate

**Access** vs **identity** are different credentials with different lifecycles:

- **Access** = "TF may read/write this org's repos/issues." Impersonal, org- or
  service-scoped: a GitHub **App installation token**, a Jira **service-account
  OAuth** (SKY-347), a Linear **`client_credentials` app-actor** token. Carries
  no person.
- **Identity** = "this TF user is `@login` / `accountId` / `viewer.id` on this
  host." A per-user *whoami*, captured once and stored. Powers per-user
  predicates (`author_in: [me]`), the personal dashboard, and routing a task to
  the right person.

Identity falls out of access for free **iff** the access credential is
user-scoped (a PAT, a user-to-server OAuth token — both answer "who am I"). An
App/service token authenticates *as the app against the org*, so it carries
access but no identity, and a separate whoami is required. Every provider's
service account is the **access** layer, never identity — so every provider
needs its own per-user identity binding regardless of how access is granted.

| Layer | GitHub | Jira | Linear |
| -- | -- | -- | -- |
| **Access** (impersonal) | App installation token | service-account OAuth (SKY-347) | `client_credentials` app-actor |
| **Identity** (per-user whoami) | `login` | `accountId` | `viewer.id` |

Distinct from **GoTrue's `auth.identities`**, which records *login* identities
(who you authenticated as to get a TF session). The tables here record
*integration* identity (who you are on a provider TF acts against). Different
layer, different schema — `auth.*` vs `public.*`.

## The schema is per-provider sibling tables, not one generic table

Each provider gets its own host/scope-scoped table:

```
user_github_identities (user_id, github_base_url, login,          source, verified_at, …)  -- SKY-396, shipped
user_jira_identities   (user_id, jira_base_url,   account_id, display_name, source, verified_at, …)  -- SKY-397, shipped
user_linear_identities (user_id, workspace_id,    linear_user_id, display_name, source, verified_at, …)  -- SKY-398
```

Each keyed `UNIQUE (user_id, <scope>)`, user-scoped RLS (self-only read/write,
no org leg), `verified_at` stamped on each authenticated confirmation, `source`
recording how the binding was captured (`pat` | `connect_oauth` | `scim` |
`login_claim`).

### Why siblings, not a generic `user_external_identities(provider, base_url, external_id, …)`

The providers genuinely share a shape — same access/identity split, same
"capture a whoami against a scope" need — so a generic table is tempting, and an
earlier SKY-396 draft proposed one. **Linear is the tiebreak against it:**

| | scope dimension | identity payload |
| -- | -- | -- |
| GitHub | **host** (github.com / GHEC / GHES) | `login` — id ≈ name (one value) |
| Jira | **host** (Cloud site / Server-DC) | `accountId` + separate `displayName` |
| Linear | **workspace** (cloud-only, no host) | `viewer.id` + separate `name` |

- **The scope column can't be uniform.** GitHub and Jira scope by host; Linear
  is cloud-only and scopes by *workspace* (a person has a distinct
  workspace-scoped `User`, and OAuth tokens are workspace-scoped). A single
  `base_url` column that means "host" for two providers and "workspace" for a
  third is a provider-defined-meaning column — and `external_id` (login vs
  accountId vs viewer.id) already is one. Two leaky columns is most of the way
  to a JSONB blob, and it pushes the provider switch into every consumer.
- **Payload isn't uniform.** GitHub's `login` is both id and display name; Jira
  and Linear split an opaque id from a human name.
- **No migration debt forces a merge.** The one real pull toward generic was
  "avoid owing a second pre-1.11 migration." But a net-new sibling is an
  additive `CREATE TABLE` with **no data backfill** (captures are fresh) — a
  routine, cheap forward migration, not the data-retrofit SKY-396 front-loads to
  avoid. The generic table's headline benefit is small, and it's paid for in
  typed clarity.

Siblings keep each scope/payload column self-documenting and each `source` CHECK
exact, and the store API typed (`GetGitHubLogin` returns a login; a Jira getter
returns accountId + displayName). The one duplication is the ~10-line RLS +
trigger + grant block per table. A uniform "which integrations has this user
bound?" read (the SKY-271 onboarding gate) just `UNION`s the small tables — the
only consumer that wants cross-provider iteration, and a trivial one.

If a *second host-scoped, single-login* provider ever appears (a GitLab-style
whoami), reconsider a shared SCM-identity table for that subset — but that
generic deliberately excludes Jira and Linear, which is why "github-or-jira" was
the wrong union to build.

## Why host/scope-scoped (the reason a column doesn't survive)

A single human can belong to two orgs on two hosts — github.com for one,
`git.corp.example.com` for another — with a *different login on each*. One
column per provider can't represent that; `(user_id, scope)` can. For the first
self-deploy customer (one org, one host) this is exactly one row per provider
per user — the old column's behaviour, with a future-proof key.

## Runtime contract: NULL-degrades-gracefully

An absent identity row is a **durable, supported state** (carried over from
SKY-264). Reads resolve the org's scope (e.g. `org_settings.github_base_url`),
look up `(user, scope)`, and treat a missing row exactly as the old NULL column:
self-features (`author_in:[me]`, personal dashboard, routing-to-you) go inert;
team reads are unaffected. Drift (rename, left-the-org) reintroduces the absent
state post-onboarding — runtime stays tolerant regardless of the onboarding gate.

## Tickets

- **SKY-396** — `user_github_identities` (shipped here).
- **SKY-397** — `user_jira_identities` (shipped; moved `users.jira_account_id` /
  `jira_display_name` off the row into a host-scoped table keyed on
  `jira.CanonicalHost`, symmetric with the per-(user, host) PAT vault key from
  SKY-442; blocks SKY-270).
- **SKY-398** — `user_linear_identities` (workspace-scoped; gated on a Linear
  integration existing).
- **SKY-271** — capture flows + onboarding gate (consumes these tables).
