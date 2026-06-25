package git

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// branchRefPrefix is the only ref namespace record-push records. A push
// can carry tags (refs/tags/*) or other refs; those aren't branches, so a
// `branch` artifact would mis-describe them. The hook forwards every
// non-delete ref and lets this verb filter — keeping the hook generic.
const branchRefPrefix = "refs/heads/"

// branchDetails is the kind-specific payload stored in the artifact's
// details_json. external_id pins the stable ref (the dedup anchor); the
// per-push facts — which commit landed, and whether this push created the
// branch — live here, so a re-push updates them in place on the one row.
type branchDetails struct {
	SHA string `json:"sha"`
	New bool   `json:"new,omitempty"`
}

// runRecordPush upserts a `branch` artifact for one pushed ref. Invoked by
// the pre-push hook, once per non-delete ref. Best-effort: a recording
// failure is reported on stderr and exits 0 so the hook (which also
// guards with `|| true`) never blocks the push. Only a malformed
// invocation (a bug in the hook) exits non-zero, to surface the bug.
func runRecordPush(host agenthost.Client, args []string) {
	fs := flag.NewFlagSet("git record-push", flag.ContinueOnError)
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
	if *remote == "" || *ref == "" {
		fmt.Fprintln(os.Stderr, "git record-push: --remote and --ref are required")
		os.Exit(2)
	}

	// Only branch refs become branch artifacts; skip tags and other refs
	// cleanly (the hook can't know the namespace policy). Not an error.
	branch, ok := strings.CutPrefix(*ref, branchRefPrefix)
	if !ok {
		return
	}

	owner, repo, ok := parseRemoteOwnerRepo(*remote)
	if !ok {
		// Unparseable remote (a non-GitHub host, a GHES layout we don't
		// model). Best-effort: nothing to anchor the artifact to, so warn
		// and exit 0 rather than failing the push.
		fmt.Fprintf(os.Stderr, "git record-push: could not parse owner/repo from remote %q; skipping\n", *remote)
		return
	}
	target := owner + "/" + repo

	details, err := json.Marshal(branchDetails{SHA: *sha, New: *isNew})
	if err != nil {
		// json.Marshal of this fixed shape can't realistically fail;
		// degrade to empty details rather than dropping the artifact.
		details = nil
	}

	artifact := domain.Artifact{
		Provider:    domain.ArtifactProviderGit,
		Kind:        domain.ArtifactKindBranch,
		Target:      target,
		ExternalID:  *ref,
		URL:         branchWebURL(owner, repo, branch),
		State:       domain.ArtifactStateBranchPushed,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderGit, domain.ArtifactKindBranch, target, *ref),
		DetailsJSON: string(details),
	}

	if _, err := host.UpsertArtifact(context.Background(), artifact); err != nil {
		fmt.Fprintf(os.Stderr, "git record-push: record branch %s: %v\n", *ref, err)
		return // best-effort: never fail the push
	}
}

// branchWebURL builds the GitHub web link for a branch. Always github.com
// — the artifact URL is a human-facing link, and in sandbox mode the
// remote URL is the per-run git proxy's address (not github.com), so
// deriving the host from it would be wrong. GHES web hosts aren't modeled
// (the product is GitHub-focused); a github.com link is the documented
// default.
func branchWebURL(owner, repo, branch string) string {
	return "https://github.com/" + owner + "/" + repo + "/tree/" + branch
}

// parseRemoteOwnerRepo extracts owner/repo from the remote URL git hands
// the pre-push hook. Handles the SCP-style and URL-style forms git emits,
// including the rewritten http://<proxy>/owner/repo form the sandbox git
// proxy produces. Returns ok=false for anything it can't resolve to
// exactly owner/repo (the caller treats that as best-effort skip).
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

	// SCP-style: [user@]host:owner/repo(.git)
	if !strings.Contains(remote, "://") {
		if colon := strings.LastIndex(remote, ":"); colon >= 0 {
			return splitOwnerRepoPath(remote[colon+1:])
		}
		return "", "", false
	}

	// URL-style: scheme://[user@]host[:port]/owner/repo(.git)
	rest := remote[strings.Index(remote, "://")+len("://"):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", false
	}
	return splitOwnerRepoPath(rest[slash+1:])
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
