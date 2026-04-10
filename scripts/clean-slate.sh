#!/usr/bin/env bash
# Wipe all local state: database, config, keychain credentials.
# Use this to test the first-run experience from scratch.

set -euo pipefail

echo "Cleaning Todo Triage local state..."

# Database
rm -f ~/.todotriage/todotriage.db ~/.todotriage/todotriage.db-wal ~/.todotriage/todotriage.db-shm
echo "  removed database"

# Config
rm -f ~/.todotriage/config.yaml
echo "  removed config"

# Keychain
for key in github_url github_pat github_username jira_url jira_pat; do
  security delete-generic-password -s todotriage -a "$key" 2>/dev/null && echo "  removed keychain: $key" || true
done

echo "Done. Restart the server for a fresh setup."
