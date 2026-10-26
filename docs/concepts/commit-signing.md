# Commit signing: creating the signer account

An org can have the commits its agents make signed, so GitHub marks them
**Verified** and repositories that require signed commits accept them. This page
is for the org admin who sets it up: what the signer account is, how to create
it, and what to do in **Settings → Commit signing** once it exists.

<!-- TODO(TFAC-1198): multi-mode runs sign once the sidecar relays
signatures; this paragraph goes with the section's local-only gate in
OrgSettings.tsx. -->
The section is in local mode's Settings. Runs in a multi-mode deployment are
not wired to sign yet, so a multi-mode deployment does not show it.

## What a signer is

The **signer** is a GitHub user account registered with your Triage Factory
org. Once it is active, every commit an agent makes in the org is authored and
committed as that account and signed with an SSH key Triage Factory generates
for it. GitHub shows those commits under the account's login and avatar.

The account has to be a **user** account. Without a signer, an org's commits
are authored as its GitHub App's bot (or as the owner of its personal access
token), and a GitHub App's bot cannot hold a signing key: GitHub verifies a
bot's commit only when GitHub itself made it. GitHub verifies a commit that
Triage Factory makes and pushes when two things hold:

- the key that signed it is registered as a **signing key** on a user account;
- the commit's **committer email** is a verified address on that same account.

The App, or the token, still pushes the commits and opens the pull requests.
The signer account only lends its identity and its signing key. It never
pushes, reads a repository, or acts on GitHub.

## Use a dedicated account

Create an account for this purpose rather than registering a person's account.
Every agent commit carries the account's name, so a person's account would
claim authorship of work they did not do. A dedicated account also limits what
a leaked key can do: the key signs as that account and no other. In local mode
with the sandbox off, an agent runs as you and can read the key from your
keychain, the same as your GitHub token, so the account it signs as should be
one that exists only for this.

## GitHub's terms for machine accounts

An account that performs automated tasks is a **machine account** under
GitHub's Terms of Service. The terms require that:

- a person creates the account, accepts the terms on its behalf, and is
  responsible for what it does;
- the account has a valid email address that someone can receive mail at;
- a person holds at most one free machine account, in addition to their own
  free personal account.

So the account belongs to someone in your organization, who should be the
person who can recover it.

## Creating the account

1. **Sign up** at GitHub with an email address you control. A shared mailbox
   or an alias that reaches your team works well, because it is the address
   that recovers the account.
2. **Turn on two-factor authentication** (Settings → Password and
   authentication). The account's key signs commits that land in your
   repositories, so protect the account like any other with that reach.
3. **Keep the email private** (Settings → Emails → Keep my email addresses
   private). On github.com Triage Factory commits under the account's noreply
   address by default, so the recovery address never needs to appear on a
   commit.
4. **Set the profile name** (Settings → Public profile → Name). This is the
   name commits carry in git, in `git log` and in blame, while GitHub shows the
   login. An account named `acme-agent` with the profile name "Acme Agent"
   commits as "Acme Agent". Set it before registering the account: Triage
   Factory reads it then. Without a profile name, commits carry the login.

The account needs no membership in your GitHub organization and no seat in it.
It never accesses a repository; GitHub checks only that the signing key and the
committer email belong to it.

### Choosing the commit email

On **github.com**, Triage Factory commits under the account's noreply address,
`<id>+<login>@users.noreply.github.com`, unless you give it another. That
address needs no mailbox and nothing to verify, and commits under it keep
verifying if the account's login is renamed. Most orgs need nothing else.

You can give the signer a real address instead, for example when a ruleset
requires commit emails on your company's domain, or because a real address
reads better in `git log`. The address has to be **added and verified on the
account** before commits under it verify. Every commit publishes it, so use an
address other than the one that recovers the account: add a second address to
the account, verify it, and give that one to Triage Factory.

On **any other host** (GitHub Enterprise Server, or GHE.com) a real address is
required. GitHub Enterprise Server has no noreply address, and GHE.com's is not
documented. Registration asks for one there.

### Managed user accounts

Every GHE.com account is a managed user, and so is every account on github.com
in an enterprise with managed users (its login ends in `_<shortcode>`). Your
identity provider provisions a managed user, so create the signer through it
like any other account. A managed user's email can start **unverified**, and
GitHub verifies no commit under an unverified address, so verify the account's
email before you register it.

## Registering the signer

In **Settings → Commit signing**, an org admin chooses **Set up signing** and
enters the account's login, plus the commit email on a host other than
github.com. Triage Factory checks that the login is a user account on your
org's GitHub, generates a key pair, and shows the public key. The private key
stays in Triage Factory and is never shown.

Then, signed in to GitHub as the signer account:

1. Open **Settings → SSH and GPG keys → New SSH key**.
2. Give the key the title Triage Factory suggests, "Triage Factory commit
   signing". Key titles are public.
3. Set **Key type** to **Signing key**, paste the public key, and add it.

Back in Triage Factory, choose **Verify**. Verify looks for the key in the
account's public list of signing keys. GitHub can take up to a minute to list a
key just added, so if Verify does not find it at once, wait and try again. When
it finds the key, the signer is **active** and agents' commits are signed from
then on.

## Statuses

- **Pending**: the signer is registered and its key has not been found on the
  account yet. Commits are authored as the signer and are not signed.
- **Active**: commits are signed.
- **Broken**: a check found that the signer's commits would not verify.
  Commits are still authored as the signer, and are not signed until a Verify
  passes. The section says why:
  - the key is no longer on the account, or a pushed commit's key was not
    found there: add the public key back as a signing key;
  - the commit email belongs to no account: add and verify it on the account,
    or change the signer's commit email;
  - the commit email is on the account but not verified: verify it there.

  Fix the cause on GitHub, then choose **Verify**.

## Changing the signer

- **Commit email.** **Change** sets a new address. Add and verify it on the
  account first: Triage Factory cannot read another account's addresses, so a
  wrong one shows up only when a pushed commit fails to verify.
- **Rotate key.** Generates a new key pair. The current key keeps signing
  until Verify finds the new key on the account; then the new key replaces it
  and Triage Factory destroys the old private key. Leave the old public key on
  the account unless it may have leaked. It can sign nothing now, and on a
  GitHub Enterprise Server with persistent commit verification turned off,
  deleting it turns the commits it signed Unverified. On github.com a commit
  that verified stays verified when its key is deleted.
- **Remove.** Deletes the signer and its keys from Triage Factory, and runs go
  back to committing as they did before. The public key stays on the account
  until you delete it there.

The design behind all of this is in
[`docs/for-agents/specs/commit-signing/`](../for-agents/specs/commit-signing/README.md).
