package sso

import "log/slog"

// ssoLog is the SSO subsystem logger, tagged so security-meaningful SSO
// events — domain verification, orphaned-provider warnings — land in the
// production audit trail.
var ssoLog = slog.Default().With("component", "sso")
