#!/usr/bin/env bash
# Wipe all local state: database, config, and keychain credentials. Use
# this to test the first-run experience from scratch.
#
# Bare repo clones under ~/.triagefactory/repos/ are deliberately
# preserved — they're expensive to re-fetch and aren't part of the
# first-run flow this script targets. Wipe them manually if you need
# to.

set -euo pipefail

echo "Cleaning Triage Factory local state..."

# Keychain keys to sweep at the end. The static list mirrors
# integrations.AllLocalSweepKeys() in internal/integrations/creds.go (the
# canonical set `triagefactory uninstall` sweeps via auth.SweepKeychain) —
# integration creds, the legacy github_username / jira_display_name keys, and the
# org-level anthropic_api_key + jira_oauth_client_secret. Drift between the two
# leaves stale entries after clean-slate (TFAC-405 was the most recent miss).
# Per-GitHub-App keys (github_app_<id>_*) are dynamic, so we enumerate App ids
# from the DB below — this MUST run before the DB is removed.
keychain_keys=(
  github_url github_pat github_username
  jira_url jira_pat jira_email jira_api_token jira_auth_method jira_display_name
  anthropic_api_key jira_oauth_client_secret
  aws_access_key_id aws_secret_access_key aws_session_token aws_region
  aws_bearer_token_bedrock bedrock_model_id bedrock_base_url
)

# Enumerate per-App keys from org_github_apps before the DB is deleted. Needs
# sqlite3 (not always present in dev) and the DB file; best-effort otherwise.
db=~/.triagefactory/triagefactory.db
if command -v sqlite3 >/dev/null 2>&1 && [ -f "$db" ]; then
  while IFS= read -r app_id; do
    [ -n "$app_id" ] || continue
    keychain_keys+=("github_app_${app_id}_pem" "github_app_${app_id}_client_secret" "github_app_${app_id}_webhook_secret")
  done < <(sqlite3 "$db" 'SELECT app_id FROM org_github_apps;' 2>/dev/null || true)
elif [ -f "$db" ]; then
  echo "  note: sqlite3 not found; skipping GitHub App keychain enumeration (github_app_<id>_* may linger)"
fi

# Database
rm -f ~/.triagefactory/triagefactory.db ~/.triagefactory/triagefactory.db-wal ~/.triagefactory/triagefactory.db-shm
echo "  removed database"

# Encrypted secret bag — the headless (no-keychain) secret backend's store.
# Desktop installs keep secrets in the OS keychain (swept below); headless
# installs keep them here. Harmless to remove either way.
rm -f ~/.triagefactory/secrets.enc
echo "  removed encrypted secret bag (if present)"

# Config (settings now live in the DB above; this only removes a stale
# pre-DB config.yaml left behind by ancient installs).
if [ -f ~/.triagefactory/config.yaml ]; then
  rm -f ~/.triagefactory/config.yaml
  echo "  removed legacy config.yaml"
fi

# Project knowledge dirs — a project's per-project repo worktrees live at
# ~/.triagefactory/projects/<id>/repos/<owner>/<repo>/ (and knowledge/summary
# files alongside). Project rows just got wiped with the database, so the
# disk state is orphaned. Worse: each repo subdir is a registered worktree of
# the bare clone in ~/.triagefactory/repos/, holding its branch as "checked
# out." A subsequent run that tries to `git fetch` that branch (e.g. the
# delegate path's `workspace add`) fails with "refusing to fetch into
# branch ... checked out at <stale path>" until the registrations
# get pruned. Wiping projects/ now and re-pruning each bare's
# worktrees/ tracker below closes that loop.
if [ -d ~/.triagefactory/projects ]; then
  # A pre-existing Claude Code session that once ran with cwd =
  # ~/.triagefactory/projects/<id>/ leaves behind
  # ~/.claude/projects/<encoded(<cwd>)>/<sessionID>.jsonl. Walk each
  # project ID dir and delete its encoded session entry BEFORE removing
  # the projects tree (mirrors removeClaudeProjectSessions in
  # cmd/uninstall/uninstall.go — keep the two in sync).
  for dir in ~/.triagefactory/projects/*; do
    [ -d "$dir" ] || continue
    resolved=$(cd "$dir" && pwd -P) || continue
    encoded=$(printf '%s' "$resolved" | tr '/.' '-')
    rm -rf ~/.claude/projects/"$encoded"
  done
  rm -rf ~/.triagefactory/projects
  echo "  removed projects dir and any stale Claude Code session JSONLs"
fi

# Prune stale worktree registrations from every preserved bare. The
# bare clones themselves (~/.triagefactory/repos/) stay — they're
# expensive to refetch and not part of the first-run flow — but their
# internal worktrees/ tracker now points at directories we just
# deleted (projects/, /tmp/triagefactory-runs/). Pruning is
# idempotent and cheap. Without this, the next `git worktree add`
# / `git fetch` against any of these bares would hit the stale-
# registration errors described in the projects-dir comment above.
if [ -d ~/.triagefactory/repos ]; then
  pruned=0
  while IFS= read -r bare; do
    git -C "$bare" worktree prune 2>/dev/null || true
    pruned=$((pruned + 1))
  done < <(find ~/.triagefactory/repos -type d -name '*.git' 2>/dev/null)
  if [ "$pruned" -gt 0 ]; then
    echo "  pruned worktrees from $pruned bare clone(s)"
  fi
fi

# Keychain — sweep the keys assembled at the top of this script (static list
# mirrors integrations.AllLocalSweepKeys(), plus any enumerated github_app_<id>_*
# keys). macOS `security` only; on Linux this loop is a harmless no-op.
for key in "${keychain_keys[@]}"; do
  security delete-generic-password -s triagefactory -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
