// Package memory implements the `triagefactory exec memory` CLI surface — the
// agent-facing verb for pulling ANOTHER entity's prior run memory on demand.
//
// The primary entity's memory (and the project knowledge base) is still
// materialized as files at spawn time; this verb covers the other half of the
// hybrid delivery: when a run starts working with a DIFFERENT entity mid-run —
// a PR it opened from a ticket, a ticket referenced in a thread — it pulls what
// past runs learned about that entity as the tool result rather than having the
// host write files into a live run tree (privsep forbids that). The agent owns
// /work and can persist what it loads into its own _scratch/ if it wants
// grep-ability.
//
// `memory` is a first-class core verb (like gh/jira/workspace), not an ee
// extension: every state access routes through the agenthost.Client
// (LocalClient in local mode, IPCClient in the sandbox) exactly like the other
// core verbs.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// HelpText is the help block for `memory` commands, surfaced both from
// `memory --help` and the top-level `exec --help`.
const HelpText = `Memory Commands:
  memory load --source <github|jira|slack> --id <source_id> [--limit N]
                                       Pull the prior run memory for ANOTHER
                                       entity as JSON. Returns quickly with an
                                       empty set when there's nothing recorded.

--id shapes (per source):
  github   owner/repo#N     (e.g. octo/repo#42)
  jira     KEY-123
  slack    <channel_id>/<thread_root_ts>

Usage notes:
  - --limit defaults to 20 (the most recent memories).
  - Prior memory for THIS task's OWN entity is already materialized under
    _scratch/entity-memory/ — use this only for a different entity you pull
    in mid-run.
  - Run id is read from $TRIAGE_FACTORY_RUN_ID (set by the delegation spawner).`

// defaultLimit is how many of an entity's most recent memories `memory load`
// returns when --limit is omitted. Enough to orient without flooding the tool
// result; the agent can raise it when it needs the full history.
const defaultLimit = 20

// Handle dispatches memory subcommands. host is the agenthost.Client every
// state access routes through (local SQLite or daemon IPC, chosen by
// agenthost.AutoDetect at the top of cmd/exec/exec.go). host is nil on the help
// route, which returns before any call.
func Handle(ctx context.Context, host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}
	switch args[0] {
	case "load":
		runLoad(ctx, host, args[1:])
	default:
		exitErr("unknown memory command: " + args[0])
	}
}

// loadArgs is the parsed `memory load` invocation.
type loadArgs struct {
	source   string
	sourceID string
	limit    int
}

// validSources is the set of entity source values --source accepts. Kept here
// (not imported from a domain set) so the CLI's usage error is self-contained;
// the host re-validates defensively (agenthost.LocalClient.MemoryLoad).
var validSources = map[string]bool{"github": true, "jira": true, "slack": true}

// parseLoadArgs scans the `memory load` flags with the hand-rolled cmd/exec/gh
// idiom (no flag package) and returns the parsed args or a usage error. Split
// out from runLoad so it is unit-testable without executing the RPC or exiting
// the process.
func parseLoadArgs(args []string) (loadArgs, error) {
	out := loadArgs{limit: defaultLimit}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--source":
			if i+1 >= len(args) {
				return loadArgs{}, fmt.Errorf("--source needs a value")
			}
			i++
			out.source = args[i]
		case "--id":
			if i+1 >= len(args) {
				return loadArgs{}, fmt.Errorf("--id needs a value")
			}
			i++
			out.sourceID = args[i]
		case "--limit":
			if i+1 >= len(args) {
				return loadArgs{}, fmt.Errorf("--limit needs a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return loadArgs{}, fmt.Errorf("invalid --limit %q: expected a positive integer", args[i])
			}
			out.limit = n
		default:
			return loadArgs{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if out.source == "" {
		return loadArgs{}, fmt.Errorf("--source is required (one of github, jira, slack)")
	}
	if !validSources[out.source] {
		return loadArgs{}, fmt.Errorf("invalid --source %q: expected one of github, jira, slack", out.source)
	}
	if out.sourceID == "" {
		return loadArgs{}, fmt.Errorf("--id is required")
	}
	return out, nil
}

func runLoad(ctx context.Context, host agenthost.Client, args []string) {
	parsed, err := parseLoadArgs(args)
	if err != nil {
		exitErr(fmt.Sprintf("usage: triagefactory exec memory load --source <github|jira|slack> --id <source_id> [--limit N]\n%s", err))
	}
	res, err := host.MemoryLoad(ctx, parsed.source, parsed.sourceID, parsed.limit)
	if err != nil {
		exitErr(err.Error())
	}
	printJSON(res)
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec memory <command> [args]\n\n%s\n", HelpText)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
