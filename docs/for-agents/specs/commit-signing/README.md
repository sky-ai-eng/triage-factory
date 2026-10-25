# Commit signing

Design of record for signing the commits delegated agents make, so GitHub
marks them Verified. An org admin registers a GitHub user account as the org's
**signer**. Agent commits are authored and committed as that account and
signed with a key only Triage Factory holds. Workspaces on the deployment
GitHub App get the deployment's signer by default.

Status: **accepted design**; §10 lists what is still open. Builds on the sealed commit identity
(**TFAC-1187**) and depends on per-ref push outcomes (**TFAC-1188**).

Scope: both run modes, core (no licence gate). The deployment signer is
multi-mode only, because the deployment App is.

---

## 0. Why

TF's commits are unsigned. An agent's commit is authored as the org's GitHub
identity, which for most orgs is a GitHub App's bot account, and a bot account
cannot hold a signing key. A bot commit is Verified only when GitHub creates
and signs it itself, as it does for a merge made in the web UI, so a commit TF
makes with git and pushes can never be Verified as the bot.

That has two costs:

- A repository that requires signed commits, through branch protection or a
  ruleset, refuses every push TF makes.
- An org that wants Verified badges on agent commits cannot get them.

GitHub verifies an SSH-signed commit when the signing key is registered as a
**signing key** on a GitHub **user** account and the commit's **committer**
email is one of that account's verified emails. The author is not checked
(§2, case E).

## 1. Goals and non-goals

Goals:

1. An org admin registers one GitHub user account per org as its signer. TF
   generates the key pair, and the admin adds the public half to the account.
2. Every commit an agent makes in that org is authored and committed as the
   signer and signed.
3. A workspace on the deployment App uses the deployment's signer unless it
   registers its own.
4. The private key never enters a jail. The deployment signer's private key
   never leaves the control plane.
5. When signatures would not verify, TF stops signing and says why, rather
   than producing commits GitHub marks Unverified.

Non-goals:

- **Enforcing signatures.** An agent can always commit with `--no-gpg-sign`.
  A repository's own rule is what enforces signing, and TFAC-1188 makes that
  refusal visible.
- Signing tags, GPG keys, per-team signers, and signing commits people make
  outside a run.

## 2. Spike findings

Run 2026-10-25 against github.com (git 2.53.0, OpenSSH 10.2p1). Each case was
an empty commit pushed on its own branch, read back through
`GET /repos/{o}/{r}/commits/{sha}` → `commit.verification`.

| case | author | committer | verified | reason |
| --- | --- | --- | --- | --- |
| A | `<login> <<id>+<login>@users.noreply.github.com>` | same | true | `valid` |
| B | `<login> <<login>@users.noreply.github.com>` | same | true | `valid` |
| C | the account's primary verified email | same | true | `valid` |
| D | case A | `x <nobody@example.invalid>` | false | `no_user` |
| E | `acme[bot] <1+acme[bot]@users.noreply.github.com>` | case A | true | `valid` |
| F | case A, signed through a shim | same | true | `valid` |
| H | case A, re-read after the key was deleted | — | true | `valid`, `verified_at` unchanged |
| H | a new case-A commit after the key was deleted | same | false | `unknown_key` |

What follows from it:

- **Only the committer counts** (E).
- **Both noreply forms verify** (A, B), so TF can derive the committer email
  from the account's id and login. No mailbox is needed, and the admin types
  no email.
- **Verification is decided at push time and stored** (H). Deleting a key
  changes no commit already pushed; only later pushes fail, with
  `unknown_key`. Rotating a key therefore breaks nothing in the history.
- **An address that belongs to no account fails with `no_user`** (D), not an
  unverified-email reason. An unverified address that does belong to the
  account was not tested.
- **`GET /users/{login}/ssh_signing_keys` needs no authentication.** It
  returns `[{id, key, title, created_at}]`, with no fingerprint and no
  verified flag, so a match is an exact comparison of the `key` string. It is
  served with `cache-control: public, max-age=60`, so a key added or removed
  can take up to a minute to show. Key titles are public.
- **Git's call to the signing program** (F), with
  `user.signingkey=key::<public key>`, is
  `<program> -Y sign -n git -f <temp file holding the public key> -U <buffer file>`.
  Git reads the signature from `<buffer file>.sig`. Both temp files go in
  `$TMPDIR`, else `/tmp`. The program path must be absolute, because
  `git commit` runs from the repository's top directory.

## 3. Settled decisions

1. **SSH signing keys, not GPG.** An SSH key has no user id to bind an email
   into, needs no keyring, and is all ed25519 in Go
   (`crypto/ed25519`, plus `golang.org/x/crypto/ssh`, already a dependency,
   for the key encoding).
2. **TF generates the key pair.** The private half never exists outside TF;
   there is no key upload.
3. **The signer authors and commits.** The commit carries one identity, a
   user account, the same shape as Claude Code's commits (authored as the
   `claude` user). The bot stays the account that pushes and that opens pull
   requests. The identity does not change with the key's health: a
   signer whose key stops verifying still authors the commits, unsigned.
4. **The committer email is derived:** `<id>+<login>@users.noreply.github.com`.
   An org signer may override it (§10.1).
5. **One signer per org.** The commit identity resolves, in order, to the
   org's signer, then the deployment signer (managed workspaces only), then
   today's identity (the App bot or the PAT owner).
6. **A per-org key travels sealed in the run's bundle, and the sidecar
   signs.** A compromised sidecar exposes that org's own signing identity, no
   more than the tokens it already holds.
7. **The deployment signer signs on the control plane, from a digest.** Its
   key is on the control pods only, like the deployment App's key. Any control
   pod answers a signing request; it is not gated on the background-brain
   lease. The control plane receives the SHA-512 digest that an SSH signature
   covers and never the commit, so no agent-written bytes are parsed in the
   most privileged process.
8. **The signer checks the committer only.** The daemon refuses to sign a
   commit whose committer is not the signer. The author is free, because a
   rebase or cherry-pick re-signs commits that keep their original author.
9. **When signatures would not verify, the next seal stops signing.** The
   reason GitHub gave is shown on the signer. A run already in flight keeps the
   configuration it started with.
10. **Commits only.** A tag (or anything else) is refused.

## 4. Architecture

### 4.1 The path of one signature

```
git commit
  └─ gpg.ssh.program = <hooks dir>/ssh-sign          (jail or local run)
       └─ triagefactory exec git sign -Y sign -n git -f … -U <buffer>
            └─ agenthost daemon: SignCommit(payload)  (sidecar in multi,
                 │                                     TF process in local)
                 ├─ check: commit object, committer == signer
                 ├─ org signer ──────── sign with the key from the bundle
                 │                       (multi) or the secret store (local)
                 └─ deployment signer ─ relay the digest to the executor,
                                         which queues it for the control plane
```

### 4.2 In the run: git config and the shim

When a run signs, its single `GIT_CONFIG_*` block (`internal/agentproc/proxies.go`
in the sandbox, `githooks.DirectAgentEnv` in a direct local spawn) gains:

```
user.name=<signer login>
user.email=<signer email>
gpg.format=ssh
commit.gpgsign=true
user.signingkey=key::<signer public key>
gpg.ssh.program=<hooks dir>/ssh-sign
```

`user.name` / `user.email` are the identity TFAC-1187 already stamps; the
signer simply becomes that identity. Git config keys pass the sandbox env
allowlist through the `GIT_CONFIG_KEY_` / `GIT_CONFIG_VALUE_` prefixes, so the
allowlist does not change.

The shim is a script embedded in `internal/githooks` beside `pre-push` and
`prepare-commit-msg`. `githooks.Ensure` writes it to the host hooks dir, which
the jail mounts read-only at `/run/tf-hooks` (`internal/cell/gvisor/jail_linux.go`)
and a local run reads in place:

```sh
#!/bin/sh
exec "${TRIAGE_FACTORY_BIN:-triagefactory}" exec git sign "$@"
```

`exec git sign` is a new exec verb. It accepts only `-Y sign -n git`, takes
the last argument as the buffer, ignores `-f` and `-U`, sends the buffer to
the daemon, and writes `<buffer>.sig`. On a refusal or a failure it prints
one line to stderr and exits 1; git then aborts the commit with
`error: gpg failed to sign the data`. The messages say what to do:

- `triagefactory: this run signs commits only as <signer>; this commit's committer is <x>`
- `triagefactory: commit signing is unavailable right now; retry the commit`

The verb is not documented to the agent; git is its only intended caller.

### 4.3 The daemon's check

`SignCommit(payload)` on the agenthost `Client` interface, served by the
sidecar-hosted daemon in multi mode, `agenthost.StartLocal` with the local
sandbox on, and in-process with it off:

1. Parse the payload as a commit object's header block (header lines up to
   the first blank line). Refuse anything without `tree` and `committer`
   headers.
2. Require the `committer` header's name and email to equal the run's signer
   identity. The timestamp is free.
3. Compute `SHA-512(payload)`.
4. Sign (§4.4) or relay the digest (§4.5).
5. Compute the resulting commit id the way git does: the armored signature
   becomes a `gpgsig` header at the end of the header block, and the id is the
   SHA-1 of `commit <size>\0<object>`.
6. Record the signature (§5.3).

The SSH signature format (`SSHSIG`, armored as `BEGIN SSH SIGNATURE`) is not
in `x/crypto`. TF implements it: the signed data is the `SSHSIG` magic, the
namespace (`git`), an empty reserved field, the hash algorithm (`sha512`) and
the digest. Its test signs with TF's implementation and verifies with
`ssh-keygen -Y verify`.

### 4.4 Per-org signing

- **Local mode.** The private key is in the secret store (the OS keychain, or
  the encrypted file backend). The daemon reads it per signature.
- **Multi mode.** `credprovision.ProvisionForConversation` seals the signer
  into `credbundle.GitHubCreds` (§5.5) when the signer is `active`. The
  sidecar signs with it. The executor never sees the key.

### 4.5 Deployment signing (multi only)

The deployment signer is operator config on the control pods, in the same
class as the deployment App:

- `TF_GITHUB_COMMIT_SIGNER_LOGIN`
- `TF_GITHUB_COMMIT_SIGNER_KEY` (secret, honours `_FILE`)

`docker-compose.yml` forwards both to the control service only. Executors get
the signer's name, email and public key through the bundle, never the private
key.

One signature:

1. The sidecar's daemon passes its check (§4.3) and makes a relay call,
   `sign_digest {digest}`, to the orchestrator.
2. The orchestrator inserts a `commit_signatures` row in state `requested`
   (§5.3), naming the conversation and the claim, and publishes `sign_request`
   on `tf_ctl`.
3. Every control pod handles `sign_request`. It is not gated on the
   background-brain lease, unlike `cred_request` (`internal/app/relay.go`).
   Each pod also sweeps for `requested` rows once a second, because `tf_ctl`
   is lossy.
4. A pod claims the row (`requested` → `signing`, conditional update with
   `RETURNING`). It checks that:
   - the claim is still active, at the boot epoch the row names;
   - the org still resolves to the deployment signer;
   - the claim is within its signing budget (§10.4).

   It then signs the digest and stores the signature (`signed`), or the
   refusal (`refused`, with the reason). Finally it publishes `sign_done`.
5. The orchestrator wakes on `sign_done` (polling the row every 250 ms as a
   backstop) and returns the signature to the sidecar.
6. After 30 s with no answer the call fails, and the shim reports
   "unavailable, retry".

The latency is a notify, a sign and a row read: milliseconds normally, and up
to the backstop intervals when a notify is dropped. A rebase re-signs every
commit it rewrites, so it pays that latency once per commit.

### 4.6 Resolving the identity

One function resolves a run's commit identity and its signing mode
(`none` | `org` | `deployment`) from the org signer row, the org's credential
class and the deployment signer's status. `credprovision` calls it in multi
mode and the spawner's `resolveCommitIdentity` in local mode. The co-author
trailer on manual runs compares the delegating person against the signer's
account id, as it compares against the bot's today.

## 5. Data

### 5.1 `org_commit_signers` (both dialects)

One row per org.

| column | |
| --- | --- |
| `org_id` | PK, FK `orgs` |
| `host` | the GitHub host the account is on, the org's host at registration |
| `github_user_id` | the account's numeric id |
| `login` | display value, refreshed by the periodic check (§7) |
| `email_override` | nullable; null means the derived noreply address |
| `public_key` | `ssh-ed25519 AAAA…` |
| `pending_public_key` | nullable, set during a rotation |
| `status` | `pending` \| `active` \| `broken` |
| `status_reason` | nullable: GitHub's verification reason, or `key_missing` |
| `status_since` | |
| `checked_at` | nullable |
| `created_at`, `updated_at`, `created_by` | |

RLS follows `org_event_sources`: members select, org admins write, and
`REVOKE ALL` from the other roles. The Postgres change is a baseline edit; the
SQLite one is a new migration.

### 5.2 Secrets

`github_commit_signing_key` and `github_commit_signing_key_pending` (OpenSSH
private-key format) in `internal/integrations`. Both go in `AllLocalSweepKeys`
and the keychain list in `scripts/clean-slate.sh`.

### 5.3 `commit_signatures` (both dialects)

One row per signature: the request queue for deployment signing and the
signing log for both kinds.

| column | |
| --- | --- |
| `id` | |
| `org_id`, `conversation_id` (FK, `SET NULL`), `claim_id`, `boot_epoch` | |
| `signer` | `org` \| `deployment` |
| `signer_user_id`, `public_key` | which account and key signed |
| `digest` | SHA-512 hex |
| `state` | `requested` \| `signing` \| `signed` \| `refused` |
| `refusal_reason`, `signature`, `claimed_by`, `signed_at` | |
| `commit_sha` | set by the daemon once the commit id is known (§4.3 step 5) |
| `repository_id`, `pushed_at` | set when a push lands this commit as a ref's new tip |
| `verification_reason`, `checked_at` | GitHub's answer for a pushed commit (§7) |
| `created_at` | |

An org signature is inserted as `signed`. Locally the daemon inserts it; in
multi mode it reaches the orchestrator as a relay record, like `record_push`.
`tf_system` (the executor) gets `INSERT`, `SELECT`, and
`UPDATE (commit_sha, repository_id, pushed_at)`. Org members get `SELECT`.

### 5.4 `deployment_commit_signer` (Postgres only)

One row per deployment signer key, keyed by the public key: `status`,
`status_reason`, `status_since`, `checked_at`. Every control pod reads it
before signing and the fleet console shows it.

### 5.5 The bundle

`credbundle.GitHubCreds` gains:

```go
Signer *SignerCreds `json:"signer,omitempty"`

type SignerCreds struct {
	Mode       string `json:"mode"`                  // "org" | "deployment"
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key,omitempty"` // org only
}
```

The signer's name, email and account id travel in the identity fields
TFAC-1187 added. `Signer` is nil when the run does not sign.

## 6. API

Under `/api/orgs/{org_id}/github/`, beside the App and PAT routes:

| route | gate | does |
| --- | --- | --- |
| `GET commit-signer` | member | the org's signer, 404 when none |
| `POST commit-signer` `{login, email?}` | admin | resolve the login on the org's host (must be a user, not a bot), generate the key pair, create the row `pending`; returns the public key and a suggested key title; 409 when the org already has one |
| `POST commit-signer/verify` | admin | look for the key in the account's public listing: found → `active`; not found → 409 `KEY_NOT_FOUND`, saying the listing can lag a minute |
| `POST commit-signer/rotate` | admin | generate a pending key; the current key keeps signing until `verify` finds the new one, then the new one replaces it |
| `PATCH commit-signer` `{email}` | admin | `null` restores the derived address |
| `DELETE commit-signer` | admin | remove the row and its secrets |
| `GET commit-identity` | member | the identity this org's runs commit as, and whether and how they sign (§4.6) |

- Writes take the org's GitHub credential lock (`githubAppRegMu`) and record
  an access-log row under a new credential kind.
- The signer counts as a connected credential for the host: add it to
  `githubCredentialConnected` (`internal/server/github_credential_class.go`),
  so `github_base_url` cannot change while one exists.
- The suggested key title is "Triage Factory commit signing", because titles
  are public.
- The fleet console gets `GET` and `POST …/verify` for the deployment signer,
  operator-gated.

## 7. Health

A signer is `pending` until its key is found, `active` while signing, and
`broken` when a check fails. Only `active` signers are sealed into bundles.

1. **Verify** (§6): the admin's action, and the only way out of `broken`
   apart from the case below.
2. **Periodic check**, on the GitHub poll cycle, at most every 15 minutes per
   signer. Using the org's credential, it refreshes the login by account id
   and confirms the key is still on the account. A missing key sets
   `broken (key_missing)`, and the same check clears it once the key is back.
3. **Post-push check.** When a push lands a signed commit as a ref's new tip
   (TFAC-1188 supplies per-ref outcomes), the orchestrator stamps that
   commit's `commit_signatures` row with the repository and push time. On its
   next cycle, the control plane reads the commit's `verification` with the
   org's credential and stores the reason. Anything other than `valid` sets
   `broken` with that reason, which stays until a verify.

The UI explains the common reasons: `unknown_key` (the key is not on the
account), `no_user` (the email belongs to no account), `unverified_email`
(verify the email on the account). Any other reason is shown verbatim.

A broken deployment signer stops signing for every managed workspace at its
next seal. Operators see it in the fleet console and in a WARN on each control
pod.

## 8. UI

A "Commit signing" section in `OrgSettings.tsx`, after "GitHub access":

- **No signer:** "Commits are authored by `<identity>` and aren't signed",
  with **Set up signing**, which takes a login and then shows the public key
  with the steps (GitHub → Settings → SSH and GPG keys → New SSH key → Key
  type: Signing key) and **Verify**.
- **Using the deployment signer:** "Commits are signed as `<login>`, this
  deployment's signing account", with **Use your own account**.
- **Pending / active / broken:** the status, the reason, and **Verify**,
  **Rotate key** and **Remove**, for org admins.

## 9. Rollout

**Phase 0 — TFAC-1188.** Per-ref push outcomes. The post-push check and a
visible "signed commits required" refusal both depend on it. (M)

**Phase 1 — Local, per-org, end to end.** The `SSHSIG` implementation and its
`ssh-keygen` test; the shim and `exec git sign`; the daemon check; the
`org_commit_signers` and `commit_signatures` tables in both dialects; the
secrets; the API; the settings section. Local mode needs git ≥ 2.34: below
that the run commits as the signer, unsigned, and `GET commit-identity` says
why. (L)

**Phase 2 — Multi, per-org.** Sealing the signer into the bundle, signing in
the sidecar, and relaying the signature record. The jail's git is Alpine
3.21's (`internal/sandbox/rootfs.go`), well past 2.34. (M)

**Phase 3 — The deployment signer.** The control-pod config, the
`sign_digest` relay call, the request flow and doorbells, the budget, the
managed-workspace default, and the fleet console status. Add the signer's
login to this repository's CLA allowlist before enabling it. (L)

**Phase 4 — Health.** The periodic and post-push checks, and the status and
reasons in the UI. (M)

*Exit criteria:* in both modes, an org with an active signer pushes commits
GitHub marks Verified as that account; a managed workspace does the same with
the deployment signer while no executor ever holds its key; a signer whose
key is removed is `broken` within one check, and the next seal stops signing.

## 10. Open questions

1. **Email override.** An enterprise rule that requires author or committer
   emails on the org's domain needs a real address, which is why the override
   exists. Without one, v1 could drop it. Keep it?
2. **GHES.** The minimum GHES version with SSH commit-signature verification
   and the `ssh_signing_keys` endpoints needs checking before GHES orgs are
   offered signing.
3. **Login renames.** The derived address contains the login. The periodic
   check re-derives it, but whether GitHub keeps verifying the old address
   after a rename is untested. If it does not, a rename makes commits from
   runs sealed before the check fail with `no_user` until they finish.
4. **The deployment signing budget.** Initial values: 600 signatures per
   minute and 5,000 per claim. A rebase of a long branch is the case to size
   for.
5. **The signing log in the UI.** `commit_signatures` is readable by members.
   Showing it (per conversation, or per signer) is not part of v1.
6. **Local verification in the jail.** With an allowed-signers file in the
   git config, `git log --show-signature` would verify inside a run. Nothing
   needs it yet.

## 11. Decision log

1. **SSH over GPG** — §3.1.
2. **TF-generated keys** — §3.2.
3. **The signer authors and commits** — chosen over a bot author with the
   signer as committer, which also verifies (§2, case E). One identity on the
   commit, and the same shape as Claude Code's commits.
4. **A derived noreply address** — §3.4, from spike cases A and B.
5. **Per-org keys in the bundle; the deployment key on the control plane** —
   §3.6 and §3.7. The deployment key would otherwise sit in every managed
   tenant's runs, where one compromised sidecar would expose a key that signs
   as one account across all of GitHub.
6. **Any control pod signs** — leader failover would otherwise stall commits
   for the lease's handover window.
7. **Digest only** — the control plane never parses agent-written bytes. The
   cost is that it cannot check what it signs; the claim check, the budget and
   the log bound that.
8. **Stop signing on failure** — chosen over continuing to sign (Unverified
   badges) and over refusing to commit (one misconfiguration stops every run's
   commits).
9. **Managed workspaces default to the deployment signer** — the deployment
   App's bot already authors their commits, so the deployment signer puts no
   new account on them. An org can register its own.

## Related

- `docs/for-agents/specs/sky-254-runsc-validation/README.md` §4 — the earlier
  note on host-side signing through a `-Y sign`-only program.
- `docs/for-agents/specs/per-run-credential-isolation/` — why the sidecar
  holds one run's credentials and nothing more.
- `docs/security/privilege-separation.md` — why secrets and hostile input meet
  only in the sidecar.
- `internal/githooks/commit_identity.go` — the identity stamping this extends.
