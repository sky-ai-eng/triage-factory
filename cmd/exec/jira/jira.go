package jira

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// Handle dispatches jira subcommands. host is the agenthost.Client every
// Jira API call routes through: in the sandbox it ships the call to the
// host daemon (which holds the ForSystem credential); in local mode the
// in-process LocalClient builds the same client directly. host is nil on
// the help route, which returns before any call.
func Handle(host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "ticket":
		handleTicket(host, cmdArgs)
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
  jira ticket edit <key> [--summary <text>] [--description <text>] [--priority <p>] [--type <t>] [--add-label <l>] [--remove-label <l>]  Update fields on an existing issue
  jira ticket set-parent <key> --parent <parent_key>           Link issue under a parent
  jira ticket set-priority <key> --priority <priority>         Update issue priority
  jira ticket search --jql <jql> [--fields <f1,f2,...>] [--max <N>] (default 50)  Search issues via JQL
  jira ticket list-children <key>                              List child issues (subtasks + epic children)
  jira ticket list-types <project>                             List available issue types
  jira ticket list-priorities                                  List available priority levels`

func printHelp() {
	fmt.Print(helpText(prog.Prefix()))
}

// helpText renders the `jira` resource-level usage under the invoked prefix.
func helpText(prefix string) string {
	return fmt.Sprintf("Usage: %s jira <resource> <action> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", prefix, HelpText)
}
