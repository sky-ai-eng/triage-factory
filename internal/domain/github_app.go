package domain

import "time"

type OrgGitHubApp struct {
	OrgID              string
	AppID              string
	Slug               string
	ClientID           string
	ClientSecretRef    string
	PEMRef             string
	WebhookSecretRef   string
	RegisteredAt       time.Time
	RegisteredByUserID string
	Active             bool
}

// OrgGitHubAppInstallation is one place the org's App is installed —
// a GitHub account (org or user) that ran the install flow. Mirrored
// from GitHub via the installation webhook (multi mode) or API
// backfill (local mode). One App can have many installations.
type OrgGitHubAppInstallation struct {
	InstallationID string
	OrgID          string
	AccountType    string
	AccountLogin   string
	InstalledAt    time.Time
}
