package domain

import (
	"encoding/json"
	"net/url"
	"strings"
)

// branchRefPrefix is the ref namespace a branch push records. A push can carry
// tags (refs/tags/*) or other refs; recording those as a branch would
// mis-describe them, so NewBranchPushAction filters to this prefix.
const branchRefPrefix = "refs/heads/"

// The states a branch's ledger rows move it between: a push leaves it pushed,
// a rejection's delete leaves it deleted.
const (
	BranchStatePushed  = "pushed"
	BranchStateDeleted = "deleted"
)

// branchPushDetails is a branch_pushed row's detail_json: which commit
// landed, and whether the push created the branch.
type branchPushDetails struct {
	SHA string `json:"sha"`
	New bool   `json:"new,omitempty"`
}

// NewBranchPushAction builds the branch_pushed ledger row for one pushed ref,
// as far as the push alone says it: repoPath is the GitHub "owner/repo" the
// push named, ref the full remote ref (refs/heads/...), sha the commit the ref
// now points to, and created whether the push created the ref. Its object is
// the branch, keyed on the ref.
//
// It returns ok=false — nothing is recorded — when ref is not a branch (a tag
// or other namespace) or repoPath is not exactly owner/repo. The row carries no
// identity, repository, URL, credential or dedup key: both observers of a push
// (the pre-push hook and the git proxy) build it where none of those is known,
// and the host-side recorder sets them.
func NewBranchPushAction(repoPath, ref, sha string, created bool) (ExternalAction, bool) {
	if !IsBranchPush(repoPath, ref) {
		return ExternalAction{}, false
	}
	// This fixed shape can't realistically fail to marshal; an error leaves
	// the row with no detail rather than dropping it.
	details, _ := json.Marshal(branchPushDetails{SHA: sha, New: created})
	act := ExternalAction{
		Provider:   ProviderGitHub,
		Action:     ActionBranchPushed,
		Target:     repoPath,
		ExternalID: ref,
		ToState:    BranchStatePushed,
		DetailJSON: string(details),
	}
	act.SetObject(ObjectKindBranch, ref)
	return act, true
}

// IsBranchPush reports whether a push of ref to repoPath is one a branch_pushed
// row records: ref is a branch (refs/heads/...) and repoPath is exactly
// owner/repo.
func IsBranchPush(repoPath, ref string) bool {
	return strings.HasPrefix(ref, branchRefPrefix) && validOwnerRepo(repoPath)
}

// BranchWebURL builds the web link for a branch ref on a GitHub host base —
// github.com, a GHES host, or a GHEC data-residency host. base is the org's
// GitHub host (its scope); an empty base names no GitHub and builds no link.
// ok=false when ref is not a branch (a tag or other namespace) or repoPath is
// not exactly owner/repo, so the caller records no link rather than a
// malformed one.
func BranchWebURL(base, repoPath, ref string) (string, bool) {
	branch, ok := strings.CutPrefix(ref, branchRefPrefix)
	if !ok || !validOwnerRepo(repoPath) {
		return "", false
	}
	return branchWebURL(base, repoPath, branch), true
}

// ParseBranchPushSHA extracts the pushed commit SHA from a branch_pushed row's
// detail_json (the {"sha":...,"new":...} payload), or "" when it's absent or
// unparseable.
func ParseBranchPushSHA(detailJSON string) string {
	if strings.TrimSpace(detailJSON) == "" {
		return ""
	}
	var d branchPushDetails
	if err := json.Unmarshal([]byte(detailJSON), &d); err != nil {
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
// derived from the push — a sandbox git remote rewritten to the per-run proxy
// address is not a usable web host — so it is passed in by the host-side
// caller that resolved it.
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
