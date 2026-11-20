# Deployment Slack app

A **deployment Slack app** is one Slack app you register once for the whole
deployment. With it configured, **Organization → Settings → Slack** offers
**Add to Slack**: an organization admin approves on Slack and has nothing to
create or paste. Without it, each organization creates a Slack app of its own
and pastes its credentials there.

- **An organization can still use its own app.** **Use your own Slack app
  instead** sits under Add to Slack. A workspace can hold both connections,
  one active at a time, and the organization switches between them in Settings
  without reinstalling either.
- **Multi mode only.** Local mode ignores these variables.

## Choose a transport

Slack delivers events to an app in one of two ways, and the app is created for
one of them.

|                   | Events API                                         | Socket Mode                       |
| ----------------- | -------------------------------------------------- | --------------------------------- |
| Slack reaches TF  | at `https://<public URL>/api/webhooks/slack`       | never; TF opens the connection    |
| Use when          | Triage Factory is reachable from the internet      | it is not                         |
| Slack Marketplace | accepted                                           | not accepted                      |
| Variable          | `TF_SLACK_SIGNING_SECRET`                          | `TF_SLACK_APP_TOKEN`              |
| Manifest          | [`slack-app-manifest.events-api.json`](slack-app-manifest.events-api.json) | [`slack-app-manifest.socket.json`](slack-app-manifest.socket.json) |

## Requirements

`TF_PUBLIC_URL` must be an `https://` URL, for both transports. The install
returns to `<TF_PUBLIC_URL>/api/slack/install/callback`, and Slack requires
redirect URLs to use HTTPS. With Socket Mode that URL has to be reachable by
the admin's browser, not by Slack.

With the Events API, the control host's clock must be within five minutes of
the real time. Triage Factory refuses a delivery whose timestamp is further
from its own clock, and logs no line for it. With the bundled Prometheus,
[`SlackDeploymentDeliveriesStale`](../monitoring.md#alerts) fires when a
control pod refuses deliveries this way and verifies none.

## 1. Create the app

1. Download the manifest for your transport and replace each
   `https://YOUR-TF-PUBLIC-URL` with your `TF_PUBLIC_URL`, with no trailing
   slash.
2. At [api.slack.com/apps](https://api.slack.com/apps), choose **Create New
   App → From a manifest**, pick the workspace that will own the app, and
   paste the file.
3. For Socket Mode, under **Basic Information → App-Level Tokens**, generate a
   token with the `connections:write` scope.

**Leave token rotation off**, as the manifest does. Triage Factory stores an
organization's bot token once and never refreshes it, and Slack says that with
token rotation "it expires every 12 hours". Slack also says "Token rotation may
not be turned off once it's turned on", so an app created with it can't be
fixed: create a new app, point the variables at it, and delete the old app in
Slack, which otherwise keeps sending its events to Triage Factory. The install
refuses an app with rotation on.

The manifests come from the code and are regenerated whenever the scopes or
events change. After an upgrade, compare your app's manifest under **App
Manifest** with the new file. A new scope reaches a workspace only when its
organization presses **Add to Slack** again.

## 2. One workspace or several

- **One workspace:** create the app in that workspace and leave distribution
  as it is. Slack: "By default, a newly built Slack app can only be installed
  in its associated workspace."
- **More than one workspace:** in the app's settings, open **Manage
  Distribution**, work through Slack's checklist under **Share Your App with
  Other Workspaces**, and press **Activate Public Distribution**.

## 3. Configure `control` and restart

All five values are on the app's **Basic Information** page. The variables
are listed in [`.env.example`](../../../../.env.example).

| Variable                  | Value                                                        | Secret |
| ------------------------- | ------------------------------------------------------------ | ------ |
| `TF_SLACK_APP_ID`         | **App ID**. It starts with `A`; the client ID is not it.     | no     |
| `TF_SLACK_CLIENT_ID`      | **Client ID**                                                | no     |
| `TF_SLACK_CLIENT_SECRET`  | **Client Secret**                                            | yes    |
| `TF_SLACK_SIGNING_SECRET` | **Signing Secret**, for the Events API                       | yes    |
| `TF_SLACK_APP_TOKEN`      | the app-level token from step 1, for Socket Mode (`xapp-…`)  | yes    |

```dotenv
# .env
TF_SLACK_APP_ID=A0123456789
TF_SLACK_CLIENT_ID=<client ID>
TF_SLACK_CLIENT_SECRET=<client secret>
TF_SLACK_SIGNING_SECRET=<signing secret>   # Events API
# TF_SLACK_APP_TOKEN=<xapp-…>              # Socket Mode instead
```

Set the first three and **exactly one** of the signing secret and the app
token; which one you set is the transport. A Socket Mode app shows a Signing
Secret too: leave `TF_SLACK_SIGNING_SECRET` unset for it, or Triage Factory
waits for Events API deliveries that never come. Any other partial set is
logged at boot and the deployment runs without a Slack app. Each secret can be
a mounted file via `<NAME>_FILE` ([Deployment secrets](../secrets.md)). Only
`control` needs these.

```sh
docker compose up -d
```

**Events API:** Slack checked the request URL when you created the app, before
Triage Factory had the signing secret, so that check failed. Now that it has
restarted, open **Event Subscriptions** in the app's settings and have Slack
verify the request URL again. If it fails again, see **Wrong signing secret**
under [Troubleshooting](#troubleshooting).

## 4. Check it took

1. `docker compose logs control | grep -i "deployment slack app"` prints
   nothing. A line there names the problem.
2. With Socket Mode, `docker compose logs control | grep "slack socket mode"`
   shows `slack socket mode: starting connection` for your App ID, and no line
   saying Slack rejected the token.
3. In an organization with the Slack feature, **Organization → Settings →
   Slack** offers **Add to Slack**.

## Rate limits

Slack has reduced the limits of `conversations.history` and
`conversations.replies` for some apps. Its
[changelog](https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps)
says:

- The reduction "will apply only to applications that are commercially
  distributed outside of the Marketplace", which are limited to "1 request per
  minute" and "a maximum of 15 objects per request".
- "Internal customer-built applications are not impacted by these changes",
  and "This change will not impact Marketplace apps."
- It does not define "commercially distributed", and says nothing about a
  company's own app distributed only to that company's workspaces. If that is
  your deployment, confirm with Slack.

Triage Factory reads a thread's history with those two methods, so an app
under the reduced limits answers slowly in long threads. With the bundled
Prometheus, `SlackDeploymentAppThrottled` fires when Slack refuses the
deployment app's calls to them.

## Slack's terms for a commercial service

Slack's [API Terms of Service](https://slack.com/terms-of-service/api) name the
Slack Marketplace as the channel for distributing an app commercially, and
count among such apps ones published outside it and templated custom apps that
connect to your product. If you run Triage Factory as a commercial service for
other companies, read those terms before choosing between a distributed
deployment app and each customer connecting its own.

## How organizations use it

- **One organization per workspace.** Slack gives an app one install per
  workspace, so the deployment app connects a workspace to one organization:
  the first to install it there. A second organization gets an error and
  connects its own app instead. To release a workspace, a Slack admin removes
  the app from it, and Triage Factory drops that connection.
- **Enterprise Grid:** install into each workspace. Organization-wide installs
  are refused.
- **A workspace that requires admin approval for apps:** Slack holds the
  install for a Slack admin. Once they approve the app, the organization admin
  presses **Add to Slack** again and the install completes.

## Troubleshooting

With the bundled Prometheus, the deployment app's problems also raise
[alerts](../monitoring.md#alerts): `SlackDeploymentAppUnusable` for a
malformed configuration or a public URL that is not HTTPS,
`SlackDeploymentSocketDown` for a Socket Mode connection that stays down,
`SlackDeploymentAppInstallRefused` for an install refused for the app's own
settings, `SlackSigningSecretMismatch` for a wrong signing secret, and
`SlackDeploymentDeliveriesStale` for a control clock more than five minutes
off.

Each of these is a line in `docker compose logs control`, except where noted.

- **Malformed configuration** — at boot: `read the deployment Slack app from
  the environment failed; the deployment runs without one`, with an error
  naming each problem (a variable not set, both or neither of the signing
  secret and the app token, an App ID that doesn't start with `A`, an app
  token that doesn't start with `xapp-`).
- **App token rejected** — `slack socket mode: Slack rejected the deployment
  Slack app's app-level token`. Every organization's connection through the
  app reads **Auth failed** in Settings. Generate a new app-level token with
  `connections:write` and set `TF_SLACK_APP_TOKEN` to it.
- **Client ID and secret from a different app** — `slack install: Slack
  installed a different app than the deployment's`. `TF_SLACK_APP_ID` and the
  client credentials name different apps; take all of them from one app's
  Basic Information. A client ID and secret that don't match each other fail
  earlier, with `slack install: code exchange failed`.
- **Missing scopes** — `slack install: the install lacks a bot token with
  every scope the deployment app needs`, naming them. The app was created from
  an older manifest; update its manifest from the current file.
- **Token rotation on** — `slack install: the deployment's Slack app has token
  rotation on`. Create a new app from the manifest, which leaves rotation off,
  set the variables to it, and delete the old app.
- **Public URL not HTTPS** — the first time an organization's Settings asks
  for the app: `the deployment Slack app is configured but the deployment's
  public URL is not HTTPS, so no org can install it`. Set `TF_PUBLIC_URL` to
  an `https://` URL.
- **Add to Slack ends on a Slack error page about the redirect URI** —
  nothing is logged, because Slack never sends the admin back. The app's
  redirect URL must be exactly `<TF_PUBLIC_URL>/api/slack/install/callback`:
  check `redirect_urls` under **App Manifest**, and that the placeholder was
  replaced.
- **Wrong signing secret** (Events API) — Triage Factory answers each such
  delivery 401 and counts it in `tf_slack_webhook_signature_mismatches_total`,
  under `names_app="deployment"`, or `"none"` for Slack's request URL check.
  Each control pod reads its own `TF_SLACK_SIGNING_SECRET` and counts its own,
  and the alert names the pod. That count rising on a pod with nothing under
  `signature="verified"` in `tf_slack_webhook_deployment_deliveries_total` on
  the same pod means that pod's secret is wrong, even while other pods verify;
  a forged request also counts, so a few and then nothing is not this. The pod
  also logs at most one warning a minute: `slack: a delivery to the deployment
  Slack app's Events API endpoint is not signed with TF_SLACK_SIGNING_SECRET`,
  with `mismatches` counting the deliveries since its last one. Set
  `TF_SLACK_SIGNING_SECRET` on that pod to the **Signing Secret** on the app's
  Basic Information page and restart it.
- **Another app sends to the endpoint** (Events API) — counted under
  `names_app="other"`, and logged as `slack: a delivery naming another Slack
  app reached the deployment Slack app's Events API endpoint`, with that app's
  id as `delivery_app_id`. That app's request URL is
  `<TF_PUBLIC_URL>/api/webhooks/slack`, usually an app you replaced: change
  its request URL or delete it.
- **Control's clock is off** (Events API) — not logged. Triage Factory answers
  each delivery 401 and counts it in
  `tf_slack_webhook_deployment_deliveries_total` under `signature="stale"`.
  Each control pod counts its own, and the alert names the pod. That count
  rising on a pod with nothing under `signature="verified"` on the same pod
  means that pod's host clock is more than five minutes from the real time; a
  replayed or forged request also counts, so a few and then nothing is not
  this. Fix the host's time sync; no restart is needed.

Slack expects an answer to each delivery "within three seconds". When more
than 95% of deliveries fail within 60 minutes, it turns off the app's event
subscriptions and emails the app's owner. Fix the cause, then re-enable them
in the app's settings on Slack.
