package jira

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/execout"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// userHelp maps each user action to its usage line, as ticketHelp does for
// ticket actions.
var userHelp = map[string]string{
	"search": "jira user search <query> --issue <key>",
}

func handleUser(host agenthost.Client, args []string) {
	if len(args) < 1 {
		execout.ExitErr("usage: " + prog.Prefix() + " jira user <action> [args]")
	}
	action := args[0]
	rest := args[1:]

	if action == "--help" || action == "-h" {
		printHelp()
		return
	}
	if hasHelpFlag(rest) {
		if h, ok := userHelp[action]; ok {
			fmt.Printf("usage: %s %s\n", prog.Prefix(), h)
			return
		}
	}

	switch action {
	case "search":
		query, issue, err := parseUserSearchArgs(rest)
		execout.ExitOnErr(err)
		matches, err := host.JiraSearchUsers(context.Background(), issue, query)
		execout.ExitOnErr(err)
		if matches.Users == nil {
			matches.Users = []jiraclient.User{}
		}
		execout.PrintJSON(matches)
	default:
		execout.ExitErr(unknownUserActionMessage(prog.Prefix(), action))
	}
}

// parseUserSearchArgs splits search's arguments into its one query and the
// --issue it is required to name: Jira answers which users can be assigned a
// particular issue, not who exists. A flag other than --issue, a second query
// word (an unquoted name), and a blank value are each refused.
func parseUserSearchArgs(args []string) (query, issue string, err error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		switch tok := args[i]; {
		case tok == "--issue":
			if i+1 >= len(args) {
				return "", "", errors.New("--issue requires a value")
			}
			if issue != "" {
				return "", "", errors.New("--issue given more than once")
			}
			issue = args[i+1]
			i++
		case strings.HasPrefix(tok, "-"):
			return "", "", fmt.Errorf("jira user search does not take %s", tok)
		default:
			positional = append(positional, tok)
		}
	}
	if len(positional) != 1 {
		return "", "", fmt.Errorf("usage: %s", userHelp["search"])
	}
	if strings.TrimSpace(positional[0]) == "" {
		return "", "", errors.New("<query> must not be blank")
	}
	if strings.TrimSpace(issue) == "" {
		return "", "", errors.New("--issue is required: Jira answers which users can be assigned a particular issue")
	}
	return positional[0], issue, nil
}

func unknownUserActionMessage(prefix, action string) string {
	actions := make([]string, 0, len(userHelp))
	for a := range userHelp {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	return fmt.Sprintf("unknown user action: %s\nvalid actions: %s\nRun '%s jira --help' for usage.",
		action, strings.Join(actions, ", "), prefix)
}
