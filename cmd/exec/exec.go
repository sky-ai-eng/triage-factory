package exec

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/todo-tinder/cmd/exec/gh"
	jiraexec "github.com/sky-ai-eng/todo-tinder/cmd/exec/jira"
	"github.com/sky-ai-eng/todo-tinder/internal/auth"
	"github.com/sky-ai-eng/todo-tinder/internal/config"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
	jiraclient "github.com/sky-ai-eng/todo-tinder/internal/jira"
)

// Handle dispatches exec subcommands.
func Handle(args []string) {
	if len(args) == 0 || args[0] == "--help" {
		printHelp()
		return
	}

	// Load credentials for API access
	creds, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading credentials: %v\n", err)
		os.Exit(1)
	}

	cfg, _ := config.Load()

	// Open DB for local state (pending reviews, etc.)
	conn, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	db.Migrate(conn)
	database := &db.DB{Conn: conn}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "gh":
		if creds.GitHubPAT == "" {
			fmt.Fprintln(os.Stderr, "GitHub not configured. Run todotinder and complete setup first.")
			os.Exit(1)
		}
		baseURL := cfg.GitHub.BaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}
		client := ghclient.NewClient(baseURL, creds.GitHubPAT)
		gh.Handle(client, database, cmdArgs)

	case "jira":
		if creds.JiraPAT == "" || creds.JiraURL == "" {
			fmt.Fprintln(os.Stderr, "Jira not configured. Run todotinder and complete setup first.")
			os.Exit(1)
		}
		jClient := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
		jiraexec.Handle(jClient, cmdArgs)

	default:
		fmt.Fprintf(os.Stderr, "unknown exec command: %s\nRun 'todotinder exec --help' for usage.\n", cmd)
		os.Exit(1)
	}
}

// HandleStatus processes status update commands from the agent.
func HandleStatus(args []string) {
	fmt.Fprintln(os.Stderr, "not implemented: status")
}

func printHelp() {
	fmt.Println(`Usage: todotinder exec <service> <resource> <action> [flags]

GitHub PR Commands:
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
  gh pr comment-delete <comment_id>                       Delete (local pending or remote)

Jira Ticket Commands:
  jira ticket view <key>                                  Issue details (summary, status, assignee, etc.)
  jira ticket transition <key> --status <status>          Transition to a workflow status
  jira ticket list-transitions <key>                      List available transitions
  jira ticket comment <key> --body <text>                 Add a comment
  jira ticket assign <key>                                Assign to the authenticated user
  jira ticket unassign <key>                              Remove assignee
  jira ticket create <project> --type <type> --summary <text> [--description <text>] [--parent <key>] [--priority <priority>]
  jira ticket set-parent <key> --parent <parent_key>      Link issue under a parent
  jira ticket list-types <project>                        List available issue types
  jira ticket list-children <key>                         List child issues (subtasks + epic children)
  jira ticket list-priorities                             List available priority levels
  jira ticket set-priority <key> --priority <priority>    Update issue priority

All commands print JSON to stdout on success, errors to stderr.`)
}
