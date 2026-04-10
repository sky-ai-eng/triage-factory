package jira

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	jiraclient "github.com/sky-ai-eng/todo-triage/internal/jira"
)

func handleTicket(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: todotriage exec jira ticket <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	switch action {
	case "view":
		ticketView(client, flags)
	case "transition":
		ticketTransition(client, flags)
	case "list-transitions":
		ticketListTransitions(client, flags)
	case "comment":
		ticketComment(client, flags)
	case "assign":
		ticketAssign(client, flags)
	case "unassign":
		ticketUnassign(client, flags)
	case "create":
		ticketCreate(client, flags)
	case "set-parent":
		ticketSetParent(client, flags)
	case "list-types":
		ticketListTypes(client, flags)
	case "list-children":
		ticketListChildren(client, flags)
	case "search":
		ticketSearch(client, flags)
	case "list-priorities":
		ticketListPriorities(client)
	case "set-priority":
		ticketSetPriority(client, flags)
	default:
		exitErr(fmt.Sprintf("unknown ticket action: %s", action))
	}
}

func ticketView(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket view <key>")
	}
	issue, err := client.GetIssue(args[0])
	exitOnErr(err)
	printJSON(issue)
}

func ticketTransition(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket transition <key> --status <status>")
	}
	key := args[0]
	status := flagVal(args, "--status")
	if status == "" {
		exitErr("--status is required")
	}
	err := client.TransitionTo(key, status)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "status": status})
}

func ticketListTransitions(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-transitions <key>")
	}
	transitions, err := client.GetTransitions(args[0])
	exitOnErr(err)
	printJSON(transitions)
}

func ticketComment(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket comment <key> --body <text>")
	}
	key := args[0]
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	err := client.AddComment(key, body)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketAssign(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket assign <key>")
	}
	err := client.AssignToSelf(args[0])
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": args[0], "assigned": "self"})
}

func ticketUnassign(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket unassign <key>")
	}
	err := client.Unassign(args[0])
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": args[0], "assigned": nil})
}

func ticketCreate(client *jiraclient.Client, args []string) {
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

	key, err := client.CreateIssue(project, issueType, summary, description, parentKey, priority)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketSetParent(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket set-parent <key> --parent <parent_key>")
	}
	key := args[0]
	parentKey := flagVal(args, "--parent")
	if parentKey == "" {
		exitErr("--parent is required")
	}
	err := client.SetParent(key, parentKey)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "parent": parentKey})
}

func ticketListChildren(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-children <key>")
	}
	children, err := client.GetChildIssues(args[0])
	exitOnErr(err)
	if children == nil {
		children = []jiraclient.Issue{}
	}
	printJSON(children)
}

func ticketSearch(client *jiraclient.Client, args []string) {
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
		maxResults = v
	}

	issues, err := client.SearchIssues(jql, fields, maxResults)
	exitOnErr(err)
	if issues == nil {
		issues = []jiraclient.Issue{}
	}
	printJSON(issues)
}

func ticketListPriorities(client *jiraclient.Client) {
	priorities, err := client.ListPriorities()
	exitOnErr(err)
	printJSON(priorities)
}

func ticketSetPriority(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket set-priority <key> --priority <priority>")
	}
	key := args[0]
	priority := flagVal(args, "--priority")
	if priority == "" {
		exitErr("--priority is required")
	}
	err := client.SetPriority(key, priority)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "priority": priority})
}

func ticketListTypes(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-types <project>")
	}
	types, err := client.ListIssueTypes(args[0])
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
