package sso

import "log/slog"

// ssoLog is the SSO subsystem logger. Mirrors the core server's ssoLog
// (re-homed here with the handlers) so security-meaningful SSO events —
// domain verification, orphaned-provider warnings — keep landing in the
// production audit trail.
var ssoLog = slog.Default().With("component", "sso")
