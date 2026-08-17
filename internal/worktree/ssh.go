package worktree

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SSHHostFromBaseURL turns a GitHub HTTPS base URL (the value stored in
// auth.Credentials.GitHubURL / OrgSettings.GitHubBaseURL) into the
// SSH probe target — e.g. "https://github.example.com" → "git@github.example.com".
//
// Defaults to "git@github.com" when the input is empty or unparseable
// rather than refusing to operate: the toggle flow has already gated
// the user past credential validation by the time we reach a probe,
// and a conservative default is more useful than a hard failure here.
// Callers that want to fail loudly on a missing host should check for
// emptiness themselves before calling.
//
// Strips userinfo, port, path, query, and fragment from the URL — only
// the hostname becomes part of the SSH target. GHE deployments served
// on non-default ports still use the standard SSH port 22 by default,
// and SSH config aliases handle anything more exotic.
func SSHHostFromBaseURL(baseURL string) string {
	const fallback = "git@github.com"
	if baseURL == "" {
		return fallback
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fallback
	}
	host := u.Hostname()
	if host == "" {
		return fallback
	}
	return "git@" + host
}

// sshPreflightCache holds a per-host cached result so a burst of simultaneous
// clone failures (e.g. during bootstrap with many repos) triggers at most one
// live SSH probe rather than N sequential 15-second probes.
//
// Cache policy: a success result is kept for the process lifetime (SSH key
// configuration doesn't change without a restart). A failure result expires
// after sshPreflightFailureTTL so the user can fix their SSH setup and have
// the new state detected on the next clone cycle.
var sshPreflightCache = struct {
	mu      sync.Mutex
	entries map[string]sshPreflightEntry
}{entries: make(map[string]sshPreflightEntry)}

// sshPreflightFailureTTL is a var (not const) so tests can shorten it
// without having to wait 60 s for failure-cache expiry. Production code
// should treat it as immutable; only ssh_test.go writes to it.
var sshPreflightFailureTTL = 60 * time.Second

// preflightImpl is the function CachedPreflightSSH delegates to. The
// default points at the real PreflightSSH; tests swap it for a mock
// that controls the result and counts invocations. Same testing-only
// caveat as sshPreflightFailureTTL — never mutated outside of tests.
var preflightImpl = PreflightSSH

type sshPreflightEntry struct {
	err      error // nil = success
	cachedAt time.Time
}

func (e sshPreflightEntry) valid() bool {
	if e.err == nil {
		return true // success cached for process lifetime
	}
	return time.Since(e.cachedAt) < sshPreflightFailureTTL
}

// PreflightSSH runs a non-interactive `ssh -T <host>` against the given
// host (typically "git@github.com" for github.com or "git@<ghe-host>"
// for GitHub Enterprise — derive the right value via SSHHostFromBaseURL)
// to verify that the user has a
// usable SSH key + agent + known_hosts entry — the prerequisites for
// `git clone` over SSH to succeed without prompting. The check is the
// canonical way to test GitHub SSH access; GitHub returns a greeting
// with the authenticated username on success and exits 1 (because no
// shell is granted), so we treat the presence of the greeting in the
// combined output as the success signal rather than the exit code.
//
// Options used:
//
//   - -T:                          disable pty allocation (we don't want a shell)
//
//   - BatchMode=yes:               never prompt for passphrases or unknown hosts
//
//   - StrictHostKeyChecking=accept-new: write the host key on first
//     connection so a clean machine doesn't fail the very first probe;
//     after that the host key is pinned the standard way.
//
//     This MUTATES the user's ~/.ssh/known_hosts on first contact with
//     each unique host. We accept this trade-off rather than using
//     UserKnownHostsFile=/dev/null (which would leave the subsequent
//     `git clone --bare` to do the same accept-new write itself, just
//     relocating the mutation) or StrictHostKeyChecking=yes (which
//     would force every new GHE user to manually `ssh-keyscan` before
//     TF could probe — a bad UX regression). The threat model
//     (`feedback_local_threat_model`: trusted-local-user, no remote
//     API surface) makes the user-provided-host attack vector
//     non-actionable in practice; the alternative would shuffle where
//     known_hosts gets written rather than eliminating writes.
//
// Returns nil on success. On failure returns an error whose Error()
// string includes the combined stdout+stderr of the ssh process so
// callers can surface the underlying reason (no agent loaded,
// publickey denied, host unreachable, etc.). The caller does not need
// to parse this string to make routing decisions — preflight failure
// itself is the "SSH side is the cause" signal.
func PreflightSSH(ctx context.Context, host string) error {
	if host == "" {
		host = "git@github.com"
	}
	cmd := exec.CommandContext(ctx, "ssh",
		"-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		host,
	)
	out, runErr := cmd.CombinedOutput()
	combined := strings.TrimSpace(string(out))

	// GitHub's greeting on a successful auth handshake (no shell granted):
	//   "Hi <username>! You've successfully authenticated, but GitHub does
	//    not provide shell access."
	// The exit code is 1 because of "no shell access", so we key off the
	// stable greeting substring rather than the status code.
	if strings.Contains(combined, "successfully authenticated") {
		return nil
	}

	// Diagnose the most common failure modes so the user gets an
	// actionable message rather than just "Permission denied
	// (publickey)". The probe is a best-effort `ssh-add -l` against
	// whatever SSH_AUTH_SOCK the binary inherited; its exit code maps
	// cleanly to "no agent" / "empty agent" / "agent loaded". host is
	// threaded through so GHE users see hints with their hostname
	// rather than a misleading "github.com".
	hint := diagnoseSSHFailure(ctx, combined, host)

	// When ssh produced no output we can't show the user *anything*
	// useful unless we surface the underlying run error: this catches
	// "ssh binary not on PATH" (exec.Error), "context cancelled before
	// ssh started", and similarly opaque states where the auth-failure
	// hints below would mislead. Auth failures yield exit-status-255
	// with a populated stderr, so combined != "" — runErr in that case
	// is just the *exec.ExitError wrapping the same exit code and we
	// don't need to repeat it.
	if combined == "" {
		if runErr != nil {
			return fmt.Errorf("ssh preflight could not run: %v\n\n%s", runErr, hint)
		}
		return fmt.Errorf("ssh preflight produced no output\n\n%s", hint)
	}
	return fmt.Errorf("%s\n\n%s", combined, hint)
}

// diagnoseSSHFailure inspects the ssh stderr and (when relevant) the
// local ssh-agent state to produce a one-paragraph hint pointing at
// the most likely fix. Returned text is plain and intended for direct
// UI display.
//
// We classify the failure by stderr first so we don't send a network-
// failure user on a wild goose chase configuring keys, then fall back
// to the auth-path probe (`ssh-add -l`) for the remaining cases.
// ssh-add's documented exit codes:
//
//	0: agent reachable, has identities → key probably not on GitHub,
//	   or BatchMode is rejecting a passphrase-protected key whose
//	   keychain unlock dialog is being suppressed.
//	1: agent reachable, no identities → most common case on macOS
//	   with passphrase-protected keys: the keychain agent is empty
//	   and BatchMode blocks the unlock prompt.
//	2: agent socket unreachable (SSH_AUTH_SOCK unset or stale).
func diagnoseSSHFailure(ctx context.Context, sshOut, host string) string {
	if host == "" {
		host = "git@github.com"
	}
	hostname := strings.TrimPrefix(host, "git@")
	const macOSNote = "On macOS, `ssh-add --apple-use-keychain ~/.ssh/id_ed25519` persists the key in the login keychain so you don't have to re-add it after every reboot."
	// settingsURL is on the user's GitHub host, not always github.com —
	// GHE deployments mirror the same /settings/keys path.
	settingsURL := fmt.Sprintf("https://%s/settings/keys", hostname)

	lower := strings.ToLower(sshOut)

	// Network-layer failures: ssh never got to the auth handshake, so
	// agent/key state is irrelevant. Telling the user to `ssh-add` here
	// would waste their time.
	switch {
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection timed out"),
		strings.Contains(lower, "could not resolve hostname"),
		strings.Contains(lower, "no route to host"),
		strings.Contains(lower, "network is unreachable"),
		strings.Contains(lower, "operation timed out"):
		return strings.Join([]string{
			fmt.Sprintf("ssh couldn't reach %s over the network — this isn't an auth issue.", host),
			"",
			"Common causes:",
			"  - Corporate firewall blocking outbound port 22",
			"  - VPN required and not connected",
			fmt.Sprintf("  - DNS lookup failing for %s", hostname),
			"",
			"If port 22 is blocked, switch the clone protocol to HTTPS in Settings.",
		}, "\n")
	}

	// Host key mismatch: known_hosts has a stale fingerprint. Rare with
	// our `accept-new` setting (would only happen if the user pinned
	// the key manually in the past and GitHub rotated theirs).
	if strings.Contains(lower, "host key verification failed") {
		return strings.Join([]string{
			fmt.Sprintf("%s's host key in your ~/.ssh/known_hosts doesn't match what the server presented.", hostname),
			"",
			"If you trust the host (it's almost certainly a key rotation), refresh the entry:",
			fmt.Sprintf("  ssh-keygen -R %s", hostname),
			fmt.Sprintf("  ssh-keyscan %s >> ~/.ssh/known_hosts", hostname),
		}, "\n")
	}

	// Default: auth-layer failure ("Permission denied (publickey)" and
	// friends). Probe the agent to pick a more specific hint.
	addCmd := exec.CommandContext(ctx, "ssh-add", "-l")
	addOut, _ := addCmd.CombinedOutput()
	exit := -1
	if addCmd.ProcessState != nil {
		exit = addCmd.ProcessState.ExitCode()
	}

	switch exit {
	case 1:
		// Agent reachable but empty — by far the most common cause
		// of "Permission denied (publickey)" with BatchMode on macOS.
		return strings.Join([]string{
			"Your ssh-agent has no keys loaded, so the connection had nothing to offer.",
			"",
			"Fix:",
			"  ssh-add ~/.ssh/id_ed25519     # or your key path",
			"",
			"Don't have a key yet?",
			"  ssh-keygen -t ed25519 -C \"you@example.com\"",
			"  ssh-add ~/.ssh/id_ed25519",
			"  cat ~/.ssh/id_ed25519.pub      # paste at " + settingsURL,
			"",
			macOSNote,
		}, "\n")
	case 2:
		// Socket missing — SSH_AUTH_SOCK isn't set in this process's
		// environment. Common when the binary was launched outside
		// a normal login shell (Finder, launchd plist, etc.).
		return strings.Join([]string{
			"ssh-agent isn't reachable from this process (SSH_AUTH_SOCK is unset or stale).",
			"",
			"Fix: start Triage Factory from a terminal that has the agent running.",
			"On macOS this is automatic in any login shell; double-clicking the binary",
			"in Finder runs it in launchd's reduced environment without the agent.",
			"",
			"Check from the terminal you launched TF in:",
			"  echo $SSH_AUTH_SOCK            # should be non-empty",
			"  ssh-add -l                     # should list your key(s)",
		}, "\n")
	case 0:
		// Agent has keys but GitHub still rejected — key probably
		// isn't on GitHub, or it's a different identity than the
		// one authorized.
		count := strings.Count(string(addOut), "\n")
		if count == 0 {
			count = 1
		}
		return strings.Join([]string{
			fmt.Sprintf("Your ssh-agent has %d key(s) loaded, but %s rejected them.", count, hostname),
			"",
			"The most likely fix:",
			"  - Make sure the public key is added at " + settingsURL,
			"  - Confirm you're connecting as the right GitHub user",
			"",
			"To see which key was offered, run:",
			fmt.Sprintf("  ssh -vT %s 2>&1 | grep 'Offering public key'", host),
		}, "\n")
	default:
		// ssh-add wasn't found, was killed, or returned an unexpected
		// status. Fall back to a generic hint that covers the broad
		// strokes without being misleading.
		return strings.Join([]string{
			"Couldn't determine the ssh-agent state — falling back to general guidance.",
			"",
			"Common fixes:",
			"  - `ssh-add ~/.ssh/id_ed25519` to load your key",
			"  - Add the public key at " + settingsURL,
			fmt.Sprintf("  - Run `ssh -vT %s` for verbose output", host),
		}, "\n")
	}
}

// CachedPreflightSSH is identical to PreflightSSH but de-duplicates probes
// across concurrent callers and caches the result: successes are kept for the
// process lifetime, failures for sshPreflightFailureTTL (60 s) so a user who
// fixes their SSH setup gets re-detected on the next clone cycle without
// restarting the process.
//
// Concurrency: the global mutex is held across the probe so the
// double-checked-lock pattern actually dedupes. N concurrent callers
// see one ssh + one ssh-add subprocess pair; the rest hit the
// populated cache when they acquire the lock. We accept global
// (rather than per-host) serialization here because production
// callers probe a single host (the configured GitHub base, derived
// via SSHHostFromBaseURL) and the probe is bounded by PreflightSSH's
// 15-second context — switching to per-host locks would only matter
// if a future caller probes multiple hosts in parallel, which
// doesn't exist today.
func CachedPreflightSSH(ctx context.Context, host string) error {
	if host == "" {
		host = "git@github.com"
	}

	sshPreflightCache.mu.Lock()
	defer sshPreflightCache.mu.Unlock()

	if e, ok := sshPreflightCache.entries[host]; ok && e.valid() {
		return e.err
	}

	err := preflightImpl(ctx, host)
	sshPreflightCache.entries[host] = sshPreflightEntry{err: err, cachedAt: time.Now()}
	return err
}
