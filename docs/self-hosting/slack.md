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
   - **Socket Mode** — events arrive over an outbound websocket; no public
     inbound URL needed. Simplest for dev or any deployment that can't expose a
     public HTTPS endpoint — but the socket runs as a single lease-held worker,
     so every workspace's events funnel through one pod.
   - **Events API** — Slack POSTs events to
     `https://<your-host>/api/webhooks/slack/<org-id>` (needs a public HTTPS URL
     Slack can reach). Delivery is stateless HTTP that any control pod behind the
     load balancer serves, so it scales horizontally instead of pinning to one
     pod — the production choice, and Slack's own recommendation.
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


## Auditing workspace connects

Connecting or disconnecting a workspace is an org-credential change, so it lands
in `access_change_log` and shows up under **Credential** in the Usage page's
"Access & credential changes" band (org admin + the `governance` entitlement):

- **Connect** → one `credential_set` row of kind `slack_workspace`, naming the
  workspace and carrying its `workspace_id` / `api_app_id`. A re-connect records
  again — the bot token has no keep-current path, so every successful connect is
  a genuine bind or rotation.
- **Disconnect** → one `credential_removed` row of the same kind.

A connect binds up to three secrets (bot token, signing secret, app-level token),
but they are acquired and swept as a unit, so they record as **one** row for the
workspace rather than three. The row is written in the same transaction as the
credential itself, so the log can't drift from what's actually stored.
