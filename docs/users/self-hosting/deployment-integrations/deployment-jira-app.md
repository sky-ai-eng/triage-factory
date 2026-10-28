# Deployment Jira app

Members connect their own Jira Cloud accounts through an Atlassian OAuth (3LO)
app. By default each workspace registers one under **Workspace Settings →
Atlassian OAuth app**. A **deployment Jira app** is one app you register once,
so workspaces without their own get one-click **Connect Jira**.

- **It is a default, not an override.** A workspace's own app always wins for
  new connections.
- **Leaving both variables unset changes nothing.** Workspaces keep bringing
  their own app.
- **Multi mode and Jira Cloud only.** Local mode ignores these variables.

## 1. Register the app

In the [Atlassian developer console](https://developer.atlassian.com/console/myapps/),
create an OAuth 2.0 (3LO) integration:

- **Permissions:** the Jira API scopes `read:jira-work`, `write:jira-work` and
  `read:jira-user`, and the User identity API scope `read:me`. Triage Factory
  also requests `offline_access`.
- **Authorization → Callback URL:**
  `<TF_PUBLIC_URL>/api/orgs/{org}/jira/connect/callback`, where `{org}` is the
  workspace's id. A workspace's **Atlassian OAuth app** card in Workspace
  Settings shows its exact URL.

Copy the app's **Client ID** and **Secret**.

## 2. Configure `control` and restart

```dotenv
# .env
TF_ATLASSIAN_CLIENT_ID=<client ID>
TF_ATLASSIAN_CLIENT_SECRET=<secret>
```

The secret can be a mounted file via `TF_ATLASSIAN_CLIENT_SECRET_FILE`
([Deployment secrets](../secrets.md)). Only `control` needs these.

```sh
docker compose up -d
```

A workspace with no app of its own now reads _Using this deployment's
Atlassian app_ under **Workspace Settings → Atlassian OAuth app**.

## Changing the app

- **Rotating the secret** of the same app: update `.env` and restart. Existing
  connections keep working.
- **Changing the client ID** to a different app: every member who connected
  through the old app has to press **Connect Jira** again, because a connection
  refreshes only through the app it was made with.
