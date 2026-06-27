package app

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// envAllowPublicLocal is the explicit, deliberate acknowledgment that
// downgrades the local-mode public-bind guard from fail-closed (refuse to
// start) to a loud warning. An operator who genuinely wants unauthenticated
// LAN access — and accepts that every request is treated as the owner — sets
// it. Anyone else hosting a local build by accident is caught at boot.
const envAllowPublicLocal = "TF_ALLOW_PUBLIC_LOCAL"

// checkLocalBind runs the local-mode public-exposure guardrail (TFAC-409
// item 1) for this App's configured bind host + public URL, reading the
// override env itself. Caller gates on local mode.
func (a *App) checkLocalBind() error {
	return assertLocalBindSafe(a.cfg.Host, os.Getenv("TF_PUBLIC_URL"), os.Getenv(envAllowPublicLocal))
}

// assertLocalBindSafe is the local-mode public-exposure guardrail. Local mode
// runs with ZERO authentication: the session middleware injects sentinel owner
// claims and RequireOrgMember/RequireOrgAdmin short-circuit to true, so every
// request is treated as the instance owner. That is correct and safe for the
// single-user loopback tool TF ships as — but catastrophic if the same build is
// bound to a network interface or fronted by a public URL, because it exposes
// the entire instance (all data, all delegated agent runs) to anyone who can
// reach the port.
//
// The guard turns that misconfiguration class into a fail-closed boot check: in
// local mode, if the server would bind a non-loopback address OR a non-local
// TF_PUBLIC_URL is set, refuse to start. The fix is to run multi mode
// (TF_MODE=multi), which mounts real auth. An operator who truly wants
// unauthenticated LAN access sets TF_ALLOW_PUBLIC_LOCAL=true, which downgrades
// the refusal to a screaming warning (the caller logs it) and proceeds.
//
// Pure and env-free for testability — checkLocalBind reads the process env and
// passes the three strings in. Returns nil when the bind is safe (loopback) or
// when the override is set; a non-nil error means "refuse to boot."
func assertLocalBindSafe(host, publicURL, allowOverride string) error {
	reasons := localExposureReasons(host, publicURL)
	if len(reasons) == 0 {
		return nil
	}
	if parseOverride(allowOverride) {
		// Acknowledged: don't block, but make the exposure impossible to miss
		// in the logs. The same reasons that would have refused boot.
		appLog.Warn("DANGER: local mode (no authentication) is exposed to a non-loopback surface; "+
			envAllowPublicLocal+" override is set so booting anyway — every request is treated as the owner",
			"reasons", strings.Join(reasons, "; "))
		return nil
	}
	return fmt.Errorf(
		"refusing to start: local mode runs with NO authentication (every request is treated as the owner), "+
			"but %s. Exposing this leaves the entire instance — all data and all delegated agent runs — open to "+
			"anyone who can reach it. Run multi-tenant mode with real auth instead (set TF_MODE=multi; see "+
			"docs/self-host-setup.md). If you genuinely want unauthenticated LAN access and accept the risk, set "+
			"%s=true to override this guard",
		strings.Join(reasons, " and "), envAllowPublicLocal)
}

// localExposureReasons returns a human-readable reason for each way the given
// bind host / public URL would expose a local-mode instance beyond loopback.
// Empty slice means the bind is loopback-only and safe.
func localExposureReasons(host, publicURL string) []string {
	var reasons []string
	if !isLoopbackHost(host) {
		shown := host
		if strings.TrimSpace(host) == "" {
			// An empty host binds every interface (Go's ":port"); name it so
			// the error isn't a confusing reference to host "".
			shown = "<all interfaces>"
		}
		reasons = append(reasons, fmt.Sprintf("it binds a non-loopback address (host %q)", shown))
	}
	if pub := strings.TrimSpace(publicURL); pub != "" && !publicURLIsLoopback(pub) {
		// Any non-empty TF_PUBLIC_URL in local mode is a strong signal the
		// instance is being fronted publicly. Fail closed on everything that
		// isn't provably loopback: a remote host, but ALSO an unparseable or
		// host-less value ("tf.example.com", "https:tf.example.com"), which a
		// public deployment can produce and which we otherwise can't vouch for.
		reasons = append(reasons, fmt.Sprintf("TF_PUBLIC_URL is set to a non-local value (%q)", pub))
	}
	return reasons
}

// publicURLIsLoopback reports whether raw is provably a loopback URL. The bar
// is deliberately high for a fail-closed guard: only a value that parses
// cleanly, carries a host, and resolves to a loopback host counts. A parse
// error or an empty Host (a bare hostname like "tf.example.com" parses as a
// path; "https:tf.example.com" parses as an opaque URL) returns false so the
// caller treats it as an exposure reason.
func publicURLIsLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether binding/advertising host keeps the server
// reachable only from the local machine. "localhost" and any loopback IP
// (127.0.0.0/8, ::1) are loopback. An empty host means "all interfaces" and is
// NOT loopback. A non-IP hostname (a DNS name) is treated as non-loopback —
// it resolves to something routable, so we fail closed rather than guess.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// parseOverride reads the override env value. Only an explicit truthy spelling
// enables the override; empty, unset, false-y, AND unrecognized values all
// leave the guard fail-closed — the safe direction, since a typo'd override
// keeps the server from booting publicly rather than silently exposing it.
func parseOverride(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}
