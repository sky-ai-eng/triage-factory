# Slack app setup (operators)

Slack support is an Enterprise, multi-mode-only feature. It's configured **per
org**, not per deployment: an org admin connects one or more workspaces from
**Settings → Slack** in the app. There's no deployment-wide `.env` knob for
Slack — the license (`features: [... slack ...]`, see [install.md](install.md))
unlocks the surface, and each org brings its own Slack app and credentials. For
what the bot then listens to, see
[concepts/tracked-events.md](../concepts/tracked-events.md#slack-events).

## The manifest flow

Slack has no one-click install for a custom app, so the connect UX is
copy-paste. In **Settings → Slack**:

1. Copy the generated app **manifest**. Two variants are offered, one per
   transport:
   - **Socket Mode** — events arrive over an outbound websocket; no inbound URL
     needed. Simplest when the deployment isn't reachable at a public HTTPS URL.
   - **Events API** — events are delivered to
     `https://<your-host>/api/webhooks/slack/<org-id>`. Requires a public HTTPS
     URL Slack can reach (the deployment's external base URL must be set).
2. Create the app at <https://api.slack.com/apps> → **Create an app from a
   manifest**, pasting the copied manifest.
3. Install the app to the workspace.
4. Paste the resulting credentials back into **Settings → Slack**.

The manifest requests the full bot scope set up front — over-asking now avoids a
reinstall-to-rescope later, since Slack scope grants are all-or-nothing per
install — and subscribes the bot to these events:

- `app_mention` — explicit @-mentions of the bot.
- `message.channels` / `message.groups` — messages in public / private channels,
  which back **engaged-thread follow-ups**: replies in a thread the bot already
  owns, with no re-@-mention required.

## Migrating an existing app

**Engaged-thread follow-ups (`message.channels` / `message.groups`) were added
to the manifest after the original one shipped only `app_mention`.** An app
created from that earlier manifest is still subscribed to `app_mention` alone,
so un-mentioned thread replies never reach Triage Factory — every follow-up
silently requires another @-mention, the exact gap engaged threads close.

Slack does not apply a manifest edit in place for you. To pick up the new event
subscriptions on an already-connected app, **update the app from the current
manifest and reinstall it**:

1. In **Settings → Slack**, copy the current manifest (Socket Mode or Events
   API, matching your transport).
2. At <https://api.slack.com/apps>, open the app's **App Manifest** page and
   replace its contents with the copied manifest (or create a fresh app from it).
3. Reinstall the app to the workspace so the new event subscriptions take
   effect.
4. If you created a fresh app, paste the new credentials back into **Settings →
   Slack**.

The history scopes the follow-up events need (`channels:history` /
`groups:history`) were already in the original scope set, so this adds **event
subscriptions**, not new permissions. But Slack only delivers events an
installed app is subscribed to, so the reinstall is required for follow-ups to
flow.

> Slack is multi-mode-only and has not shipped in any release, so this migration
> only affects preview / dogfooding installs — there are no
> backwards-compatibility concerns for released deployments.
