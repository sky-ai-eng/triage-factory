// Package ghbase holds the GitHub base-URL derivations — the canonical form of
// the web base an org configures, the deployment's default GitHub (where the
// deployment App lives), and the REST API mount that follows from a web base. It is a leaf: stdlib only, so
// every package that needs the derivation can import it (internal/auth and
// internal/db both sit below internal/github, which imports them). One copy,
// no lockstep to keep.
package ghbase

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
)

// GitHubCom is the public github.com web base — the literal, for the sites
// that mean github.com specifically whatever the deployment's default is: the
// GoTrue GitHub OAuth login provider, the api.github.com mount, the
// codeload/uploads hosts. Everything that means "the deployment's GitHub"
// reads DefaultBaseURL instead.
const GitHubCom = "https://github.com"

// EnvDefaultBaseURL names the deployment's default GitHub: the GitHub the
// deployment App lives on, and the URL setup offers an org choosing its own.
// Optional; unset means GitHubCom. Plain config, not a secret, in the same
// class as the port — read once at boot by InitDefaultBaseURLFromEnv.
const EnvDefaultBaseURL = "TF_DEFAULT_GITHUB_HOST"

// defaultBaseURL is the resolved deployment default, read once at boot. It
// starts as GitHubCom so a process that never inits — a test, a tool that
// skips the boot spine — behaves as a deployment with the variable unset.
var (
	defaultBaseURL       = GitHubCom
	defaultBaseURLInit   bool
	defaultBaseURLInitMu sync.RWMutex
)

// DefaultBaseURL returns the deployment's default GitHub web base — GitHubCom
// unless TF_DEFAULT_GITHUB_HOST named another. It is the host the deployment
// App is on and the value setup offers for an org's github_base_url; an org
// with no github_base_url is not on it, or on any GitHub. Safe from any
// goroutine.
func DefaultBaseURL() string {
	defaultBaseURLInitMu.RLock()
	defer defaultBaseURLInitMu.RUnlock()
	return defaultBaseURL
}

// InitDefaultBaseURL sets the process-wide default. Production code calls it
// once at boot via InitDefaultBaseURLFromEnv, with the runmode contract: a
// repeat with the same value is a no-op, a repeat with a different value is an
// error and leaves the state alone, so nothing can re-point a running process
// under consumers that already resolved hosts against the first answer.
func InitDefaultBaseURL(base string) error {
	canonical, err := ParseDefaultBaseURL(base)
	if err != nil {
		return err
	}
	defaultBaseURLInitMu.Lock()
	defer defaultBaseURLInitMu.Unlock()
	if defaultBaseURLInit {
		if defaultBaseURL == canonical {
			return nil
		}
		return fmt.Errorf("%s already initialized as %q; cannot re-init as %q", EnvDefaultBaseURL, defaultBaseURL, canonical)
	}
	defaultBaseURL = canonical
	defaultBaseURLInit = true
	return nil
}

// InitDefaultBaseURLFromEnv reads TF_DEFAULT_GITHUB_HOST and installs the
// deployment default. Unset or blank is GitHubCom; anything else must be a
// valid web base or boot fails, since a default nobody can reach would leave
// the deployment App unusable.
func InitDefaultBaseURLFromEnv() error {
	return InitDefaultBaseURL(os.Getenv(EnvDefaultBaseURL))
}

// ParseDefaultBaseURL validates a TF_DEFAULT_GITHUB_HOST value and returns its
// canonical form. Empty (after trimming) is GitHubCom. A non-empty value is held
// to exactly the rule github_base_url is held to — NormalizeBaseURL — because it
// is the same kind of value: it is what setup offers as an org's setting, so a
// shape the setting would refuse is a shape the default must refuse too.
func ParseDefaultBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return GitHubCom, nil
	}
	base, ok := NormalizeBaseURL(raw)
	if !ok {
		return "", fmt.Errorf("%s=%q is not a GitHub web base: want an http(s) URL with a host and no credentials, query, or fragment (e.g. https://ghe.example.com)", EnvDefaultBaseURL, raw)
	}
	return base, nil
}

// CanonicalBaseURL returns the spelling a GitHub or Jira base URL is stored and
// compared under: surrounding whitespace and trailing slashes trimmed, and the
// scheme and authority (host and port) lowercased, because both compare
// case-insensitively. The path keeps its case byte for byte, because a GHES or
// Jira Data Center context path may be case-sensitive. A value without "://"
// has no authority to find and is only trimmed.
//
// It is the one canonicalizer: every GitHub host and Jira site a row is keyed
// under, and every lookup of one, passes through it, so two spellings of one
// server are one key. It does not validate; NormalizeBaseURL validates and
// then calls it.
//
// Lowercasing is ASCII only. DNS names compare case-insensitively in ASCII
// alone, and the SQLite migration that rewrites stored hosts into this form
// (202610050001_provider_scopes_and_linear.sql) folds with lower(), which covers exactly that
// range; the two must agree on every stored value.
func CanonicalBaseURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	sep := strings.Index(s, "://")
	if sep < 0 {
		return s
	}
	rest := s[sep+len("://"):]
	end := strings.IndexByte(rest, '/')
	if end < 0 {
		end = len(rest)
	}
	cut := sep + len("://") + end
	return asciiLower(s[:cut]) + s[cut:]
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// NormalizeBaseURL validates a user-entered GitHub/Jira base URL and returns its
// canonical form (CanonicalBaseURL of scheme://host[/path]). One function
// for the reachability probe, for the settings write that persists the same
// value, and for the deployment default, so a URL one door accepts is exactly
// the one the others hold. A base URL must parse, use http or https, and carry
// a host — and must NOT carry credentials (userinfo), a query, or a fragment.
// Those forms are rejected as bad input rather than flowing into a malformed
// derived URL — without this, "https://host?x=1" would become
// "https://host?x=1/api/v3" and surface a confusing 200 + unreachable instead
// of "fix your URL". A path is preserved because Jira Data Center can live
// under a context path. For a GitHub base a path is unusual — GHES mounts the
// API at the host root, so it flows into APIBase's /api/v3 suffix — but the
// reachability verdict is host-level (the probe only asks whether that host
// answers), so an odd path can't produce a wrong reachable/unreachable result.
func NormalizeBaseURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", false
	}
	return CanonicalBaseURL(u.Scheme + "://" + u.Host + u.Path), true
}

// APIBase derives the REST API base from a user-facing GitHub web base,
// covering the three host classes TF supports:
//
//   - github.com           → https://api.github.com (public, fixed).
//   - <tenant>.ghe.com      → https://api.<tenant>.ghe.com. GitHub Enterprise
//     Cloud *data residency* puts the API on an api.* subdomain, mirroring
//     github.com→api.github.com — NOT the GHES path mount. These hosts are
//     public and per-enterprise, which is why the base URL is free-entry.
//   - everything else (GHES) → {base}/api/v3, the path-mounted REST root on
//     the same (typically private) host.
//
// The base goes through CanonicalBaseURL first, so "https://GitHub.com" is the
// public host like any other spelling of it. An empty base is no GitHub, and
// has no API base: "".
//
// This is the only API-mount derivation in the repo: the client, the App JWT
// mint endpoint, the credential validator, the reachability probe and the
// sidecar's proxies all read it, so nothing can disagree about where an org's
// API lives.
func APIBase(base string) string {
	base = CanonicalBaseURL(base)
	if base == "" {
		return ""
	}
	if base == GitHubCom {
		return "https://api.github.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// Unparseable — fall back to the GHES path mount rather than guess a
		// public host, which keeps an odd input on the host it named.
		return base + "/api/v3"
	}
	host := u.Hostname()
	switch {
	case host == "github.com":
		return "https://api.github.com"
	case strings.HasSuffix(host, ".ghe.com"):
		// Data residency: the API host is api.<tenant>.ghe.com. Built from
		// u.Host (not host) so an explicit port — rare but valid — is kept. If
		// the caller already handed us that api.* host (a stored API base, or a
		// paste of the wrong field), return it as-is rather than double-prefix
		// it into api.api.<tenant>.ghe.com.
		if strings.HasPrefix(host, "api.") {
			return u.Scheme + "://" + u.Host
		}
		return u.Scheme + "://api." + u.Host
	default:
		return base + "/api/v3"
	}
}
