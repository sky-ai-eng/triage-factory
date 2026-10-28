# Deployment Linear app

A workspace installs Triage Factory in its Linear workspace through a Linear
OAuth app. By default each workspace registers its own under **Workspace
Settings → Linear OAuth app**. A **deployment Linear app** is one app you
register once: a workspace with no app of its own clicks **Install Triage
Factory**, approves on Linear, and has nothing to create or paste.

- **It is a default, not an override.** A workspace that registered its own
  app keeps using it.
- **Leaving both variables unset changes nothing.** Workspaces keep
  registering their own app or pasting an API key.
- **Multi mode only.** Local mode ignores these variables.

## 1. Create the app in Linear

Linear recommends owning the app from a dedicated Linear workspace, so every
admin of that workspace can manage it. Signed in there as an admin, open this
link with `https://tfac.example.com` replaced by your `TF_PUBLIC_URL` (three
places) and `Example%20Corp` by the developer name workspace admins should see
(2–80 characters, spaces as `%20`):

```text
https://linear.app/settings/api/applications/new?distribution=public&developer.name=Example%20Corp&oauth.client_name=Triage%20Factory&oauth.client_uri=https://tfac.example.com&oauth.redirect_uris=https://tfac.example.com/api/linear/install/callback&oauth.redirect_uris=https://tfac.example.com/api/linear/connect/callback&oauth.grant_types=authorization_code
```

Check the pre-filled form against this before creating the app:

| Parameter             | Value                                                                                                                                                                          |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `distribution`        | `public`, so any Linear workspace can authorize it.                                                                                                                            |
| `developer.name`      | Your name, as the app's developer.                                                                                                                                             |
| `oauth.client_name`   | `Triage Factory`. Shown on Linear's install page; it is also the name of the app user Triage Factory acts as. Linear refuses names containing "Linear".                        |
| `oauth.client_uri`    | Your `TF_PUBLIC_URL`.                                                                                                                                                          |
| `oauth.redirect_uris` | `<TF_PUBLIC_URL>/api/linear/install/callback` and `<TF_PUBLIC_URL>/api/linear/connect/callback`. Linear requires `https` (it also accepts `http://localhost` for development). |
| `oauth.grant_types`   | `authorization_code` only.                                                                                                                                                     |

Leave webhooks off; Triage Factory polls Linear. Create the app, then copy its
**client ID** and **client secret** from the app's page.

## 2. Configure `control` and restart

```dotenv
# .env
TF_LINEAR_CLIENT_ID=<client ID>
TF_LINEAR_CLIENT_SECRET=<client secret>
```

Set both or neither; one without the other is logged at boot and treated as
neither. The secret can be a mounted file via `TF_LINEAR_CLIENT_SECRET_FILE`
([Deployment secrets](../secrets.md)). Only `control` needs these.

```sh
docker compose up -d
```

## 3. Check it took

1. `docker compose logs control | grep -i "deployment linear app"` prints
   nothing. A line there names the missing variable.
2. In a workspace with no Linear app of its own, **Workspace Settings → Linear
   OAuth app** reads _Using this deployment's Triage Factory app_, and **Linear
   connection** offers **Install Triage Factory**.

## Rotating the client secret

In Linear, open the app under **Settings → Administration → API** and rotate
its secret, then update `.env` and run `docker compose up -d` soon after.
Issued tokens keep working and refresh with the new secret; no workspace has to
install again.

## Removing the app

Connections made through the deployment app refresh only with its secret. If
you unset the pair, those workspaces' Linear connections keep working until
their access tokens expire (within a day), then fail until each workspace
reconnects through its own app or a personal API key. To change the secret,
rotate it instead.
