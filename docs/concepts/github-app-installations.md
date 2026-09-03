# GitHub App installations: ownership and propagation

How Triage Factory learns which GitHub App installations a workspace holds, and
how the changes an admin makes on GitHub's side flow back in. The rules here are
tenancy boundaries, not conventions — read this before changing
`internal/db/github_app_backfill.go`, `internal/grantmirror`, or the bind
ceremony in `internal/server/github_managed_bind.go`.

## Installation vs. grant

A GitHub App **installation** is the App's presence on one GitHub **account** —
an organization or a user. Which repositories that installation may touch is the
installation's **grant**: either every repository on the account (`all`) or an
enumerated selection (`selected`).

The two change independently, and TF stores them separately:

- `org_github_app_installations` — one row per (workspace, GitHub account).
- `reachable_repositories` — the mirrored grant: which repositories each
  installation reaches, feeding the repository picker and the team-repos write
  gate.

Adding or removing repositories on GitHub's installation settings page changes
the **grant** and keeps the same installation id. Only installing the App on a
new account, or uninstalling it, changes the **installation set**.

## Two credential shapes, two answers to "whose installation is this?"

**A workspace with its own App key** (the `byo_app` credential class) gets
tenancy answered by GitHub itself. Its key can only list installations of its
own App, so everything `GET /app/installations` returns under that key belongs
to that workspace by construction. TF may therefore *discover*: the reconcile
(`BackfillInstallationsFromAPI`) writes every installation the listing reports
and soft-removes any it no longer reports, every GitHub poll cycle.

**A workspace on the deployment App** (the `managed_app` class, multi mode
only) shares one App key with every other workspace on the deployment. The same
listing now returns *every tenant's* installations, and nothing about the
credential says which belong where. So for these workspaces the question has
exactly one source of truth:

> The reconcile refreshes; it never discovers. It may update installation rows
> the workspace already has and may never create one. Creating the link between
> a workspace and an installation is the bind ceremony's job alone.

The bind ceremony (the Connect button) proves the link — the person completing
the install can see the installation and administers the account it targets —
and writes the row. The scoped reconcile then keeps bound rows current from the
shared listing and touches nothing else. It has two doors with two scopes: the
Settings refresh button runs it for one workspace
(`RefreshManagedInstallations`), and the poll cadence runs it for every managed
workspace at once from a single listing (`RefreshAllManagedInstallations`,
driven by `grantmirror.RunDeployment`) — one `GET /app/installations` per
GitHub cycle whatever the tenant count, since under a shared key the answer is
the same whoever asks. Two mechanisms hold the boundary on both doors: the
reconcile filters to bound installation ids before writing, and a unique index
over live `(github_host, installation_id)` pairs refuses a second workspace's
claim outright.
An installation belongs to exactly one workspace per GitHub host; after an
uninstall the id is freed, and connecting it elsewhere is an ordinary new bind.

Leaving the class is as deliberate as entering it. The deployment App is
something a workspace *chooses*, so the bind is the only way in and the
disconnect verb is the only way out: `POST
/api/orgs/{org_id}/github/managed/disconnect` soft-removes every installation
the workspace holds (the same removal an `installation.deleted` delivery
performs, reach-cache cascade and token invalidation included), resets the
class to the rowless default, and records one access change per account
disconnected. Its per-installation form
(`…/github/managed/installations/{installation_id}/disconnect`) drops one
account and keeps the class — until it drops the last one, which is the full
disconnect. Nothing is uninstalled on GitHub: the installation persists there
unbound, and Connect re-binds it. The other credential doors — the PAT bind,
BYO registration, BYO import — refuse a managed workspace that still holds a
live installation row and name the disconnect as the way out, so a workspace's
live rows and its class can never disagree. That is what lets the receiver
above route on the row alone.

What the rule forbids is exactly one thing: TF *guessing* — attributing an
installation from the shared listing to a workspace whose admin never proved
it. Everything after the proof updates freely, as the next section enumerates.

## What an admin's change on GitHub does, and how TF picks it up

| Change on GitHub | What it is | Own App key | Deployment App |
|---|---|---|---|
| Add/remove repositories on a connected account | Grant change, same installation | Grant pass, every poll cycle; `installation_repositories` webhook | Grant pass, every poll cycle |
| Switch `all` ↔ `selected` repositories | Installation field | Reconcile, every poll cycle; webhook | Reconcile, every poll cycle; Settings → refresh |
| Suspend / unsuspend the installation | Installation field | Reconcile, every poll cycle; webhook | Reconcile, every poll cycle; Settings → refresh |
| Rename the account | Installation field (`account_login`) | Reconcile, every poll cycle | Reconcile, every poll cycle; Settings → refresh |
| Install the App on a **new** account | New installation | Reconcile discovers it, every poll cycle | Connect button — one bind ceremony per account, additive |
| Uninstall from an account | Installation removed | Reconcile soft-removes; webhook | Reconcile soft-removes, every poll cycle; Settings → refresh |

The **grant pass** is `grantmirror.RunOrg`'s per-installation half, and it runs
every GitHub poll cycle for *both* classes: it reads each bound installation's
repository list with that installation's own token, which sees exactly that
installation's grant and nobody else's — so it needs no tenant scoping, and new
repositories reach the repository picker on the next cycle. A soft-removed
installation's `reachable_repositories` rows are deleted with it, so the picker
never offers reach TF no longer has.

Pull is the contract for both classes. GitHub never retries a webhook delivery
it failed to make, and some changes have no delivery at all — an account rename
fires no event TF subscribes to, and `account_login` is what token minting
matches an installation on, so a rename that only a webhook could report would
break minting for that account permanently. Every installation field the
listing reports converges on the cadence for both classes; a webhook, where one
exists, only makes it converge sooner.

## Installed but not bound

An installation of the deployment App can exist with no workspace bound to it,
through paths that are entirely ordinary: a member without install rights
selected the account, GitHub sent the install to an owner instead, and the
owner approved it later; someone installed from the App's public page on
GitHub without ever pressing Connect; a reinstall came back without the
cookie the original Connect set. It is an expected state, not a fault, and TF
treats it as one:

- The install callback with no ceremony behind it redirects to
  `/github/installed`, a page that says the install went through and is not yet
  connected, and offers the Connect button. **Recovery is the ordinary bind
  ceremony**: Connect mints a fresh record, GitHub hands the existing
  installation back with the same id, both gates run, and the bind lands. There
  is no "adopt an existing installation" path — one way to create a binding is
  enough.
- A managed workspace with no bound installation shows the same button as its
  Settings empty state.
- The deployment webhook receiver acknowledges a delivery for an unbound
  installation with 2xx, writes nothing, and logs **one line at INFO** naming
  the installation id and (for installation events) the account login; the
  callback and a refused bind log the same facts. That log is the operator's
  signal, and the only place an unbound installation's account is ever named.

What TF deliberately does **not** do is list unbound installations anywhere.
On a shared App that list is every other prospective tenant's GitHub account,
and no filtering makes it safe — narrowing it to what the viewer can reach
would mean rendering a shared config surface from their personal GitHub
permissions. The recovery needs a button, not a list.

## Current limitations, deployment-App workspaces only

- **Webhooks do not reach them yet.** The webhook receiver is addressed per
  workspace and verifies against that workspace's own secret, which a
  deployment-App workspace does not have — deliveries for the shared App are
  refused. Until a deployment-level receiver exists, webhook rows in the table
  above apply only to workspaces with their own key, and a deployment-App
  workspace converges on the poll cadence alone.
