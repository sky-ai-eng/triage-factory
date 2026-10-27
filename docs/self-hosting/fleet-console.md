# Fleet console & operators

The **fleet console** is the deployment-wide administration view: a live picture
of every Triage Factory process, its telemetry, run timings, and LLM spend. It's
EE-licensed (the `fleet` entitlement) and **operator-gated** — visible only to
deployment operators, not to ordinary org members, however senior.

This page covers granting operator access (the thing that makes the console
appear) and what the console shows.

> **Multi-mode only.** Operators are a deployment-scoped identity that only makes
> sense when there's a fleet to administer. Local mode is a single process with a
> single implicit operator, so the console never appears there.

## Why you don't see a "Fleet" tab yet

The nav entry is gated on **three** conditions, all of which must hold:

1. **Multi-mode** (`TF_MODE=multi`).
2. **The `fleet` entitlement** is active — i.e. your license includes `fleet`.
3. **You are a deployment operator** — one of your sign-in methods holds a
   verified email that is in the `operators` set.

Licensing the feature satisfies (2) but not (3). The common surprise:

> **Being an org owner or admin does not make you an operator.** An operator is
> *org-less deployment config* — fleet administration spans every org, so it is
> deliberately not a per-org member role. The founder of an org gets no operator
> access from that alone. There is no environment variable and no in-app UI for
> it; operators are granted from the shell, the same trust boundary as
> `jwk-init` (whoever can run commands against the deployment can grant it).

So a freshly-licensed deployment shows no Fleet tab until you grant yourself
operator access below.

## Grant operator access

Use the `triagefactory operator` CLI. It opens its own admin-pool DB handle
(independent of the running server), so run it inside the control container,
which already has the database env. It refuses to run on an executor: the
executor's database role (`tf_system`) has no access to operator grants. It
also refuses in local mode, which has no operators to grant.

```sh
# Grant — use a verified email of one of your sign-in methods
docker compose exec control triagefactory operator add you@example.com

# List every operator, with the account each grant is bound to
docker compose exec control triagefactory operator list

# Revoke
docker compose exec control triagefactory operator remove you@example.com
```

- **Use a verified email from one of your sign-in methods.** The email is how
  you name the person; the grant itself belongs to their account. A GitHub
  sign-in's address counts when GitHub reports it verified. An SSO sign-in's
  address counts only when its domain is verified for the org whose connection
  it came through; an identity provider can assert any address, so an
  unverified domain proves nothing. For a bootstrap-floor deployment that's your
  verified GitHub OAuth email. Emails are lower-cased and trimmed at the store
  boundary, so case and surrounding whitespace don't matter.
- **The grant binds to an account.** If an account already holds the address
  verified, `add` binds the grant to it and prints the account's user id.
  Otherwise the grant is pending, and the first sign-in that verifies the
  address binds it. A pending grant admits whoever holds its address verified;
  a bound one admits its account only, even after the address changes on that
  account or is later verified by someone else. `operator list` shows each
  grant's account, or `(pending)`.
- **`add` is idempotent** — re-granting an existing operator keeps its original
  provenance and succeeds. It binds a pending grant whose address now has a
  verified holder, and never moves a bound one: to give a grant to a different
  account, `remove` it and `add` it again.
- **Deleting the account deletes the grant.** The grant does not go back to
  pending.
- Each grant/revoke appends a best-effort `auth_events` audit row (org-less, so
  it lives in the deployment-wide auth log rather than any org's
  `access_change_log`).

## See the console

`GET /api/me` re-reads operator status on every call, so once you've been
granted there's **no restart** — just reload the app. A **Fleet** pill appears
in the top nav, and the console lives at `/<your-org>/fleet`.

It's a DB-backed live view (10-second poll, no Prometheus dependency) over:

- the **instances registry** — every process's identity, role, build version,
  and heartbeat;
- per-instance **telemetry** — host memory headroom, the dispatch memory gate,
  and semaphore occupancy the executors publish each heartbeat;
- **run timings** — queue/exec durations from the `conversations` table; and
- **LLM spend** — from `llm_spend`, over a 24H / 7D window.

## Troubleshooting

- **Licensed `fleet` but no Fleet tab.** You're almost certainly not an operator
  yet — run `operator add` above and reload. Confirm with `operator list`.
- **Granted operator but still no tab.** Run `operator list`. If the grant is
  bound to a user id that isn't your account, someone else held the address
  verified when it was bound: `remove` it and `add` it again once your account
  holds the address. If it is `(pending)`, the email doesn't match a verified
  address on one of your sign-in methods (it's normalized, but a *different*
  address won't match). If it's your SSO address, its domain must be verified
  for your org — otherwise grant the address of a sign-in method that is
  verified, such as your GitHub email. Also check that the license actually
  includes `fleet` (a community build ignores the license entirely — see
  [self-host setup](install.md)).
- **`operator` CLI errors on the DB.** It needs the same database env the server
  uses; run it via `docker compose exec control triagefactory …`, not on the
  host where that env is absent.
