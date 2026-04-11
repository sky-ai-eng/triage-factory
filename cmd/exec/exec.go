package exec

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/todo-triage/cmd/exec/gh"
	jiraexec "github.com/sky-ai-eng/todo-triage/cmd/exec/jira"
	"github.com/sky-ai-eng/todo-triage/internal/auth"
	"github.com/sky-ai-eng/todo-triage/internal/config"
	"github.com/sky-ai-eng/todo-triage/internal/db"
	ghclient "github.com/sky-ai-eng/todo-triage/internal/github"
	jiraclient "github.com/sky-ai-eng/todo-triage/internal/jira"
)

// Handle dispatches exec subcommands.
func Handle(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
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
	if err := db.Migrate(conn); err != nil {
		fmt.Fprintf(os.Stderr, "error running migrations: %v\n", err)
		os.Exit(1)
	}
	database := &db.DB{Conn: conn}

	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "gh":
		if isHelp(cmdArgs) {
			gh.Handle(nil, nil, cmdArgs)
			return
		}
		if creds.GitHubPAT == "" {
			fmt.Fprintln(os.Stderr, "GitHub not configured. Run todotriage and complete setup first.")
			os.Exit(1)
		}
		baseURL := cfg.GitHub.BaseURL
		if baseURL == "" {
			baseURL = creds.GitHubURL
		}
		client := ghclient.NewClient(baseURL, creds.GitHubPAT)
		gh.Handle(client, database, cmdArgs)

	case "jira":
		if isHelp(cmdArgs) {
			jiraexec.Handle(nil, cmdArgs)
			return
		}
		if creds.JiraPAT == "" || creds.JiraURL == "" {
			fmt.Fprintln(os.Stderr, "Jira not configured. Run todotriage and complete setup first.")
			os.Exit(1)
		}
		jClient := jiraclient.NewClient(creds.JiraURL, creds.JiraPAT)
		jiraexec.Handle(jClient, cmdArgs)

	default:
		fmt.Fprintf(os.Stderr, "unknown exec command: %s\nRun 'todotriage exec --help' for usage.\n", cmd)
		os.Exit(1)
	}
}

// HandleStatus processes status update commands from the agent.
func HandleStatus(args []string) {
	fmt.Fprintln(os.Stderr, "not implemented: status")
}

func isHelp(args []string) bool {
	return len(args) == 0 || args[0] == "--help" || args[0] == "-h"
}

func printHelp() {
	fmt.Printf("Usage: todotriage exec <service> <resource> <action> [flags]\n\n%s\n\n%s\n\nAll commands print JSON to stdout on success, errors to stderr.\n", gh.HelpText, jiraexec.HelpText)
}
