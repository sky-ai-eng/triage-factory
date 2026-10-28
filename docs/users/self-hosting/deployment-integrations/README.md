# Deployment integrations

Apps and accounts you register once for the whole deployment, so workspaces can
connect without registering their own. Each one is optional: leave its
variables unset and every workspace sets up its own, as it does by default. A
workspace that has its own keeps using it. Local mode ignores all of these.

- [GitHub App](deployment-github-app.md) — one App every workspace without its
  own can connect to
- [Jira app](deployment-jira-app.md) — one Atlassian OAuth app for one-click
  Jira Cloud connections
- [Linear app](deployment-linear-app.md) — one Linear OAuth app every workspace
  can install
- [Commit signer](deployment-commit-signer.md) — signed, Verified commits for
  workspaces on the deployment GitHub App

Each page covers registering it and the variables to set in `.env`.
