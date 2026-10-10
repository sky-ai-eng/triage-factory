package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// issueHelp maps each issue action to its usage line, for `linear issue
// <action> --help` and the unknown-action error. HelpText is the overview.
var issueHelp = map[string]string{
	"view":          "linear issue view <id>",
	"list-states":   "linear issue list-states <id>",
	"transition":    "linear issue transition <id> --state <name-or-id>",
	"comment":       "linear issue comment <id> --body <text>",
	"assign":        "linear issue assign <id>",
	"unassign":      "linear issue unassign <id>",
	"create":        "linear issue create <team-key> --title <text> [--description <text>] [--parent <id>] [--priority <0-4>] [--label <name>]...",
	"edit":          "linear issue edit <id> [--title <text>] [--description <text>] [--priority <0-4>] [--add-label <name>]... [--remove-label <name>]...",
	"set-parent":    "linear issue set-parent <id> --parent <id>",
	"set-priority":  "linear issue set-priority <id> --priority <0-4>",
	"list-children": "linear issue list-children <id>",
	"search":        "linear issue search --team <key> [--state <name>]... [--assignee me|none] [--max <N>] (default 50)",
}

// issueFlags is each action's flags, mapped to whether the flag may be given
// more than once. An action absent here takes no flags.
var issueFlags = map[string]map[string]bool{
	"transition":   {"--state": false},
	"comment":      {"--body": false},
	"create":       {"--title": false, "--description": false, "--parent": false, "--priority": false, "--label": true},
	"edit":         {"--title": false, "--description": false, "--priority": false, "--add-label": true, "--remove-label": true},
	"set-parent":   {"--parent": false},
	"set-priority": {"--priority": false},
	"search":       {"--team": false, "--state": true, "--assignee": false, "--max": false},
}

// ValueFlags is every linear flag, all of which take a value: the input
// execflags.HasHelpFlag needs to tell `--body "--help"` from a help request.
// Exported because the exec dispatcher runs the same check one level up, to
// route help before it resolves run identity.
var ValueFlags = func() map[string]bool {
	out := map[string]bool{}
	for _, flags := range issueFlags {
		for f := range flags {
			out[f] = true
		}
	}
	return out
}()

func handleIssue(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: " + prog.Prefix() + " linear issue <action> [flags]")
	}
	action := args[0]
	flags := args[1:]

	if action == "--help" || action == "-h" {
		printHelp()
		return
	}
	if execflags.HasHelpFlag(flags, ValueFlags) {
		if h, ok := issueHelp[action]; ok {
			fmt.Printf("usage: %s %s\n", prog.Prefix(), h)
			return
		}
		// An unknown action with --help falls through to the unknown-action
		// error, which names the valid set.
	}

	ctx := context.Background()
	switch action {
	case "view":
		a := parseOrExit(action, flags, 1)
		issue, err := host.LinearGetIssue(ctx, a.positional[0])
		exitOnErr(err)
		printJSON(issue)
	case "list-states":
		a := parseOrExit(action, flags, 1)
		states, err := host.LinearListStates(ctx, a.positional[0])
		exitOnErr(err)
		printJSON(nonNil(states))
	case "transition":
		a := parseOrExit(action, flags, 1)
		state := a.required("--state")
		moved, err := host.LinearTransition(ctx, a.positional[0], state)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "issue": a.positional[0], "state": moved})
	case "comment":
		a := parseOrExit(action, flags, 1)
		body := a.required("--body")
		commentID, err := host.LinearAddComment(ctx, a.positional[0], body)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "issue": a.positional[0], "comment_id": commentID})
	case "assign":
		a := parseOrExit(action, flags, 1)
		exitOnErr(host.LinearAssignSelf(ctx, a.positional[0]))
		printJSON(map[string]any{"ok": true, "issue": a.positional[0], "assigned": "self"})
	case "unassign":
		a := parseOrExit(action, flags, 1)
		exitOnErr(host.LinearUnassign(ctx, a.positional[0]))
		printJSON(map[string]any{"ok": true, "issue": a.positional[0], "assigned": nil})
	case "create":
		issueCreate(ctx, host, flags)
	case "edit":
		issueEdit(ctx, host, flags)
	case "set-parent":
		a := parseOrExit(action, flags, 1)
		parent := a.required("--parent")
		exitOnErr(host.LinearSetParent(ctx, a.positional[0], parent))
		printJSON(map[string]any{"ok": true, "issue": a.positional[0], "parent": parent})
	case "set-priority":
		a := parseOrExit(action, flags, 1)
		priority := parsePriority(a.required("--priority"))
		exitOnErr(host.LinearSetPriority(ctx, a.positional[0], priority))
		printJSON(map[string]any{"ok": true, "issue": a.positional[0], "priority": priority})
	case "list-children":
		a := parseOrExit(action, flags, 1)
		children, err := host.LinearListChildren(ctx, a.positional[0])
		exitOnErr(err)
		printJSON(nonNil(children))
	case "search":
		issueSearch(ctx, host, flags)
	default:
		exitErr(unknownActionMessage(prog.Prefix(), action))
	}
}

func issueCreate(ctx context.Context, host agenthost.Client, flags []string) {
	a := parseOrExit("create", flags, 1)
	req := agenthost.LinearCreateIssueRequest{
		TeamKey:     a.positional[0],
		Title:       a.required("--title"),
		Description: a.value("--description"),
		Parent:      a.value("--parent"),
		Labels:      a.values["--label"],
	}
	if a.has("--priority") {
		req.Priority = parsePriority(a.value("--priority"))
	}
	issue, err := host.LinearCreateIssue(ctx, req)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "id": issue.ID, "identifier": issue.Identifier, "url": issue.URL})
}

func issueEdit(ctx context.Context, host agenthost.Client, flags []string) {
	a := parseOrExit("edit", flags, 1)
	var edit agenthost.LinearIssueEdit
	if a.has("--title") {
		v := a.value("--title")
		edit.Title = &v
	}
	if a.has("--description") {
		v := a.value("--description")
		edit.Description = &v
	}
	if a.has("--priority") {
		v := parsePriority(a.value("--priority"))
		edit.Priority = &v
	}
	edit.AddLabels = a.values["--add-label"]
	edit.RemoveLabels = a.values["--remove-label"]
	if edit.IsEmpty() {
		exitErr("at least one of --title, --description, --priority, --add-label, --remove-label is required")
	}
	exitOnErr(host.LinearUpdateIssue(ctx, a.positional[0], edit))
	printJSON(map[string]any{"ok": true, "issue": a.positional[0]})
}

func issueSearch(ctx context.Context, host agenthost.Client, flags []string) {
	a := parseOrExit("search", flags, 0)
	req := agenthost.LinearSearchRequest{
		TeamKey:  a.required("--team"),
		States:   a.values["--state"],
		Assignee: a.value("--assignee"),
		Max:      50,
	}
	switch req.Assignee {
	case "", agenthost.LinearAssigneeMe, agenthost.LinearAssigneeNone:
	default:
		exitErr("--assignee must be me or none")
	}
	if a.has("--max") {
		n, err := strconv.Atoi(a.value("--max"))
		if err != nil || n < 1 || n > agenthost.LinearSearchMaxResults {
			exitErr(fmt.Sprintf("--max must be a number from 1 to %d", agenthost.LinearSearchMaxResults))
		}
		req.Max = n
	}
	issues, err := host.LinearSearch(ctx, req)
	exitOnErr(err)
	printJSON(nonNil(issues))
}

// unknownActionMessage is the mistyped-verb error: what was wrong, the valid
// set, and the command that expands it, under the prefix the caller invoked.
func unknownActionMessage(prefix, action string) string {
	return fmt.Sprintf("unknown issue action: %s\nvalid actions: %s\nRun '%s linear issue --help' for usage.",
		action, strings.Join(issueActions(), ", "), prefix)
}

// issueActions lists the valid issue verbs, derived from issueHelp so a verb
// cannot be dispatched without a usage line.
func issueActions() []string {
	out := make([]string, 0, len(issueHelp))
	for a := range issueHelp {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// --- argument parsing ---

// issueArgs is an action's arguments, split into positionals and flag values.
type issueArgs struct {
	positional []string
	values     map[string][]string
}

func (a issueArgs) has(flag string) bool { return len(a.values[flag]) > 0 }

func (a issueArgs) value(flag string) string {
	if v := a.values[flag]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// required is flag's value, exiting when it is absent or empty.
func (a issueArgs) required(flag string) string {
	v := a.value(flag)
	if strings.TrimSpace(v) == "" {
		exitErr(flag + " is required")
	}
	return v
}

// parseIssueArgs splits args for action, which takes exactly positionals
// positional arguments and the flags issueFlags gives it. Every flag takes a
// value, and the token after a flag is its value whatever it looks like, so a
// body that starts with "--" is a body. A flag the action does not take, a
// flag with no value, a non-repeatable flag given twice, and the wrong number
// of positionals are each an error rather than something to guess past.
func parseIssueArgs(action string, args []string, positionals int) (issueArgs, error) {
	allowed := issueFlags[action]
	out := issueArgs{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if !strings.HasPrefix(tok, "-") {
			out.positional = append(out.positional, tok)
			continue
		}
		repeats, ok := allowed[tok]
		if !ok {
			return issueArgs{}, fmt.Errorf("linear issue %s does not take %s", action, tok)
		}
		if i+1 >= len(args) {
			return issueArgs{}, fmt.Errorf("%s requires a value", tok)
		}
		if len(out.values[tok]) > 0 && !repeats {
			return issueArgs{}, fmt.Errorf("%s given more than once", tok)
		}
		out.values[tok] = append(out.values[tok], args[i+1])
		i++
	}
	if len(out.positional) != positionals {
		return issueArgs{}, fmt.Errorf("usage: %s", issueHelp[action])
	}
	return out, nil
}

func parseOrExit(action string, args []string, positionals int) issueArgs {
	a, err := parseIssueArgs(action, args, positionals)
	exitOnErr(err)
	return a
}

func parsePriority(raw string) int {
	p, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || p < 0 || p > 4 {
		exitErr("--priority must be 0-4 (0 none, 1 urgent, 2 high, 3 normal, 4 low)")
	}
	return p
}

// nonNil renders an empty list as [] rather than null.
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// --- output ---

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
