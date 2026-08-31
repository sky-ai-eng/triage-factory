package domain

import "time"

// OrgJiraApp is a per-org Atlassian OAuth (3LO) app registration — the
// (client_id, client_secret) of the Atlassian app the per-user "Connect Jira"
// authorize/token flow runs against. It mirrors OrgGitHubApp but is far
// simpler: an Atlassian OAuth app has no installations, no PEM, and no webhook
// secret, only the OAuth client credentials.
//
// One row per org (org_id is the PK). The row is the per-org OVERRIDE in the
// credential precedence: an org with no row falls back to the deployment app
// (multi) or has no app at all (local — where the BYO app IS this row). The
// client_secret never lives in this table — ClientSecretRef is a pointer into
// the secret store (Vault in multi mode, the OS keychain in local), the same
// shape OrgGitHubApp.ClientSecretRef uses.
type OrgJiraApp struct {
	OrgID              string
	ClientID           string
	ClientSecretRef    string
	RegisteredAt       time.Time
	RegisteredByUserID string
}
