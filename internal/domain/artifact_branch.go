package domain

import (
	"encoding/json"
	"net/url"
	"strings"
)

// branchRefPrefix is the ref namespace a branch artifact records. A push can
// carry tags (refs/tags/*) or other refs; recording those as a `branch` would
// mis-describe them, so NewBranchArtifact filters to this prefix.
const branchRefPrefix = "refs/heads/"

// branchDetails is the kind-specific payload in a branch artifact's
// DetailsJSON. The dedup anchor is the ref (the stable key); the per-push
// facts — which commit landed, and whether the push created the branch — live
// here, so a re-push updates them in place on the one deduped row.
type branchDetails struct {
	SHA string `json:"sha"`
	New bool   `json:"new,omitempty"`
}

// NewBranchArtifact builds the durable `branch` artifact for one pushed ref.
// repoPath is the GitHub "owner/repo"; ref is the full remote ref
// (refs/heads/...); sha is the commit the ref now points to; created is true
// when the push created the ref (the remote held no prior value for it).
//
// It returns ok=false — the caller skips, no artifact — when ref is not a
// branch (a tag or other namespace) or repoPath is not exactly owner/repo (a
// malformed or multi-segment path we won't anchor a github.com link to).
//
// This is the single source of truth for the branch-artifact shape. Both
// capture writers build through it: the pre-push hook (TFAC-460) and the
// git-proxy receive-pack backstop (TFAC-467). Sharing the constructor — rather
// than each writer building a struct by hand — is what guarantees they land on
// the same deduped row: identical DedupKey, URL, and DetailsJSON, so a normal
// hook+push is never double-recorded and the proxy only adds rows the hook
// missed (a `git push --no-verify`).
func NewBranchArtifact(repoPath, ref, sha string, created bool) (Artifact, bool) {
	branch, ok := strings.CutPrefix(ref, branchRefPrefix)
	if !ok {
		return Artifact{}, false
	}
	repoPath = strings.TrimSuffix(repoPath, "/")
	if !validOwnerRepo(repoPath) {
		return Artifact{}, false
	}

	details, err := json.Marshal(branchDetails{SHA: sha, New: created})
	if err != nil {
		// This fixed shape can't realistically fail to marshal; degrade to
		// empty details rather than dropping the artifact entirely.
		details = nil
	}

	return Artifact{
		Provider:    ArtifactProviderGit,
		Kind:        ArtifactKindBranch,
		Target:      repoPath,
		ExternalID:  ref,
		URL:         branchWebURL(repoPath, branch),
		State:       ArtifactStateBranchPushed,
		DedupKey:    ArtifactDedupKey(ArtifactProviderGit, ArtifactKindBranch, repoPath, ref),
		DetailsJSON: string(details),
	}, true
}

// validOwnerRepo reports whether repoPath is exactly two non-empty segments
// (owner/repo). A single segment, a trailing slash, or a nested path (GHES
// /scm/..., nested GitLab groups) is rejected rather than guessed.
func validOwnerRepo(repoPath string) bool {
	parts := strings.Split(repoPath, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// branchWebURL builds the GitHub web link for a branch. Always github.com: the
// URL is a human-facing link, and the artifact's source (a sandbox git remote
// rewritten to the per-run proxy address) is not a usable web host, so the host
// is never derived from it. GHES web hosts aren't modeled — the product is
// GitHub-focused and a github.com link is the documented default.
//
// Each branch path segment is URL-escaped: a branch name can legally contain
// '#', '%', '?', space, etc., which would otherwise break the link (a '#' would
// start a fragment). '/' separators are preserved (feature/x maps to
// .../tree/feature/x), so escaping is per-segment. owner/repo are already
// validated clean path segments, so they are not re-escaped.
func branchWebURL(repoPath, branch string) string {
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "https://github.com/" + repoPath + "/tree/" + strings.Join(segs, "/")
}
