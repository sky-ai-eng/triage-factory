package main

import (
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
)

// bootIdentity names what this process IS, resolved once at the top of run()
// and passed down — the same discipline TF_MODE and TF_ROLE follow, and for the
// same reason. Before it existed, four layers each inferred the answer from a
// different ambient signal at a different moment: TF_MODE's local default,
// argv0's applet name, the presence of the exec-verb socket, and the sandbox
// marker. The layers disagreed. A verb invoked inside a jail ran the server's
// whole boot spine — deployment-secret resolution, a SQLite open, the full
// migration suite — before anything asked what the process was, and the DB that
// opened under the jail's HOME landed a junk state directory in the middle of
// the agent's own worktree, one `git add -A` from being committed.
//
// Deciding first, once, is what makes each path's absent machinery structural
// rather than merely unused.
type bootIdentity int

const (
	// bootServer is the long-running HTTP server: no CLI args at all.
	bootServer bootIdentity = iota

	// bootHostCLI is a CLI invocation on a real host — operator subcommands
	// and the local-mode agent verbs. It owns local state: mode and role
	// resolution, the deployment secrets, the SQLite DB.
	bootHostCLI

	// bootJailCLI is a CLI invocation inside a run sandbox. A pure RPC client:
	// every state access and every credential resolves daemon-side over the
	// per-run socket, so it needs no mode, no role, no secrets, and no
	// database — and constructs none of them.
	bootJailCLI
)

func (b bootIdentity) String() string {
	switch b {
	case bootServer:
		return "server"
	case bootHostCLI:
		return "host-cli"
	case bootJailCLI:
		return "jail-cli"
	}
	return "unknown"
}

// resolveBootIdentity decides which of the three this process is, from the two
// inputs that answer two different questions: args (post-applet-resolution)
// says what the caller wants, and sandboxed says where the process is.
//
// No args means the server, marker or not. A bare invocation is not a shape
// anything produces inside a jail — the agent's tool allowlist admits only
// `<binary> exec ...` — whereas a stray marker in an operator's environment is
// a shape that can happen, and it must never be able to refuse a server boot.
// Under the same reasoning as below, the args signal wins that cell.
//
// sandboxed MUST come from the marker the sandbox env assembler owns, not from
// a heuristic over the rest of the environment: every other variable a jailed
// process can observe (the proxy pairs, the git-hooks bin, an unset TF_MODE)
// has a legitimate non-sandbox configuration too.
//
// The marker is a CORRECTNESS signal, not a security boundary, and nothing here
// is built on trusting it. An in-jail process that unsets it for a child buys
// that child nothing: the child resolves to host-cli, opens a junk database
// inside the worktree, and fails run-identity lookup against it — strictly
// worse than the jailed path it declined. Nothing is gated on the marker that
// the jail's own filesystem, network, and credential isolation do not gate
// independently.
func resolveBootIdentity(args []string, sandboxed bool) bootIdentity {
	if len(args) == 0 {
		return bootServer
	}
	if sandboxed {
		return bootJailCLI
	}
	return bootHostCLI
}

// inRunSandbox reports whether this process is running inside the agent jail,
// per the marker the sandbox env assembler sets. Exact match: any other value
// means the variable came from somewhere that isn't the assembler, and the only
// safe reading of that is "not sandboxed" — a truthy-looking stray export must
// not take a host CLI down the jailed path.
func inRunSandbox() bool {
	return os.Getenv(agentproc.SandboxMarkerEnvVar) == agentproc.SandboxMarkerEnvValue
}
