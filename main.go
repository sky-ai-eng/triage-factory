package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
	"github.com/sky-ai-eng/triage-factory/ee"
	"github.com/sky-ai-eng/triage-factory/internal/app"
	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/procname"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"

	// Enterprise Edition SSO: registers its store factories, route installer,
	// and LoginExtension via init(). Gated at runtime on the `sso` entitlement.
	_ "github.com/sky-ai-eng/triage-factory/ee/sso"

	// Enterprise Edition Slack workspace connect: registers its store
	// factories and route installer via init(). Gated at runtime on the
	// `slack` entitlement.
	_ "github.com/sky-ai-eng/triage-factory/ee/slack"

	// Enterprise Edition fleet console: registers the /api/fleet route
	// installer via init(). Gated at runtime on is_operator AND the
	// deployment-scoped `fleet` entitlement (entitlements.Active()).
	_ "github.com/sky-ai-eng/triage-factory/ee/fleet"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Version is the binary's release tag, set by the linker at build time
// (`-ldflags "-X main.Version=v0.1.0"`). Local builds without that flag
// see the literal "dev", so anything in the wild claiming to be "dev" is
// known to be unreleased.
var Version = "dev"

func main() {
	if err := run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "triagefactory:", err)
		os.Exit(1)
	}
}

// run is the real entrypoint. It resolves this process's boot identity,
// initializes the runtime mode, dispatches CLI subcommands, and otherwise
// boots the server — returning an error instead of calling log.Fatal so
// deferred cleanup runs and the boot path is testable.
func run(ctx context.Context, args []string) error {
	// What is this process? Answer that first, from argv plus the sandbox
	// marker, and branch on the answer — rather than letting each layer below
	// infer it from whichever ambient signal it happens to read (see
	// bootident.go). The jailed CLI is a pure RPC client: it needs none of the
	// boot spine that follows and must not run it, since every step of it
	// writes host state — starting with a SQLite file that, under the jail's
	// HOME, lands inside the agent's own worktree.
	cliArgs := resolveCLIArgs(args)
	if resolveBootIdentity(cliArgs, inRunSandbox()) == bootJailCLI {
		return dispatchJailCLI(cliArgs)
	}

	// Initialize the runtime mode flag (TF_MODE env, default local) before
	// anything touches a path or opens a DB.
	if err := runmode.InitFromEnv(); err != nil {
		return fmt.Errorf("runmode: %w", err)
	}

	// The deployment's default GitHub (TF_DEFAULT_GITHUB_HOST): the web base
	// an org with no github_base_url resolves to, and the GitHub the
	// deployment App is on. Resolved here, once, with the mode — every
	// identity below (server, host CLI, the sidecar harness) keys hosts
	// through it, so it must be the same answer for all of them. Applies in
	// both modes; a value that is not a web base fails boot rather than
	// pointing every unset workspace somewhere unresolvable.
	if err := ghbase.InitDefaultBaseURLFromEnv(); err != nil {
		return fmt.Errorf("default github host: %w", err)
	}

	// Local-mode OS sandbox posture (TF_LOCAL_SANDBOX). Resolved right after
	// the mode because it is a function of it: on is the default for local
	// mode on Linux, meaningless in multi (already a gVisor jail), and
	// unhonorable off Linux. An explicit value that cannot be honored fails
	// boot here rather than degrading silently — see runmode's doc for why
	// that direction is the only safe one for a safety knob. Whether
	// bubblewrap actually works on this host is probed separately, at server
	// boot (internal/app), so a CLI subcommand is never held hostage to it.
	if err := runmode.InitLocalSandboxFromEnv(); err != nil {
		return fmt.Errorf("runmode local sandbox: %w", err)
	}

	// Resolve the deployment role (TF_ROLE) right after the mode and
	// before the argv dispatch, so the `migrate` subcommand and every
	// subsystem see the same role. Multi mode requires control or
	// executor — anything else (unset, "all", a typo) fails boot loudly
	// with a pointer at the split blueprint, since multi is always the
	// control+executor split. Local mode ignores TF_ROLE entirely (the
	// single-process shape is gated on the mode, not a role); a set value
	// logs one warning rather than bricking a laptop over a stray env var.
	ignored, rawRole, err := runmode.InitRoleFromEnv()
	if err != nil {
		return fmt.Errorf("runmode role: %w", err)
	}
	if ignored {
		bootLog.Warn("TF_ROLE ignored in local mode: local is single-process by construction (roles exist only in multi mode)",
			"value", rawRole)
	}

	// Capture the deployment secrets (from ${NAME}_FILE if set, else the plain
	// env var) into process memory and unset them from the environment, before
	// anything reads them — including the `migrate` subcommand's DSN below.
	// After this they're gone from os.Environ(), so child processes don't
	// inherit them; readers pull them back via secretenv.Get. (A _FILE-supplied
	// secret is also absent from this process's own /proc/environ + `docker
	// inspect`; a plain env one can't be — see the secretenv package doc.)
	// Fails fast on a set-but-unreadable file.
	secretenv.SetWarnFunc(func(format string, args ...any) { bootLog.Warn(fmt.Sprintf(format, args...)) })
	if err := secretenv.Resolve(); err != nil {
		return fmt.Errorf("resolve deployment secrets: %w", err)
	}

	// CLI subcommands short-circuit before any server wiring: exec/status
	// are used by delegated Claude Code agents; install/uninstall/
	// migrate/jwk-init are user-facing. argv0 selects the surface — see
	// resolveCLIArgs for the `tfac` applet's implicit `exec`. A leading flag
	// falls through to server mode, which is why the host-cli identity and the
	// server identity share this whole boot sequence.
	if handled, err := dispatchCLI(cliArgs); handled {
		return err
	}

	// Server mode. Set this process's kernel-visible name and the "proc"
	// log tag: `ps`/`/proc/<pid>/comm` reads tf-orchestrator, and every
	// subsequent log line carries proc=orchestrator — distinguishing this
	// process from a concurrently-running cap-broker whose stdout
	// interleaves into the same container log stream. No-op-safe on
	// every platform/mode: local mode and non-Linux hosts still get the
	// title/tag, just with an empty (rather than absent) capability set
	// reported below, since they never spawn a cap-broker to compare
	// against anyway.
	procname.SetTitle("tf-orchestrator")
	logging.SetProcess("orchestrator")
	logBootLine()

	// Verify any Enterprise license token (TF_LICENSE) and register the
	// entitlements checker before wiring subsystems, so feature gates see
	// the right answer from first boot. No/invalid token → community
	// default (every enterprise feature off). Never fatal.
	ee.Install()

	// Translate SIGINT/SIGTERM into context cancellation so the
	// HTTP server and background workers shut down gracefully.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := app.LoadConfig(args[1:])
	if err != nil {
		return err
	}
	cfg.Version = Version
	static, err := frontendDist() // go:embed lives in package main (embed.go)
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	a, err := app.New(ctx, cfg, static)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := a.Close(); cerr != nil {
			fmt.Fprintln(os.Stderr, "triagefactory: close:", cerr)
		}
	}()

	fmt.Printf("Triage Factory running at %s\n", cfg.BrowserURL)
	// Multi-mode is a headless multi-tenant server — never worth a browser
	// launch attempt, regardless of whether the operator remembered
	// --no-browser. Local mode still respects the flag.
	if !cfg.NoBrowser && runmode.Current() == runmode.ModeLocal {
		openBrowser(cfg.BrowserURL)
	}

	return a.Run(ctx)
}

// resolveCLIArgs turns a full argv into the args dispatchCLI routes on: the
// program name stripped, plus the applet's implicit `exec` prefix when the
// binary was invoked as `tfac`.
//
// `tfac <verb>` and `tfac exec <verb>` resolve identically — both spellings are
// permanently valid (no deprecation, no stderr note: the applet's output lands
// in agent transcripts). At most ONE leading `exec` collapses, so
// `tfac exec exec foo` keeps the second one and falls through to the usual
// unknown-command error. The collapse is unambiguous because no `exec`
// subcommand of that name exists — this reserves it.
func resolveCLIArgs(args []string) []string {
	userArgs := args[1:]
	if filepath.Base(args[0]) != prog.AppletName {
		return userArgs
	}
	if len(userArgs) > 0 && userArgs[0] == "exec" {
		return userArgs
	}
	return append([]string{"exec"}, userArgs...)
}

var bootLog = logging.Component("boot")

// logBootLine emits one legibility boot line: this process's uid and
// effective capability set, so a security reviewer can check the
// cap-broker/orchestrator split is real by reading logs / /proc/<pid>/status
// instead of trusting the code.
// Expected to read "CapEff=(empty)" whenever the exec-time capability drop
// applied (the default, multi-mode, Linux path) — the drop happens before
// this Go binary's own code ever runs, so there is nothing left to
// enumerate but the confirmation.
func logBootLine() {
	names, err := capinfo.Effective()
	if err != nil {
		bootLog.Warn("boot: failed to read own effective capabilities", "error", err)
		return
	}
	bootLog.Info("boot", "uid", os.Getuid(), "CapEff", capinfo.Describe(names))
}
