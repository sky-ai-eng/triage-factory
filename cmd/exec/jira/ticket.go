package jira

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
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// ticketHelp maps each ticket subcommand to its usage line. Mirrored
// in jira.HelpText (the master overview); the per-action lookup here
// powers `jira ticket <action> --help`. Drift between the two is
// caught manually for now — they live in adjacent files and any
// rename has to touch both.
var ticketHelp = map[string]string{
	"view":             "jira ticket view <key>",
	"transition":       "jira ticket transition <key> --status <status>",
	"list-transitions": "jira ticket list-transitions <key>",
	"comment":          "jira ticket comment <key> --body <text>",
	"assign":           "jira ticket assign <key>",
	"unassign":         "jira ticket unassign <key>",
	"create":           "jira ticket create <project> --type <type> --summary <text> [--description <text>] [--parent <key>] [--priority <priority>]",
	"edit":             "jira ticket edit <key> [--summary <text>] [--description <text>] [--priority <priority>] [--type <type>] [--add-label <label>] [--remove-label <label>]",
	"set-parent":       "jira ticket set-parent <key> --parent <parent_key>",
	"set-priority":     "jira ticket set-priority <key> --priority <priority>",
	"search":           "jira ticket search --jql <jql> [--fields <f1,f2,...>] [--max <N>] (default 50)",
	"list-children":    "jira ticket list-children <key>",
	"list-types":       "jira ticket list-types <project>",
	"list-priorities":  "jira ticket list-priorities",
}

// ValueFlags is every jira flag that takes a value — the input
// execflags.HasHelpFlag needs to tell `--body "--help"` (a comment
// whose text is literally "--help") from a help request. Exported
// because the exec dispatcher runs the same check one level up, to
// route help before it resolves run identity.
//
// Only value-taking flags belong here: listing a boolean would make
// `--someBool --help` a false negative. jira ticket has no boolean
// flags today, so this is exactly the flag set.
var ValueFlags = map[string]bool{
	"--status":       true,
	"--body":         true,
	"--type":         true,
	"--summary":      true,
	"--description":  true,
	"--parent":       true,
	"--priority":     true,
	"--add-label":    true,
	"--remove-label": true,
	"--jql":          true,
	"--fields":       true,
	"--max":          true,
}

// hasHelpFlag reports a standalone --help/-h among args. Per-action
// help dispatch trips on this BEFORE the action body runs so the
// agent can run e.g. `jira ticket view --help` without the leading
// arg being misread as the issue key.
func hasHelpFlag(args []string) bool {
	return execflags.HasHelpFlag(args, ValueFlags)
}

func handleTicket(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: " + prog.Prefix() + " jira ticket <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	// `jira ticket --help` — the resource level, below which the top-level
	// dispatcher's own help check no longer reaches.
	if action == "--help" || action == "-h" {
		printHelp()
		return
	}

	// Per-action --help: print just that action's usage and exit cleanly,
	// without invoking the action body (which would otherwise try to
	// interpret --help as an issue key / project / etc.).
	if hasHelpFlag(flags) {
		if h, ok := ticketHelp[action]; ok {
			fmt.Printf("usage: %s %s\n", prog.Prefix(), h)
			return
		}
		// Unknown action with --help — fall through to the unknown-action
		// error below so the user sees that the action itself is wrong, with
		// the valid set alongside it.
	}

	switch action {
	case "view":
		ticketView(host, flags)
	case "transition":
		ticketTransition(host, flags)
	case "list-transitions":
		ticketListTransitions(host, flags)
	case "comment":
		ticketComment(host, flags)
	case "assign":
		ticketAssign(host, flags)
	case "unassign":
		ticketUnassign(host, flags)
	case "create":
		ticketCreate(host, flags)
	case "edit":
		ticketEdit(host, flags)
	case "set-parent":
		ticketSetParent(host, flags)
	case "list-types":
		ticketListTypes(host, flags)
	case "list-children":
		ticketListChildren(host, flags)
	case "search":
		ticketSearch(host, flags)
	case "list-priorities":
		ticketListPriorities(host)
	case "set-priority":
		ticketSetPriority(host, flags)
	default:
		exitErr(unknownActionMessage(prog.Prefix(), action))
	}
}

// unknownActionMessage is the mistyped-verb error: what was wrong, the valid
// set, and the command that expands it — spelled under the prefix the caller
// actually invoked, so the hint is copy-pasteable.
func unknownActionMessage(prefix, action string) string {
	return fmt.Sprintf("unknown ticket action: %s\nvalid actions: %s\nRun '%s jira ticket --help' for usage.",
		action, strings.Join(ticketActions(), ", "), prefix)
}

// ticketActions lists the valid ticket verbs for the unknown-action error.
// Derived from ticketHelp (sorted for a stable message) so a new verb can't be
// added to the dispatch without appearing here — ticketHelp already has to
// grow for its usage line.
func ticketActions() []string {
	out := make([]string, 0, len(ticketHelp))
	for a := range ticketHelp {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func ticketView(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket view <key>")
	}
	issue, err := host.JiraGetIssue(context.Background(), args[0])
	exitOnErr(err)
	printJSON(issue)
}

func ticketTransition(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket transition <key> --status <status>")
	}
	key := args[0]
	status := flagVal(args, "--status")
	if status == "" {
		exitErr("--status is required")
	}
	err := host.JiraTransitionTo(context.Background(), key, status)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "status": status})
}

func ticketListTransitions(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-transitions <key>")
	}
	transitions, err := host.JiraGetTransitions(context.Background(), args[0])
	exitOnErr(err)
	printJSON(transitions)
}

func ticketComment(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket comment <key> --body <text>")
	}
	key := args[0]
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	err := host.JiraAddComment(context.Background(), key, body)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketAssign(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket assign <key>")
	}
	err := host.JiraAssignToSelf(context.Background(), args[0])
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": args[0], "assigned": "self"})
}

func ticketUnassign(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket unassign <key>")
	}
	err := host.JiraUnassign(context.Background(), args[0])
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": args[0], "assigned": nil})
}

func ticketCreate(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket create <project> --type <type> --summary <text> [--description <text>] [--parent <key>] [--priority <priority>]")
	}
	project := args[0]
	issueType := flagVal(args, "--type")
	summary := flagVal(args, "--summary")
	description := flagVal(args, "--description")
	parentKey := flagVal(args, "--parent")
	priority := flagVal(args, "--priority")

	if issueType == "" {
		exitErr("--type is required")
	}
	if summary == "" {
		exitErr("--summary is required")
	}

	key, err := host.JiraCreateIssue(context.Background(), project, issueType, summary, description, parentKey, priority)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketEdit(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket edit <key> [--summary <text>] [--description <text>] [--priority <priority>] [--type <type>] [--add-label <label>] [--remove-label <label>]")
	}
	key := args[0]

	fields := jiraclient.UpdateIssueFields{}
	if flagPresent(args, "--summary") {
		v := flagVal(args, "--summary")
		fields.Summary = &v
	}
	if flagPresent(args, "--description") {
		v := flagVal(args, "--description")
		fields.Description = &v
	}
	if flagPresent(args, "--priority") {
		v := flagVal(args, "--priority")
		fields.Priority = &v
	}
	if flagPresent(args, "--type") {
		v := flagVal(args, "--type")
		fields.IssueType = &v
	}
	addLabels, err := flagVals(args, "--add-label")
	exitOnErr(err)
	fields.AddLabels = addLabels

	removeLabels, err := flagVals(args, "--remove-label")
	exitOnErr(err)
	fields.RemoveLabels = removeLabels

	if fields.IsEmpty() {
		exitErr("at least one of --summary, --description, --priority, --type, --add-label, --remove-label is required")
	}

	err = host.JiraUpdateIssue(context.Background(), key, fields)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketSetParent(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket set-parent <key> --parent <parent_key>")
	}
	key := args[0]
	parentKey := flagVal(args, "--parent")
	if parentKey == "" {
		exitErr("--parent is required")
	}
	err := host.JiraSetParent(context.Background(), key, parentKey)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "parent": parentKey})
}

func ticketListChildren(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-children <key>")
	}
	children, err := host.JiraGetChildIssues(context.Background(), args[0])
	exitOnErr(err)
	if children == nil {
		children = []jiraclient.Issue{}
	}
	printJSON(children)
}

func ticketSearch(host agenthost.Client, args []string) {
	jql := flagVal(args, "--jql")
	if jql == "" {
		exitErr("--jql is required")
	}

	var fields []string
	if f := flagVal(args, "--fields"); f != "" {
		for _, field := range strings.Split(f, ",") {
			fields = append(fields, strings.TrimSpace(field))
		}
	}

	maxResults := 50
	if m := flagVal(args, "--max"); m != "" {
		v, err := strconv.Atoi(m)
		if err != nil {
			exitErr("--max must be a number")
		}
		if v <= 0 {
			// The client coerces a non-positive cap to a default, so an
			// unguarded `--max 0` would surprise by returning more than the
			// no-flag default of 50. Reject it with a clear message instead.
			exitErr("--max must be a positive number")
		}
		maxResults = v
	}

	issues, err := host.JiraSearchIssues(context.Background(), jql, fields, maxResults)
	exitOnErr(err)
	if issues == nil {
		issues = []jiraclient.Issue{}
	}
	printJSON(issues)
}

func ticketListPriorities(host agenthost.Client) {
	priorities, err := host.JiraListPriorities(context.Background())
	exitOnErr(err)
	printJSON(priorities)
}

func ticketSetPriority(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket set-priority <key> --priority <priority>")
	}
	key := args[0]
	priority := flagVal(args, "--priority")
	if priority == "" {
		exitErr("--priority is required")
	}
	err := host.JiraSetPriority(context.Background(), key, priority)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "priority": priority})
}

func ticketListTypes(host agenthost.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-types <project>")
	}
	types, err := host.JiraListIssueTypes(context.Background(), args[0])
	exitOnErr(err)
	printJSON(types)
}

// --- helpers ---

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// flagPresent reports whether `flag` appears as a flag argument (not
// as the value of a preceding `--xxx` flag). The preceding-flag skip
// is what prevents `--description --summary` from falsely reporting
// --summary as present (where it's actually --description's value),
// which would otherwise trigger an unintended summary clear in `edit`.
func flagPresent(args []string, flag string) bool {
	for i, a := range args {
		if a != flag {
			continue
		}
		if i > 0 && strings.HasPrefix(args[i-1], "--") {
			// Preceding token is a value-taking flag; this is its value.
			continue
		}
		return true
	}
	return false
}

// flagVals collects every value of a repeatable flag (e.g.
// --add-label foo --add-label bar). Skips occurrences that are
// themselves values of a preceding flag (mirroring flagPresent).
// Returns an error if any qualifying occurrence has no following
// value — surfacing trailing-flag typos rather than silently dropping
// them and producing a partial update.
func flagVals(args []string, flag string) ([]string, error) {
	var vals []string
	for i, a := range args {
		if a != flag {
			continue
		}
		if i > 0 && strings.HasPrefix(args[i-1], "--") {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s requires a value", flag)
		}
		vals = append(vals, args[i+1])
	}
	return vals, nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
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
