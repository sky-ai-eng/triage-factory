package jira

import (
	"fmt"
	"os"

	jiraclient "github.com/sky-ai-eng/todo-triage/internal/jira"
)

// Handle dispatches jira subcommands.
func Handle(client *jiraclient.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "ticket":
		handleTicket(client, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown jira resource: %s\n", resource)
		os.Exit(1)
	}
}

// HelpText is the help output for jira commands, shared with the top-level exec help.
const HelpText = `Jira Ticket Commands:
  jira ticket view <key>                                       Issue details
  jira ticket transition <key> --status <status>               Transition to a workflow status
  jira ticket list-transitions <key>                           List available transitions
  jira ticket comment <key> --body <text>                      Add a comment
  jira ticket assign <key>                                     Assign to the authenticated user
  jira ticket unassign <key>                                   Remove assignee
  jira ticket create <project> --type <type> --summary <text> [--description <text>] [--parent <key>] [--priority <priority>]
  jira ticket set-parent <key> --parent <parent_key>           Link issue under a parent
  jira ticket set-priority <key> --priority <priority>         Update issue priority
  jira ticket search --jql <jql> [--fields <f1,f2,...>] [--max <N>]  Search issues via JQL
  jira ticket list-children <key>                              List child issues (subtasks + epic children)
  jira ticket list-types <project>                             List available issue types
  jira ticket list-priorities                                  List available priority levels`

func printHelp() {
	fmt.Printf("Usage: todotriage exec jira <resource> <action> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", HelpText)
}
