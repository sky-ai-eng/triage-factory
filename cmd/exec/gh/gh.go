package gh

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// PRHelpText is the `gh pr` half of the help, printed on its own by
// `gh pr --help` and folded into the gh-wide and exec-wide overviews.
const PRHelpText = `GitHub PR Commands:
  gh pr view <number> [--repo o/r] [-v]                  PR details + reviews + comments
  gh pr diff <number> [--repo o/r] [--file <path>] [--stdout]
                                                          Persists the diff to
                                                          ./_tfac/pr-diffs/<owner>__<repo>__<number>/
                                                          (full.diff + manifest.json) and prints the
                                                          manifest JSON. Use Read/Grep on full.diff.
                                                          --file <path>: print one file's diff inline.
                                                          --stdout: print the whole diff inline.
  gh pr files <number> [--repo o/r]                       Slim per-file summary (path, status,
                                                          +/-, binary). No patch (use pr diff for
                                                          content); PR-level totals come from
                                                          pr view. "truncated":true means the list
                                                          hit the 1000-file cap.
  gh pr thread-view <number> <comment_id> [--page N]      Comment thread with replies
  gh pr review-view <review_id> --pr <N> [--repo o/r] [-v]  Expand a review + inline comments
  gh pr review-dismiss <review_id> --pr <N> [--repo o/r] [--body <reason>]
                                                          Dismiss a submitted review (defaults the
                                                          reason to "Dismissed").

PR Creation:
  gh pr create --title <T> (--body <B> | --body-file <path>) --base <branch> [--head <branch>] [--draft] [--repo o/r]
                                                          Open a pull request. The head branch
                                                          must already be pushed to the upstream
                                                          (git push first). --head defaults to
                                                          the current branch.
                                                          For multi-line markdown bodies use
                                                          --body-file <path> (or "-" for stdin)
                                                          to skip shell escaping. --body and
                                                          --body-file are mutually exclusive.
                                                          Inside a delegated Triage Factory run
                                                          this queues the PR for human approval
                                                          (the user opens it via the UI); run
                                                          standalone, it posts to GitHub
                                                          immediately. Output JSON includes a
                                                          "status" field that disambiguates.

Review Lifecycle (managed locally, submitted atomically):
  gh pr start-review <number> [--repo o/r] [--fresh]      Start a local review draft. --fresh resets an
                                                          in-progress draft to a clean slate (clears the
                                                          staged comments + finalized body/verdict) so
                                                          you can restart a botched review.
  gh pr add-review-comment <review_id> --file <path> --line <N> (--body <text> | --body-file <path>) [--start-line <N>] [--severity <level>]
                                                          --body-file reads the body from a file ("-" for
                                                          stdin), mutually exclusive with --body — safer
                                                          than a heredoc when the body has backticks.
                                                          --severity tags the finding's level. Valid
                                                          values (case-insensitive): BLOCKER, MAJOR,
                                                          MINOR, CLEAN. Renders as a chip in the human
                                                          approval UI and a badge on the posted comment.
                                                          Optional — omit for an un-badged comment.
  gh pr finalize-review <review_id> --event <approve|comment|request_changes> (--body <text> | --body-file <path>)
                                                          Hand the drafted review off for human approval
                                                          (does NOT submit to GitHub — approval does).
                                                          Output: {"status":"drafted_awaiting_approval"}.

Direct Comments (hit GitHub API immediately):
  gh pr add-comment <number> --body <text>                Add top-level comment
  gh pr comment-reply <comment_id> --pr <N> --body <text> Reply to a thread
  gh pr comment-react <comment_id> --repo o/r --emoji <e> React to a comment
  gh pr comment-update <comment_id> --body <text> [--severity <level>]
                                                          Edit a comment. Numeric id = published comment
                                                          (top-level or review). A non-numeric id (from
                                                          add-review-comment) = a staged review comment;
                                                          pass --severity to keep its chip on edit.
  gh pr comment-delete <comment_id>                       Delete a comment. Numeric id = published;
                                                          non-numeric id = staged review comment.`

// ActionsHelpText is the `gh actions` half, printed on its own by
// `gh actions --help`.
const ActionsHelpText = `GitHub Actions Commands:
  gh actions download-logs <run_id> [--repo o/r]          Download & extract the full log
                                                          archive for a workflow run into
                                                          ./_tfac/ci-logs/<run_id>/
  gh actions list-runs (--pr <N> | --sha <SHA>) [--repo o/r]
                                                          List recent workflow runs for a PR's head
                                                          commit or an explicit SHA.`

// RepoResolutionHelpText applies to every gh command, so both halves append it.
const RepoResolutionHelpText = `Repo Resolution (all gh commands):
  Priority order: --repo flag > TRIAGE_FACTORY_REPO env var > .git/config origin of cwd.
  Commands fail with a clear error if none resolve.`

// HelpText is the full gh help, shared with the top-level exec help.
var HelpText = PRHelpText + "\n\n" + ActionsHelpText + "\n\n" + RepoResolutionHelpText

// ValueFlags is every gh flag that takes a value. Two callers need exactly this
// set and would drift if each kept its own: firstPositional (which must skip a
// flag's value when hunting for the leading positional arg) and the --help scan
// (which must not read `--body "--help"` as a help request).
//
// Booleans (-v/--verbose, --stdout, --draft, --fresh) are deliberately absent:
// listing one would make `--stdout --help` look like the boolean's value and
// swallow a real help request.
var ValueFlags = map[string]bool{
	"--repo":       true,
	"--file":       true,
	"--pr":         true,
	"--body":       true,
	"--body-file":  true,
	"--line":       true,
	"--start-line": true,
	"--event":      true,
	"--status":     true,
	"--severity":   true,
	"--title":      true,
	"--base":       true,
	"--head":       true,
	"--emoji":      true,
	"--sha":        true,
	"--page":       true,
}

// Handle dispatches gh subcommands. host is the agenthost.Client every
// GitHub API call (and the PR/review DB write paths) route through: in the
// sandbox it ships each call to the host daemon, which resolves the org's
// App-installation-or-PAT credential and makes the request; in local mode the
// in-process LocalClient builds the same client directly on the user's
// machine. host is nil on the help route, which returns before any call.
func Handle(ctx context.Context, host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "pr":
		handlePR(ctx, host, cmdArgs)
	case "actions":
		handleActions(ctx, host, cmdArgs)
	default:
		fmt.Fprintln(os.Stderr, unknownVerbMessage("gh resource", "gh resources", resource,
			[]string{"pr", "actions"}, prog.Prefix()+" gh --help"))
		os.Exit(1)
	}
}

// unknownVerbMessage is the shared shape of every "you typed something that
// isn't a verb here" error: what was wrong, what the valid set is, and the
// command that expands it — which the help routing above makes work at this
// depth. A message that names only the mistake costs the agent turns of blind
// retrying to recover from.
//
// Deliberately harness-neutral: it asserts only what is true of this binary.
// The exec CLI cannot tell which agent harness is driving it, so any claim
// about what OTHER tooling is or isn't reachable would be wrong half the time
// — `gh` itself is available and recommended on one path and withheld on the
// other.
func unknownVerbMessage(kind, kindPlural, got string, valid []string, helpCmd string) string {
	return fmt.Sprintf("unknown %s: %s\nvalid %s: %s\nRun '%s' for usage.",
		kind, got, kindPlural, strings.Join(valid, ", "), helpCmd)
}

func printHelp() {
	fmt.Print(helpText(prog.Prefix()))
}

// helpText renders the `gh` resource-level usage under the invoked prefix.
func helpText(prefix string) string {
	return fmt.Sprintf("Usage: %s gh <resource> <action> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", prefix, HelpText)
}
