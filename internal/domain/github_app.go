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
