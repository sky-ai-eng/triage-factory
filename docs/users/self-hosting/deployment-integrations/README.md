# Deployment integrations

Information on Apps and accounts you register once for the whole deployment.
Individual orgs can connect their event-generating integrations through these
without needing to register their own. Each one is optional: leave its
variables unset and every org sets up its own, as it does by default. An
organization that has its own keeps using it. Local mode ignores all of these.

- [GitHub App](deployment-github-app.md) — one App every organization without
  its own can connect to
- [Jira app](deployment-jira-app.md) — one Atlassian OAuth app for one-click
  Jira Cloud connections
- [Linear app](deployment-linear-app.md) — one Linear OAuth app every
  organization can install
- [Commit signer](deployment-commit-signer.md) — signed, Verified commits for
  organizations on the deployment GitHub App

Each page covers registering it and the variables to set in `.env`.
