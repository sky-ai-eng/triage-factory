package domain

import "time"

// OrgLinearApp is an org's own Linear OAuth app registration: the (client_id,
// client_secret) of the app the workspace install ceremony and the per-user
// Connect run against. It is the Linear sibling of OrgJiraApp.
//
// One row per org. The row is the per-org override in the app precedence; an
// org with no row falls back to the deployment app, or has none. The
// client_secret never lives in the table: ClientSecretRef names the org secret
// that holds it.
type OrgLinearApp struct {
	OrgID              string
	ClientID           string
	ClientSecretRef    string
	RegisteredAt       time.Time
	RegisteredByUserID string
}

// The reasons an org's Linear install stops being live, stored as
// org_linear_installs.removed_reason.
const (
	// LinearInstallRemovedDisconnected is an org admin's disconnect.
	LinearInstallRemovedDisconnected = "disconnected"
	// LinearInstallRemovedRevoked is Linear refusing the install's refresh
	// token: the app was removed from the workspace in Linear.
	LinearInstallRemovedRevoked = "install_revoked"
	// LinearInstallRemovedFailed is a ceremony whose install row was written
	// and whose credential then failed to store.
	LinearInstallRemovedFailed = "install_failed"
)

// OrgLinearInstall is the Linear workspace that installed the org's resolved
// OAuth app as an app user, whichever app that was. It is the queryable half
// of the install; the refresh token lives in the org secret
// linear_app_install.
//
// One row per org. A live row (RemovedAt zero) holds its workspace against
// every other org: one Linear workspace installs into at most one TF org.
type OrgLinearInstall struct {
	OrgID string
	// InstallID is minted per install and stored in the install's secret too,
	// so a writer acting on what it read names exactly that install: a
	// removal can never land on a newer install of the same org.
	InstallID       string
	WorkspaceID     string
	WorkspaceURLKey string
	// AppUserID is viewer.id under the install's token: the app user TF acts
	// as in the workspace, which is how a poller recognises its own writes.
	AppUserID string
	// AppClientID is the client id of the app that minted the install. A
	// refresh needs that app's secret, so an app row replaced later must not
	// be mistaken for the one this install belongs to.
	AppClientID string
	// InstalledByUserID is the org admin who completed the ceremony. A soft
	// reference: the row outlives the user.
	InstalledByUserID string
	InstalledAt       time.Time
	// RemovedAt is when the install stopped being live, zero while it is.
	RemovedAt time.Time
	// RemovedReason is one of the LinearInstallRemoved* values, "" while the
	// install is live.
	RemovedReason string
}

// Live reports whether the install is current.
func (i OrgLinearInstall) Live() bool { return i.RemovedAt.IsZero() }
