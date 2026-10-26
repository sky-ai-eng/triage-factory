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
executor's database role (`tf_system`) has no access to operator grants.

```sh
# Grant — use a verified email of one of your sign-in methods
docker compose exec triagefactory triagefactory operator add you@example.com

# List every operator, with the account each grant is bound to
docker compose exec triagefactory triagefactory operator list

# Revoke
docker compose exec triagefactory triagefactory operator remove you@example.com
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

## Deployment commit signer

The deployment commit signer ([setup](install.md#86-optional-configure-the-deployment-commit-signer))
has three routes beside the console's. They are **operator-gated but not
licensed**: commit signing is core, and Verify is the only way out of a signer
a pushed commit broke, which a community deployment needs as much as a licensed
one. Like the rest of the console, a non-operator gets `404` and an API token
gets `403`; sign in with a session. A control pod with no deployment signer
configured answers `404`. Each answers for the signer the pod that serves it is
configured with, so every control pod should carry the same
`TF_GITHUB_COMMIT_SIGNER_*` settings.

| Route | Does |
| --- | --- |
| `GET /api/fleet/commit-signer` | The signer and its health: the account it signs as (`github_user_id` is null until a check has read the account the configured login names, and until then no workspace commits as it), the committer address its commits carry, its public key, and `status` (`pending`, `active`, `broken`) with `status_reason` and `checked_at`. |
| `POST /api/fleet/commit-signer/verify` | Checks the signer now and, when it passes, makes it `active`: its key is among the account's signing keys and, for a signer a pushed commit broke, that commit verifies now. `409 KEY_NOT_FOUND` (the key is not on the account, or the login names no user), `409 COMMIT_UNVERIFIED` (with GitHub's reason and what to fix on the account), `422 SSH_SIGNING_UNSUPPORTED`, `502 UPSTREAM_UNAVAILABLE`. |
| `GET /api/fleet/commit-signer/usage?since=&until=` | The signatures made as the deployment signer in the window, every org's: `total`, `by_hour` and `by_org` (hours and orgs with none left out). The window parses like every usage node: RFC3339 or `YYYY-MM-DD`, both absent is the current month to now, one absent leaves that side open. The signer has no budget of its own; each org's signatures count against that org's. |

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
  uses; run it via `docker compose exec triagefactory …` (the control service),
  not on the host where that env is absent.
