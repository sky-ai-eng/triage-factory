# Deployment GitHub App

Every Triage Factory organization reaches GitHub through a GitHub App. By
default each one registers or imports its own from **Organization → Settings →
GitHub access**. A **deployment GitHub App** is one App you register once for
the whole deployment: an org admin presses **Connect GitHub…**, names a GitHub
account, confirms on GitHub, and that account's installation is bound to their
organization.

- **It is a default, not an override.** An organization with its own App
  keeps using it. Connect refuses an organization that already holds a
  credential and names the one to disconnect first.
- **Leaving the variables unset changes nothing.** Every organization
  keeps bringing its own App.
- **Multi mode only.** Local mode ignores these variables.
- **Tenancy is unchanged.** An installation belongs to at most one
  organization, and each organization sees only the accounts it connected.

## 1. Decide which GitHub it lives on

The App is registered on the deployment's default GitHub, which is github.com
unless `TF_DEFAULT_GITHUB_HOST` names a GitHub Enterprise Server (set before
the first organization was created; see [install](../install.md#3-render-the-deployment)).
An organization whose own GitHub base URL names a different host can't use the
deployment App and brings its own.

## 2. Create the App

Open **Settings → Developer settings → GitHub Apps → New GitHub App** on the
account that should own it. A GitHub organization you control survives any
one person leaving: `https://github.com/organizations/<org>/settings/apps/new`
(or the same path on your GHES host). Anything not listed keeps its default.

| Field                                                      | Value                                                                                                                                                |
| ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| **GitHub App name**                                        | Unique on that GitHub. Org admins see it on GitHub's install page.                                                                                   |
| **Homepage URL**                                           | Your `TF_PUBLIC_URL`.                                                                                                                                |
| **Redirect URI**                                           | `<TF_PUBLIC_URL>/api/github/managed/callback`, absolute. It must stay the first entry.                                                               |
| **Request user authorization (OAuth) during installation** | **Checked.** This is how TF learns who completed an installation. It greys out **Setup URL**; leave that blank and **Redirect on update** unchecked. |
| **Enable Device Flow**                                     | Unchecked.                                                                                                                                           |
| **Webhook → Active**                                       | Checked.                                                                                                                                             |
| **Webhook URL**                                            | `<TF_PUBLIC_URL>/api/webhooks/github`                                                                                                                |
| **Webhook secret**                                         | Generate with `openssl rand -hex 32`, paste it, and keep it. GitHub never shows it again.                                                            |
| **SSL verification**                                       | Enabled.                                                                                                                                             |
| **Where can this GitHub App be installed?**                | **Any account.**                                                                                                                                     |

**Permissions.** Everything not listed stays _No access_.

| Repository permission | Access                    |
| --------------------- | ------------------------- |
| Actions               | Read-only                 |
| Checks                | Read-only                 |
| Commit statuses       | Read-only                 |
| Contents              | Read and write            |
| Issues                | Read and write            |
| Metadata              | Read-only (set by GitHub) |
| Pull requests         | Read and write            |

| Organization permission | Access                                                                                                                                                                                          |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Members**             | **Read-only — required.** TF checks that the person connecting an organization is an owner of it. Without it, connecting an organization fails, and any repository admin could install the App. |

| Account permission  | Access                                                                                                                  |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| **Email addresses** | **Read-only — required.** Members link their GitHub accounts through the App, which reads their verified primary email. |

If you change permissions later, each account that installed the App must
accept the new set on GitHub before it takes effect.

**Subscribe to events:** Check run, Check suite, Issue comment, Pull request,
Pull request review, Pull request review comment, Push.

Press **Create GitHub App**.

## 3. Collect the four values

From the App's **General** page:

| Value                                                                               | Variable                       |
| ----------------------------------------------------------------------------------- | ------------------------------ |
| **App ID** (the number under _About_, not the Client ID)                            | `TF_GITHUB_APP_ID`             |
| **Client secrets → Generate a new client secret** (shown once)                      | `TF_GITHUB_APP_CLIENT_SECRET`  |
| **Private keys → Generate a private key** (downloads a `.pem`; keep the whole file) | `TF_GITHUB_APP_PRIVATE_KEY`    |
| The webhook secret from step 2                                                      | `TF_GITHUB_APP_WEBHOOK_SECRET` |

## 4. Configure `control` and restart

Set all four. If any is missing or malformed, the deployment runs as if none
were set. Mount the PEM as a file, since a multi-line value doesn't survive a
`.env` well ([Deployment secrets](../secrets.md)):

```yaml
# compose.override.yml  (merged automatically by docker compose)
secrets:
  tf_gh_app_key: { file: ./secrets/github-app.private-key.pem }
services:
  control:
    secrets: [tf_gh_app_key]
```

```dotenv
# .env
TF_GITHUB_APP_ID=123456
TF_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/tf_gh_app_key
TF_GITHUB_APP_WEBHOOK_SECRET=<the webhook secret>
TF_GITHUB_APP_CLIENT_SECRET=<the client secret>
```

```sh
docker compose up -d
```

Keep the PEM `chmod 600` (see [Deployment secrets](../secrets.md) for ownership). In a
source checkout, `secrets/` and `compose.override.yml` are gitignored. Only
`control` needs these variables; executors never hold the App key.

## 5. Check it took

1. `docker compose logs control | grep -i "deployment github app"` prints
   nothing. A line there names the variable that is wrong.
2. On GitHub, open the App → **Advanced → Recent Deliveries**. The first
   `ping` failed with `401` because TF didn't have the secret yet. Press
   **Redeliver**; it should return `204`, which confirms the URL, the secret
   and TLS. A `302` means a proxy in front of TF (Cloudflare Access, IAP,
   oauth2-proxy) answered instead: exempt `POST <TF_PUBLIC_URL>/api/webhooks/github`
   and `/api/webhooks/github/{org_id}` from it. TF verifies every delivery's
   signature itself. Without webhooks everything still works; changes arrive
   on the next poll instead.
3. In an organization with no GitHub credential, **Organization → Settings →
   GitHub access** offers **Connect GitHub…**.

From there, connecting accounts, linking members' GitHub accounts and switching
an organization onto the deployment App are done in the app.

## Optional: sign people in through the App

By default, people sign in with the GitHub OAuth app from
[install step 2](../install.md#2-create-the-github-oauth-app-sign-in), and
whoever connects an organization approves Triage Factory on GitHub twice: once
for the OAuth app at sign-in, and again for the App at **Connect GitHub…**. The
App can handle sign-in instead. The approval at sign-in is then the only one,
and **Connect GitHub…** doesn't ask again.

1. On the App's settings page, under **Identifying and authorizing users**,
   press **Add redirect URI** and add `<TF_PUBLIC_URL>/auth/v1/callback`.
   Keep `/api/github/managed/callback` first: GitHub sends people to the first
   entry when they finish installing the App.
2. Check that the App can be installed on **Any account**. GitHub refuses a
   private App's sign-in to anyone outside the account that owns it.
3. Set `GH_CLIENT_ID` to the App's **Client ID** (on its **General** page) and
   `GH_CLIENT_SECRET` to the same client secret as
   `TF_GITHUB_APP_CLIENT_SECRET`. On a new deployment, pass them to
   `deploy init` as `--github-client-id` and `--github-client-secret-file`. On
   a running one, set them in `.env` and run `docker compose up -d`.

People keep their accounts when you switch, because sign-in matches them by
their GitHub account. Anyone who hasn't approved the App yet sees GitHub's
approval page for it at their next sign-in.

When you rotate the App's client secret, update `GH_CLIENT_SECRET` and
`TF_GITHUB_APP_CLIENT_SECRET` together.

## Troubleshooting

| Message                                                                             | Fix                                                                                                           |
| ----------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `This deployment isn't set up to connect GitHub accounts.`                          | One of the four variables is missing or malformed on `control`; see step 5.1.                                 |
| `…missing the “Request user authorization (OAuth) during installation” setting.`    | Check that box on the App.                                                                                    |
| `…not granted the organization members permission`                                  | Add Organization → Members: Read-only, then have each installed account accept the new permissions on GitHub. |
| `…not granted the account email addresses permission` (log)                         | Add Account → Email addresses: Read-only. Members approve it the next time they link.                         |
| `This organization is pointed at <host>; the deployment's GitHub App is on <host>.` | That organization's GitHub isn't the deployment default. It brings its own App.                               |
| `GitHub rejected the deployment App's ID and private key` (log)                     | `TF_GITHUB_APP_ID` names a different App than the PEM. Use the App ID, not the Client ID.                     |
| Sign-in ends on GitHub's 404 page, `This is not the web page you are looking for.`  | Sign-in runs through a private App. On the App's **Advanced** page, press **Make public**.                    |
