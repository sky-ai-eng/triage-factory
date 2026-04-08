package gh

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/github"
)

// HelpText is the help output for gh commands, shared with the top-level exec help.
const HelpText = `GitHub PR Commands:
  gh pr view <number> [--repo o/r] [-v]                  PR details + reviews + comments
  gh pr diff <number> [--repo o/r] [--file <path>]       Raw diff (optionally per-file)
  gh pr files <number> [--repo o/r]                       List changed files
  gh pr thread-view <number> <comment_id> [--page N]      Comment thread with replies
  gh pr review-view <review_id> --pr <N> [--repo o/r] [-v]  Expand a review + inline comments

Review Lifecycle (managed locally, submitted atomically):
  gh pr start-review <number> [--repo o/r]                Start a local pending review
  gh pr add-review-comment <review_id> --file <path> --line <N> --body <text> [--start-line <N>]
  gh pr comment-list-pending <review_id>                  List pending review comments
  gh pr submit-review <review_id> --event <approve|comment|request_changes> --body <text>

Direct Comments (hit GitHub API immediately):
  gh pr add-comment <number> --body <text>                Add top-level comment
  gh pr comment-reply <comment_id> --pr <N> --body <text> Reply to a thread
  gh pr comment-react <comment_id> --repo o/r --emoji <e> React to a comment
  gh pr comment-update <comment_id> --body <text>         Edit (local pending or remote)
  gh pr comment-delete <comment_id>                       Delete (local pending or remote)`

// Handle dispatches gh subcommands.
func Handle(client *github.Client, database *db.DB, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "pr":
		handlePR(client, database, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown gh resource: %s\n", resource)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Printf("Usage: todotinder exec gh <resource> <action> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", HelpText)
}
