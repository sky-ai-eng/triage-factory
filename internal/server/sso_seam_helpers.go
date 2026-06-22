package server

import (
	"os"
	"strings"
)

// This file holds the two SSO-adjacent helpers that survive the SSO
// extraction because the ExtensionAPI seam delegates to them — they are
// generic enough to stay in core (no SSO types), and re-homing them here
// keeps them when their original files (the SSO handlers) move to ee/sso.

// gotrueAdminBaseURL returns the in-network GoTrue base URL for admin calls,
// preferring the value wired into the auth config (SetAuthDeps) and falling
// back to TF_GOTRUE_URL for paths that run before/without the full auth
// stack (e.g. tests). Trailing slash trimmed. Exposed to the Enterprise
// Edition via ExtensionAPI.GotrueAdminBaseURL.
func (s *Server) gotrueAdminBaseURL() string {
	if s.authCfg != nil && s.authCfg.gotrueURL != "" {
		return s.authCfg.gotrueURL
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("TF_GOTRUE_URL")), "/")
}

// emailDomain extracts the lower-cased email domain for an exact-match
// lookup. It requires exactly one '@' with a non-empty local part and a
// non-empty domain, trims surrounding whitespace and any trailing FQDN-root
// dot, returns the FULL domain after the '@', and requires an FQDN (at least
// one dot). Returns ("", false) for anything that isn't a plain address.
// Exposed to the Enterprise Edition via ExtensionAPI.EmailDomain.
func emailDomain(raw string) (string, bool) {
	e := strings.TrimSpace(raw)
	at := strings.IndexByte(e, '@')
	if at <= 0 {
		return "", false
	}
	rest := e[at+1:]
	if strings.IndexByte(rest, '@') >= 0 {
		return "", false
	}
	domain := strings.ToLower(strings.TrimRight(rest, "."))
	if domain == "" {
		return "", false
	}
	if strings.ContainsAny(domain, " \t\r\n") {
		return "", false
	}
	if !strings.Contains(domain, ".") {
		return "", false
	}
	return domain, true
}
