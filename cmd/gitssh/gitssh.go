// Package gitssh is the GIT_SSH_COMMAND dispatcher a managed local run's git
// execs instead of ssh. It has two arms, chosen by the host git asks for:
//
//   - The org's own git host, in ANY spelling — bridge the session onto the
//     run's git proxy over smart HTTP. No ssh runs, no key is read, and from
//     the proxy inward the session is indistinguishable from an HTTPS one:
//     same token source, same ref gate, same outcome-aware capture.
//   - Any other host — exec the real ssh with the argv git handed us,
//     unchanged. TF holds no credential for a foreign host and deliberately
//     records nothing about it, so there is nothing to manage.
//
// # Why a dispatcher at all
//
// The managed channel routes git through the proxy with insteadOf rewrites,
// and those cover the canonical spellings of the org host. They are a prefix
// match, so they miss variants (an explicit port, an unusual ssh:// form), and
// an operator's own `url.<ssh>.pushInsteadOf` for the org host outranks them
// on push — git prefers pushInsteadOf when rewriting a push URL, and a local
// run inherits the operator's global config. A push that escapes that way goes
// out over the operator's private key, is recorded by nothing, and never meets
// the ref gate. This dispatcher is the layer under the rewrites where nothing
// can escape: git cannot open an ssh transport without execing it.
//
// # Why bridging is possible at all
//
// GitHub — GHES included — authenticates PAT and App installation tokens over
// HTTPS only, so a managed SSH-shaped session has to become HTTPS somewhere.
// The rewrites already do that conversion for the spellings they match; this
// does it for everything they don't. git's side of the deal is generous: it
// execs us with the host and the remote command in argv and then speaks the
// pkt-line protocol over our stdio, so there is no TLS, no certificate and no
// trust store between us and the session.
//
// # What it is not
//
// Not a security boundary. An agent that unsets GIT_SSH_COMMAND gets ordinary
// ssh back, exactly as it can unset every other process-scoped control on the
// local path; this is the same courtesy isolation as the rest of local mode.
// And it holds no credential of its own: a bridged session authenticates to
// the proxy with the run's placeholder token, and a passed-through one uses
// whatever ssh finds on the host.
package gitssh

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Env vars the spawner writes into a managed local run's environment. The
// dispatcher reads its whole configuration from them because git chooses when
// to exec it and with which argv; there is no other channel.
//
// The token is the run's proxy placeholder, not a credential: it authorizes
// the loopback hop to this run's own proxy and buys nothing anywhere else. It
// already rides the same env in the http.<base>.extraHeader git-config entry,
// so naming it here adds no exposure.
const (
	// UpstreamHostEnvVar carries the org's git host, bare (no scheme, no
	// port) — the one host whose sessions bridge.
	UpstreamHostEnvVar = "TF_GIT_SSH_UPSTREAM_HOST"

	// ProxyURLEnvVar carries the run git proxy's own http:// address. The
	// bridge dials the proxy directly rather than the run's fake-GHE origin:
	// both mount the same handler, and the direct address needs no TLS trust.
	ProxyURLEnvVar = "TF_GIT_SSH_PROXY_URL"

	// ProxyTokenEnvVar carries the run's placeholder token, presented to the
	// proxy as the HTTP Basic password.
	ProxyTokenEnvVar = "TF_GIT_SSH_PROXY_TOKEN"
)

// ProxyBasicUser is the username half of the Basic credential the proxy
// expects. The proxy validates the password alone, but a well-formed header
// needs both halves, and this matches what the git-config rewrites encode.
const ProxyBasicUser = "x-run"

// Handle runs `triagefactory git-ssh <ssh-argv...>`: the whole dispatcher.
// It manages its own exit status because git reads the ssh command's status as
// the transport's, and never returns.
func Handle(args []string) {
	os.Exit(run(args, os.Stdin, os.Stdout, os.Stderr))
}

// run is Handle's testable body. It returns the process exit status; the
// passthrough arm replaces the process instead of returning.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// git probes an unrecognized ssh command with `-G` (print configuration)
	// to decide whether it may pass OpenSSH-style options. Answering success
	// is accurate: the bridge parses the option forms git emits, and the
	// passthrough arm hands them to the real ssh, which defined them. The
	// spawner also pins GIT_SSH_VARIANT so the probe normally never fires —
	// this keeps the answer right if it ever does, because the fallback
	// variant cannot express a port and git dies rather than degrade.
	for _, a := range args {
		if a == "-G" {
			return 0
		}
	}

	cfg, configured := bridgeConfigFromEnv()
	remoteCommand, bridged := bridgedCommand(args, cfg, configured)
	if !bridged {
		return execSSH(args, stderr)
	}
	if err := bridge(cfg, remoteCommand, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "triagefactory git-ssh: %v\n", err)
		return 1
	}
	return 0
}

// bridgedCommand answers the dispatcher's one question — does this session
// belong on the run's proxy — and returns the remote command git asked for
// when it does.
//
// git always ends the argv with the destination and then the remote command,
// whatever options precede them, so the decision needs no option parsing. An
// argv shorter than that is not a git-driven invocation and has nothing to
// route on; an unconfigured env has nowhere to route it.
func bridgedCommand(args []string, cfg bridgeConfig, configured bool) (remoteCommand string, bridged bool) {
	if !configured || len(args) < 2 {
		return "", false
	}
	if !strings.EqualFold(hostOnly(args[len(args)-2]), cfg.upstreamHost) {
		return "", false
	}
	return args[len(args)-1], true
}

// hostOnly strips the "user@" git prepends to the destination. The user half
// is an ssh identity and says nothing about which host this is.
func hostOnly(dest string) string {
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		return dest[i+1:]
	}
	return dest
}

// execSSH replaces this process with the real ssh, so git keeps talking to a
// single process with the stdio, exit status and signal behavior it would have
// had if it had execed ssh itself. Returns only on failure to exec.
func execSSH(args []string, stderr io.Writer) int {
	path, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintf(stderr, "triagefactory git-ssh: ssh not found for a host this run does not manage: %v\n", err)
		return 127
	}
	argv := append([]string{"ssh"}, args...)
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "triagefactory git-ssh: exec ssh: %v\n", err)
		return 127
	}
	return 0 // unreachable: a successful Exec never returns
}
