package linear

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// userHelp maps each user action to its usage line, as issueHelp does for
// issue actions.
var userHelp = map[string]string{
	"search": "linear user search <query>",
}

func handleUser(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: " + prog.Prefix() + " linear user <action> [args]")
	}
	action := args[0]
	rest := args[1:]

	if action == "--help" || action == "-h" {
		printHelp()
		return
	}
	if execflags.HasHelpFlag(rest, ValueFlags) {
		if h, ok := userHelp[action]; ok {
			fmt.Printf("usage: %s %s\n", prog.Prefix(), h)
			return
		}
	}

	switch action {
	case "search":
		query, err := parseUserSearchArgs(rest)
		exitOnErr(err)
		users, err := host.LinearSearchUsers(context.Background(), query)
		exitOnErr(err)
		printJSON(nonNil(users))
	default:
		exitErr(unknownUserActionMessage(prog.Prefix(), action))
	}
}

// parseUserSearchArgs takes exactly one argument, the query. search takes no
// flags, so anything that looks like one is refused rather than searched for.
func parseUserSearchArgs(args []string) (string, error) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return "", fmt.Errorf("usage: %s", userHelp["search"])
	}
	if strings.TrimSpace(args[0]) == "" {
		return "", errors.New("<query> must not be blank")
	}
	return args[0], nil
}

func unknownUserActionMessage(prefix, action string) string {
	actions := make([]string, 0, len(userHelp))
	for a := range userHelp {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	return fmt.Sprintf("unknown user action: %s\nvalid actions: %s\nRun '%s linear --help' for usage.",
		action, strings.Join(actions, ", "), prefix)
}
