package hook

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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

	owner, repo, ok := parseRemoteOwnerRepo(*remote)
	if !ok {
		// Unparseable remote (a non-GitHub host, a GHES layout we don't
		// model). Best-effort: nothing to anchor the artifact to, so warn
		// and return rather than failing the push.
		fmt.Fprintf(os.Stderr, "hook record-push: could not parse owner/repo from remote %q; skipping\n", *remote)
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

	// Bound the write so the hook can never block the push indefinitely.
	// The IPC path already has its own 30s call timeout; this also caps the
	// local SQLite path, where a wedged write (WAL lock, full disk) would
	// otherwise hold git open with no backstop. On timeout the upsert errors,
	// we report and return — best-effort, the push still proceeds.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := host.UpsertArtifact(ctx, artifact); err != nil {
		fmt.Fprintf(os.Stderr, "hook record-push: record branch %s: %v\n", *ref, err)
		return // best-effort: never fail the push
	}
}

// parseRemoteOwnerRepo extracts owner/repo from the remote URL git hands
// the pre-push hook. Handles the SCP-style and URL-style forms git emits,
// including the rewritten http://<proxy>/owner/repo form the sandbox git
// proxy produces. Returns ok=false for anything it can't resolve to
// exactly owner/repo on a recognized host (the caller treats that as a
// best-effort skip).
//
// Host gating: the artifact URL is hardcoded to github.com, so a non-GitHub
// remote (gitlab.com/group/repo) must NOT be recorded — it would mint a
// wrong github.com link for someone else's repo. Accepted hosts are
// github.com itself and any loopback/private IP (the sandbox git proxy's
// veth address, where the real upstream was github.com). A GHES/other public
// host is skipped rather than mis-recorded — correct-web-host GHES support is
// explicitly out of scope (see branchWebURL).
//
// A deliberately small, self-contained parser rather than importing
// cmd/exec/gh's unexported equivalent: gh is a heavy subcommand package
// and exporting its helper to share ~20 lines would widen its API and
// invert the dependency. If a third copy appears, that's the rule-of-three
// trigger to extract a shared leaf.
func parseRemoteOwnerRepo(remote string) (owner, repo string, ok bool) {
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

	if !isGitHubOrProxyHost(host) {
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

// isGitHubOrProxyHost reports whether host (a possibly host:port authority)
// is one record-push trusts to be github.com: literally github.com, or a
// loopback/private IP — the sandbox git proxy's veth address, which fronts
// a github.com upstream. Any other public host (gitlab.com, a GHES domain)
// is rejected so its pushes aren't mis-recorded under a github.com URL.
func isGitHubOrProxyHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "github.com" {
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
