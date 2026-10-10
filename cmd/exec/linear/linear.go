package linear

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// Handle dispatches linear subcommands. host is the agenthost.Client every
// Linear call routes through: in the sandbox it ships the call to the run's
// daemon, which holds the org's Linear credential; in local mode the in-process
// LocalClient resolves it directly. host is nil on the help route, which
// returns before any call.
func Handle(host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	resource := args[0]
	cmdArgs := args[1:]

	switch resource {
	case "issue":
		handleIssue(host, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown linear resource: %s\n", resource)
		os.Exit(1)
	}
}

// HelpText is the help output for linear commands, shared with the top-level
// exec help.
const HelpText = `Linear Issue Commands:
  linear issue view <id>                                       Issue details (<id> is an identifier like ENG-123, or a UUID)
  linear issue list-states <id>                                Workflow states of the issue's team
  linear issue transition <id> --state <name-or-id>            Move to a workflow state of the issue's team
  linear issue comment <id> --body <text>                      Add a comment
  linear issue assign <id>                                     Assign to the org's Linear identity
  linear issue unassign <id>                                   Remove assignee
  linear issue create <team-key> --title <text> [--description <text>] [--parent <id>] [--priority <0-4>] [--label <name>]...
  linear issue edit <id> [--title <text>] [--description <text>] [--priority <0-4>] [--add-label <name>]... [--remove-label <name>]...  Update fields on an existing issue
  linear issue set-parent <id> --parent <id>                   Make the issue a sub-issue of another
  linear issue set-priority <id> --priority <0-4>              0 none, 1 urgent, 2 high, 3 normal, 4 low
  linear issue list-children <id>                              List sub-issues and their states
  linear issue search --team <key> [--state <name>]... [--assignee me|none] [--max <N>] (default 50)  Search a team's issues`

func printHelp() {
	fmt.Print(helpText(prog.Prefix()))
}

// helpText renders the `linear` resource-level usage under the invoked prefix.
func helpText(prefix string) string {
	return fmt.Sprintf("Usage: %s linear <resource> <action> [flags]\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", prefix, HelpText)
}
