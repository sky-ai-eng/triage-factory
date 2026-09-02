package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// warnManagedInstallationsOffDefaultHost names, at boot, every live managed
// installation row keyed under a GitHub other than the deployment's default.
//
// The deployment App is registered on the default GitHub, and everything that
// reaches a managed installation keys on that host alone: the static webhook
// route looks a delivery up under it, and the bind ceremony writes new rows
// under it. A managed row under any other string — a different GitHub, a case
// difference, a path segment, a bind from before the default was set — is
// bound on paper and unreachable in practice, and nothing migrates it: rows
// are keyed under the host they were written with, so changing the default is
// a fresh-install decision. This line is the only signal the operator gets, so
// it names the org, the installation and both hosts. It never refuses to
// start: the rows are stale, not dangerous, and the workspaces they belong to
// have a way out (bind again, or bring their own App).
//
// A read failure is logged and swallowed for the same reason: a boot that
// cannot answer this question is still a boot worth finishing.
func (a *App) warnManagedInstallationsOffDefaultHost(ctx context.Context) {
	host := ghbase.DefaultBaseURL()
	rows, err := a.stores.GitHubApps.ListManagedInstallationsOffHostSystem(ctx, host)
	if err != nil {
		appLog.Warn("could not check managed installations against the deployment's default GitHub host",
			"default_host", host, "error", err)
		return
	}
	for _, inst := range rows {
		appLog.Warn("managed installation is bound under a GitHub other than the deployment default; "+
			"the deployment App's webhooks cannot route to it and the bind ceremony keys on the default alone. "+
			"Rows are not migrated between hosts: rebind the workspace under the default, or have it bring its own App",
			"org", inst.OrgID, "installation", inst.InstallationID, "account", inst.AccountLogin,
			"installation_host", inst.GitHubHost, "default_host", host)
	}
}
