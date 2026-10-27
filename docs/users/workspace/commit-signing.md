# Commit signing

Commit signing makes the commits your agents push show as **Verified** on
GitHub, so repositories that require signed commits accept them. You register
a GitHub **user** account as the signer; agents' commits are authored as that
account and signed with an SSH key Triage Factory generates for it. Your GitHub
App (or token) still pushes and opens the pull requests; the signer account
never accesses a repository.

On a multi-mode deployment, a workspace using the deployment's GitHub App may
already sign as the deployment's signer: **Settings → Commit signing** then
reads "Commits are signed as `<login>`, this deployment's signing account".
Registering your own signer replaces it for your workspace. Operators set up
the deployment's signer in
[Deployment commit signer](../self-hosting/deployment-commit-signer.md).

## Create a dedicated account

Use an account made for this, not a person's: every agent commit carries its
name, and a leaked key can only sign as that account. In local mode with the
sandbox off, an agent can read the key from your keychain, so this matters
there too.

GitHub treats it as a **machine account**: a person creates it and is
responsible for it, it needs an email address someone receives, and a person
may hold at most one free machine account besides their own.

1. **Sign up** with an address you control, such as a shared mailbox. It is the
   account's recovery address.
2. **Turn on two-factor authentication.**
3. **Keep the email private** (Settings → Emails → Keep my email addresses
   private).
4. **Set the profile name** (Settings → Public profile → Name). It is the name
   commits carry in git; without one, commits carry the login. Set it before
   registering.

The account needs no membership or seat in your GitHub organization.

### Commit email

- **github.com:** commits use the account's noreply address by default,
  which needs no mailbox and survives a login rename. To use a real address
  (for example, a ruleset requires your company's domain), add and verify it on
  the account first. It appears on every commit, so don't use the recovery
  address.
- **GitHub Enterprise Server or GHE.com:** a real, verified address is
  required.
- **Managed users** (every GHE.com account, and github.com accounts whose
  login ends in `_<shortcode>`): create the account through your identity
  provider, and verify its email before registering. GitHub verifies no commit
  under an unverified address.

## Register the signer

1. In **Settings → Commit signing**, choose **Set up signing** and enter the
   login (and the commit email, off github.com). Triage Factory generates a key
   pair and shows the public key.
2. Signed in to GitHub as the signer: **Settings → SSH and GPG keys → New SSH
   key**. Use the suggested title, set **Key type** to **Signing key**, paste
   the key, and add it.
3. Back in Triage Factory, choose **Verify**. GitHub can take a minute to list
   a new key; if Verify doesn't find it, wait and retry.

## Statuses

- **Pending:** the key isn't on the account yet. Commits are authored as the
  signer, unsigned.
- **Active:** commits are signed.
- **Broken:** commits would not verify, so they're authored as the signer and
  unsigned until a Verify passes. The reason shown is one of:
  - the key is no longer on the account: add it back as a signing key;
  - the commit email belongs to no account: add and verify it, or change the
    signer's email;
  - the commit email isn't verified: verify it on the account.

  Fix it on GitHub, then choose **Verify**.

While your own signer is pending or broken, commits are unsigned; they never
fall back to the deployment's signer.

## Changing the signer

- **Change** sets a new commit email. Add and verify it on the account first:
  a wrong address only shows up when a pushed commit fails to verify.
- **Rotate key** generates a new key. The current key keeps signing until
  Verify finds the new one on the account. Leave the old public key on the
  account unless it may have leaked: on a GitHub Enterprise Server with
  persistent commit verification off, deleting it turns the commits it signed
  Unverified.
- **Remove** deletes the signer and its keys from Triage Factory. Commits go
  back to how they were made before, or to the deployment's signer if it has
  one. Delete the public key on GitHub yourself.
