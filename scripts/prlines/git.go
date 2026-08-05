package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type repo struct {
	root string
}

func openRepo() (*repo, error) {
	out, err := runCmd(context.Background(), 15*time.Second, "", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a git work tree: %w", err)
	}
	return &repo{root: strings.TrimSpace(out)}, nil
}

func (r *repo) git(args ...string) (string, error) {
	return r.gitTimeout(60*time.Second, args...)
}

func (r *repo) gitTimeout(d time.Duration, args ...string) (string, error) {
	return runCmd(context.Background(), d, r.root, "git", args...)
}

// gitOK reports whether a command succeeded, for the many probes here whose
// failure is an answer rather than an error.
func (r *repo) gitOK(args ...string) bool {
	_, err := r.git(args...)
	return err == nil
}

func runCmd(ctx context.Context, d time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%s %s: timed out after %s", name, strings.Join(args, " "), d)
		}
		if msg != "" {
			return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// ---------------------------------------------------------------------------
// Base resolution
// ---------------------------------------------------------------------------

// baseRef is where the diff starts, and how we decided that.
type baseRef struct {
	Ref    string // a ref this repo can resolve, e.g. "origin/main"
	Source string // human-readable provenance, printed in the run header
}

// resolveBase walks the four — and only four — places a PR's base can come
// from, in descending order of authority:
//
//  1. --base, because an explicit answer ends the discussion.
//  2. The open pull request for this branch, read via gh. This is the only
//     source that is authoritative rather than inferred: it is literally the
//     ref GitHub will diff against, so it gets stacked branches right without
//     anyone having to tell it anything.
//  3. branch.<name>.tfBase in git config — the offline escape hatch, for a
//     stacked branch whose PR does not exist yet.
//  4. The repository's default branch, from refs/remotes/<remote>/HEAD, or
//     from gh, or by probing the conventional names.
//
// The chain is exhaustive because the question "what will this be diffed
// against" has exactly one genuinely ambiguous answer: a branch with no PR
// that was not cut from the default branch. Steps 2 and 3 exist to remove that
// case, and when both are absent step 4 refuses to guess between two candidate
// default branches rather than silently picking one.
func resolveBase(r *repo, opts options, branch string) (baseRef, []string, error) {
	var warns []string

	if opts.base != "" {
		if !r.gitOK("rev-parse", "--verify", "--quiet", opts.base+"^{commit}") {
			return baseRef{}, warns, fmt.Errorf("--base %q does not resolve to a commit", opts.base)
		}
		return baseRef{Ref: opts.base, Source: "--base"}, warns, nil
	}

	configured := ""
	if branch != "" {
		if v, err := r.git("config", "--get", "branch."+branch+".tfBase"); err == nil {
			configured = strings.TrimSpace(v)
		}
	}

	if branch != "" && !opts.noGH {
		pr, err := lookupPR(r, branch)
		switch {
		case err != nil:
			warns = append(warns, fmt.Sprintf("gh pull-request lookup failed (%v); falling back", err))
		case pr != nil:
			ref, err := r.remoteRefFor(opts.remote, pr.BaseRefName)
			if err != nil {
				warns = append(warns, fmt.Sprintf("PR #%d bases on %q, which this clone has no ref for; falling back", pr.Number, pr.BaseRefName))
				break
			}
			if pr.IsCrossRepository {
				warns = append(warns, fmt.Sprintf("PR #%d is from a fork; %s is this clone's copy of the base branch and may not be the upstream's — pass --base if the counts look wrong", pr.Number, ref))
			}
			if configured != "" && configured != ref {
				warns = append(warns, fmt.Sprintf("branch.%s.tfBase is %q but PR #%d bases on %q; using the PR", branch, configured, pr.Number, ref))
			}
			src := fmt.Sprintf("open PR #%d", pr.Number)
			if pr.State != "" && pr.State != "OPEN" {
				src = fmt.Sprintf("PR #%d (%s)", pr.Number, strings.ToLower(pr.State))
			}
			return baseRef{Ref: ref, Source: src}, warns, nil
		}
	}

	if configured != "" {
		if !r.gitOK("rev-parse", "--verify", "--quiet", configured+"^{commit}") {
			return baseRef{}, warns, fmt.Errorf("branch.%s.tfBase is %q, which does not resolve to a commit", branch, configured)
		}
		return baseRef{Ref: configured, Source: "branch." + branch + ".tfBase"}, warns, nil
	}

	ref, src, err := r.defaultBranch(opts)
	if err != nil {
		return baseRef{}, warns, err
	}
	return baseRef{Ref: ref, Source: src}, warns, nil
}

// defaultBranch resolves the fallback base. refs/remotes/<remote>/HEAD is the
// only local, authoritative answer; it is unset in plenty of clones (including
// every shallow CI checkout), which is why the probe list exists — and why an
// ambiguous probe is an error rather than a coin flip.
func (r *repo) defaultBranch(opts options) (string, string, error) {
	if out, err := r.git("symbolic-ref", "--quiet", "--short", "refs/remotes/"+opts.remote+"/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return ref, "default branch via refs/remotes/" + opts.remote + "/HEAD", nil
		}
	}

	if !opts.noGH {
		if name, err := ghDefaultBranch(r); err == nil && name != "" {
			if ref, err := r.remoteRefFor(opts.remote, name); err == nil {
				return ref, "default branch via gh (run `git remote set-head " + opts.remote + " -a` to cache it)", nil
			}
		}
	}

	var found []string
	for _, name := range []string{"main", "master", "trunk", "develop"} {
		if ref, err := r.remoteRefFor(opts.remote, name); err == nil {
			found = append(found, ref)
		}
	}
	switch len(found) {
	case 1:
		return found[0], "only conventional default branch present", nil
	case 0:
		return "", "", fmt.Errorf("cannot determine the base branch: %s has no HEAD and no conventional default branch; pass --base", opts.remote)
	default:
		return "", "", fmt.Errorf("cannot determine the base branch: %s has several candidates (%s); run `git remote set-head %s -a` or pass --base",
			opts.remote, strings.Join(found, ", "), opts.remote)
	}
}

func (r *repo) remoteRefFor(remote, branch string) (string, error) {
	ref := remote + "/" + branch
	if r.gitOK("rev-parse", "--verify", "--quiet", ref+"^{commit}") {
		return ref, nil
	}
	return "", fmt.Errorf("no such ref: %s", ref)
}

// resolveRemote picks the remote to read base branches from: the requested one,
// else origin, else the sole remote if there is exactly one.
func (r *repo) resolveRemote(requested string) (string, error) {
	out, err := r.git("remote")
	if err != nil {
		return "", err
	}
	var remotes []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			remotes = append(remotes, l)
		}
	}
	if requested != "" {
		for _, rm := range remotes {
			if rm == requested {
				return rm, nil
			}
		}
		return "", fmt.Errorf("no remote named %q (have: %s)", requested, strings.Join(remotes, ", "))
	}
	for _, rm := range remotes {
		if rm == "origin" {
			return "origin", nil
		}
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	if len(remotes) == 0 {
		return "", errors.New("this clone has no remotes; pass --base with a local ref")
	}
	return "", fmt.Errorf("several remotes and none named origin (%s); pass --remote", strings.Join(remotes, ", "))
}

// fetchBase refreshes the base's remote-tracking ref. A stale one is the second
// way to get the wrong merge base: if the branch was cut from a commit this
// clone has not seen on the base ref, the merge base walks back further than it
// should and the diff picks up other people's work. Best effort — being offline
// costs accuracy, not a run.
func (r *repo) fetchBase(remote, ref string) error {
	prefix := remote + "/"
	if !strings.HasPrefix(ref, prefix) {
		return nil // a local ref or a raw sha; nothing to refresh
	}
	_, err := r.gitTimeout(30*time.Second, "fetch", "--quiet", "--no-tags", remote, strings.TrimPrefix(ref, prefix))
	return err
}

// mergeBaseOf returns the commit GitHub's "Files changed" view diffs from.
// Criss-cross history can produce more than one; git and GitHub both pick a
// single best one, so the extras are reported rather than silently dropped.
func (r *repo) mergeBaseOf(base, head string) (string, int, error) {
	out, err := r.git("merge-base", "--all", base, head)
	if err != nil {
		return "", 0, fmt.Errorf("no common ancestor between %s and %s: %w", base, head, err)
	}
	var bases []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			bases = append(bases, l)
		}
	}
	if len(bases) == 0 {
		return "", 0, fmt.Errorf("no common ancestor between %s and %s", base, head)
	}
	return bases[0], len(bases), nil
}

func (r *repo) currentBranch() string {
	out, err := r.git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "" // detached HEAD
	}
	return strings.TrimSpace(out)
}

// branchForRef names the local branch a head ref points at, which is what the
// per-branch base sources key off. It is empty for a detached HEAD, a raw sha,
// a remote-tracking ref or a revision expression — none of which name a branch
// whose pull request or tfBase could be looked up, so base resolution falls
// through to the default branch rather than answering for some other branch.
func (r *repo) branchForRef(ref string) string {
	if ref == "" || ref == "HEAD" {
		return r.currentBranch()
	}
	if r.gitOK("show-ref", "--verify", "--quiet", "refs/heads/"+ref) {
		return ref
	}
	return ""
}

func (r *repo) resolveCommit(rev string) (string, error) {
	out, err := r.git("rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (r *repo) describe(rev string) string {
	out, err := r.git("show", "--no-patch", "--format=%h %ad %s", "--date=short", rev)
	if err != nil {
		return rev
	}
	return strings.TrimSpace(out)
}

func (r *repo) isDirty() bool {
	out, err := r.git("status", "--porcelain", "--untracked-files=no")
	return err == nil && strings.TrimSpace(out) != ""
}

// diff runs the comparison the counts are built from. Every knob git exposes
// through config is pinned on the command line, because the whole point of a
// script is that two people running it on the same commits get the same table:
// diff.external, diff.noprefix, diff.mnemonicPrefix and diff.algorithm would
// each otherwise change the output.
func (r *repo) diff(from, to string) (string, error) {
	args := []string{
		"-c", "core.quotepath=false",
		"diff",
		"--unified=0",
		"--no-color",
		"--no-ext-diff",
		"--no-textconv",
		"--find-renames",
		"--diff-algorithm=myers",
		"--src-prefix=a/",
		"--dst-prefix=b/",
		from,
	}
	if to != "" {
		args = append(args, to)
	}
	return r.gitTimeout(120*time.Second, args...)
}

// blob reads a file's content at a revision, or from the working tree when rev
// is empty (the --worktree comparison).
func (r *repo) blob(rev, path string) ([]byte, error) {
	if rev == "" {
		return os.ReadFile(filepath.Join(r.root, path))
	}
	out, err := r.gitTimeout(30*time.Second, "show", rev+":"+path)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// ---------------------------------------------------------------------------
// GitHub lookups (optional — gh is never a hard dependency)
// ---------------------------------------------------------------------------

type prInfo struct {
	Number            int    `json:"number"`
	BaseRefName       string `json:"baseRefName"`
	State             string `json:"state"`
	IsCrossRepository bool   `json:"isCrossRepository"`
}

// lookupPR returns the PR for the named branch, or nil when there is none.
// The branch is passed explicitly rather than left to gh's default, which is
// the checked-out one — that would answer for the wrong branch under --head.
// "No pull request found" is an ordinary outcome — a branch that has not been
// pushed yet is the normal case for the first run — so it is not an error.
func lookupPR(r *repo, branch string) (*prInfo, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, nil
	}
	out, err := runCmd(context.Background(), 20*time.Second, r.root,
		"gh", "pr", "view", branch, "--json", "number,baseRefName,state,isCrossRepository")
	if err != nil {
		if strings.Contains(err.Error(), "no pull requests found") || strings.Contains(err.Error(), "no open pull requests") {
			return nil, nil
		}
		return nil, err
	}
	return parsePRInfo(out)
}

func parsePRInfo(out string) (*prInfo, error) {
	var pr prInfo
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, fmt.Errorf("parsing gh output: %w", err)
	}
	if pr.BaseRefName == "" {
		return nil, nil
	}
	return &pr, nil
}

func ghDefaultBranch(r *repo) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", err
	}
	out, err := runCmd(context.Background(), 20*time.Second, r.root,
		"gh", "repo", "view", "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
