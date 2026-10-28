# Commit signing

Design of record for signing the commits delegated agents make, so GitHub
marks them Verified. An org admin registers a GitHub user account as the org's
**signer**. Agent commits are authored and committed as that account and
signed with a key only Triage Factory holds. Workspaces on the deployment
GitHub App get the deployment's signer by default.

Status: **accepted design**; §10 lists what is still open. Builds on the sealed commit identity
(**TFAC-1187**) and the per-ref push outcomes (**TFAC-1188**), both merged.

Scope: both run modes, core (no licence gate). The deployment signer is
multi-mode only, because the deployment App is.

---

## 0. Why

TF's commits are unsigned. An agent's commit is authored as the org's GitHub
identity, which for most orgs is a GitHub App's bot account, and a bot account
cannot hold a signing key. A bot commit is Verified only when GitHub creates
and signs it itself, as it does for a merge made in the web UI, so a commit TF
makes with git and pushes can never be Verified as the bot.

That has three costs:

- A repository that requires signed commits, through branch protection or a
  ruleset, refuses every push TF makes.
- An org that wants Verified badges on agent commits cannot get them.
- Every commit is named for the bot: GitHub shows `<slug>[bot]`
  (`triage-factory[bot]` on the cloud deployment) in the commit list and in
  blame, and `git log` shows the same name.

GitHub verifies an SSH-signed commit when the signing key is registered as a
**signing key** on a GitHub **user** account and the commit's **committer**
email is one of that account's verified emails. The author is not checked
(§2, case E).

## 1. Goals and non-goals

Goals:

1. An org admin registers one GitHub user account per org as its signer. TF
   generates the key pair, and the admin adds the public half to the account.
2. Every commit an agent makes in that org is authored and committed as the
   signer and signed. GitHub shows it under the signer's login and avatar,
   and git under the account's profile name (§4.2), so an org names its
   agent's commits by naming the account.
3. A workspace on the deployment App uses the deployment's signer unless it
   registers its own.
4. No private key enters a jail. In multi mode no private key leaves the
   control plane.
5. When signatures would not verify, TF stops signing and says why, rather
   than producing commits GitHub marks Unverified.

Non-goals:

- **Enforcing signatures.** An agent can always commit with `--no-gpg-sign`.
  A repository's own rule is what enforces signing, and TFAC-1188 makes that
  refusal visible.
- Signing tags, GPG keys, per-team signers, and signing commits people make
  outside a run.
- **Renaming the bot on pull requests, reviews and comments.** Those are the
  App's actions, and GitHub shows them as the App. Making them as the signer
  would take a user token for the signer's account.
- **Checking other people's signatures inside a run.** The allowed-signers
  file (§4.2) names only the run's signer. GitHub's API answers for anyone
  else's commits.

## 2. Spike findings

Two runs against github.com (git 2.53.0, OpenSSH 10.2p1). Each case was an
empty commit pushed on its own branch, read back through
`GET /repos/{o}/{r}/commits/{sha}` → `commit.verification`.

Run 2026-10-25:

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

Run 2026-10-26, a login rename and an unverified address. The account was
renamed `<old>` → `<new>` and back, about 76 s apart. A swapped round, with
the names exchanged after the rename back, matched R1–R3.

| case | committer | when | verified | reason |
| --- | --- | --- | --- | --- |
| R1 | `<id>+<old>@users.noreply.github.com` | after the rename | true | `valid` |
| R2 | `<old>@users.noreply.github.com` | after the rename | false | `no_user` |
| R3 | `<id>+<new>@…` and `<new>@…` | after the rename | true | `valid` |
| R4 | R2, re-read after the rename back | — | true | `valid`, a new `verified_at` |
| R5 | a case-B commit pushed before the rename, re-read after it | — | true | `valid`; `committer.login` null |
| U1 | an address on the account, not verified | — | false | `unverified_email` |
| U2 | U1, re-read after the address was verified | — | true | `valid` |

What follows from it:

- **Only the committer counts** (E).
- **The id form of the noreply address survives a rename; the login-only form
  does not** (A, B, R1–R3). `<id>+<login>@users.noreply.github.com` verifies
  with any login the account has held, so TF derives that form. No mailbox is
  needed, and the admin types no email. `<login>@users.noreply.github.com`
  verifies only while the login is current.
- **A passing result is stored at push time; a failing one is evaluated again
  on every read** (H, R4, R5, U2). Deleting a key changes no verified commit;
  only later pushes fail, with `unknown_key`, so rotating a key breaks nothing
  in the history. A commit that failed reads `valid` once the fault is fixed,
  which Verify relies on (§7).
- **An address that belongs to no account fails with `no_user`** (D, R2), and
  one on the account but not verified fails with `unverified_email` (U1). An
  unverified address still links the commit to the account; only
  verification needs it verified.
- **A rename releases the old login at once.** `GET /users/<old>` answered 404
  straight after, so anyone could claim the name. TF identifies a signer by
  account id (§5.1), so signing is unaffected.
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

From GitHub's documentation (`github/docs`, the source of docs.github.com,
read 2026-10-26), for hosts other than github.com:

- **Every supported GHES version verifies SSH signatures** and serves the
  SSH signing-key endpoints (3.18 through 3.22). 3.7 is the earliest version
  whose documentation describes SSH signing.
- **GHES has no noreply address.** The noreply address and **Keep my email
  address private** are documented for github.com and GitHub Enterprise
  Cloud only. A GHES account commits under an address of its own.
- **GHES persists verification by default from 3.17, and a site admin can
  turn it off** (`ghe-config app.persist-commit-signature-verification.enabled
  false`). With it off, a commit's verification is evaluated again against
  the account's current keys, so deleting a key from the account turns that
  key's past commits Unverified.
- **GHE.com verifies SSH signatures** once the key is on the account there and
  the committer email is verified on it. Its noreply address is not
  documented. Every GHE.com account is a managed user, and a managed user
  created after 2024-08-01 starts with an unverified email.

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
4. **On github.com the committer email is derived unless the signer sets
   one.** The default is `<id>+<login>@users.noreply.github.com`, the form
   that keeps verifying across a login rename (§2), so an admin types no
   email. A signer may name a real address instead, verified on its account
   (§2, case C): an org signer through `PATCH commit-signer`, the deployment
   signer through `TF_GITHUB_COMMIT_SIGNER_EMAIL`. On any other host the
   address is required, because GHES has no noreply address and GHE.com's is
   not documented (§2, §10.1). A ruleset that requires commit emails on the
   org's domain rejects the noreply address, so for that org a real address
   is the only way to commit at all. A real address also reads better in
   `git log`. TF cannot read another account's verified emails, so a wrong
   address shows up in the post-push check, and Verify confirms the fix
   (§7). The address is on every commit, so it should not be the one that
   recovers the account.
5. **One signer per org.** The commit identity resolves, in order, to the
   org's signer, then the deployment signer (managed workspaces only), then
   today's identity (the App bot or the PAT owner).
6. **In multi mode every signature is made on the control plane.** A per-org
   key stays in the secret store and the deployment key in control-pod config;
   no signing key is sealed into a bundle or reaches an executor. A signing
   key does not expire: it works until someone removes it from the account,
   and it verifies in any repository on GitHub, not only the org's. An App
   org's bundle otherwise holds only installation tokens, which expire within
   an hour, so a key in it would turn a compromised sidecar's one-hour
   exposure into an indefinite one. Instead, a compromised sidecar can only
   request signatures while its claim is active, within the budget, and each
   one is logged. Local mode has no control plane; its daemon signs in the TF
   process.
7. **The control plane signs a digest, and any control pod answers.** It
   receives the SHA-512 digest that an SSH signature covers and never the
   commit, so no agent-written bytes are parsed in the most privileged
   process. A signing request is not gated on the background-brain lease.
8. **The signer checks the committer only.** The daemon refuses to sign a
   commit whose committer is not the signer. The author is free, because a
   rebase or cherry-pick re-signs commits that keep their original author.
9. **When signatures would not verify, signing stops at the next
   signature.** The reason GitHub gave is shown on the signer. Every signature
   checks the signer's status, in both modes. A run that starts while the
   signer is not `active` is configured not to sign. A run that started while
   it was `active` gets a refusal saying signing is off for the rest of the
   run and to commit with `git -c commit.gpgsign=false`, so it commits
   unsigned instead of stopping. An org that switches signers mid-run gets the
   same: a run's identity is fixed when its sandbox starts, so the run keeps
   committing as the old signer, unsigned, and its next run starts with the
   new one.
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
                 ├─ local ─ sign with the key from the secret store
                 └─ multi ─ relay the digest to the executor, which queues
                            it for the control plane (§4.5)
```

### 4.2 In the run: git config and the shim

When a run signs, its single `GIT_CONFIG_*` block (`internal/agentproc/proxies.go`
in the sandbox, `githooks.DirectAgentEnv` in a direct local spawn) gains:

```
user.name=<signer name>
user.email=<signer email>
gpg.format=ssh
commit.gpgsign=true
user.signingkey=key::<signer public key>
gpg.ssh.program=<hooks dir>/ssh-sign
gpg.ssh.allowedSignersFile=<allowed-signers path>
```

`user.name` / `user.email` are the identity TFAC-1187 already stamps; the
signer simply becomes that identity. The name is the account's profile name,
or its login when the profile has none, as git writes it into the committer
header: git trims whitespace and `, : ; < > " \ '` from both ends of a name and
drops `<`, `>` and newlines anywhere, so the identity is resolved with the same
rule (`commitsign.IdentName`) and the daemon's committer check compares the
string git wrote. GitHub shows a commit linked to an
account under the account's login and avatar, and the name is what `git log`,
blame in an editor, and other git tools show. Claude Code's commits split the
same way: `Claude` in git, `claude` on GitHub. A login cannot contain a space,
so a name like "Triage Factory" exists only as the profile name. Git config
keys pass the sandbox env allowlist through the `GIT_CONFIG_KEY_` /
`GIT_CONFIG_VALUE_` prefixes, so the allowlist does not change.

The allowed-signers file lets git inside the run check the run's own
commits: `git verify-commit`, `git log --show-signature`, and repository
tooling that calls them. Without it every local check fails with
`gpg.ssh.allowedSignersFile needs to be configured`, signed or not. It holds
one line per key, `<signer email> namespaces="git" <public key>`, for the
signer's key and, during a rotation, its pending key, so a run that started
before `verify` promotes the new key still checks its later commits. A
local run whose signer rotates to a key the file does not list yet gets the
key added when it first signs with it. A run continued in a new process
writes a new file from the keys current then, so a commit it signed earlier
with a key the signer has since dropped no longer checks inside the run. The
file holds only public values. It is presented read-only, like the run's CA
bundle: in multi mode the sidecar writes it into the run's socket directory
beside `ca-bundle.crt` and the jail sees it at `/run/tf-allowed-signers`; in
local mode TF writes it under the state root and the local sandbox binds it
read-only. A run therefore cannot add a key to make its own check pass,
although the check gates nothing: GitHub and the daemon's check (§4.3) do.
Commits signed by anyone else still fail locally with "No principal
matched"; GitHub's API reports their verification.

The shim is a script embedded in `internal/githooks` beside `pre-push` and
`prepare-commit-msg`. `githooks.Ensure` writes it to the host hooks dir, which
the jail mounts read-only at `/run/tf-hooks` (`internal/cell/gvisor/jail_linux.go`)
and a local run reads in place. Git calls `gpg.ssh.program` to check
signatures as well as to make them (`git verify-commit` and `git log
--show-signature` run it with `-Y find-principals`, `-Y verify` and `-Y
check-novalidate`), so only the signing call goes to TF and every other call
to `ssh-keygen`:

```sh
#!/bin/sh
if [ "$1" = "-Y" ] && [ "$2" = "sign" ]; then
	exec "${TRIAGE_FACTORY_BIN:-triagefactory}" exec git sign "$@"
fi
exec ssh-keygen "$@"
```

`exec git sign` is a new exec verb. It accepts only `-Y sign -n git`, takes
the last argument as the buffer, ignores `-f` and `-U`, sends the buffer to
the daemon, and writes `<buffer>.sig`. On a refusal or a failure it prints
one line to stderr and exits 1; git then aborts the commit with
`error: gpg failed to sign the data`. The agent reads that line and nothing
else, so each message says what happened, that nothing was committed, and the
exact next step. Every one starts `triagefactory: `:

| case | message |
| --- | --- |
| the committer is not the signer | `this run signs commits only as <signer name> <<signer email>>, and this commit's committer is <name> <<email>>. Nothing was committed. Run the command again without setting the committer: no GIT_COMMITTER_NAME or GIT_COMMITTER_EMAIL, and no -c user.name or -c user.email. --author is fine.` |
| not a commit | `this run signs commits only, and this is a <object type>. Nothing was created. Create it without signing; for a tag, drop -s.` |
| the org is over its per-minute budget | `this org has made <limit> signed commits in the last minute, its limit, so signing resumes in <n>s. Nothing was committed. Wait <n>s, then run the same git command again. A rebase that stopped here resumes with git rebase --continue; if that refuses because of staged changes, run git reset --hard first, and the rebase applies this commit's changes again.` |
| the claim has spent its budget | `this run has made <limit> signed commits, the most one run may make, so signing is off for the rest of this run. Nothing was committed. Run this and every later commit with git -c commit.gpgsign=false. A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false.` |
| the signer is not `active`, or not the org's | `commit signing is off for the rest of this run: <reason>. Nothing was committed. Run this and every later commit with git -c commit.gpgsign=false. A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false. Commits are still authored as <signer name>.` |
| no answer within 30 s | `commit signing did not answer within 30s. Nothing was committed. Run the same git command again. A rebase that stopped here resumes with git rebase --continue; if that refuses because of staged changes, run git reset --hard first, and the rebase applies this commit's changes again. If it fails three times in a row, commit with git -c commit.gpgsign=false; for a rebase, abort it and run it again that way.` |
| the claim is no longer active | `this run no longer holds its engagement, so commit signing refused. Nothing was committed.` |

`<reason>` is a sentence, never a code: "the signer's key is no longer on its
GitHub account", "an org admin removed the signer", "the org now signs as a
different account", "GitHub reported a recent commit's email as not verified
on the signer's account".

The rebase clauses follow what git does when a rebase's commit cannot be
signed:

- A commit that fails while the rebase picks it leaves its changes staged.
  Before git 2.56 the pick is rescheduled, and `git rebase --continue` refuses
  with "you have staged changes"; `git reset --hard` clears them and the
  rescheduled pick applies them again. From git 2.56 the sequencer treats a
  failed commit after a clean pick as a stop like a conflict, not a
  reschedule, so `--continue` commits the staged changes, as it does after a
  resolved conflict below.
- A commit that fails inside `git rebase --continue`, after a conflict was
  resolved, is not rescheduled; the next `--continue` commits the resolution,
  which a reset would throw away. Hence "if that refuses".
- A rebase keeps signing once it has started (`rebase-merge/gpg_sign_opt`),
  whatever `-c commit.gpgsign=false` says on `--continue`, so a run whose
  signing is off aborts it and runs it again.

The tests in `cmd/exec/git` follow each of these through git.

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
4. Sign in the TF process (local, §4.4) or relay the digest (multi, §4.5).
5. Compute the resulting commit id the way git does: the armored signature
   becomes a `gpgsig` header at the end of the header block, and the id is the
   SHA-1 of `commit <size>\0<object>`.
6. Record the commit id on the signature's row (§5.3).

The SSH signature format (`SSHSIG`, armored as `BEGIN SSH SIGNATURE`) is not
in `x/crypto`. TF implements it in `internal/commitsign`: the signed data is
the `SSHSIG` magic, the namespace (`git`), an empty reserved field, the hash
algorithm (`sha512`) and the digest. Its test signs with TF's implementation
and verifies with `ssh-keygen -Y verify`.

### 4.4 Where the keys are

- **Local mode.** A per-org key is in the secret store (the OS keychain, or
  the encrypted file backend). The daemon reads it, and the signer's status,
  for every signature. There is no deployment signer.

  The key is out of a run's reach only while the local sandbox is on, which
  masks the store. With it off (macOS, or `TF_LOCAL_SANDBOX=off`) the agent
  runs as the operator and can read the key from the store, as it can the
  org's GitHub token. It can also run `exec git sign` with signing settings
  of its own, past the claim, budget and committer checks (§4.3, §4.7),
  though only as the org's signer. Local mode is not a security boundary,
  and signing does not make it one: an org that signs locally should sign
  with a dedicated account, so a key that leaks signs only as that account.
  TFAC-885 tracks what closing this would take.
- **Multi mode.** A per-org key is in the secret store, which only control
  pods open (executors run `pgstore.NewWithoutSecrets`). The deployment key is
  operator config on the control pods, in the same class as the deployment
  App:
  - `TF_GITHUB_COMMIT_SIGNER_LOGIN`
  - `TF_GITHUB_COMMIT_SIGNER_KEY` (secret, honours `_FILE`)
  - `TF_GITHUB_COMMIT_SIGNER_EMAIL` (optional; unset means the derived
    noreply address)

  `docker-compose.yml` forwards all three to the control service only.
  `credprovision.ProvisionForConversation` seals the signer's public half
  into `credbundle.GitHubCreds` (§5.5) when the signer is `active`. The
  signer's name, email and public key are all an executor gets.

Each signature reads the signer's current key, so a run that started before a
rotation signs with the new key once `verify` promotes it. Git does not
compare the signature's key with `user.signingkey`; a Phase 1 test commits
across a rotation to hold that.

### 4.5 Signing on the control plane (multi only)

One signature, the same for an org signer and the deployment signer:

1. The sidecar's daemon passes its check (§4.3) and makes a relay call,
   `sign_digest {digest, signer, signer_user_id}`, to the orchestrator. The
   signer kind and account id are the ones the run was sealed with.
2. The orchestrator inserts a `commit_signatures` row in state `requested`
   (§5.3), naming the conversation, the claim and that signer, and publishes
   `sign_request` on `tf_ctl`.
3. Every control pod handles `sign_request`. It is not gated on the
   background-brain lease, unlike `cred_request` (`internal/app/relay.go`).
   Each pod also sweeps for `requested` rows once a second, because `tf_ctl`
   is lossy.
4. A pod claims the row (`requested` → `signing`, conditional update with
   `RETURNING`). It checks that:
   - the claim is still active, at the boot epoch the row names;
   - the org still resolves (§4.6) to the signer the row names, the same kind
     and account, and that signer is `active`;
   - the claim and the org are within their signing budgets (§4.7).

   A sidecar that names a different signer gains nothing: the check passes
   only for the signer the org resolves to now. The pod then reads that
   signer's key (the org's from the secret store, the deployment's from its
   config), signs the digest and stores the signature (`signed`), or stores
   the refusal (`refused`, with the reason, and for the per-minute budget the
   seconds until signing resumes). Finally it publishes `sign_done`.
5. The orchestrator wakes on `sign_done` (polling the row every 250 ms as a
   backstop) and returns the signature to the sidecar.
6. After 30 s with no answer the call fails, and the shim prints the
   no-answer message (§4.2).

The latency is a notify, a sign and a row read: milliseconds normally, and up
to the backstop intervals when a notify is dropped. A rebase re-signs every
commit it rewrites, so it pays that latency once per commit. With every
control pod down, a signed commit fails with "unavailable" until one is back.

### 4.6 Resolving the identity

One function resolves a run's commit identity and its signing mode
(`none` | `org` | `deployment`) from the org signer row, the org's credential
class and the deployment signer's status. `credprovision` calls it in multi
mode and the spawner's `resolveCommitIdentity` in local mode. Every signature
calls it again: the daemon locally, the control pod in multi mode. The co-author
trailer on manual runs compares the delegating person against the signer's
account id, as it compares against the bot's today.

### 4.7 Signing budgets

Two limits, operator config read where the signature is made (the control
pods in multi mode, the TF process locally), and counted over
`commit_signatures` rows (§5.3), so no counter is stored:

- `TF_MAX_COMMIT_SIGNATURES_PER_ORG_MINUTE`, default 600: signatures for one
  org in any 60 s. Over it, the refusal carries the seconds until the oldest
  counted signature leaves the window.
- `TF_MAX_COMMIT_SIGNATURES_PER_CLAIM`, default 5000: signatures for one
  claim. Once spent, signing is off for the rest of that claim.

They bound what one compromised or runaway run, or one org, can get signed.
The count is read before a signature is made and its row is written after,
so signatures made at the same moment can each pass at the limit: a limit
is exceeded by at most the signatures in flight when it is reached.
A rebase is the case they are sized for: it re-signs every commit it
rewrites, so a 300-commit branch is 300 signatures in seconds, while a
typical run makes a handful. The deployment signer has no limit of its own:
signing costs the control plane microseconds and GitHub does not limit
signatures, so its total is the sum of its orgs', which
`tf_commit_signatures_total{signer="deployment"}` counts by org (§7, Metrics).
The defaults are a starting point, tuned from
`tf_commit_signatures_total` (§7, Metrics).

## 5. Data

### 5.1 `org_commit_signers` (both dialects)

One row per org.

| column | |
| --- | --- |
| `org_id` | PK, FK `orgs` |
| `host` | the GitHub host the account is on, the org's host at registration |
| `github_user_id` | the account's numeric id |
| `login` | display value, refreshed by the periodic check (§7) |
| `name` | nullable: the account's profile name, refreshed by the periodic check |
| `email_override` | nullable; null means the derived noreply address |
| `public_key` | `ssh-ed25519 AAAA…` |
| `pending_public_key` | nullable, set during a rotation |
| `status` | `pending` \| `active` \| `broken` |
| `status_reason` | nullable: GitHub's verification reason, or `key_missing` |
| `status_since` | |
| `checked_at` | nullable |
| `created_at`, `updated_at`, `created_by` | |

Two orgs may register the same account. Each generates its own key, the
account holds both, and each row's health is its own key's, so removing one
org's key breaks only that org.

RLS follows `org_event_sources`: members select, org admins write, and
`REVOKE ALL` from the other roles. The Postgres change is a baseline edit; the
SQLite one is a new migration.

### 5.2 Secrets

`github_commit_signing_key` and `github_commit_signing_key_pending` (OpenSSH
private-key format) in `internal/integrations`. Both go in `AllLocalSweepKeys`
and the keychain list in `scripts/clean-slate.sh`.

### 5.3 `commit_signatures` (both dialects)

One row per signature: in multi mode the queue the control pods sign from,
and in both modes the signing log.

| column | |
| --- | --- |
| `id` | |
| `org_id`, `conversation_id` (FK, `SET NULL`), `claim_id`, `boot_epoch` | |
| `signer` | `org` \| `deployment` |
| `signer_host`, `signer_user_id`, `public_key` | which account and key signed |
| `digest` | SHA-512 hex |
| `state` | `requested` \| `signing` \| `signed` \| `refused` |
| `refusal_reason`, `signature`, `claimed_by`, `signed_at` | |
| `commit_sha` | set by the daemon once the commit id is known (§4.3 step 5) |
| `repository_id`, `pushed_at` | set when a push lands this commit as a ref's new tip |
| `verification_reason`, `checked_at` | GitHub's answer for a pushed commit (§7) |
| `created_at` | |

Locally the daemon inserts the row as `signed`. In multi mode the
orchestrator inserts it as `requested` (§4.5), and the daemon's commit id
reaches it as a relay record, like `record_push`. `tf_system` (the executor)
gets `INSERT`, `SELECT`, and `UPDATE (commit_sha, repository_id, pushed_at)`.
Org members get `SELECT`.

The signing budgets (§4.7) are counts over these rows, per org and per
claim, so no counter is stored. The same rows give per-signer totals:
grouping by `signer_host, signer_user_id` totals one account across every org
that uses it, the deployment signer included.

### 5.4 `deployment_commit_signer` (Postgres only)

One row per deployment signer key, keyed by the host and the public key:
`github_user_id`, `login` and `name` (read from the account the login names,
like an org signer's, and refreshed by id after), `status`, `status_reason`,
`status_since`, `checked_at`. Every control pod reads it before signing, and
`triagefactory commit-signer status` shows it. Rotating the key in config starts a new row, pending;
the host in the key keeps a deployment whose default GitHub changed from
inheriting another host's account. Until a check has read the account,
`github_user_id` is null and no workspace commits as the signer.

### 5.5 The bundle

`credbundle.GitHubCreds` gains:

```go
Signer *SignerCreds `json:"signer,omitempty"`

type SignerCreds struct {
	Mode             string `json:"mode"` // "org" | "deployment"
	PublicKey        string `json:"public_key"`
	PendingPublicKey string `json:"pending_public_key,omitempty"` // during a rotation
}
```

The signer's name, email and account id travel in the identity fields
TFAC-1187 added. `Signer` is nil when the run does not sign. The bundle never
carries a private signing key (§3.6).

### 5.6 The audit log

The governance feeds carry what an auditor acts on, and `commit_signatures`
keeps the rest. The rows below are written for every org; only the feed that
shows them is licensed.

- **Signer changes** go to the access log under a new credential kind,
  `github_commit_signer`: register, verify, rotate, an email change, and
  removal (§6).
- **Pushes.** A `branch_pushed` or `branch_push_failed` row's `detail_json`
  gains `signer`: the login that signed the pushed tip, or `unsigned`. The
  orchestrator fills it when it records the push, from the `commit_signatures`
  row whose `commit_sha` is the new tip, the same lookup that stamps that row
  for the post-push check (§7). On a refused push, `unsigned` beside a ruleset
  refusal names the cause.
- **Refusals** are a security signal, recorded like `gh_write_denied`: a new
  action, `commit_sign_denied`, under `CredentialNone`, because nothing was
  signed. `detail_json` carries `{reason}`:
  - `committer_mismatch`, with the committer the commit named: an agent asked
    to sign as someone else. The daemon's check (§4.3) records it.
  - `budget_exhausted`: a run reached its per-claim cap. One row per
    conversation, keyed the way `EgressDenialDedupKey` keys an egress denial,
    because the signal is that it happened.
  - `claim_inactive`: a request from a claim that is no longer active. The
    control pod records it (§4.5).

  The governance feed's `ACTION_META` gains the action;
  `feature_parity_test.go` holds the two lists together.

Kept out: each signature (a rebase of 100 commits would be 100 rows, and a
signature is not an external act), "unavailable" timeouts (an operations
fault, for metrics), "signing is off" refusals after a signer breaks (the
signer's status already shows it), and health changes (TF acted on nothing
external; the signer shows its status and since when).

## 6. API

Under `/api/orgs/{org_id}/github/`, beside the App and PAT routes:

| route | gate | does |
| --- | --- | --- |
| `GET commit-signer` | member | the org's signer, 404 when none |
| `POST commit-signer` `{login, email?}` | admin | resolve the login on the org's host (must be a user, not a bot), generate the key pair, create the row `pending`; returns the public key and a suggested key title; 409 when the org already has one; `email` is required on a host other than github.com (§3.4); 422 `SSH_SIGNING_UNSUPPORTED` when the host answers 404 for the signing-key listing of a user that exists, the way `internal/githubapp/hook.go` treats a GHES without the deliveries endpoint |
| `POST commit-signer/verify` | admin | look for the key in the account's public listing (not found → 409 `KEY_NOT_FOUND`, saying the listing can lag a minute); for a signer the post-push check broke, also re-read the commit that failed (still failing → 409 `COMMIT_UNVERIFIED`, with GitHub's reason); both pass → `active` |
| `POST commit-signer/rotate` | admin | generate a pending key; the current key keeps signing until `verify` finds the new one, then the new one replaces it |
| `PATCH commit-signer` `{email}` | admin | `null` restores the derived address, and is refused on a host other than github.com |
| `DELETE commit-signer` | admin | remove the row and its secrets |
| `GET commit-identity` | member | the identity this org's runs commit as, and whether and how they sign (§4.6) |

- Writes take the org's GitHub credential lock (`githubAppRegMu`) and record
  an access-log row under a new credential kind (§5.6).
- The signer counts as a connected credential for the host: add it to
  `githubCredentialConnected` (`internal/server/github_credential_class.go`),
  so `github_base_url` cannot change while one exists. The PAT unbind and the
  App disconnect, which otherwise clear the host, keep it while a signer
  exists, as the PAT unbind keeps it for a staged App registration; removing
  the signer then clears it when nothing else is connected.
- The suggested key title is "Triage Factory commit signing", because titles
  are public.
- The deployment signer has no routes. Its config is env on the control pods,
  so whoever can change or fix it has a shell there, and the operator CLI
  serves it the way `triagefactory operator` serves operator grants:
  `triagefactory commit-signer status` reads the row, and
  `triagefactory commit-signer verify` is its Verify, the only way out of a
  signer a pushed commit broke. It is core and needs no licence. Its health is
  also in a WARN on each control pod and `tf_deployment_commit_signer_up`,
  and its signatures by org in `tf_commit_signatures_total{signer="deployment"}`
  (§7). A managed workspace's admin sees it through `GET commit-identity`.

## 7. Health

A signer is `pending` until its key is found, `active` while signing, and
`broken` when a check fails. Only an `active` signer is sealed into a bundle
or signs.

1. **Verify** (§6): the admin's action, and the only way out of `broken`
   apart from the case below. For a signer the post-push check broke, it also
   re-reads the commit that failed: the newest `commit_signatures` row for the
   signer with a failing `verification_reason`. GitHub evaluates a failing
   result again on every read (§2, cases R4 and U2), so that commit reads
   `valid` once the fault is fixed. This is how TF confirms an email it cannot
   see. The commit holds the signer only while it can still say something
   about the signer as it is: one signed with a key other than the one being
   verified (a rotation replaced it), one whose committer address is not the
   one the signer now commits as (an email change replaced it), or one GitHub
   no longer has, is skipped, and the next push is checked instead. Otherwise
   a fault fixed by a rotation or an email change would keep the signer
   broken for good. Verify reads the account by its id, so a renamed login is
   read under its new name and a released one is never read.
2. **Periodic check**, on the GitHub poll cycle, at most every 15 minutes per
   signer. Using the org's credential, it refreshes the login and name by
   account id and confirms the key is still on the account. A missing key sets
   `broken (key_missing)`, and the same check clears it once the key is back.
   An account GitHub no longer has is a missing key. A pending signer waits
   for Verify either way.
3. **Post-push check.** When a push lands a signed commit as a ref's new tip
   (TFAC-1188 supplies per-ref outcomes), the orchestrator stamps that
   commit's `commit_signatures` row with the repository and push time; the
   first push of a commit is the one kept. On its next cycle, the control
   plane reads the commit's `verification` with the org's credential and
   stores the reason. Anything other than `valid` sets `broken` with that
   reason, which stays until a verify. A failure counts against the signer
   only while it can say something about the signer as it is, by the test
   Verify's re-read uses (point 1). A signer already `broken` keeps its
   reason, except that a failing commit replaces `key_missing`, unless the
   commit's reason is `unknown_key`: that is the same fault seen on a commit,
   and the periodic check clears it once the key is back. A commit GitHub
   cannot be read for is asked about again each cycle for a day after its
   push, and so is one whose read ends in GitHub's own verification service
   failing (`gpgverify_unavailable`, `gpgverify_error`): that is no verdict
   on the commit, and Verify answers it as GitHub being unavailable.

The UI explains the common reasons: `unknown_key` (the key is not on the
account), `no_user` (the email belongs to no account), `unverified_email`
(verify the email on the account). Any other reason is shown verbatim.

A broken deployment signer stops signing for every managed workspace at the
next signature. Operators see it in `triagefactory commit-signer status`, in a
WARN on each control pod, and in `tf_deployment_commit_signer_up`.

The deployment signer belongs to no org, so its periodic check differs: every
control pod reads the row every 30 s and, once it is due, checks the account
and its keys itself, without a credential, since both are public. Finding the
key activates a `pending` deployment signer too: the operator adds the key to
the account before configuring it, so there is no admin step to wait for. A
check that cannot read GitHub writes nothing, so the row stays due; the pod
waits a minute before trying again, doubling up to the check interval, so
anonymous rate limits are not spent on retries. A pod's boot waits up to 5 s
for its first tick before it signs or seals: until a check finds a new key it
has no account, and a run that asked in that window would be told signing is
off for the rest of the run. The first check that finds the
key binds it to that account's id and later checks follow the id, as an org
signer's do: a configured login that no longer names the account is a WARN on
each check, and signing as another account takes a new key. Its post-push
check runs in each managed workspace's cycle with that workspace's credential,
and Verify re-reads the failing commit, from any org, the same way.

### Metrics

| metric | labels | meaning |
| --- | --- | --- |
| `tf_commit_signatures_total` | `signer`, `outcome`, `reason`, `org_id` | Signature requests by result. `signer` is `org` or `deployment`. `outcome` is `signed`, `refused` or `unanswered`. `reason` names a refusal (`committer_mismatch`, `not_a_commit`, `signer_inactive`, `org_minute_budget`, `claim_budget`, `claim_inactive`) and is empty otherwise. |
| `tf_commit_signing_duration_seconds` (histogram) | `signer`, `mode` | From the daemon's request to the signature in hand. `mode` is `local` or `control_plane`. Buckets from 5 ms to 30 s. |
| `tf_commit_signing_requests_pending` | — | `requested` rows no control pod has claimed. **Zero is the steady state.** |
| `tf_commit_signing_oldest_pending_age_seconds` | — | Age of the oldest of them. |
| `tf_commit_signing_swept_total` | — | Requests a control pod found by its once-a-second sweep rather than the `sign_request` doorbell. A steady rate means `tf_ctl` is dropping notifications, and each of those signatures waited up to a second. |
| `tf_commit_signing_limit` | `budget` | The configured limits (`org_per_minute`, `per_claim`), so a rule compares two series instead of hard-coding a number. |
| `tf_commit_signers` | `status` | Org signers by status: `pending`, `active`, `broken`. |
| `tf_commit_signer_broken` | `org_id`, `reason` | 1 for each broken org signer, with its reason. No series for a signer that is not broken. |
| `tf_deployment_commit_signer_up` | — | 1 while the deployment signer is `active`, 0 while it is not. No series when none is configured. |
| `tf_commit_signer_checks_total` | `check`, `result` | Health checks: `check` is `verify`, `periodic` or `post_push`; `result` is GitHub's reason (`valid`, `unknown_key`, `no_user`, `unverified_email`, `other`), `key_missing`, or `error` when GitHub could not be read. |
| `tf_commit_signing_pushes_total` | `signed`, `org_id` | Pushes from runs whose org resolves to a signer, by whether the pushed tip was signed. An unsigned one is a run that committed with signing off. |

Each signature is one span, `commit_signing.sign`, in the run's trace: from
the daemon through the relay to the control pod's signature, with the signer
kind, outcome and reason as attributes.

The rules worth shipping with it:

```promql
tf_deployment_commit_signer_up == 0                                                    # every managed workspace is committing unsigned
increase(tf_commit_signatures_total{reason="committer_mismatch"}[1h]) > 0             # a run asked to sign as someone else
increase(tf_commit_signatures_total{reason="claim_budget"}[1h]) > 0                   # a run reached its cap
increase(tf_commit_signatures_total{outcome="unanswered"}[15m]) > 0                   # signatures timing out
tf_commit_signing_oldest_pending_age_seconds > 5                                      # control pods are not signing
```

## 8. UI

A "Commit signing" section in `OrgSettings.tsx`, after "GitHub access":

- **No signer:** "Commits are authored by `<identity>` and aren't signed",
  with **Set up signing**, which takes a login and then shows the public key
  with the steps (GitHub → Settings → SSH and GPG keys → New SSH key → Key
  type: Signing key) and **Verify**.
- **Using the deployment signer:** "Commits are signed as `<login>`, this
  deployment's signing account", with **Use your own account**.
- **Pending / active / broken:** the status, the reason, and **Verify**,
  **Rotate key** and **Remove**, for org admins. The commit email is shown
  with **Change**, which says to add and verify the address on the account
  first. After a rotation the section says to leave the old public key on the
  account unless it may have leaked: TF has destroyed its private half, so it
  signs nothing, and on a GHES with persistent verification off, deleting it
  turns its past commits Unverified (§2).
- **On a managed-user account** (every GHE.com account, and Enterprise
  Managed Users on github.com), the setup steps add that the account's email
  must be verified first.

## 9. Rollout

Every phase ships its metrics (§7, Metrics) and documents them, with their
rules, in `docs/users/self-hosting/monitoring.md`.

**Phase 0 — TFAC-1188 (merged).** Per-ref push outcomes
(`gitproxy.PushedRef`). The post-push check and a visible "signed commits
required" refusal both depend on it.

**Phase 1 — Local, per-org, end to end.** The `SSHSIG` implementation and its
`ssh-keygen` test; the shim and `exec git sign`; the daemon check; the
`org_commit_signers` and `commit_signatures` tables in both dialects; the
secrets; the API, including the host checks (§3.4, §6); the settings
section; the audit rows (§5.6); the allowed-signers file (§4.2). Local mode needs git ≥ 2.34: below that the
run commits as the signer, unsigned, and `GET commit-identity` says why. (L)

**Phase 2 — Multi, per-org.** Sealing the signer's public half into the
bundle; the `sign_digest` relay call, the request flow and doorbells, and the
budget; signing on the control plane from the secret store; relaying the
commit id; the allowed-signers file and its read-only mount. The jail's git is Alpine 3.21's (`internal/sandbox/rootfs.go`),
well past 2.34. (L)

**Phase 3 — The deployment signer.** The control-pod config, the
`deployment_commit_signer` table, the managed-workspace default, and the
`commit-signer` CLI. Add the signer's login to this repository's CLA
allowlist before enabling it. (M)

**Phase 4 — Health.** The periodic and post-push checks, and the status and
reasons in the UI. (M)

*Exit criteria:* in both modes, an org with an active signer pushes commits
GitHub marks Verified as that account; a managed workspace does the same with
the deployment signer; no executor ever holds a signing key; a signer whose
key is removed is `broken` within one check, and signing stops at the next
signature.

## 10. Open questions

1. **GHE.com's noreply address.** It is not documented, so a GHE.com signer
   needs a real address (§3.4). A spike against a GHE.com tenant would show
   whether a derived address verifies there.

## 11. Decision log

1. **SSH over GPG** — §3.1.
2. **TF-generated keys** — §3.2.
3. **The signer authors and commits** — chosen over a bot author with the
   signer as committer, which also verifies (§2, case E). One identity on the
   commit, and the same shape as Claude Code's commits.
4. **A derived noreply address, or a real one** — §3.4, from spike cases A,
   B, C and R1–R3. The id form, because the login-only form stops verifying
   when the login changes. Chosen over the noreply address alone, which
   leaves an org whose rules require its own domain unable to commit.
5. **Every signing key on the control plane** — §3.6. Chosen over sealing
   per-org keys into the bundle for the sidecar to sign with, which saves a
   round trip per commit and keeps commits working while every control pod is
   down, but puts a key that does not expire into a bundle otherwise made of
   tokens that do. The deployment key could not go in a bundle in any case: it
   would sit in every managed tenant's runs, where one compromised sidecar
   would expose a key that signs as one account across all of GitHub. One
   signing path in multi mode is also less code than two.
6. **Any control pod signs** — leader failover would otherwise stall commits
   for the lease's handover window.
7. **Digest only** — the control plane never parses agent-written bytes. The
   cost is that it cannot check what it signs; the claim check, the budget and
   the log bound that.
8. **Stop signing on failure** — chosen over continuing to sign (Unverified
   badges) and over refusing to commit (one misconfiguration stops every run's
   commits). A run already going when its signer stops verifying is told to
   rerun the command without signing, so it loses one command, not its
   commits.
9. **Managed workspaces default to the deployment signer** — the deployment
   App's bot already authors their commits, so the deployment signer puts no
   new account on them. An org can register its own.
10. **Signing reaches the governance feeds, not a screen of its own** —
    §5.6. Chosen over a signing log in the UI, which would list every
    signature when an auditor asks whether a push was signed and whether a
    gate refused something.
11. **Detect SSH signing support; don't check versions** — §6. Every
    supported GHES version has it, and a 404 from the signing-key listing
    identifies a server that does not, the way TF already handles a GHES
    without hook deliveries.
12. **A real address off github.com** — §3.4. Chosen over deriving one for
    every host: GHES has no noreply address, and GHE.com's is not documented.
13. **The run's own signer in an allowed-signers file** — §4.2. Chosen over
    none, which fails repository tooling that runs `git verify-commit` on
    TF's own commits, and over every contributor's keys, which GitHub's API
    already answers for.
14. **Budgets as operator config, counted per org and per claim** — §4.7.
    Chosen over fixed numbers, which nobody can size before there is data,
    and over a per-signer count, which would let one managed workspace spend
    the deployment signer's limit for every other.

## Related

- `specs/sky-254-runsc-validation/README.md` §4 — the earlier
  note on host-side signing through a `-Y sign`-only program.
- `specs/per-run-credential-isolation/` — why the sidecar
  holds one run's credentials and nothing more.
- `docs/users/security/privilege-separation.md` — why secrets and hostile input meet
  only in the sidecar.
- `internal/githooks/commit_identity.go` — the identity stamping this extends.
