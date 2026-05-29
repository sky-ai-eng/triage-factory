package gh

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// HelpText is the help output for gh commands, shared with the top-level exec help.
const HelpText = `GitHub PR Commands:
  gh pr view <number> [--repo o/r] [-v]                  PR details + reviews + comments
  gh pr diff <number> [--repo o/r] [--file <path>]       Raw diff (optionally per-file)
  gh pr files <number> [--repo o/r]                       List changed files
  gh pr thread-view <number> <comment_id> [--page N]      Comment thread with replies
  gh pr review-view <review_id> --pr <N> [--repo o/r] [-v]  Expand a review + inline comments

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
  gh pr start-review <number> [--repo o/r]                Start a local pending review
  gh pr add-review-comment <review_id> --file <path> --line <N> --body <text> [--start-line <N>] [--severity <level>]
                                                          --severity tags the finding's level. Valid
                                                          values (case-insensitive): BLOCKER, MAJOR,
                                                          MINOR, CLEAN. Renders as a chip in the human
                                                          approval UI and a badge on the posted comment.
                                                          Optional — omit for an un-badged comment.
  gh pr comment-list-pending <review_id>                  List pending review comments
  gh pr submit-review <review_id> --event <approve|comment|request_changes> --body <text>

Direct Comments (hit GitHub API immediately):
  gh pr add-comment <number> --body <text>                Add top-level comment
  gh pr comment-reply <comment_id> --pr <N> --body <text> Reply to a thread
  gh pr comment-react <comment_id> --repo o/r --emoji <e> React to a comment
  gh pr comment-update <comment_id> --body <text>         Edit (local pending or remote)
  gh pr comment-delete <comment_id>                       Delete (local pending or remote)

GitHub Actions Commands:
  gh actions download-logs <run_id> [--repo o/r]          Download & extract the full log
                                                          archive for a workflow run into
                                                          ./_scratch/ci-logs/<run_id>/

Repo Resolution (all gh commands):
  Priority order: --repo flag > TRIAGE_FACTORY_REPO env var > .git/config origin of cwd.
  Commands fail with a clear error if none resolve.`

// Handle dispatches gh subcommands. host is the agenthost.Client the
// PR write paths route through; it may be nil for help routes.
func Handle(client *github.Client, host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "pr":
		handlePR(client, host, cmdArgs)
	case "actions":
		handleActions(client, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown gh resource: %s\n", resource)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec gh <resource> <action> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", HelpText)
}
