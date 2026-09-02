package hook

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// runRecordPush upserts a `branch` artifact for one pushed ref. Invoked by
// the pre-push hook, once per non-delete ref. Best-effort: a recording
// failure is reported on stderr and returns so the hook (which also guards
// with `|| true`) never blocks the push. Only a malformed invocation (a bug
// in the hook) exits non-zero, to surface the bug.
func runRecordPush(host agenthost.Client, args []string) {
	fs := flag.NewFlagSet("hook record-push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		remote = fs.String("remote", "", "remote URL being pushed to (git's $2)")
		ref    = fs.String("ref", "", "remote ref being updated, e.g. refs/heads/feature")
		sha    = fs.String("sha", "", "local commit SHA being pushed")
		isNew  = fs.Bool("new", false, "true when the branch did not exist on the remote before this push")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(2) // flag already printed the error
	}
	if *remote == "" || *ref == "" || *sha == "" {
		// --sha is required, not just --remote/--ref: the commit SHA is part
		// of the durable "what landed" record (it's the branch's pushed head
		// in details_json), so an empty one would store a meaningless
		// artifact. The hook always supplies it for a real push; an empty
		// value means a malformed invocation.
		fmt.Fprintln(os.Stderr, "hook record-push: --remote, --ref, and --sha are required")
		os.Exit(2)
	}

	// Bound the whole callback so the hook can never block the push
	// indefinitely. The IPC path already has its own 30s call timeout; this also
	// caps the local SQLite reads (base resolve) and the write, where a wedged
	// operation (WAL lock, full disk) would otherwise hold git open with no
	// backstop. On timeout the read/write errors, we report and return —
	// best-effort, the push still proceeds.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Gate the remote on the org's OWN GitHub host — github.com, a GHES host, or
	// a GHEC data-residency host — so a push to an unrelated remote (gitlab, a
	// personal fork elsewhere) isn't recorded as an org branch artifact. The
	// artifact sink re-anchors the recorded link to this same host, so a GHES
	// org's branch link is correct end-to-end; the two resolve identically.
	// An unresolvable base (orgGitHubHost returns "") falls back to a
	// github.com-only gate, preserving the historical behavior on a read miss.
	owner, repo, ok := parseRemoteOwnerRepo(*remote, orgGitHubHost(ctx, host))
	if !ok {
		// A remote that isn't the org's GitHub host, or an unparseable one.
		// Best-effort: skip rather than fail the push.
		fmt.Fprintf(os.Stderr, "hook record-push: remote %q is not the org's GitHub host (or unparseable); skipping\n", *remote)
		return
	}

	// Build through the shared constructor so the hook and the git-proxy
	// receive-pack backstop (TFAC-467) land on the same deduped row. A
	// non-branch ref (tag, etc.) returns ok=false — skip it cleanly, since
	// the hook forwards every non-delete ref and can't know the namespace
	// policy. Not an error.
	artifact, ok := domain.NewBranchArtifact(owner+"/"+repo, *ref, *sha, *isNew)
	if !ok {
		return
	}

	if _, err := host.UpsertArtifact(ctx, artifact); err != nil {
		fmt.Fprintf(os.Stderr, "hook record-push: record branch %s: %v\n", *ref, err)
		return // best-effort: never fail the push
	}
}

// orgHostResolver is the subset of the host client the pre-push hook needs to
// learn the org's GitHub host. Only the in-process LocalClient (local mode)
// implements it — the sandbox hook stands down before reaching here (the
// pre-push script exits on TF_GIT_PUSH_CAPTURE=proxy), so the IPC client never
// needs it.
type orgHostResolver interface {
	GithubWebHostBase(ctx context.Context) (string, error)
}

// orgGitHubHost resolves the org's GitHub web host (github.com, a GHES host, or
// a GHEC data-residency host) for the remote gate, or "" when it can't be
// resolved — a host client that doesn't expose it (the sandbox IPC path, which
// never records here) or a base read failure. "" makes parseRemoteOwnerRepo
// fall back to a github.com-only gate rather than reject every remote.
func orgGitHubHost(ctx context.Context, host agenthost.Client) string {
	whr, ok := host.(orgHostResolver)
	if !ok {
		return ""
	}
	base, err := whr.GithubWebHostBase(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook record-push: resolve org github host: %v; falling back to the deployment default host gate\n", err)
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// parseRemoteOwnerRepo extracts owner/repo from the remote URL git hands
// the pre-push hook. Handles the SCP-style and URL-style forms git emits,
// including the rewritten http://<proxy>/owner/repo form the sandbox git
// proxy produces. Returns ok=false for anything it can't resolve to
// exactly owner/repo on a recognized host (the caller treats that as a
// best-effort skip).
//
// Host gating: a branch artifact is a GitHub entity, so a remote that isn't the
// org's GitHub host (gitlab.com/group/repo, a personal fork elsewhere) must NOT
// be recorded. baseHost is the org's resolved GitHub host — github.com, a GHES
// host, or a GHEC data-residency host; a remote is accepted when its host
// matches it (or is a loopback/private proxy address). An empty baseHost (the
// org base couldn't be resolved) falls back to a github.com-only gate. See
// isTrustedHost.
//
// A deliberately small, self-contained parser rather than importing
// cmd/exec/gh's unexported equivalent: gh is a heavy subcommand package
// and exporting its helper to share ~20 lines would widen its API and
// invert the dependency. If a third copy appears, that's the rule-of-three
// trigger to extract a shared leaf.
func parseRemoteOwnerRepo(remote, baseHost string) (owner, repo string, ok bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", "", false
	}

	var host, path string
	if !strings.Contains(remote, "://") {
		// SCP-style: [user@]host:owner/repo(.git)
		colon := strings.LastIndex(remote, ":")
		if colon < 0 {
			return "", "", false
		}
		host, path = stripUserInfo(remote[:colon]), remote[colon+1:]
	} else {
		// URL-style: scheme://[user@]host[:port]/owner/repo(.git)
		rest := remote[strings.Index(remote, "://")+len("://"):]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return "", "", false
		}
		host, path = stripUserInfo(rest[:slash]), rest[slash+1:]
	}

	if !isTrustedHost(host, baseHost) {
		return "", "", false
	}
	return splitOwnerRepoPath(path)
}

// stripUserInfo drops a leading "user@" from a host authority.
func stripUserInfo(host string) string {
	if at := strings.LastIndex(host, "@"); at >= 0 {
		return host[at+1:]
	}
	return host
}

// isTrustedHost reports whether host (a possibly host:port authority) is one
// record-push trusts to be the org's GitHub. baseHost is the org's resolved
// GitHub host:
//
//   - a case-insensitive match against baseHost — the org's own host, whether
//     that's github.com, a GHES host, or a GHEC data-residency host — is trusted;
//   - a loopback/private IP is trusted as the sandbox git proxy's veth address
//     (which fronts the org upstream). The sandbox hook stands down in favor of
//     the proxy's own capture, so this is a belt-and-suspenders accept;
//   - when baseHost is empty (the org base couldn't be resolved), it falls back
//     to the deployment's default GitHub — the host an org with no base URL is
//     on — so a transient resolver miss doesn't silently stop recording pushes
//     there.
//
// Anything else (gitlab.com, a github.com push from a GHES org) is rejected so
// its pushes aren't recorded — and, post-anchoring, mis-linked — under the org's
// host.
func isTrustedHost(host, baseHost string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if baseHost == "" {
		// No org host resolved: the gate is the deployment's default GitHub,
		// the host an org with no base URL is on.
		if u, err := url.Parse(ghbase.DefaultBaseURL()); err == nil {
			baseHost = u.Hostname()
		}
	}
	if baseHost != "" && strings.EqualFold(host, baseHost) {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return false
}

// splitOwnerRepoPath takes the path portion after the host and resolves it
// to exactly two segments (owner/repo), tolerating a trailing slash and the
// .git suffix. Multi-segment paths (GHES /scm/..., nested GitLab groups)
// are rejected rather than guessed — matching cmd/exec/gh's parser.
func splitOwnerRepoPath(path string) (owner, repo string, ok bool) {
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
