package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// localCheckout describes the git checkout `pr diff` / `add-review-comment`
// resolve their diff and comment anchor from. The agent reads the PR through
// its worktree (CreateForPR checks out the PR head), so the commit it is
// actually looking at is the worktree HEAD — NOT the live PR head, which a
// concurrent push can move out from under it. Sourcing the diff, the comment
// validation, and the comment's anchor commit all from this one HEAD keeps the
// three in the same frame: a line the agent read maps to the same line GitHub
// anchors the submitted comment to.
//
// ok is false when the cwd isn't inside a git worktree (no checkout to anchor
// against) — callers fall back to the GitHub API path, which is internally
// consistent on its own (anchor + validation both off the live head).
type localCheckout struct {
	cwd     string
	headSHA string
	ok      bool
}

// resolveLocalCheckout returns the worktree HEAD for cwd, or ok=false when cwd
// isn't a git worktree. A detached/empty HEAD is treated as unusable so callers
// fall back to the API rather than anchoring to a meaningless ref.
func resolveLocalCheckout(cwd string) localCheckout {
	if cwd == "" {
		return localCheckout{}
	}
	if err := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return localCheckout{}
	}
	head, err := gitOutput(cwd, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return localCheckout{}
	}
	return localCheckout{cwd: cwd, headSHA: head, ok: true}
}

// gitOutput runs `git -C cwd <args...>` and returns trimmed stdout.
func gitOutput(cwd string, args ...string) (string, error) {
	full := append([]string{"-C", cwd}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveBaseCommit resolves the PR's base ref to a commit the local bare knows
// about. The blobless bare clone fetched every branch at clone time, so the
// base branch is usually present even though only the PR head was fetched for
// this run. Tries the remote-tracking ref first (most likely fresh), then the
// local branch, then the bare name verbatim. Returns ok=false when none
// resolve — the base branch was created after the clone, or pruned — and the
// caller falls back to the API diff.
func resolveBaseCommit(cwd, baseRef string) (string, bool) {
	if baseRef == "" {
		return "", false
	}
	for _, cand := range []string{
		"origin/" + baseRef,
		"refs/remotes/origin/" + baseRef,
		"refs/heads/" + baseRef,
		baseRef,
	} {
		if sha, err := gitOutput(cwd, "rev-parse", "--verify", "--quiet", cand+"^{commit}"); err == nil && sha != "" {
			return sha, true
		}
	}
	return "", false
}

// resolveDiffBase picks the commit the PR diff is framed against, the way GitHub
// frames a PR diff: the PR's recorded base commit (base.sha). That commit is the
// authoritative base — diffing it three-dot against HEAD reproduces GitHub's own
// file set, and it's immune to a stale local base-branch tracking ref.
//
// Resolution order:
//   - recorded baseSHA present in the local object store → use it.
//   - recorded baseSHA recorded but NOT present locally (the base branch was
//     never fetched into this worktree, or it advanced past what was fetched) →
//     ok=false, so the caller uses the API diff. We deliberately do NOT fall
//     back to resolving the base branch *name*: a clone-time-frozen
//     origin/<base> that predates the PR's branch point is exactly what replayed
//     an already-merged commit's changes as phantom hunks (TFAC-505). The API
//     diff frames against the true base server-side.
//   - no recorded baseSHA (host didn't populate base.sha) → best-effort resolve
//     the base branch's tracking ref, which CreateForPR now refreshes at
//     materialization (WithBaseBranch).
func resolveDiffBase(cwd, baseSHA, baseRef string) (string, bool) {
	if baseSHA != "" {
		if sha, err := gitOutput(cwd, "rev-parse", "--verify", "--quiet", baseSHA+"^{commit}"); err == nil && sha != "" {
			return sha, true
		}
		return "", false
	}
	return resolveBaseCommit(cwd, baseRef)
}

// localUnifiedDiff returns the worktree-HEAD-framed unified diff for the PR:
// `git diff <base>...HEAD`, the three-dot (merge-base) form GitHub itself uses
// for a PR diff. When file != "", scopes to that single path.
//
// baseCommit MUST be the PR's true base (the recorded base.sha, via
// resolveDiffBase) — not a base branch ref resolved by name. The three-dot
// merge-base is only the PR's branch point when baseCommit is at or ahead of it;
// a base ref that has fallen BEHIND the branch point (a clone-time-frozen
// origin/<base>) collapses the merge-base to that stale ancestor and replays
// every intervening already-merged commit as a phantom hunk (TFAC-505).
func localUnifiedDiff(cwd, baseCommit, file string) (string, error) {
	args := []string{"diff", "--no-color", "-M", baseCommit + "...HEAD"}
	if file != "" {
		args = append(args, "--", file)
	}
	return gitOutput(cwd, args...)
}

// parseDiffSummaries derives the per-file manifest rows from a unified diff in a
// single pass, so the summary and the full.diff the agent reads come from one
// source of truth (the local checkout) rather than a separate API listing that
// could describe a different commit. Status/rename/binary come from the git
// extended-header lines; additions/deletions are counted off the +/- body.
func parseDiffSummaries(diff string) []fileSummary {
	var out []fileSummary
	var cur *fileSummary
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &fileSummary{Path: diffHeaderNewPath(line), Status: "modified"}
		case cur == nil:
			// preamble before the first file header — ignore
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "removed"
		case strings.HasPrefix(line, "rename from "):
			cur.Status = "renamed"
			cur.PreviousFilename = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			cur.Status = "renamed"
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "Binary files "):
			cur.Binary = true
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			// file markers, not content — never counted
		case strings.HasPrefix(line, "+"):
			cur.Additions++
		case strings.HasPrefix(line, "-"):
			cur.Deletions++
		}
	}
	flush()
	return out
}

// diffHeaderNewPath extracts the b/ path from a `diff --git a/<old> b/<new>`
// header. Falls back to the a/ path when the b/ side can't be isolated (e.g. a
// path with " b/" embedded), which still names the file for the listing. The
// rename-to header, when present, overrides this with the exact new path.
func diffHeaderNewPath(header string) string {
	rest := strings.TrimPrefix(header, "diff --git ")
	if i := strings.Index(rest, " b/"); i >= 0 {
		return rest[i+len(" b/"):]
	}
	return strings.TrimPrefix(strings.Fields(rest)[0], "a/")
}

// remoteAheadBy reports how many commits the live PR head (remoteHead) is ahead
// of the local checkout (localHead) via the compare API's ahead_by. Returns 0
// when the SHAs match (the common, fresh case — no API call) or when the
// compare can't be fetched/parsed (best-effort staleness, never fatal to a
// diff). The scoped client folds owner/repo in for the host's credential tier.
func remoteAheadBy(ctx context.Context, client ghAPI, owner, repo, localHead, remoteHead string) int {
	if localHead == "" || remoteHead == "" || localHead == remoteHead {
		return 0
	}
	path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", owner, repo, localHead, remoteHead)
	data, err := client.Get(ctx, path)
	if err != nil {
		return 0
	}
	var cmp struct {
		AheadBy int `json:"ahead_by"`
	}
	if json.Unmarshal(data, &cmp) != nil {
		return 0
	}
	return cmp.AheadBy
}
