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
// malformed or multi-segment path we won't anchor a link to). The artifact
// carries no URL and no dedup key: both writers build it where neither the
// org's host nor the repository's registry row is known, so the host-side sink
// that writes it sets the link (BranchArtifactWebURL) and keys it on the
// registry row (BranchDedupKey).
//
// This is the single source of truth for the branch-artifact shape. Both
// capture writers build through it: the pre-push hook (TFAC-460) and the
// git-proxy receive-pack backstop (TFAC-467). Sharing the constructor and the
// sink — rather than each writer building a struct by hand — is what
// guarantees they land on the same deduped row: identical DedupKey, URL, and
// DetailsJSON, so a normal hook+push is never double-recorded and the proxy
// only adds rows the hook missed (a `git push --no-verify`).
func NewBranchArtifact(repoPath, ref, sha string, created bool) (Artifact, bool) {
	if !strings.HasPrefix(ref, branchRefPrefix) || !validOwnerRepo(repoPath) {
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
		State:       ArtifactStateBranchPushed,
		DetailsJSON: string(details),
	}, true
}

// BranchDedupKey is the key a branch artifact is recorded under:
// git:branch:<repository id>:<ref>, where the repository id is the registry
// row's. A branch has no provider id, and the registry row is what TF holds
// for its repository: a rename moves the row's name and not its id, and a
// repository created under a freed name gets a row of its own. "" when either
// half is empty.
func BranchDedupKey(repositoryID, ref string) string {
	if repositoryID == "" || ref == "" {
		return ""
	}
	return BranchDedupKeyPrefix(repositoryID) + ref
}

// BranchDedupKeyPrefix is what the key of every branch recorded in the
// repository whose registry row id is repositoryID begins with: a read for one
// repository's branches matches it. "" for an empty id.
func BranchDedupKeyPrefix(repositoryID string) string {
	if repositoryID == "" {
		return ""
	}
	return ArtifactDedupKey(ArtifactProviderGit, ArtifactKindBranch, repositoryID, "") + ":"
}

// BranchArtifactWebURL builds the web link for a pushed branch ref on a specific
// GitHub host base — github.com, a GHES host, or a GHEC data-residency host.
// base is the org's user-facing web base (as reported by the credential
// resolver's BaseURLFor); an empty base names no GitHub and builds no link.
//
// It exists because a branch artifact is built where the org's host isn't
// knowable — the sandbox pre-push hook has no credentials, and
// NewBranchArtifact has no org context — so the host-side artifact sink sets
// the link here once the base resolves. ok=false when ref is not a branch (a
// tag or other namespace) or repoPath is not exactly owner/repo, so the sink
// leaves the existing URL untouched rather than clobber it with a malformed
// link.
func BranchArtifactWebURL(base, repoPath, ref string) (string, bool) {
	branch, ok := strings.CutPrefix(ref, branchRefPrefix)
	if !ok || !validOwnerRepo(repoPath) {
		return "", false
	}
	return branchWebURL(base, repoPath, branch), true
}

// ParseBranchArtifactSHA extracts the pushed commit SHA from a branch artifact's
// DetailsJSON (the {"sha":...,"new":...} payload), or "" when it's absent or
// unparseable. The external-action audit log keys a branch push on
// (conversation, ref, sha) — the ref is the artifact's ExternalID and the sha lives here —
// so the git hook+proxy twin (identical sha) collapses to one row while a new
// push (different sha) is recorded distinctly. Best-effort: an empty sha just
// yields a less-specific dedup key, never a dropped audit row.
func ParseBranchArtifactSHA(detailsJSON string) string {
	if strings.TrimSpace(detailsJSON) == "" {
		return ""
	}
	var d branchDetails
	if err := json.Unmarshal([]byte(detailsJSON), &d); err != nil {
		return ""
	}
	return d.SHA
}

// validOwnerRepo reports whether repoPath is exactly two non-empty segments
// (owner/repo). A single segment, a trailing slash, or a nested path (GHES
// /scm/..., nested GitLab groups) is rejected rather than guessed.
func validOwnerRepo(repoPath string) bool {
	parts := strings.Split(repoPath, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// branchWebURL builds the web link for a branch on the given GitHub host base
// (github.com or a GHES/GHEC host), or "" for an empty base. The base can't be
// derived from the artifact's source — a sandbox git remote rewritten to the
// per-run proxy address is not a usable web host — so it is passed in by the
// host-side caller that resolved it.
//
// Each branch path segment is URL-escaped: a branch name can legally contain
// '#', '%', '?', space, etc., which would otherwise break the link (a '#' would
// start a fragment). '/' separators are preserved (feature/x maps to
// .../tree/feature/x), so escaping is per-segment. owner/repo are already
// validated clean path segments, so they are not re-escaped.
func branchWebURL(base, repoPath, branch string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return base + "/" + repoPath + "/tree/" + strings.Join(segs, "/")
}
