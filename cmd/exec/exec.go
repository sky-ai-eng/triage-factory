package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/convident"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/gh"
	gitexec "github.com/sky-ai-eng/triage-factory/cmd/exec/git"
	jiraexec "github.com/sky-ai-eng/triage-factory/cmd/exec/jira"
	linearexec "github.com/sky-ai-eng/triage-factory/cmd/exec/linear"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/memory"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/workspace"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
)

// Handle dispatches exec subcommands for the HOST CLI boot identity: the
// operator's own invocation and the local-mode delegated agent, both of which
// serve every state access in-process against the local SQLite DB.
//
// The DB is opened eagerly, before the verb switch, because this identity
// always needs it: the LocalClient reads it for run state and for credential
// resolution, so there is nothing to gain from deferring it and one fewer
// order-of-operations question by not.
func Handle(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(helpText(prog.Prefix(), hostHelpSources()))
		return
	}

	stores, closeDB, err := openLocalStores()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = closeDB() }()

	// Help routes skip this because the help output doesn't need run identity.
	dispatch(args, func() agenthost.Client {
		client, derr := agenthost.NewLocalFromEnv(context.Background(), stores)
		if derr != nil {
			// convident-derived errors (env unset, unknown run) are already
			// written for the reader; they get a clean stderr message rather
			// than the wrapping the constructor would otherwise apply.
			if errors.Is(derr, convident.ErrConversationIdentityMissing) ||
				errors.Is(derr, convident.ErrConversationIdentityNotFound) {
				fmt.Fprintln(os.Stderr, derr.Error())
			} else {
				fmt.Fprintf(os.Stderr, "agenthost: %v\n", derr)
			}
			os.Exit(1)
		}
		return client
	})
}

// HandleSandboxed dispatches exec subcommands for the JAILED CLI boot
// identity, over a client the caller has already dialed. It is a pure RPC
// front end: no database is opened here — not conditionally, not lazily —
// because a jailed process has no local state to open. Everything a verb
// touches, state and credentials alike, lives behind the socket.
//
// That is structural, not an optimization. Under the jail's HOME the local
// opener would mint a fresh empty SQLite file inside the run worktree — junk
// the agent can see, snapshot, and commit — and then answer every read from
// it, which is how a live run got told its own conversation did not exist.
//
// The dispatcher closes the client on the paths that return normally, the same
// way it closes one it built itself; the caller keeps its own deferred Close
// (idempotent) for the paths that don't reach a verb at all.
func HandleSandboxed(client agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(helpText(prog.Prefix(), helpSources(client)))
		return
	}
	dispatch(args, func() agenthost.Client { return client })
}

// openLocalStores opens and migrates the host CLI's local SQLite state. A
// package var so a test can substitute an opener that fails if it is ever
// reached, which is how the jailed path's "never opens a database" property is
// asserted rather than merely read.
//
// Migrations run before the first store read on every invocation. goose's
// per-invocation logging ("no migrations to run…") is silenced — exec runs on
// every delegated-agent tool call and the noise drowns out the actual command
// output; migration errors still surface through the returned error.
var openLocalStores = func() (db.Stores, func() error, error) {
	conn, err := db.Open()
	if err != nil {
		return db.Stores{}, nil, fmt.Errorf("error opening database: %w", err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		_ = conn.Close()
		return db.Stores{}, nil, fmt.Errorf("error running migrations: %w", err)
	}
	return sqlite.New(conn), conn.Close, nil
}

// dispatch is the verb switch both boot identities share, so a verb behaves
// identically whichever client it was handed. buildAgentHost is called only by
// the verbs that need a client — help routes return before it runs, which is
// what makes `exec gh pr --help` work with no run in scope.
func dispatch(args []string, buildAgentHost func() agenthost.Client) {
	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "gh":
		// GitHub API calls route through the agenthost client. In the jail the
		// IPC client ships each call to the host daemon, which resolves the
		// org's App-installation-or-PAT credential (github.Resolver.ClientForRepo)
		// and makes the request; the jail never reads a token, the keychain, or
		// dbus. On the host CLI the in-process LocalClient builds the same
		// client directly on the user's machine — the unchanged local path.
		// Like the jira branch, this never loads a credential here.
		// context.Background() is the deliberate root: exec is a short-lived
		// per-tool-call CLI process with no parent ctx to inherit. It threads
		// through gh.Handle into every GitHub API call so the surface is
		// ctx-aware (a future signal-rooted ctx slots in here without touching
		// the call sites).
		if isHelp(cmdArgs, gh.ValueFlags) {
			gh.Handle(context.Background(), nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		gh.Handle(context.Background(), host, cmdArgs)

	case "jira":
		// Jira API calls route through the agenthost client. In the jail the
		// IPC client ships each call to the host daemon, which builds the
		// org's bot-attributed system client (ForSystem) and holds the
		// credential; the jail never reads a token, the keychain, or dbus. On
		// the host CLI the in-process LocalClient builds the same ForSystem
		// client directly — the unchanged local path. Bot-authored writes by
		// design; no per-user routing in the jail (user-attributed Jira writes
		// are the server-side handlers).
		if isHelp(cmdArgs, jiraexec.ValueFlags) {
			jiraexec.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		jiraexec.Handle(host, cmdArgs)

	case "linear":
		// Linear calls route through the agenthost client exactly as the jira
		// branch's do: the daemon (or, on the host CLI, the in-process
		// LocalClient) resolves the org's service credential and makes the
		// call, so the jail never holds a Linear credential. Writes are
		// attributed to the org's Linear identity by design; user-attributed
		// Linear writes are the server-side handlers.
		if isHelp(cmdArgs, linearexec.ValueFlags) {
			linearexec.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		linearexec.Handle(host, cmdArgs)

	case "workspace":
		// No credentials needed — workspace acts on the agenthost client
		// (DB + filesystem on the host CLI, IPC + filesystem in the jail).
		if isHelp(cmdArgs, workspace.ValueFlags) {
			workspace.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		workspace.Handle(host, cmdArgs)

	case "memory":
		// `memory load` reads entity memory host-side through the agenthost
		// client (DB on the host CLI, IPC in the jail where the read + the
		// best-effort touch land on the daemon). No credentials — like
		// workspace. Mirrors the gh case's host lifecycle: help routes skip
		// buildAgentHost() (no run identity needed), otherwise Close is deferred.
		if isHelp(cmdArgs, memory.ValueFlags) {
			memory.Handle(context.Background(), nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		memory.Handle(context.Background(), host, cmdArgs)

	case "git":
		// Git's commit signing program, not an agent verb: the run's git
		// config points gpg.ssh.program at the ssh-sign shim, which calls
		// this. Absent from the help index for that reason.
		os.Exit(gitexec.Handle(context.Background(), buildAgentHost, cmdArgs, os.Stderr))

	default:
		// EE-registered verbs (agenthost.RegisterExtension's exec-side
		// counterpart, e.g. "slack") land here — the switch's fallthrough
		// path. A registered runner gets the same buildAgentHost() the
		// built-in cases use; the runner's own entitlement check happens
		// inside host.CallExtension, not here.
		if sub, ok := subcommandRegistry[cmd]; ok {
			os.Exit(runRegistered(sub, cmdArgs, buildAgentHost))
		}
		fmt.Fprint(os.Stderr, unknownCommandText(prog.Prefix(), cmd))
		os.Exit(1)
	}
}

// runRegistered executes a registered subcommand and returns its exit code.
// Help routes are served with a nil host — buildAgentHost is never invoked —
// which is the same short-circuit the built-in cases apply and what makes
// `exec slack --help` answer with no run identity in scope. The deferred
// Close runs before this returns (the caller os.Exits AFTER), so the host is
// released on every non-help path.
func runRegistered(sub Subcommand, cmdArgs []string, buildAgentHost func() agenthost.Client) int {
	if isHelp(cmdArgs, sub.ValueFlags) {
		return sub.Run(context.Background(), cmdArgs, nil)
	}
	host := buildAgentHost()
	defer func() { _ = host.Close() }()
	return sub.Run(context.Background(), cmdArgs, host)
}

// HandleStatus is the `triagefactory status` subcommand's entry point.
// Not implemented yet.
func HandleStatus(args []string) {
	fmt.Fprintln(os.Stderr, "not implemented: status")
}

// isHelp reports whether this invocation is a help request at ANY depth, so
// the dispatcher routes it before building the agenthost. That short-circuit is
// what makes `exec gh pr --help` work outside a run: resolving run identity
// first would fail a help request for want of a conversation it never needed.
//
// The scan runs over the whole arg tail rather than args[0] alone (which is a
// resource name at this level, never a flag), with the verb surface's own
// value-taking flags supplied so a `--body "--help"` payload is not mistaken
// for a request.
func isHelp(args []string, valueFlags map[string]bool) bool {
	return len(args) == 0 || execflags.HasHelpFlag(args, valueFlags)
}

// availabilityTimeout bounds the help route's best-effort manifest read:
// help must never hang on a sick daemon, and a miss just yields the
// unannotated index.
const availabilityTimeout = 3 * time.Second

// helpSources asks the run's agenthost for its source manifest, best-effort: a
// nil client or any failure yields nil, which helpText renders as the full
// unannotated index. Over-inclusion is the safe degrade — a listed verb the org
// lacks refuses with an error naming the reason, while a hidden one leaves the
// agent improvising flags against live commands, which is the exact failure
// filtered help must not reintroduce.
func helpSources(client agenthost.Client) domain.SourceManifest {
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), availabilityTimeout)
	defer cancel()
	sources, err := client.SourceManifest(ctx)
	if err != nil {
		return nil
	}
	return sources
}

// hostHelpSources resolves the help index's manifest for the host CLI. Only a
// delegated run gets an annotated index: with no run identity in scope (an
// operator's bare terminal) it returns nil — the full listing — WITHOUT
// opening the DB, preserving help's no-state property. With identity present,
// the read goes through the same local client the verbs use, and every
// failure degrades to nil rather than to an error: a help request must never
// fail for want of its notes.
func hostHelpSources() domain.SourceManifest {
	if os.Getenv(convident.ConversationIDEnvVar) == "" {
		return nil
	}
	stores, closeDB, err := openLocalStores()
	if err != nil {
		return nil
	}
	defer func() { _ = closeDB() }()
	client, err := agenthost.NewLocalFromEnv(context.Background(), stores)
	if err != nil {
		return nil
	}
	defer func() { _ = client.Close() }()
	return helpSources(client)
}

// helpText renders the top-level help. Only agent-facing verbs are listed. The
// pre-push hook's branch-capture callback is deliberately NOT an exec
// subcommand — it lives under the internal `triagefactory hook` namespace (see
// cmd/hook), off the agent's `Bash(<bin> exec *)` allowlist, so a stuck agent
// scanning this help can neither see nor invoke it.
//
// The per-verb blocks name their verbs bare (`gh pr view`, not
// `<prefix> gh pr view`), so the invoked prefix appears in the usage line alone.
//
// sources is the run's source manifest, the same answer its <tools> section
// and <run_context> notes derive from; nil means unresolved and renders the
// full surface without notes. Non-nil annotates the index: gh/workspace/memory
// are always listed (GitHub is required, the other two sourceless); a core
// source — GitHub, Jira, Linear, never unlicensed — is listed with the refusal
// table's note for its state whenever it is not sealed (explained means not
// yet); a registered family whose source is unlicensed or absent from the
// manifest is omitted outright — an unlicensed surface degrades to absence,
// the same rule every other gate applies — and any other is listed with its
// state's note. Index-only either way: an omitted family's own `--help` still
// answers, because the verbs are compiled in and "unknown command" for a real
// name sends a caller hunting for a typo.
func helpText(prefix string, sources domain.SourceManifest) string {
	sections := []string{
		coreSourceHelpSection(sources, eventsource.KindGitHub, gh.HelpText),
		coreSourceHelpSection(sources, eventsource.KindJira, jiraexec.HelpText),
		coreSourceHelpSection(sources, eventsource.KindLinear, linearexec.HelpText),
		workspace.HelpText, memory.HelpText,
	}
	sections = append(sections, registeredHelpSections(sources)...)
	return fmt.Sprintf("Usage: %s <command> [args]\n\n%s\n\nCommands print their result to stdout on success and errors to stderr. Most commands print JSON; workspace add prints a raw path.\n", prefix, strings.Join(sections, "\n\n"))
}

// withSourceNote appends the refusal table's note for kind's entry under a
// help section, when the entry needs one. Its job is to stop an agent from
// telling a user it can work a ticket and discovering the refusal mid-task.
func withSourceNote(help, kind string, entry domain.SourceEntry) string {
	note := agenthost.SourceNote(kind, entry)
	if note == "" {
		return help
	}
	return help + "\n\n  NOTE: " + note
}

// coreSourceHelpSection is a core source's help section, annotated when the
// manifest resolved and the source is not sealed. A core source the manifest
// does not name has no credential this run can use, which reads as
// unconfigured.
func coreSourceHelpSection(sources domain.SourceManifest, kind, help string) string {
	if sources == nil {
		return help
	}
	entry, ok := sources.Entry(kind)
	if !ok {
		entry = domain.SourceEntry{State: domain.SourceUnconfigured}
	}
	return withSourceNote(help, kind, entry)
}

// registeredHelpSections returns the registered families' help sections in
// name order — deterministic output, the same canonical-order rule the tools
// reference applies — filtered and annotated per Subcommand.SourceKind when
// the manifest resolved.
func registeredHelpSections(sources domain.SourceManifest) []string {
	names := make([]string, 0, len(subcommandRegistry))
	for name := range subcommandRegistry {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []string
	for _, name := range names {
		sub := subcommandRegistry[name]
		if sources == nil || sub.SourceKind == "" {
			out = append(out, sub.HelpText)
			continue
		}
		entry, ok := sources.Entry(sub.SourceKind)
		if !ok || entry.State == domain.SourceUnlicensed {
			continue
		}
		out = append(out, withSourceNote(sub.HelpText, sub.SourceKind, entry))
	}
	return out
}

// unknownCommandText is the loser path: an unrecognized verb under any invoked
// name exits 1 with this on stderr — nothing retries or falls through to server
// mode.
func unknownCommandText(prefix, cmd string) string {
	return fmt.Sprintf("unknown exec command: %s\nRun '%s --help' for usage.\n", cmd, prefix)
}
