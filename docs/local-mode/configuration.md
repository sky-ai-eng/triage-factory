# Configuration (local mode)

Settings are stored in the SQLite DB at `~/.triagefactory/triagefactory.db`
(table `settings`, single row holding a YAML blob) and edited exclusively via the
Settings page. The poll interval defaults to 5 minutes for both GitHub and Jira;
configurable values are 30s, 1m, 2m, 5m. Maintain this configuration through the
UI, not direct DB access.

## Jira setup

Jira uses a two-stage flow in Settings:

1. Enter your Jira URL and Personal Access Token, click **Connect**. Credentials
   are validated and stored immediately.
2. The card expands to reveal project selection, poll interval, and status
   configuration. Statuses are fetched automatically from your Jira instance.
3. **Save** is disabled until you've configured projects, pickup statuses, and an
   in-progress status.

## Credentials

All credentials (GitHub PAT, Jira PAT, the Anthropic key, GitHub App private
keys) are stored outside the database — in the OS keychain on desktop, or an
encrypted file on headless installs. Token fields in Settings show "leave blank
to keep current" when a token is already stored.

Where exactly they land, and how the headless encrypted-file backend works, is
covered in [Secret storage](secret-storage.md).
