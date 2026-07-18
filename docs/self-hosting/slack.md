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

