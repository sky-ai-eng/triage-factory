# Deployment commit signer

Organizations on the [deployment GitHub App](deployment-github-app.md) commit as
the App's bot, and GitHub never marks a bot's commit Verified unless GitHub
made it. The deployment commit signer is a GitHub **user** account whose name
and SSH signing key those organizations' agent commits carry, so GitHub marks them
Verified and repositories that require signed commits accept them.

An organization that registers its own signer in **Organization → Settings →
Commit signing** uses that one instead. An organization whose own signer is
broken commits unsigned; it never falls back to the deployment's. Local mode ignores
these variables.

## 1. Create the account

Create a GitHub **user** account on the deployment's GitHub for this alone,
not a person's: every commit is authored as it, and a leaked key can sign only
as it. GitHub treats it as a machine account, so a person creates it, is
responsible for it, and gives it an email address someone receives.

- Turn on two-factor authentication.
- Set a profile name (**Settings → Public profile → Name**). Commits carry it;
  without one they carry the login.
- Keep the email private, and use a second verified address, not the recovery
  one, if you set `TF_GITHUB_COMMIT_SIGNER_EMAIL` (step 4).
- On GHE.com, or in an enterprise with managed users, create the account
  through your identity provider and verify its email before step 4.

The account needs no membership or seat in your GitHub organization. If your
repositories run a CLA check, add its login to the allowlist first: commits are
authored as that account.

## 2. Generate the key

On a machine you trust, with no passphrase (TF can't unlock one):

```sh
ssh-keygen -t ed25519 -N '' -C 'Triage Factory commit signing' -f ./secrets/commit-signer
```

## 3. Add the public key to the account

Signed in as the signer account: **Settings → SSH and GPG keys → New SSH key**,
**Key type: Signing key**, paste `./secrets/commit-signer.pub`. Key titles are
public, so name it after the product.

## 4. Configure `control` and restart

```yaml
# compose.override.yml
secrets:
  tf_commit_signer_key: { file: ./secrets/commit-signer }
services:
  control:
    secrets: [tf_commit_signer_key]
```

```dotenv
# .env
TF_GITHUB_COMMIT_SIGNER_LOGIN=TriageFactory
TF_GITHUB_COMMIT_SIGNER_KEY_FILE=/run/secrets/tf_commit_signer_key
# TF_GITHUB_COMMIT_SIGNER_EMAIL=agent@example.com
```

| Variable                        | Required       | Meaning                                                                                                                                                                         |
| ------------------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TF_GITHUB_COMMIT_SIGNER_LOGIN` | with the key   | The account's login. Renaming the account later changes nothing; TF follows it by id.                                                                                           |
| `TF_GITHUB_COMMIT_SIGNER_KEY`   | with the login | The unencrypted ed25519 private key in OpenSSH format. Supply it as a file with `TF_GITHUB_COMMIT_SIGNER_KEY_FILE` ([Deployment secrets](../secrets.md)).                       |
| `TF_GITHUB_COMMIT_SIGNER_EMAIL` | off github.com | The committer address, verified on the account. On github.com, unset means the account's noreply address. Don't use the account's recovery address: it appears on every commit. |

Half a configuration (a login without a key, a key without a login, an email
alone, a key that isn't an unencrypted ed25519 key, or no email off github.com)
refuses the boot and names the variable. Give every `control` pod the same
three values. Executors never hold the key.

```sh
docker compose up -d
```

## 5. Check it took

Within a minute `control` logs
`deployment commit signer is active; managed workspaces sign as it`. If it logs
`deployment commit signer is pending` followed by
`the deployment commit signer's key is not among the account's signing keys`,
step 3 is missing, or GitHub hasn't listed the new key yet (allow a minute).

```sh
docker compose exec control triagefactory commit-signer status   # shows the signer
docker compose exec control triagefactory commit-signer verify   # checks it again now
```

An organization on the deployment App shows **Commits are signed as
TriageFactory, this deployment's signing account** under **Organization →
Settings → Commit signing**. Runs pick the signer up at their next start.

## Rotating the key

Add the new public key to the account first, then change the key and restart
the `control` pods. Until a check finds the new key on the account,
organizations don't sign. Leave the old public key on the account unless it may have leaked:
on a GHES with persistent commit verification off, deleting it turns the
commits it signed Unverified.

## Changing the account

Generate a new key as well. A key stays bound to the account it was first
found on, so changing only `TF_GITHUB_COMMIT_SIGNER_LOGIN` leaves it on the old
account, and each `control` pod logs a warning saying so.

## When it breaks

If the key is removed from the account or GitHub fails a pushed commit for its
address, organizations commit unsigned from their next signature, and the
`DeploymentCommitSignerDown` alert fires after 15 minutes
([Monitoring](../monitoring.md#alerts)). `triagefactory commit-signer status` on
a `control` container shows why. Fix the cause on the account, then run
`triagefactory commit-signer verify` there. A key put back
on the account also recovers on the next periodic check.
