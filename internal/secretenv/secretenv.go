// Package secretenv keeps deployment secrets out of the process environment.
//
// At boot, Resolve captures each deployment secret into process memory and
// then UNSETS it from the environment (both NAME and NAME_FILE). Downstream
// code reads it back through Get instead of os.Getenv. The effect:
//
//   - Child processes don't inherit them. os.Unsetenv scrubs Go's os.Environ()
//     — the env every os/exec child inherits — so git, gh, the SDK installer,
//     and (on the direct/local agent path) the agent itself no longer see them.
//     In multi mode the sandboxed agent already gets a curated env; this closes
//     the remaining helper-subprocess exposure. Verified in-package by spawning
//     a child and reading its environment.
//   - Absent from `docker inspect` and from this process's /proc/<pid>/environ
//     WHEN supplied via NAME_FILE — the value is never an env var, only the
//     path is (NAME_FILE is the de-facto file-secret convention; Postgres/MySQL
//     and most official images do this). A plain NAME= value CANNOT be scrubbed
//     from this process's own /proc/environ in-process: that view is the
//     kernel's exec-time snapshot, and os.Unsetenv rewrites only Go's copy of
//     the environment, not the kernel region. So prefer NAME_FILE for the
//     sensitive ones — child inheritance is closed either way, but only NAME_FILE
//     also keeps the value out of this process's own /proc/environ.
//
// What this deliberately does NOT do — and can't — is remove the value from
// this process's own heap: any program that USES a secret must hold it in
// memory, where /proc/<pid>/mem (root/same-uid + ptrace) can reach it. "Out
// of the child-inherited environment (and, via NAME_FILE, this process's
// /proc/environ too), into heap only" is the achievable end state.
//
// Get falls back to os.Getenv for any name Resolve did not capture, so it is a
// safe drop-in at every read site (and code paths that never call Resolve —
// e.g. unit tests — keep working against the plain environment).
//
// Resolve runs once at boot, before any secret is read and before any
// goroutine spawns (so the env mutation is race-free). It fails fast: a
// NAME_FILE that is set but unreadable is a misconfiguration, not something to
// paper over.
package secretenv

import (
	"fmt"
	"os"
	"strings"
)

// fileEnvSuffix forms the file-indirection variable: TF_SECRET_ENCRYPTION_KEY
// -> TF_SECRET_ENCRYPTION_KEY_FILE.
const fileEnvSuffix = "_FILE"

// Secrets is the deployment-secret set the binary reads. Resolve captures and
// unsets exactly these; ordinary config stays plain env. Cross-container
// secrets consumed by the postgres/gotrue images (not this binary) have their
// own upstream *_FILE conventions and are not listed here.
var Secrets = []string{
	"TF_SECRET_ENCRYPTION_KEY", // AEAD master key for the org-secrets vault
	"TF_SESSION_ENCRYPTION_KEY",
	"TF_COOKIE_SECRET",
	"TF_LICENSE",                   // signed Enterprise license token
	"TF_GOTRUE_SERVICE_ROLE_TOKEN", // RS256 admin bearer for GoTrue's SSO admin API
	"TF_ATLASSIAN_CLIENT_SECRET",   // deployment Atlassian OAuth (3LO) app secret
	// The deployment GitHub App's three undisclosable halves. Its numeric App
	// ID is deliberately absent: it is not a secret, and GET /app returns the
	// rest of the App's identity anyway. The private key most wants the _FILE
	// form — a PEM is multi-line, which a .env carries badly and a mounted file
	// carries natively.
	"TF_GITHUB_APP_PRIVATE_KEY",
	"TF_GITHUB_APP_WEBHOOK_SECRET",
	"TF_GITHUB_APP_CLIENT_SECRET",
	"TF_DATABASE_URL",
	"TF_DATABASE_DIRECT_URL",
	"TF_AUTHENTICATOR_PASSWORD",
	"TF_BLOB_ACCESS_KEY",
	"TF_BLOB_SECRET_KEY",
}

// held is the in-memory secret store, populated by Resolve. A name present
// here (even with an empty value) is authoritative — Get does not fall back to
// the environment for it, because Resolve has already unset it there.
var held = map[string]string{}

// warnf is the log sink for the both-set case. main() wires it to the boot
// logger; the default keeps the package usable (and silent) in tests.
var warnf = func(format string, args ...any) {}

// SetWarnFunc installs the warning sink (called once from main before Resolve).
func SetWarnFunc(fn func(format string, args ...any)) { warnf = fn }

// Get returns a secret's value. If Resolve captured the name, the held value
// is returned; otherwise Get falls back to os.Getenv (so it is a drop-in for
// os.Getenv and works before Resolve has run).
func Get(name string) string {
	if v, ok := held[name]; ok {
		return v
	}
	return os.Getenv(name)
}

// Resolve captures every secret in Secrets into process memory and unsets it
// (and its _FILE variant) from the environment. A secret's value comes from
// its _FILE if set, else its plain env var; a set-but-unreadable _FILE is a
// fatal error. When both NAME and NAME_FILE are set, the file wins and a
// warning is emitted.
func Resolve() error {
	return resolveInto(held, Secrets)
}

func resolveInto(dst map[string]string, names []string) error {
	for _, name := range names {
		fileVar := name + fileEnvSuffix
		var value string
		if path := strings.TrimSpace(os.Getenv(fileVar)); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("%s=%q: %w", fileVar, path, err)
			}
			// Strip a trailing line ending — secret files conventionally end
			// in one. Only \r\n, not all whitespace: a leading/inner space
			// could be meaningful for some future secret, so don't touch it.
			value = strings.TrimRight(string(data), "\r\n")
			if os.Getenv(name) != "" {
				warnf("both %s and %s are set; using the file", name, fileVar)
			}
		} else {
			value = os.Getenv(name)
		}
		dst[name] = value
		// Remove from the environment so child processes don't inherit it
		// (os.Environ() no longer lists it). This does NOT retroactively scrub
		// a plain NAME= value from this process's /proc/environ or `docker
		// inspect` — those reflect the exec-time / container-create env, which a
		// running process can't rewrite; only a NAME_FILE value (never an env
		// var to begin with) is absent there. See the package doc.
		_ = os.Unsetenv(name)
		_ = os.Unsetenv(fileVar)
	}
	return nil
}
