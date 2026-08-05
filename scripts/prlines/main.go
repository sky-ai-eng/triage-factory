// Command prlines counts the lines a pull request changes, split into the
// Code / Tests / Documentation / Comments buckets .github/pull_request_template.md
// asks for, and prints them as a table that pastes straight into it.
//
// The counts come from the diff GitHub actually shows under "Files changed":
// the three-dot comparison between the head commit and the MERGE BASE of head
// and the PR's base branch. Comparing against the base branch's tip instead is
// the usual way a hand-count goes wrong — it folds in every commit that landed
// on the base after this branch was cut, which on a week-old branch can be most
// of the diff.
//
// Base resolution walks four sources, highest authority first; see
// resolveBase in git.go for why that list is exhaustive.
//
//  1. --base
//  2. the open pull request for this branch (via gh, if installed)
//  3. git config branch.<name>.tfBase
//  4. the repository's default branch
//
// Usage:
//
//	./scripts/pr-lines.sh                 # count HEAD against the resolved base
//	./scripts/pr-lines.sh --worktree      # include uncommitted (tracked) changes
//	./scripts/pr-lines.sh --base origin/aa/TFAC-123   # stacked branch
//	./scripts/pr-lines.sh --explain       # show how every file was bucketed
//	./scripts/pr-lines.sh --json          # machine-readable, for agents
//
// The table goes to stdout and everything else to stderr, so
// `./scripts/pr-lines.sh | pbcopy` copies exactly what belongs in the PR body.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type options struct {
	base             string
	head             string
	remote           string
	worktree         bool
	noFetch          bool
	noGH             bool
	includeGenerated bool
	jsonOut          bool
	explain          bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "prlines: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options
	fs := flag.NewFlagSet("prlines", flag.ContinueOnError)
	fs.StringVar(&opts.base, "base", "", "base ref to diff from (skips base resolution)")
	fs.StringVar(&opts.head, "head", "HEAD", "head ref to diff to")
	fs.StringVar(&opts.remote, "remote", "", "remote holding the base branch (default: origin, or the only remote)")
	fs.BoolVar(&opts.worktree, "worktree", false, "compare the working tree instead of a commit (tracked files only)")
	fs.BoolVar(&opts.noFetch, "no-fetch", false, "skip refreshing the base's remote-tracking ref")
	fs.BoolVar(&opts.noGH, "no-gh", false, "do not consult gh for the pull request or default branch")
	fs.BoolVar(&opts.includeGenerated, "include-generated", false, "count lock files and generated code as Code")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a markdown table")
	fs.BoolVar(&opts.explain, "explain", false, "print the per-file breakdown to stderr")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Count a PR's changed lines by kind, for the PR template's table.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	r, err := openRepo()
	if err != nil {
		return err
	}

	remote, err := r.resolveRemote(opts.remote)
	if err != nil && opts.base == "" {
		return err
	}
	opts.remote = remote

	branch := r.currentBranch()
	base, warns, err := resolveBase(r, opts, branch)
	if err != nil {
		return err
	}

	if !opts.noFetch {
		if err := r.fetchBase(opts.remote, base.Ref); err != nil {
			warns = append(warns, fmt.Sprintf("could not refresh %s (%v); counting against this clone's copy, which may be behind", base.Ref, err))
		}
	}

	headRev := opts.head
	if opts.worktree {
		headRev = ""
		warns = append(warns, "--worktree counts uncommitted changes to tracked files; run `git add -N .` first if new files should be included")
	} else if r.isDirty() {
		warns = append(warns, "working tree has uncommitted changes, which are NOT counted; pass --worktree to include them")
	}

	// The working tree is not a commit, so its merge base is computed against
	// HEAD — the same branch point either way, since uncommitted work cannot
	// move where the branch was cut from.
	rangeHead := opts.head
	if opts.worktree {
		rangeHead = "HEAD"
	}
	mergeBaseRev, headCommit, warns, err := resolveRange(r, base.Ref, rangeHead, warns)
	if err != nil {
		return err
	}
	// An empty commit range is a mistake normally — you are on the base branch,
	// or pointed at the wrong one — but it is the ordinary state of a branch
	// whose first commit is still uncommitted, so --worktree still has work.
	if mergeBaseRev == headCommit && !opts.worktree {
		return fmt.Errorf("%s is an ancestor of %s — there is nothing to count; is this branch actually the base?", rangeHead, base.Ref)
	}

	raw, err := r.diff(mergeBaseRev, headRev)
	if err != nil {
		return err
	}
	files, err := parseUnifiedDiff(raw)
	if err != nil {
		return err
	}

	res := count(r, files, mergeBaseRev, headRev, opts.includeGenerated)
	res.Warnings = append(warns, res.Warnings...)

	if opts.jsonOut {
		return emitJSON(os.Stdout, r, base, mergeBaseRev, headCommit, branch, res, opts)
	}

	printHeader(os.Stderr, r, base, mergeBaseRev, headCommit, branch, res, opts)
	if opts.explain {
		printPerFile(os.Stderr, res)
	}
	renderTable(os.Stdout, res.Buckets)
	return nil
}

func resolveRange(r *repo, baseRef, headRef string, warns []string) (string, string, []string, error) {
	headCommit, err := r.resolveCommit(headRef)
	if err != nil {
		return "", "", warns, fmt.Errorf("head %q does not resolve to a commit: %w", headRef, err)
	}
	mb, n, err := r.mergeBaseOf(baseRef, headRef)
	if err != nil {
		return "", "", warns, err
	}
	if n > 1 {
		warns = append(warns, fmt.Sprintf("%s and %s have %d merge bases (criss-cross history); using %s, same as git's three-dot diff", baseRef, headRef, n, shortSHA(mb)))
	}
	return mb, headCommit, warns, nil
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

var rows = []string{"Code", "Tests", "Documentation", "Comments"}

func renderTable(w io.Writer, b buckets) {
	vals := [][2]int{
		{b.Code.Added, b.Code.Removed},
		{b.Tests.Added, b.Tests.Removed},
		{b.Documentation.Added, b.Documentation.Removed},
		{b.Comments.Added, b.Comments.Removed},
	}

	// Widths match the template's own table when the numbers are small enough
	// to fit, so a pasted table is a clean diff against the placeholder.
	kindW, addW, remW := len("Documentation"), len("Added"), len("Removed")
	for _, v := range vals {
		addW = max(addW, len(strconv.Itoa(v[0])))
		remW = max(remW, len(strconv.Itoa(v[1])))
	}

	fmt.Fprintf(w, "| %-*s | %-*s | %-*s |\n", kindW, "Kind", addW, "Added", remW, "Removed")
	fmt.Fprintf(w, "| %s | %s | %s |\n", strings.Repeat("-", kindW), strings.Repeat("-", addW), strings.Repeat("-", remW))
	for i, name := range rows {
		fmt.Fprintf(w, "| %-*s | %*d | %*d |\n", kindW, name, addW, vals[i][0], remW, vals[i][1])
	}
}

func printHeader(w io.Writer, r *repo, base baseRef, mergeBaseRev, headCommit, branch string, res result, opts options) {
	head := branch
	if head == "" {
		head = "detached HEAD"
	}
	if opts.worktree {
		head += " + working tree"
	}

	fmt.Fprintf(w, "base        %s  (%s)\n", base.Ref, base.Source)
	fmt.Fprintf(w, "merge-base  %s\n", r.describe(mergeBaseRev))
	fmt.Fprintf(w, "head        %s  %s\n", shortSHA(headCommit), head)

	summary := fmt.Sprintf("%d file(s) changed", res.Files)
	var excl []string
	if !res.Generated.empty() {
		excl = append(excl, fmt.Sprintf("generated %s", res.Generated))
	}
	if res.BinaryFiles > 0 {
		excl = append(excl, fmt.Sprintf("%d binary file(s)", res.BinaryFiles))
	}
	if !res.Blank.empty() {
		excl = append(excl, fmt.Sprintf("blank %s", res.Blank))
	}
	if len(excl) > 0 {
		summary += "; excluded: " + strings.Join(excl, ", ")
	}
	fmt.Fprintf(w, "diff        %s\n", summary)
	fmt.Fprintf(w, "buckets     documentation > comments > tests > code (per line, first match wins)\n")

	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "warning     %s\n", warn)
	}
	fmt.Fprintln(w)
}

func printPerFile(w io.Writer, res result) {
	width := 0
	for _, f := range res.PerFile {
		width = max(width, len(f.Path))
	}
	for _, f := range res.PerFile {
		var parts []string
		if f.Excluded {
			parts = append(parts, "["+f.Class+", excluded]")
			if !f.Buckets.Code.empty() {
				parts = append(parts, f.Buckets.Code.String())
			}
		} else {
			for _, b := range []struct {
				name string
				p    pair
			}{
				{"code", f.Buckets.Code},
				{"tests", f.Buckets.Tests},
				{"documentation", f.Buckets.Documentation},
				{"comments", f.Buckets.Comments},
			} {
				if !b.p.empty() {
					parts = append(parts, fmt.Sprintf("%s %s", b.name, b.p))
				}
			}
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, f.Path, strings.Join(parts, "  "))
	}
	fmt.Fprintln(w)
}

func emitJSON(w io.Writer, r *repo, base baseRef, mergeBaseRev, headCommit, branch string, res result, opts options) error {
	type refInfo struct {
		Ref    string `json:"ref"`
		Source string `json:"source,omitempty"`
		Commit string `json:"commit,omitempty"`
	}
	payload := struct {
		Base      refInfo `json:"base"`
		Head      refInfo `json:"head"`
		MergeBase string  `json:"mergeBase"`
		Worktree  bool    `json:"worktree"`
		result
	}{
		Base:      refInfo{Ref: base.Ref, Source: base.Source},
		Head:      refInfo{Ref: branch, Commit: headCommit},
		MergeBase: mergeBaseRev,
		Worktree:  opts.worktree,
		result:    res,
	}
	if !opts.explain {
		payload.PerFile = nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
