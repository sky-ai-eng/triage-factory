# GitHub connection model — tenancy, identity, and the shared App

Design of record for how a Triage Factory workspace holds GitHub access:
what a connection *is*, which shapes are supported, and what has to be
true before an official one-click Sky AI GitHub App can exist beside the
bring-your-own one.

Status: **accepted design.** The tickets in §4 are the substrate — every
one of them is correct in the product as it ships today and none of them
require the shared App to exist. The shared App itself is **not in this
epic**; §5 records what it will need and why each piece is deferred.

Scope note: both modes. Unlike the horizontal-scaling epic, this one is
not multi-only — local mode has a GitHub connection too, and several
tickets fix bugs that bite a laptop today. Per-ticket mode impact is
called out where it differs.

---

## 0. The seven shapes, out of one binary

| # | Mode | Credential | Status |
| --- | --- | --- | --- |
| 1 | local | PAT | ships |
| 2 | local | BYO App | ships |
| 3 | multi self-host | PAT | ships |
| 4 | multi self-host | BYO App | ships |
| 5 | multi SaaS | PAT | works, unoffered |
| 6 | multi SaaS | BYO App | works, unoffered |
| 7 | multi SaaS | **official one-click App** | not built |

Rows 1–6 all share one property: **the org owns its own App key**, so
`GET /app/installations` under that key returns only that org's
installations and there is no cross-tenant write path anywhere. Row 7
breaks it — the PEM is deployment-level, so the same call returns every
tenant's installations.

That single difference is why row 7 is deferred rather than incremental,
and why several tickets below exist: they remove the assumptions row 7
would otherwise silently invalidate.

### The vocabulary

- **Workspace** — a TF tenant. One `org_id`. The user-facing word is
  *workspace*, never "organization" (that word is taken by GitHub and
  reads as a GitHub org in copy). `org_id` in the schema, workspace in
  the UI.
- **Installation** — one GitHub *account's* grant of an App. Per account,
  not per repository. An account has at most one installation of a given
  App.
- **The grant** — the repositories an installation can reach.
  `GET /installation/repositories`. The only set the runtime cares about.
- **The tracked set** — the repositories a TF team watches.
  `team_github_repos`. Always a subset of the grant in practice, but
  nothing enforces that, which is what makes scope drift possible.
- **Access vs identity** — *access* is impersonal and carries no person
  (the installation token). *Identity* is a per-user whoami captured once
  (`user_github_identities`). Under an App they are never the same
  credential. Under a PAT they are fused, which is the shape the App
  exists to escape.

---

## 1. Decisions already made — do not re-derive these

Cheap agents re-litigate design when a ticket leaves room. These are
closed. If a ticket seems to require reopening one, stop and ask rather
than choosing.

**1.1 · A workspace has exactly one credential slot.** Enforced in the
schema, not by convention: `org_github_apps.org_id` is the PRIMARY KEY
(sqlite baseline `:500`, pg `:5881`), `github_base_url` lives on
`org_settings` (sqlite `:340`), and App-XOR-PAT is advisory-locked on
write (`internal/server/org_credentials.go:110-130`) with a single read
gate (`internal/github/resolver.go:375-386`). One workspace: one
credential, one GitHub host. It may hold *many installations* (see 1.2),
but only ever through one App.

**1.2 · Many installations per workspace is already supported.**
`org_github_app_installations` is PK `(org_id, installation_id)` with a
partial unique on `(org_id, account_login) WHERE removed_at IS NULL` —
many installations, at most one active per account. An enterprise-owned
internal App produces one installation *per GitHub org in the
enterprise*, and the schema already holds that. What does not hold is
`installationFor` (see ticket TFAC-804).

**1.3 · An installation belongs to exactly one workspace, per GitHub
host.** Sharing one installation across two workspaces is refused: the
grant is not divisible so neither workspace's repo scoping constrains the
other, both act as the same bot identity so attribution collapses,
`installation.deleted` fires once so one cannot leave without cutting off
the other, the rate budget is shared, and the account owner has no way to
enumerate who is riding their installation.

The escape hatch is a second App: one installation per *App* per account,
so a different App is a different installation with its own id, grant,
revocation path and rate limit.

**This invariant is deliberately unenforced today.** No unique index. See
ticket TFAC-807 for the reasoning and §5 for where enforcement lands.

**1.4 · The repository picker shows the grant, never the viewer's
intersection with it.** Not a privacy call — a consistency one. The
intersection (`GET /user/installations/{id}/repositories`) is a *per-user
view backing a shared write*: an admin with narrower GitHub access opens
a team's tracked-repo picker, sees fewer repositories than are actually
tracked, saves, and either untracks them or shows config that disagrees
with reality — while nothing at runtime breaks, because delegation mints
from the grant and never consults anyone's personal access.

Generalized: **no shared config surface may render from the viewer's
GitHub permissions.** Two workspace admins must see the same page.

**1.5 · `GET /user/installations` is a gate, never a source.** It answers
yes/no about one installation at one instant, for the person binding. It
must never populate a list, back a render, or be persisted. (Row 7 only;
nothing calls it today.)

**1.6 · Tracking changes stay forward-only.** Unchanged from `CLAUDE.md`.
Nothing in this epic prunes tasks, closes entities, or reconciles
`task_teams` when a grant narrows or a repo is untracked.

---

## 2. What already exists — reuse, don't rebuild

Verified against the tree at `d858de17`. Line numbers are current as of
that commit; re-grep rather than trusting them blindly if the file has
moved.

**Storage.** `org_github_apps` (one row per org: `app_id`, `slug`,
`client_id`, `client_secret_ref`, `pem_ref`, `webhook_secret_ref`,
`owner_type`, `active`, `bot_user_id`) and
`org_github_app_installations` (`installation_id`, `org_id`,
`account_type`, `account_login`, `installed_at`, `removed_at`). Secrets
are refs; values live in Vault/keychain. Postgres RLS: the app pool may
*read* installations and is hard-denied all writes (pg baseline
`:5946-5962`) — every write goes through the admin pool.

**Discovery.** `DiscoverAppInstallations`
(`internal/db/github_app_backfill.go:55`) mints an app JWT and lists
`GET {apiBase}/app/installations`; `ReconcileInstallations` (`:107`)
set-diffs the mirror and soft-removes what GitHub no longer reports. Its
own comment names the assumption row 7 breaks: *"the per-org App owns
every installation it reports (v1 is per-org Apps only)."* Manual
trigger: `POST /api/orgs/{org_id}/github/app/installations/refresh`
(`internal/server/github_app_handlers.go:153-211`) — the local-mode
discovery path, since a NAT'd laptop never receives webhooks.

**Minting.** `internal/githubapp/mint.go` — `MintScopedInstallationToken`
(`:315-328`) POSTs `{"repositories":[names],"permissions":{…}}`;
`ListInstallations` (`:437`) pages `/app/installations` and parses a
deliberately narrow subset (`:416-427`). Resolver entry points
`TokenForRepoScoped` / `TokenForReposScoped`
(`internal/github/resolver.go:504-545`). Per-team scoping is enforced by
**GitHub**, not by TF code — `internal/credprovision` mints one
repo-scoped token per repository in a run's authorized set, and
`internal/credbundle/repo_token.go` resolves per `(owner, repo)` and
falls through to the PAT rather than to a sibling repository's token.

**Webhooks.** `internal/server/github_webhooks.go` — `POST
/api/webhooks/github/{org_id}`, tenant from the path, HMAC verified
against that org's stored secret. Handles `installation.created` →
`UpsertInstallation` and `deleted` → `MarkInstallationRemoved` plus the
token-cache invalidate hook (`internal/server/server.go:581-583`).
Everything else publishes `webhook:github:*` to the bus and 204s.

**Transitions.** `internal/server/github_access.go` owns all five
App/PAT transitions (register-staged, import-staged, cutover,
switch-to-PAT, discard-staged), each advisory-locked against the others.

**Caches.** `internal/github/tokencache.go` keyed
`(orgID, installationID)`; `internal/github/repo_coverage_cache.go`, 5
min TTL, positive-only, keyed `orgID + lower(owner/repo)`.

**Hosts.** `githubAPIBase` already maps public → `api.github.com`,
`*.ghe.com` → its `api.*` subdomain, anything else → `/api/v3`. GHES and
residency are handled.

---

## 3. Conventions these tickets must follow

- **Dual-dialect or it doesn't exist.** Every store change is the `db`
  interface + both `internal/db/sqlite` and `internal/db/postgres`
  implementations + a case in the `internal/db/dbtest` conformance suite.
  A ticket's acceptance criteria must name the conformance case.
- **Migrations are forward-only, fresh-install-only.** New files under
  `internal/db/migrations-sqlite/NNNNNNNNNNNN_description.sql` and
  `internal/db/migrations-postgres/`, 12-digit `YYYYMMDDNNNN` version,
  `-- +goose Up` / `-- +goose Down` with the Down block a
  `SELECT 'down not supported';` no-op.
- **Backward compatibility.** No multi-mode deployment exists yet, so the
  Postgres baseline is freely editable and no deployed-state analysis is
  required for Postgres-only changes. **Local mode is different** —
  SQLite schema, `~/.triagefactory` state, and anything on the
  `TF_ROLE=all` behavior path ships as a forward migration and must work
  against an existing laptop install.
- **No ticket references in code comments**, with the single exception of
  the `TODO(TFAC-NNN):` deferral marker at the site of a known gap.
  Comments explain *why*, never *what*.
- **Read the surface before describing it.** Several tickets touch
  settings and the setup wizard. Those files were rebuilt on
  `aa/design-language`; read the current file, do not describe it from
  the ticket text or from a filename.

---

## 4. The tickets

Eleven, grouped by what they unblock. Dependencies are stated per
ticket; where none is stated, the ticket is independent and can be
picked up in any order.

---

### TFAC-803 · Store GitHub's numeric ids for the things we bind on

**Depends on:** nothing.
**Modes:** both.

#### The defect

The account **login** is currently a credential-resolution join key, not
merely provenance:

- `internal/github/resolver.go:678-704` — `installationFor` selects an
  installation by `strings.EqualFold(in.AccountLogin, target)`.
- `internal/github/repo_coverage_cache.go:20` — the coverage cache is
  deliberately *not* keyed by installation, on the stated grounds that
  "an account login maps 1:1 to an installation."
- the partial unique on the mirror is `(org_id, account_login)`.

GitHub logins are renameable. So **a GitHub organization rename breaks
token minting today** — not just audit trails. `installation.id` and the
numeric account id are stable across renames; the login is not.

Both values are already on the wire and discarded:
`internal/githubapp/mint.go:416-427` parses id, account login, account
type and `created_at` off `/app/installations` and drops the rest. The
`installation` webhook payload carries `installation.account.id` and
`sender.id` likewise.

#### What to build

Two nullable columns, both dialects:

- `org_github_app_installations.account_id` — GitHub's numeric account
  id, stored as TEXT (see the note on types below).
- `user_github_identities.github_user_id` — same, for the authenticating
  human.

Widen the `/app/installations` parse and the webhook payload struct to
capture them. Populate on every write path. Backfill opportunistically:
a row whose `account_id` is NULL gets filled on the next reconcile or
the next webhook that names it. **Do not** add a backfill job that walks
every org — the reconcile already runs.

Migrate the *match* in `installationFor` to prefer `account_id` when both
sides have one, falling back to the case-insensitive login match when
either side is NULL. Keep `account_login` as a display field; it is what
the UI renders and it must stay current.

**Types:** TEXT rather than BIGINT, matching how `entities.source_id` and
`org_github_app_installations.installation_id` already store
GitHub-issued ids. Consistency with the surrounding schema beats the
marginal index efficiency, and it avoids a second convention.

#### Invariants — what must be observable

- An installation whose GitHub account is renamed continues to resolve
  and to mint. State this as a test: seed a row with `account_id` set and
  `account_login` stale, resolve by the *new* login, assert the
  installation is found.
- **Negative space:** an installation whose `account_id` is NULL resolves
  exactly as it does today — no regression for un-backfilled rows, and no
  error path introduced by the NULL.
- Two installations on the same host never resolve to each other because
  their logins collide after a rename.
- A row's `account_login` is updated, not preserved, when GitHub reports
  a new one for a known `account_id`.

#### Write ownership

`account_id` is written by exactly two writers: `UpsertInstallation` (the
webhook path) and `BackfillInstallationsFromAPI` (the reconcile path).
Nothing else may write it. `github_user_id` is written by the identity
upsert in `internal/server/github_connect.go:293-301` and the PAT bind at
`:509`; nothing else.

#### Acceptance criteria

- Both migrations land, Down blocks are the standard no-op.
- `internal/db/dbtest` gains a conformance case asserting round-trip of
  `account_id` through `UpsertInstallation` /
  `ListInstallationsForOrgSystem`, and of `github_user_id` through
  `UpsertGitHubIdentity` / the reverse lookup, on **both** dialects.
- A resolver test covers the rename case and the NULL case above.
- No existing test changes its expected values. If one does, the change
  is wrong.

#### Deferred

Repository numeric ids are **not** in scope — see TFAC-810. Scoping the
token mint by `repository_ids` instead of names is **not** in scope; it
depends on TFAC-809 and is separately tracked.

---

### TFAC-804 · Resolve an installation when a workspace has more than one

**Depends on:** TFAC-803 (uses `account_id` when present).
**Modes:** both.

#### The defect

`internal/github/resolver.go:678-704`: with an empty `target`,
`installationFor` resolves **only when exactly one installation exists**,
and otherwise matches no installation at all. That is always true under a
private BYO App — one owning account, one installation — and false the
moment an enterprise-owned internal App lands, which produces one
installation per GitHub org in the enterprise.

This is the concrete code cost of supporting enterprise-owned Apps
(decision 1.2), and it fails closed but silently: callers see
"no credentials" rather than "ambiguous."

#### What to build

Give `installationFor` a deterministic answer for the multi-installation
case rather than a count check.

- `target` non-empty: match on `account_id` when both sides have one,
  else case-insensitive `account_login`. Unchanged behavior, better key.
- `target` empty and **one** installation: return it. Unchanged.
- `target` empty and **more than one**: this is a caller error, not a
  data error. Return a distinguishable error (not `ErrNoGitHubCredentials`)
  naming how many installations matched, so the failure reads as
  "ambiguous, tell me which account" rather than "you have no GitHub
  access."

Then audit every caller that passes an empty target and give it one.
Enumerate by invariant, not by example: **every call site that reaches
`installationFor` must be able to name the account it means**, because
the repository it is acting on determines the account. Where the caller
holds an `owner/repo`, the `owner` is the target.

#### Invariants

- A workspace with N installations resolves a token for a repository in
  any of them.
- **Negative space:** a workspace with N installations never resolves a
  repository in account X to account Y's installation. A cross-account
  mint must be impossible, not merely unlikely — a token minted from the
  wrong installation would either 422 at GitHub or, worse, succeed with
  the wrong grant.
- A workspace with exactly one installation behaves byte-identically to
  today, including for empty targets.

#### Acceptance criteria

- Table-driven resolver tests for: one installation + empty target, N
  installations + empty target (ambiguous error), N installations +
  explicit target, target matching by `account_id` against a stale login,
  target matching no installation.
- Every empty-target call site either passes a target or is documented in
  the PR description as legitimately ambiguity-free.

#### Interactions with siblings

Shares `internal/github/resolver.go` with TFAC-803. If both are in flight,
A1 lands first; A2 rebases onto its `account_id` match rather than
duplicating it.

---

### TFAC-805 · Model installation suspension

**Depends on:** nothing.
**Modes:** multi in practice (local receives no webhooks), but the model
and the reconcile path are both-dialect.

#### The defect

`internal/server/github_webhooks.go:153-155` — `suspend` and `unsuspend`
fall to the `default` branch: published to the bus, 204, nothing else.
The handler's own comment says the mirror "only tracks existence, not
those finer states." Consequences:

- `domain.OrgGitHubAppInstallation`
  (`internal/domain/github_app.go:53-59`) carries five fields and no
  suspension, so a suspended installation is indistinguishable from a
  working one.
- `onInstallationRemoved` never fires, so the cached installation token
  in `internal/github/tokencache.go` **stays valid until expiry**.
- `repo_coverage_cache` is positive-only on a 5-minute TTL, so cached
  reachability also persists.

Net: up to an hour of 403s presented as generic failures, on a row that
still reads as connected.

#### What to build

- `suspended_at` (nullable timestamp) and `suspended_by` (nullable text —
  the suspending GitHub login) on `org_github_app_installations`, both
  dialects, and on the domain type.
- Handle `installation.suspend` → set them and invalidate the token cache
  for that `(org, installation)`; `installation.unsuspend` → clear them.
  Reuse the existing `onInstallationRemoved` hook shape rather than
  inventing a second one — rename it to reflect that it now means
  "this installation's tokens are no longer valid," and keep `deleted`
  wired to it.
- **Pick suspension up in the reconcile too**, not only from webhooks.
  `GET /app/installations` returns `suspended_at` / `suspended_by` on the
  installation object; `mint.go:416-427` currently discards them. Widen
  that parse and have `ReconcileInstallations` write them. This is what
  makes suspension correct in a deployment that missed a delivery, and
  it is the only path that works at all in local mode.
  **Verify the field names against a live response before relying on
  them** — if they are absent, capture that in the PR and fall back to
  the webhook-only path rather than inventing field names.

#### Lifecycle

Every arrow, and what a crash at each does:

| From | To | Trigger | Crash mid-way |
| --- | --- | --- | --- |
| active | suspended | `installation.suspend`, or reconcile sees `suspended_at` | delivery is lost (GitHub does not retry); the next reconcile corrects it |
| suspended | active | `installation.unsuspend`, or reconcile sees NULL | same |
| suspended | removed | `installation.deleted` | `removed_at` set; suspension columns retained, not cleared |
| removed | active | re-install → `created` | `removed_at` cleared **and** suspension columns cleared in the same statement |

The last row is the one an agent gets wrong: a re-install must not
inherit the previous installation's suspension.

#### Invariants

- A suspended installation is distinguishable from an active one in
  `ListInstallationsForOrgSystem` output.
- No cached token survives a suspension. Assert this directly: prime the
  token cache, deliver a `suspend`, assert a subsequent resolve does not
  return the cached value.
- **Negative space:** a suspension does not soft-remove the row, does not
  fire the removal path's other effects, and does not stop the org's
  poller for *other* installations.
- `unsuspend` restores exactly the prior state — no residue.

#### Product surface

This ticket deliberately ships the **model** and the **invalidation**
only. It does not add a UI state, a banner, or a change to what polling
does when it encounters a suspended installation. Those are a design
decision, not an implementation one, and belong to the org-page work.
The status DTO (`internal/server/github_app_handlers.go`) **should**
carry the new fields so the front end can consume them when it is ready —
adding a field to that response is the whole product-surface
follow-through here. Say so in the PR rather than leaving it implicit.

#### Acceptance criteria

- Migration both dialects; `dbtest` conformance case for the four
  lifecycle rows above.
- Webhook handler tests for `suspend`, `unsuspend`, and `created`-after-
  `deleted`-while-suspended.
- A test asserting token-cache invalidation on suspend.
- The status DTO carries the fields; its existing test is extended, not
  replaced.

---

### TFAC-807 · Denormalize the GitHub host onto the installation row

**Depends on:** nothing.
**Modes:** both.

#### Why

`org_github_app_installations` has **no host column**. The host is
reachable only by joining `org_settings.github_base_url` (see
`internal/db/postgres/github_apps.go:268-273`). The baseline's own
comment justifying the composite PK (sqlite `:517-521`, pg `:5912-5916`)
cites "per-org GHES bases" while leaving the host unreachable from the
row itself.

That matters because installation ids are unique per GitHub *deployment*,
not universally: a self-host aggregating orgs across two GHES instances
can see the same numeric id twice, meaning different things.

#### What to build

`github_host TEXT NOT NULL` on `org_github_app_installations`, both
dialects, backfilled from the owning org's `org_settings.github_base_url`
at migration time, and written by both installation writers thereafter.
Normalize it identically to how `resolveGitHubHost`
(`internal/server/github_connect.go:80-86`) normalizes for identity
keying — same string for the same host, or the column is useless.

**Ship the column only.** Do **not** add
`UNIQUE (github_host, installation_id)`. See below.

#### The constraint, and why it is deferred rather than forgotten

Decision 1.3 — an installation belongs to exactly one workspace per host
— is real policy, but it is **unenforceable-by-index today for a reason
that is not squeamishness**: there is no cross-tenant write path in rows
1–6. Each org owns its App, so `/app/installations` under org A's key
returns only A's installations, and `UpsertInstallation` runs only for
deliveries HMAC-verified against A's own secret. A row claiming another
org's installation id is inert anyway, because A's App key cannot mint
against B's installation.

The constraint becomes load-bearing exactly when row 7 exists, and it
must land there **paired** with scoping `ReconcileInstallations` — see
§5.2. An advisory lock is *not* a substitute for the index at that point:
the lock serializes concurrent binds, but the realistic row-7 bug is a
background job attributing every tenant's installation to one org, and a
lock does not validate, it only orders.

Record the invariant in a comment on the migration — the *why*, not the
*what*: that uniqueness is intentionally unenforced, and what would have
to change to enforce it.

#### Acceptance criteria

- Migration both dialects, with a backfill that leaves no NULL.
- `dbtest` conformance case round-tripping `github_host`.
- A test that two orgs on different hosts may hold the same
  `installation_id` — i.e. that nothing accidentally enforces uniqueness.

---

### TFAC-806 · Make the GitHub credential class explicit

**Depends on:** nothing.
**Modes:** both.
**This is the highest-leverage ticket in the epic.** Read it fully.

#### The defect

Row 7 is the only shape where an org legitimately has **no
`org_github_apps` row** — the shared App's key is deployment-level, and
`org_github_apps.app_id` is UNIQUE, so N managed orgs cannot each hold a
row pointing at one shared app id.

Today "no row" is read as exactly one thing — **PAT** — at no fewer than
seven sites:

- `internal/github/resolver.go:375-386` (`activeApp`, the XOR gate)
- `internal/github/resolver.go:639` (`OrgIdentityFor`)
- `internal/poller/manager.go:786`
- `internal/server/repos_handler.go:64`, `:108`
- `internal/server/credentials.go:325`
- `internal/server/github_app_handlers.go:76`
- `internal/server/github_webhooks.go:68-83` (nil App → 404)

Add row 7 and every one is wrong by default. The failure direction is bad
too: `activeApp` fails **open toward PAT** on a store read error
(`resolver.go:376-380`), so a managed org would degrade silently to a
credential it does not have.

The fix is not to add the seventh shape. It is to stop inferring meaning
from a missing row, so that adding it later is a new `case` a compiler
and a reviewer both see.

#### What to build

`org_settings.github_credential_class TEXT NOT NULL DEFAULT 'pat'`, both
dialects. Values: `pat`, `byo_app`. **`managed_app` is a documented
future value and must not be written by this ticket** — but every switch
statement introduced here must handle an unknown value explicitly rather
than falling through to a default arm.

App-validated, not CHECK-constrained, matching the established
convention for `source` columns (see the baseline comment at sqlite
`:68`: *"source is app-validated, not CHECK-constrained"*). A CHECK would
have to be migrated to add `managed_app`; app validation would not.

Backfill: `byo_app` where an `org_github_apps` row exists, `pat`
otherwise. Note this correctly assigns `byo_app` to a *staged* App
(`active = false`) that has a live PAT — the class names which credential
system the org is in, and `active` continues to name which one is live.
Those are two orthogonal facts and the ticket must not collapse them.

Then convert the seven sites to branch on the class rather than on row
presence.

#### Write ownership — the trap to avoid

**`github_credential_class` is owned by the credential transitions, not
by the settings writer.**

`UpdateSettings` (`internal/db/sqlite/orgs.go`, and its Postgres twin)
is a column-listed upsert: `INSERT INTO org_settings (…explicit list…)
… ON CONFLICT DO UPDATE SET …same list…`. A new column that is **absent
from both lists** is left untouched on update and takes its DEFAULT on
insert, which is exactly the behavior needed.

**Do not add `github_credential_class` to either list.** Adding it —
which looks like tidiness — would reset the class to the zero value on
every settings save, silently converting a BYO-App org to PAT. Add a
comment at the upsert saying why the column is deliberately absent.

The five writers that *do* own it, all in
`internal/server/github_access.go` unless noted:

| Transition | Writes |
| --- | --- |
| PAT bind (`internal/server/org_credentials.go`) | `pat` |
| App register-staged (`github_app_register.go:556-580`) | `byo_app` |
| App import-staged (`github_app_import.go:466-486`) | `byo_app` |
| Cutover (App becomes live) | `byo_app` (already, but assert) |
| Switch-to-PAT | `pat` |
| Discard staged | `pat` if no App row remains |

Each of these already runs inside a transaction and under the keyed
advisory lock (`internal/server/advisorylock.go:101`,
`githubAppRegRMWLockSalt`). The class write goes **inside** the same
transaction as the credential change — never a follow-up write, because
a crash between them would leave the class lying about the credential.

#### Invariants

- For every org, at every instant, the class agrees with reality: `pat`
  iff no active App, `byo_app` iff an `org_github_apps` row exists.
- **Negative space:** a bulk `POST /api/settings/org` save does not
  change the class. Assert this directly — it is the failure this ticket
  most likely introduces.
- A crash between a credential transition and its class write is
  impossible, because they are one transaction.
- Every converted call site behaves byte-identically for `pat` and
  `byo_app`. This ticket is a pure refactor at the behavior level; the
  only new observable is the column.

#### Product surface

None. The class is derived from actions the user already takes (binding a
PAT, registering an App); there is no new setting, no wizard step, and no
API field. If a ticket-reader finds themselves adding a picker for it,
that is a misreading — the class is a projection of the credential
choice, not a second choice.

#### Acceptance criteria

- Migration both dialects with the backfill.
- `dbtest` conformance case: class round-trips, **and** an
  `UpdateSettings` call leaves an existing non-default class intact.
- A test per transition asserting the class after it.
- The seven call sites converted, with existing tests unchanged.

---

### TFAC-808 · Dedup GitHub webhook deliveries

**Depends on:** nothing.
**Modes:** multi (local receives no webhooks), but the store is
both-dialect.

#### The defect

`X-GitHub-Delivery` is read only to populate the published event's
metadata (`internal/server/github_webhooks.go:96`, `:154`, `:171`) — it
is never persisted and never checked. A redelivery therefore re-runs
`UpsertInstallation` / `MarkInstallationRemoved` and re-publishes to the
bus.

GitHub does **not** auto-retry failed deliveries, but it does support
manual and API-driven redelivery
(`POST /app/hook/deliveries/{id}/attempts`), which is exactly the
recovery path an operator will use — so redeliveries are a normal
occurrence, not an anomaly.

#### What to build

A delivery-dedup table, modelled on the existing ee implementation
(`ee/slack/store/pg/pg.go:267-280`): insert-on-conflict-do-nothing, and a
prune of rows older than 72h.

**Key it on `(installation_id, delivery_id)`, not `(org_id, delivery_id)`.**
The org-keyed form cannot be written before the tenant is known, which is
precisely the shape row 7 needs; the installation-keyed form costs
nothing today and does not have to be re-keyed later. A delivery with no
installation id (`github_app_authorization`, for instance) keys on
delivery id alone.

Dedup must gate **before any side effect** — before the upsert, before
the bus publish.

#### Loser contract

A delivery whose insert conflicts is a *duplicate*, and the correct
response is a **204, not an error**. Losing is terminal for that request
and carries no retry: GitHub has already been told the first copy
succeeded, and telling it the duplicate failed would provoke exactly the
redelivery the table exists to absorb. Log at debug, not warn.

#### Invariants

- Delivering the same `(installation_id, delivery_id)` twice produces the
  side effects exactly once.
- **Negative space:** two *different* deliveries that happen to describe
  the same installation both take effect — dedup is per delivery, never
  per subject. A ticket-reader who dedups on installation id has broken
  the reconcile.
- A duplicate returns 204 and publishes nothing.
- Rows older than 72h are pruned, and pruning a row does not resurrect
  the ability to double-apply a delivery GitHub would no longer send.

#### Acceptance criteria

- Migration both dialects; `dbtest` conformance case for the
  insert-conflict path and the prune.
- Handler test: same delivery twice → one upsert, one publish, two 204s.
- Handler test: two deliveries, same installation → two upserts.

---

### TFAC-809 · Rate-limit the GitHub webhook route

**Depends on:** nothing.
**Modes:** multi (the limiter is a no-op in local by construction).

#### The defect

`internal/server/server.go:1329-1334` mounts the receiver as a bare
`http.HandlerFunc` — no middleware of any kind. The structurally
identical Slack receiver takes `api.SignedWebhookRateLimit`
(`ee/slack/install.go:116` → `internal/server/ratelimit.go:216-217`, 20
req/s, burst 100, no-op in local mode). That tier was built for exactly
this description — a route that authenticates every request itself via a
signature before any side effect (`internal/server/extension.go:47-56`) —
and core's own signed webhook predates it and never got wrapped.

It matters more than it looks because **two store reads happen before
signature verification** (`github_webhooks.go:66-88`): the org's App row,
then the vault secret. So an unauthenticated request costs a DB read plus
a secret read, unthrottled.

#### What to build

Wrap the mount in the existing signed-webhook limiter. Do not invent a
new tier and do not change its constants.

Additionally, add a short-TTL cache for the resolved webhook secret keyed
by org, so a burst of invalid requests against one org does not multiply
into vault traffic. The ordering (org → secret → verify) is **not**
fixable today: under per-org URLs the secret is reachable only via the
org. State that in a `TODO(TFAC-NNN)` at the site pointing at the row-7
ticket that inverts it, or omit the marker entirely if no such ticket
exists yet — **do not invent an id**.

While here, close the org-existence oracle: no App → 404 and bad
signature → 401 lets an unauthenticated caller distinguish orgs that have
a registered App. Return the same status for both.

#### Invariants

- A flood against a valid org URL is throttled at the documented rate.
- **Negative space:** legitimate GitHub deliveries at normal volume are
  never throttled, and the limiter is inert in local mode.
- An unauthenticated caller cannot distinguish "org has no App" from
  "signature invalid" by status code, body, or timing to the extent
  timing is controllable.

#### Interactions with siblings

Shares `github_webhooks.go` with TFAC-805 and TFAC-808. Dedup (A6) must sit
*inside* the rate limiter and *after* signature verification. If more than
one of these is in flight, sequence A7 → A6 → A3 and rebase.

---

### TFAC-810 · Repository identity: source, external id, get-or-create

**Depends on:** nothing.
**Modes:** both.

#### Why

TF stores GitHub's repository id **nowhere**. Every repo reference in the
product is case-normalized `(owner, repo)` text:

- `team_github_repos` PK `(team_id, owner, repo)` (sqlite `:468-476`)
- `repo_profiles` `UNIQUE(org_id, owner, repo)` (sqlite `:1398-1425`)
- `run_worktrees.repo_id` — a slug, despite the name
- `entities.source_id` — `"owner/repo#18"`
- `pending_prs`, `pending_reviews`, `teams.pinned_repos`

So a rename or transfer silently orphans all of them — and worse, TF
cannot **detect** that it happened. Under polling a renamed repository is
indistinguishable from a 404 plus a brand-new repository.

`repo_profiles` is already the repository registry in all but name:
surrogate uuid PK, `UNIQUE(org_id, owner, repo)`, plus `clone_url`,
`default_branch`, `base_branch`. It is a repository row that happens to
have a profiling job attached.

#### What to build

On `repo_profiles`, both dialects:

- `source TEXT NOT NULL DEFAULT 'github'` — app-validated, **not**
  CHECK-constrained, matching the convention the baseline states
  explicitly for other `source` columns.
- `external_id TEXT` (nullable) — the provider's own repository id,
  discriminated by `source`. Deliberately not named `github_repo_id`:
  `(source, external_id)` mirrors the existing `entities (source,
  source_id)` shape and avoids a column per provider when GitLab lands.
- Widen the unique to `(org_id, source, owner, repo)`.

Then make the row **get-or-create**: a repository gets a row when it is
first tracked, not when profiling first succeeds. Populate `external_id`
wherever a GitHub payload already carries the repository id; leave it
NULL where none is available rather than fetching one.

**Do not rename the table** — that is TFAC-813, and mixing 184
occurrences of mechanical churn into a behavior change is how both get
reviewed badly.

**Keep the profile columns where they are** (`profile_text`,
`profiled_at`, `clone_status`, `pulls_etag`, …). Splitting a registry
from its profile is a cleaner end state and is easy *after* anything FKs
to the row; doing it now pays the same migration twice.

#### Invariants

- Tracking a repository creates its row immediately, before any profiling
  runs.
- **Negative space:** an existing profiled repository is not duplicated
  by the get-or-create, and its uuid does not change. The widened unique
  must not split existing rows.
- A row with NULL `external_id` behaves exactly as today everywhere.
- `source` defaults to `github` for every existing row.

#### Acceptance criteria

- Migration both dialects; the unique widening must be proven
  non-splitting by a test that seeds pre-migration-shaped rows.
- `dbtest` conformance case for get-or-create idempotence.
- No change to profiling behavior, TTL, or the profiler's trigger path.

---

### TFAC-811 · Detect renames and rewrite slugs

**Depends on:** TFAC-810.
**Modes:** both.

#### Why

This is where TFAC-810's value is realized. With `external_id` populated,
a rename is *detectable*: same `(source, external_id)`, different
`(owner, repo)`. Without it, a rename is data loss nobody notices.

#### What to build

A rename handler that, on detecting the condition, rewrites the slug in
**one transaction** across every table that stores one: `team_github_repos`,
`repo_profiles`, `run_worktrees.repo_id`, `pending_prs`,
`pending_reviews`, `teams.pinned_repos`, and `entities.source_id` (a
prefix rewrite of `"old/name#N"` → `"new/name#N"`).

Enumerate by invariant: **every table storing an `owner/repo` slug or a
slug-prefixed identifier must be covered.** Grep for the shape rather
than working from this list, and state in the PR which tables the grep
found — if it finds one this ticket does not name, that is a finding, not
a deviation.

Also invalidate the coverage cache entries for both the old and the new
slug (`internal/github/repo_coverage_cache.go` keys on
`orgID + lower(owner/repo)`).

**Check before scoping** whether the App's `default_events`
(`internal/server/github_app_register.go:206-214`) subscribes to
`repository`, which carries `action: renamed`. If it does, wire the
handler to that event as the fast path. Either way the detection must
also work from the polling path, because local mode has no webhooks.

#### Loser contract

Two concurrent detections of the same rename must not both rewrite.
Serialize on the repository row (`SELECT … FOR UPDATE` in Postgres; the
single writer in SQLite makes this moot), and make the rewrite
idempotent: a second run finds the slug already current and is a no-op,
not an error.

#### Invariants

- After a rename, every tracked-repo row, profile, worktree ledger entry
  and entity resolves under the new slug.
- **Negative space:** a repository that is *deleted* and a *different*
  repository created with the same name is **not** treated as a rename —
  the `external_id` differs, so the condition does not hold. A test must
  cover this, because it is the case that corrupts data if the detection
  keys on the wrong thing.
- A rename does not create, close, or re-mint any task. Forward-only
  tracking (decision 1.6) is unaffected: the same tasks continue to exist
  against the same entities, now correctly named.
- A partial rewrite is impossible — all tables or none.

#### Acceptance criteria

- `dbtest` conformance case for the multi-table rewrite, both dialects.
- A test for the delete-and-recreate-same-name case above.
- A test that the rewrite is idempotent.

---

### TFAC-812 · Mirror the installation grant, reconciled by pull

**Depends on:** TFAC-807 (host), TFAC-810 (repository rows).
**Modes:** both. **Largest ticket in the epic — size accordingly.**

#### Why

The set of repositories an installation can reach is **not stored
anywhere**. `GET /installation/repositories` is called in
`internal/github/events.go` but never mirrored, so nothing can compute —
server-side — which repositories the App can reach that no team tracks
(*reach without purpose*), or which repositories a team tracks that sit
outside the grant (*scope drift*). Both are states the org page must
draw, and both are security-relevant rather than cosmetic.

`ReconcileInstallations` (`internal/db/github_app_backfill.go:107`)
reconciles **existence** only.

Push alone cannot be the answer: **GitHub does not auto-retry webhook
deliveries at all.** A lost `installation_repositories` delivery leaves
the mirror stale permanently. So the mirror must be **correct by pull,
with webhooks as a latency optimization** — which is also the only shape
that works in local mode, so both modes stop needing different truth.

#### What to build

- A per-installation granted-repository mirror. **Store full repository
  names**, not counts: the org page names the repositories involved, and
  the set difference against the tracked set has to be computable
  server-side. Size for it — an installation granted "all repositories"
  on a large org is thousands of rows, times N installations.
- Capture `repository_selection` (`all` | `selected`) on the installation
  row. `mint.go:417-418` explicitly discards it today. It is what says
  whether drift is even *possible* for a given installation, and without
  it the page cannot distinguish "nothing is drifting" from "drift is
  impossible here."
- A periodic reconcile that refreshes the mirror, running on the same
  gating as the existing profiler/reconcile subscribers. It must be
  leader-gated in multi mode like every other background pass, and must
  run in local mode too.
- Keep `installation_repositories` webhook handling as the fast path if
  it is cheap to add, but the ticket is complete without it. The pull is
  the contract.

#### Invariants

- After a reconcile, the mirror equals what GitHub reports, including
  removals — a repository revoked from a selective grant disappears from
  the mirror.
- **Negative space:** a failed or partial fetch never *empties* the
  mirror. A reconcile that cannot reach GitHub leaves the previous
  answer in place; a mirror that silently empties would render "reach
  without purpose" as a false all-clear, which is worse than stale.
- A suspended installation (TFAC-805) is not reconciled to empty either —
  its 403s are not evidence of a narrowed grant.
- The mirror is never consulted on the token-minting path. It exists for
  display and drift computation; minting continues to go through
  `installationFor` and GitHub's own enforcement.

#### Interactions with siblings

Widens the same `/app/installations` parse as TFAC-805. Whichever lands
second rebases; do not duplicate the struct.

#### Acceptance criteria

- Migration both dialects; `dbtest` conformance case covering add,
  remove, and the failed-fetch-preserves-mirror case.
- A test that a `selected` installation and an `all` installation are
  distinguishable after reconcile.
- The reconcile is registered on the same subscriber path as the existing
  passes and is verified not to run on a non-leader in multi mode.

---

### TFAC-813 · Rename `repo_profiles` to `repositories`

**Depends on:** TFAC-810, TFAC-811. **Do this last.**
**Modes:** both.

#### Why

After TFAC-810 the table is the repository registry and the name lies. The
rename should land before anything FKs to it, and after the behavior
changes so that a reviewer can read this diff as pure churn.

#### What to build

A mechanical rename: the table in both migration trees, the store
(`internal/db/repos.go`, `RepoStore` at `:32`), the domain type
(`internal/domain/repo.go:6`, `RepoProfile`), and every reference — **184
occurrences across 20+ files** at time of writing, including
`internal/curator/repos.go`, `internal/delegate/spawner.go`,
`internal/worktree/worktree.go`, `internal/app/startup.go` and
`internal/server/{repos_handler,team_repos,projects,credentials}.go`.

Decide the Go-side naming in one place and apply it consistently:
`RepoStore` → `RepositoryStore`, `RepoProfile` → `Repository`. Keep the
profile-specific *fields* named as they are.

#### Invariants

- **Zero behavior change.** This is the entire acceptance criterion. No
  test's expected values change; no test is added except where a name
  appears in a fixture.
- The migration renames rather than recreates, so no data is copied and
  no ids change.

#### Acceptance criteria

- `go build ./...` and `go test ./...` pass with no test expectation
  edits.
- `./scripts/lint.sh` clean.
- The PR description states the occurrence count found and confirms the
  grep is exhaustive.

---

### TFAC-814 · Correct the multi-installation copy

**Depends on:** nothing. **Front-end only.**

The GitHub App "supports multiple installations" in four places. That is
true only for enterprise-owned and public Apps; a private BYO App
supports exactly **one**, on its owning account — which is what every
current customer has.

Current sites on `aa/design-language`:

- `frontend/src/pages/setup/GitHubStep.tsx:111`
- `frontend/src/pages/settings/GitHubAppPanel.tsx:20` (doc comment),
  `:131`
- `frontend/src/pages/settings/stack/GitHubAccessControl.tsx:569`, `:572`

Replace with copy that describes what the App actually buys a
single-installation customer: a durable bot identity that does not die
with an employee, and per-team repository scoping enforced by GitHub.
Do **not** replace it with "supports one installation" — that reads as a
limitation, and it stops being true the moment a customer registers an
enterprise-owned App.

---

## 5. Deferred — the shared App, and what it will need

Nothing below is in this epic. It is recorded so that no ticket above
half-implements it and no reviewer assumes it is missing by oversight.

**5.1 · The bind.** Row 7 needs a proof that the person in the TF session
is the person GitHub just authorized, because the callback redirect is an
unsigned GET and installation ids are sequential. The design is settled:
turn *Request user authorization (OAuth) during installation* **on for
the official App only** (it is a per-registration setting, and BYO Apps
are registered from TF's own manifest, which leaves it off). GitHub then
delivers `code` and `installation_id` to one callback — and a registration
with that checkbox set **cannot** have a Setup URL, so the setup-URL route
is moot for both Apps. Verify with `GET /user/installations`, then require
install authority over the target account: for a User target the
authorizing id must equal the account id; for an Organization target,
`GET /user/memberships/orgs/{org}` must report `admin`. The App's manifest
already requests `members: read`.

`state` is **not** returned when GitHub initiates the flow during
installation, so CSRF protection is an HttpOnly `SameSite=Lax`
pending-bind cookie plus a single-use server-side record — the pattern
`internal/server/github_connect.go:53-55`, `:154-162` already uses.

**5.2 · The two paired constraints.** Scoping `ReconcileInstallations` so
it can never attribute an installation to an org that has not bound it,
**and** `UNIQUE (github_host, installation_id) WHERE removed_at IS NULL`
as the backstop. Neither is optional and neither ships without the other.

**5.3 · The webhook receiver's second path.** A shared App has one static
URL and one deployment-level secret, so the tenant cannot come from the
path and the signature stops discriminating between orgs. That is a
second handler beside the existing one, not a parameterization of it:
route by `installation.id → org_id`, resolve the secret *before* any org
exists (which incidentally fixes the read-before-verify ordering in
TFAC-809), drop unmapped ids before any side effect and before any
`bus.Publish`, and let `created` for an unknown installation park as
unbound rather than attach to whichever org a URL named.

**5.4 · Resolver tier 2.** `internal/github/resolver.go:116-118` reserves
the numbered gap. `using_hosted_default` is a hardcoded `false`
(`internal/server/github_app_handlers.go:20`) with no GitHub-side
consumer; Jira's equivalent is live and is the right precedent, including
the trap not to repeat — a per-org row whose secret reads empty falls
*through* to the first-party app and the card then reports
`using_hosted_default=true` with `app: null`
(`internal/jira/oauthapp.go:131-134`).

**5.5 · Repository keys as foreign keys.** TFAC-810 and TFAC-811 give
repositories a durable identity and make renames non-destructive. Turning
every slug column into an FK on the repository row is the clean end
state, but each table is its own migration and each write path becomes a
get-or-create. Sequence cheapest-first — `team_github_repos`, then
`run_worktrees`, then `pending_prs` / `pending_reviews` /
`pinned_repos`. **`entities` last or never:** it is the durable core with
events and tasks hanging off it, and TFAC-811's rewrite already keeps
`source_id` correct, so the FK buys little there for the most risk in the
product.

**5.6 · Token scoping by `repository_ids`.** Real but small: scoped mints
bypass the token cache (`internal/github/resolver.go:441-445`), so the
mint-to-use window is minutes. Worth doing once TFAC-810 makes an id
available at mint time; not worth doing on its own.

**5.7 · MSP / multi-workspace sharing.** Decision 1.3 refuses it. Nothing
in this epic forecloses it — the enforcing index is deliberately not
shipped. Revisiting means `ListInstallationsForOrgSystem` returning
claims from multiple workspaces, the resolver deciding whose config
applies, RLS on a genuinely shared row, and attribution. The index is the
trivial part.
