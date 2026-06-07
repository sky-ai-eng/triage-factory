package exec

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/gh"
	jiraexec "github.com/sky-ai-eng/triage-factory/cmd/exec/jira"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/workspace"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Handle dispatches exec subcommands.
func Handle(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	// Open DB for local state (pending reviews, etc.). Credentials and
	// per-org settings (GitHub base URL) follow the same path — both
	// route through DB-backed stores.
	//
	// The DB open is unconditional even when the sandboxed agenthost
	// path will win below — credential loading (the loadCreds closure)
	// still needs SecretStore access, and a future ticket
	// will route those reads through the IPC client too. Today the
	// production sandbox path doesn't exercise gh/jira subcommands
	// (SKY-256 hasn't shipped the musl-static binary yet), so reading
	// the local DB for creds is fine even when the daemon socket
	// exists.
	conn, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	// Silence goose's per-invocation logging ("no migrations to run…")
	// — exec runs on every delegated-agent tool call and the noise
	// drowns out the actual command output. Migration errors still
	// surface via the returned error.
	goose.SetLogger(goose.NopLogger())
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		fmt.Fprintf(os.Stderr, "error running migrations: %v\n", err)
		os.Exit(1)
	}
	stores := sqlite.New(conn)

	cmd := args[0]
	cmdArgs := args[1:]

	// AutoDetect returns the right state-access seam for the current
	// process: IPCClient when /run/tf.sock is bind-mounted in (the
	// sandboxed-agent path), LocalClient otherwise (the local-mode CLI
	// and the local-mode delegated-agent path). Help routes skip this
	// because the help output doesn't need run identity resolution.
	buildAgentHost := func() agenthost.Client {
		ctx := context.Background()
		client, derr := agenthost.AutoDetect(ctx, stores)
		if derr != nil {
			// runident-derived errors (env unset, unknown run) get a
			// clean stderr message rather than the wrapping AutoDetect
			// would otherwise apply.
			if errors.Is(derr, runident.ErrRunIdentityMissing) || errors.Is(derr, runident.ErrRunIdentityNotFound) {
				fmt.Fprintln(os.Stderr, derr.Error())
			} else {
				fmt.Fprintf(os.Stderr, "agenthost: %v\n", derr)
			}
			os.Exit(1)
		}
		return client
	}

	// Credentials route through the SecretStore. exec is invoked per-run
	// with TRIAGE_FACTORY_RUN_ID; resolve the run's orgID so the
	// SecretStore read is scoped to the right tenant.
	//
	// Local mode falls back to runmode.LocalDefaultOrgID when
	// ResolveRunIdentity errors so `--help` (and stray invocations
	// outside a delegated run) still load the configured creds for
	// the single tenant. Multi mode refuses: an unidentified exec
	// invocation has no valid tenant to bill against, and resolving
	// to the sentinel org would hit a SecretStore with no rows at
	// best, leak to the wrong tenant at worst. Help paths short-
	// circuit before loadCreds runs, so this branch never fires for
	// genuine `--help`.
	// resolveOrgID picks the right tenant for credential + settings reads.
	// In local mode it falls back to the sentinel org so `--help` and
	// stray invocations still work; in multi-mode it errors out since
	// there's no valid tenant to attribute the read to.
	resolveOrgID := func(ctx context.Context) (string, error) {
		ident, err := runident.ResolveRunIdentity(ctx, stores, os.Getenv(runident.RunIdentityEnvVar))
		switch {
		case err == nil:
			return ident.OrgID, nil
		case runmode.Current() == runmode.ModeLocal:
			return runmode.LocalDefaultOrgID, nil
		default:
			return "", fmt.Errorf("cmd/exec invoked without a valid %s; this command can only run inside a delegated agent run", runident.RunIdentityEnvVar)
		}
	}
	loadCreds := func() (string, string, string, string, string, error) {
		ctx := context.Background()
		orgID, err := resolveOrgID(ctx)
		if err != nil {
			return "", "", "", "", "", err
		}
		c, lerr := integrations.Load(ctx, stores.Secrets, orgID)
		orgSet, _ := stores.Orgs.GetSettingsSystem(ctx, orgID) // settings read failures degrade to empty BaseURL — the gh branch already prefers creds.GitHubURL as a fallback
		return c.GitHubURL, c.GitHubPAT, c.JiraURL, c.JiraPAT, orgSet.GitHubBaseURL, lerr
	}

	switch cmd {
	case "gh":
		if isHelp(cmdArgs) {
			gh.Handle(nil, nil, cmdArgs)
			return
		}
		ghURL, ghPAT, _, _, ghBase, lerr := loadCreds()
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "error loading credentials: %v\n", lerr)
			os.Exit(1)
		}
		if ghPAT == "" {
			fmt.Fprintln(os.Stderr, "GitHub not configured. Run triagefactory and complete setup first.")
			os.Exit(1)
		}
		baseURL := ghBase
		if baseURL == "" {
			baseURL = ghURL
		}
		client := ghclient.NewClient(baseURL, ghPAT)
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		gh.Handle(client, host, cmdArgs)

	case "jira":
		// Jira API calls route through the agenthost client. In the sandbox
		// the IPC client ships each call to the host daemon, which builds the
		// org's bot-attributed system client (ForSystem) and holds the
		// credential; the jail never reads a token, the keychain, or dbus. In
		// local mode AutoDetect returns the in-process LocalClient, which
		// builds the same ForSystem client directly — the unchanged local
		// path. Bot-authored writes by design; no per-user routing in the
		// sandbox (user-attributed Jira writes are the server-side handlers).
		//
		// Unlike the gh branch, this never calls loadCreds — the credential
		// resolves host-side (or in-process via the LocalClient), so the DB
		// opened at the top of Handle is consulted here only by the local-mode
		// LocalClient; the sandbox IPC path ignores it.
		if isHelp(cmdArgs) {
			jiraexec.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		jiraexec.Handle(host, cmdArgs)

	case "workspace":
		// No credentials needed — workspace acts on the agenthost client
		// (DB + filesystem in local mode, IPC + filesystem in sandbox).
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		workspace.Handle(host, cmdArgs)

	default:
		fmt.Fprintf(os.Stderr, "unknown exec command: %s\nRun 'triagefactory exec --help' for usage.\n", cmd)
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
	fmt.Printf("Usage: triagefactory exec <command> [args]\n\n%s\n\n%s\n\n%s\n\nCommands print their result to stdout on success and errors to stderr. Most commands print JSON; workspace add prints a raw path.\n", gh.HelpText, jiraexec.HelpText, workspace.HelpText)
}
