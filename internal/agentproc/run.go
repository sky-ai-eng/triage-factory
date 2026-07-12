package agentproc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// RunOptions configures one `claude -p` invocation. Callers populate
// every field they care about; zero-values fall back to claude's
// defaults (model unset, no resume, default max turns).
type RunOptions struct {
	// Cwd is the working directory the subprocess runs in.
	Cwd string

	// Model is passed via --model. Empty omits the flag.
	Model string

	// SessionID, when non-empty, switches the invocation to
	// `--resume <id>`. Used for the crash-reclaim resume, the open-run
	// resume path, and the curator's per-message resumption against a
	// long-lived project session.
	SessionID string

	// Message is the value passed to `-p`. For an initial invocation
	// this is the full prompt (mission + envelope); for a resume it's
	// just the new user turn.
	Message string

	// AllowedTools is the comma-joined --allowedTools value. Callers
	// build this themselves (see internal/delegate.BuildAllowedTools
	// and internal/curator.BuildAllowedTools) — different runtimes
	// have different threat models.
	AllowedTools string

	// AddDirs is the list of paths passed as `--add-dir`. Claude Code's
	// per-tool path safety checks (notably the rm guard) treat the cwd
	// as the only allowed working directory by default. Subdirectories
	// like `<projectDir>/knowledge-base/` and `<projectDir>/repos/`
	// need to be added explicitly so the agent can rm files there.
	// Empty list omits the flag entirely.
	AddDirs []string

	// SystemPrompt, if non-empty, is passed as --append-system-prompt.
	// Sits after Claude Code's default system prompt rather than
	// replacing it; useful for runtime-specific role-shaping (the
	// curator's "you are the Curator for project X" prompt) without
	// clobbering CC's safety / tool-use defaults.
	SystemPrompt string

	// MaxTurns sets --max-turns. Zero omits the flag.
	MaxTurns int

	// Interactive switches the invocation into the SDK's streaming-input
	// mode: BuildArgs emits `--input-format stream-json` and omits
	// `-p`/Message (the initial message is sent over stdin by the
	// caller). This is the only mode in which the SDK exposes its live
	// controls — interrupt, setPermissionMode, and the canUseTool
	// permission callback — so RunInteractive sets it. The one-shot Run
	// path leaves it false.
	Interactive bool

	// PermissionPrompts opts the streaming-input wrapper into the
	// canUseTool permission callback. The wrapper sets options.canUseTool
	// ONLY when BuildArgs emits the matching --permission-prompts flag,
	// which it does only when this is true. RunInteractive derives it
	// from whether the caller supplied a non-nil PermissionHandler:
	//
	//   - handler supplied   → PermissionPrompts=true  → wrapper wires
	//     canUseTool, so off-allowlist tools route to the handler (the
	//     "ask" path); allowlist matches still short-circuit to allow.
	//   - handler nil        → PermissionPrompts=false → wrapper omits
	//     canUseTool entirely → behavior is byte-identical to the
	//     headless allowlist-only path (off-allowlist tools auto-deny,
	//     no callback). This is what keeps autonomous runs prompt-free.
	//
	// Without this opt-in the streaming wrapper would set canUseTool
	// unconditionally and a nil handler would deny-all everything off the
	// --allowedTools list. Interactive-mode only; the one-shot path
	// ignores it.
	PermissionPrompts bool

	// ExtraEnv is appended to os.Environ() for the subprocess. Use
	// this for run-scoped variables like TRIAGE_FACTORY_RUN_ID and
	// TRIAGE_FACTORY_REPO that the delegated CLI subcommands read.
	ExtraEnv []string

	// GitUserName / GitUserEmail, when both set, stamp the org's GitHub
	// identity as the author + committer of every commit the agent makes —
	// injected as process-scoped user.name / user.email git config alongside
	// core.hooksPath (githooks.IdentityConfigPairs), in both run modes. The
	// delegate resolves them from the org GitHub identity (TFAC-452); empty
	// (either unset) skips the injection entirely, so the agent inherits ambient
	// git config exactly as before — preserving today's behavior and tests.
	GitUserName  string
	GitUserEmail string

	// TraceID is stamped onto every emitted message's RunID field.
	// Storage-neutral: delegate uses the agent run UUID, the curator
	// uses its own message-group id.
	TraceID string

	// OrgID scopes credential resolution for this invocation. In
	// multi mode the runner resolves the org's configured Anthropic /
	// Bedrock credentials via Secrets and injects them into the Node
	// subprocess's env, stripping credential-bearing keys from the
	// inherited env so a misconfigured operator-level env var can't
	// leak across tenants. In local mode an empty OrgID — or the
	// LocalDefaultOrgID sentinel with no configured per-org override —
	// falls back to the host's Claude Code subscription via the
	// inherited env (no env injection or filtering).
	//
	// Required in multi mode; empty + multi-mode raises a typed
	// ErrNoCredentialsConfigured error before the subprocess spawns.
	OrgID string

	// Secrets is the per-org secret reader used when OrgID is non-
	// empty. Callers pass their db.SecretStore (which satisfies
	// SecretsReader structurally). May be nil in local mode with
	// empty OrgID — the resolver no-ops and the subprocess inherits
	// the host env unchanged.
	Secrets SecretsReader

	// OnResult, when non-nil, is invoked once per turn-terminal `result`
	// envelope the streaming-input reader folds, with that turn's parsed
	// Result. It is the per-turn signal a live caller needs to react
	// promptly to a completed turn (the delegate driver uses it to detect
	// the autonomous run's terminal turn and close the process, rather
	// than waiting for the whole query to drain). Called from the reader
	// goroutine; it must not block. Ignored by the one-shot Run path
	// (which returns on the first result anyway) — only RunInteractive
	// threads it through.
	OnResult func(*Result)

	// GitProxy, when non-nil, wires a per-run git credential proxy into
	// the sandbox branch so the agent can push/fetch over git-over-HTTPS
	// without the real GitHub credential ever entering the box. The
	// caller (delegate spawner) builds it over the GitHub resolver's
	// TokenFor (App-or-PAT); the proxy holds the token host-side and the
	// sandbox git is routed at it via injected GIT_CONFIG env entries.
	//
	// nil for runs with no git egress need — prompt-only scorer /
	// classifier / profiler calls, and Jira-only runs that pre-clone
	// nothing. Local-mode + non-sandbox paths ignore it (the agent runs
	// directly on the host with the operator's own git credentials).
	GitProxy *GitProxyConfig

	// LLMResolver, when non-nil, resolves the org's LLM env map instead of
	// Run's built-in raw-secret resolution — the seam the brain wires to
	// internal/llmcred so a role-mode Bedrock org mints short-lived STS
	// session credentials (there is no raw key to read). It is consulted
	// ONLY on the resolve-from-secrets path: a run carrying a sealed bundle
	// on ctx (the executor) still reads bundle.LLM, unchanged. The map's
	// semantics match resolveCredentials' — a non-nil empty map means
	// "inherit the host's ambient credentials" (local subscription); a nil
	// LLMResolver keeps today's behavior in every mode. Set at the
	// brain-side / all-local call sites (delegate, scorer, profiler,
	// classifier); a mint failure surfaces here and the caller skips.
	LLMResolver func(ctx context.Context, orgID string) (map[string]string, error)

	// LLMCredentialSource, when non-nil, is the executor's live re-reader of
	// the run's newest sealed LLM material (the analog of GitProxy's
	// TokenSource). startProxiesForSandbox wires it into the SigV4 proxy so a
	// role-mode run whose STS session credentials are re-minted mid-run picks
	// up the fresh triple without a proxy restart — reading the newest sealed
	// bundle, never the run-start snapshot. Returns the newest bundle.LLM env
	// map plus its expiry (zero = non-expiring). Wired by the delegate on the
	// executor bundle path only; nil everywhere else keeps the run-start
	// snapshot (bearer / anthropic, brain-side consumers, local).
	LLMCredentialSource func(ctx context.Context) (env map[string]string, expiry time.Time, err error)

	// StartAgentHost, when non-nil, starts the per-run host agenthost
	// daemon in the sandbox branch. The daemon owns the run identity
	// and serves the RPCs the sandboxed `triagefactory exec`
	// subcommands send over /run/tf.sock; the callback returns the
	// bind-mount entry agentproc adds to the sandbox spec plus a
	// closer the deferred chain calls on Run exit (normal, error,
	// or ctx cancellation).
	//
	// Callback indirection rather than an agenthost.Server-typed
	// field because agentproc must NOT import cmd/exec/agenthost —
	// that package transitively imports internal/agentmeta →
	// internal/ai → internal/agentproc and the cycle would block
	// compile. The closure shape lets the delegate spawner own all
	// the agenthost wiring while agentproc stays library-style on
	// the seam between sandbox setup and the agent subprocess.
	//
	// Local-mode + non-sandbox paths ignore this entirely. Callers
	// whose argv never invokes `triagefactory exec` (classifier /
	// scorer / profiler one-shot calls) leave this nil; the sandbox
	// branch detects nil and skips the daemon setup. Skipping is safe
	// because no agenthost client will ever try to dial in those
	// flows.
	StartAgentHost func() (mount sandbox.Mount, closer io.Closer, err error)

	// ReadOnlyRepoMounts are extra host directories bind-mounted READ-ONLY
	// into the sandbox under Cwd. The Curator populates this with its shared
	// per-(org, repo) pinned-repo worktrees so the agent reads them without a
	// per-session checkout copy and cannot write them (the mount is ro, so an
	// in-jail write fails). Each Source MUST live outside Cwd — the per-run
	// chown of the writable Cwd never touches the shared tree.
	//
	// Sandbox-only: the direct (local / non-Linux) path ignores these entirely
	// because local materializes the worktree under Cwd directly (N=1, no jail,
	// no cross-session sharing). See ReadOnlyRepoMount. TFAC-61.
	ReadOnlyRepoMounts []ReadOnlyRepoMount
}

// ReadOnlyRepoMount is one host directory exposed read-only inside the sandbox
// at RelPath under the agent's Cwd (/work). agentproc creates the empty
// mount-point directory under Cwd (so it's owned by the sandbox UID like the
// rest of /work and exists as a bind target inside the /work mount), then adds
// a `ro`-option nested bind mount of Source onto /work/<RelPath>.
type ReadOnlyRepoMount struct {
	// Source is the host path of the shared worktree to expose read-only. It
	// MUST be outside the run's Cwd so chownWorktreeForSandbox never recurses
	// into it (chowning a shared tree to one session's UID is the bug this
	// avoids).
	Source string
	// RelPath is the mount location relative to Cwd, e.g. "repos/owner/repo".
	RelPath string
}

// NoopSink discards all stream events. Suitable for one-shot agent
// calls (classifier, scorer, profiler) that only care about the
// terminal Outcome.Result.Result string and don't need to persist
// per-message rows or push to a websocket. The parsing overhead per
// message is negligible for the few-second calls these sites make.
type NoopSink struct{}

func (NoopSink) OnSession(string) error               { return nil }
func (NoopSink) OnMessage(*domain.AgentMessage) error { return nil }

// UsageSink accumulates aggregate token usage across one Run. It's the
// drop-in replacement for NoopSink at the one-shot system-job call sites
// (scorer, repo-profiler, classifier) that want the token breakdown the
// per-message usage already carries (stream.go populates InputTokens /
// OutputTokens / CacheReadTokens / CacheCreationTokens on each assistant
// message) without persisting a transcript. The terminal Result still
// carries CostUSD/duration/turns; this sink fills the token gap so
// system_llm_runs can record cache-rate breakdowns. See TFAC-451.
//
// Summing across messages assumes each AgentMessage carries its own turn's
// token counts, NOT a cumulative running total — true for the Claude Agent
// SDK stream, where every assistant message reports the usage of the API
// call that produced it. A provider that instead emitted running totals on
// the final message would be double-counted here; revisit this if a
// multi-turn path is ever pointed at such a backend.
//
// Not concurrency-safe — construct one per Run call (Run drives the sink
// from a single goroutine, same contract as every other Sink).
type UsageSink struct {
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int
}

func (s *UsageSink) OnSession(string) error { return nil }

func (s *UsageSink) OnMessage(m *domain.AgentMessage) error {
	if m == nil {
		return nil
	}
	if m.InputTokens != nil {
		s.InputTokens += *m.InputTokens
	}
	if m.OutputTokens != nil {
		s.OutputTokens += *m.OutputTokens
	}
	if m.CacheReadTokens != nil {
		s.CacheReadTokens += *m.CacheReadTokens
	}
	if m.CacheCreationTokens != nil {
		s.CacheCreationTokens += *m.CacheCreationTokens
	}
	return nil
}

// Sink is the storage-side adapter that turns parsed stream events
// into rows + websocket pushes. Implementations are constructed per
// invocation (they typically close over a runID or projectID) and are
// not concurrency-safe — Run drives the sink from a single goroutine.
type Sink interface {
	// OnSession fires once, the first time the stream emits a
	// system/init event with a session_id. Implementations persist
	// the id to whatever table owns "this conversation's resume key"
	// (runs.session_id for delegate; projects.curator_session_id
	// for the curator). Returning an error is logged but does not
	// abort the run — the stream continues and the result still
	// lands; callers can re-attempt session capture on resume.
	OnSession(sessionID string) error

	// OnMessage fires per fully-accumulated assistant or tool message.
	// Implementations insert + broadcast. Returning an error is
	// logged and skipped; the run does not abort because a single
	// row failed to insert.
	OnMessage(msg *domain.AgentMessage) error
}

// Outcome bundles what Run observed: the terminal Result (nil if no
// `result` event was seen), the captured session id (empty if the
// stream never emitted system/init), and the captured stderr buffer
// (full — callers truncate for display).
//
// SessionID is exposed here in addition to flowing through Sink.OnSession
// so callers that need it post-run (memory-gate retry, takeover
// validation) don't have to plumb their own capture into the sink.
type Outcome struct {
	Result    *Result
	SessionID string
	Stderr    string
}

// Run spawns `claude` with the given options, pumps the stream-json
// output through Sink, and waits for the subprocess to exit.
//
// Cancellation: when ctx is cancelled mid-run, the goroutine sends
// SIGKILL to the entire process group (Setpgid is used so child
// processes the agent spawned go down with it), then waits for the
// subprocess to exit and returns ctx.Err().
//
// Error semantics:
//   - nil error + non-nil Outcome.Result: normal termination; caller
//     processes the Result (memory gate, completion JSON, etc.).
//   - nil error + nil Outcome.Result: subprocess exited cleanly
//     without emitting a `result` event. Treat as involuntary failure.
//   - non-nil error: argv-build / Start failure, stream malformed
//     mid-stream, subprocess crashed, or ctx cancelled. Outcome.Stderr
//     is populated when the subprocess produced any.
func Run(ctx context.Context, opts RunOptions, sink Sink) (*Outcome, error) {
	// Derived ctx so the stream-error path can SIGKILL the process
	// group via cmd.Cancel without affecting the caller's ctx. Without
	// this, a stream read failure (cap exceeded, malformed mid-stream)
	// would leave the subprocess alive with bytes still to write; the
	// kernel stdout pipe fills, the subprocess blocks on write, and
	// cmd.Wait below deadlocks indefinitely.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// EnsureSDK installs Node + Agent SDK + wrapper.mjs on first call
	// and returns the absolute wrapper path. Failures here are usually
	// "Node not on PATH" — surface them to the caller so the run lands
	// in failed state with a clear message rather than spawning a
	// broken subprocess.
	wrapperPath, err := EnsureSDK()
	if err != nil {
		return nil, fmt.Errorf("agent runtime: %w", err)
	}

	// We spawn the SDK via `node wrapper.mjs <flags>` rather than the
	// `claude` CLI: the wrapper translates the flag-based argv BuildArgs
	// emits into Agent SDK Options, so the call site stays runtime-agnostic,
	// and the SDK shares Claude Code's auth / config / session store so
	// behavior is identical for the user.
	//
	// Branch: multi-mode + Linux routes through the gVisor sandbox for
	// tenant isolation (newSandboxCommand — shared with the interactive
	// RunInteractive); local-mode + non-Linux take the direct subprocess
	// path (newDirectCommand, unchanged behavior; its Setpgid + Cancel hook
	// own the SIGKILL-the-process-group teardown). For the sandbox branch,
	// cleanup bundles every teardown (sandbox, agenthost daemon, scratch
	// dir, proxies); deferring it here fires it on every exit, including the
	// StdoutPipe / Start error returns in the shared tail below.
	var proc runProc
	if shouldSandbox() {
		sandboxProc, cleanup, serr := newSandboxCommand(runCtx, opts, wrapperPath)
		if serr != nil {
			return nil, serr
		}
		defer cleanup()
		proc = sandboxProc
	} else {
		nodeArgs := append([]string{wrapperPath}, BuildArgs(opts)...)
		// Local-mode TF_CLAUDE_BINARY override (non-sandbox path only): point the
		// SDK at a specific Claude binary. Validated here so a bad path fails the
		// spawn with a clear message rather than an opaque SDK exec error. The
		// sandbox branch deliberately ignores it — it runs the image-baked binary.
		bin, berr := claudeBinaryOverride()
		if berr != nil {
			return nil, berr
		}
		if bin != "" {
			nodeArgs = append(nodeArgs, "--claude-bin", bin)
		}
		directCmd, derr := newDirectCommand(runCtx, opts, nodeArgs)
		if derr != nil {
			return nil, derr
		}
		directProc, perr := newExecProc(directCmd)
		if perr != nil {
			return nil, perr
		}
		proc = directProc
	}

	if err := proc.Start(); err != nil {
		return &Outcome{Stderr: proc.Stderr()}, fmt.Errorf("start agent runtime: %w", err)
	}

	stream := NewStreamState()
	result, streamErr := consumeStream(proc.Stdout(), sink, stream, opts.TraceID)

	// If the stream reader bailed before a terminal result, the
	// subprocess is likely still running and may have more data to
	// write. Kill the process (via ctx cancel) now so Wait below doesn't
	// block forever on a stuck pipe write.
	if streamErr != nil && result == nil {
		cancel()
	}

	waitErr := proc.Wait()

	outcome := &Outcome{
		Result:    result,
		SessionID: stream.SessionID(),
		Stderr:    proc.Stderr(),
	}

	// Stream-level malformation with no terminal result is the
	// stronger signal — surface it before any wait error.
	if streamErr != nil && result == nil {
		return outcome, fmt.Errorf("stream: %w", streamErr)
	}

	// Wait without a captured result is involuntary failure; let the
	// caller distinguish cancel from crash via ctx.Err(). Checked before
	// the deferred cleanup tears the cgroup down, so the OOM attribution
	// reads live state.
	if waitErr != nil && result == nil {
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		if proc.OOMKilled() {
			return outcome, fmt.Errorf("agent runtime killed: %w (%d MB; tune TF_RUN_MEMORY_LIMIT_MB): %v",
				ErrRunMemoryLimit, runMemoryLimitMB(), waitErr)
		}
		return outcome, fmt.Errorf("agent runtime exited with error: %w", waitErr)
	}

	return outcome, nil
}

// newSandboxCommand builds the gVisor-sandboxed `node /sdk/wrapper.mjs`
// command for one multi-mode run and returns it alongside a cleanup func
// that tears down everything the bring-up allocated. Shared by the one-shot
// Run and the interactive RunInteractive so both spawn — and tear down —
// sandbox runs identically.
//
// cleanup bundles the teardowns in defer-LIFO order — sb.Close() → agenthost
// closer.Close() → scratch-dir remove → proxies.Shutdown() — matching the
// inline-defer order the one-shot Run used before this extraction. It is
// idempotent (a sync.Once guards the body), so a caller may defer it even on
// a path that also invokes it directly. The proxy shutdown runs on a fresh
// 5 s detached context so a cancelled run still drains cleanly rather than
// tearing the TCP close and leaking the proxy goroutine.
//
// On error, cleanup has already run for whatever was half-allocated (mirroring
// sandbox.Wrap's own self-cleanup contract); the returned cleanup is then a
// spent no-op, safe to defer or ignore.
//
// PROPERTY B INVARIANT: credentials are resolved here on the host side, then
// routed through a per-run LLM proxy bound to the sandbox's host-side veth IP
// (see the configureProxies callback). The agent's env carries only the proxy
// URL + a per-run placeholder credential; the real key lives in the proxy
// process on the host and is injected into the upstream HTTP request right
// before it leaves the box.
func newSandboxCommand(runCtx context.Context, opts RunOptions, wrapperPath string) (runProc, func(), error) {
	// Teardown state, accumulated as each setup step below succeeds. cleanup
	// runs the undos in LIFO order and is single-shot via once, so the error
	// paths can invoke it eagerly and the caller can still defer it safely.
	var (
		proxies    *runProxies
		scratchCwd string
		ahCloser   io.Closer
		sb         *sandbox.Sandbox
		run        sandbox.LaunchedRun
		sidecar    sandbox.LaunchedSidecar
		once       sync.Once
	)
	cleanup := func() {
		once.Do(func() {
			// The run process + its memory cgroup first: kill the runtime (or
			// reclaim the cgroup on an abandoned bring-up) before its netns is
			// torn down below.
			if run != nil {
				_ = run.Close()
			}
			// The credential sidecar next, BEFORE sb.Close() below releases the
			// subnet index its uid was derived from — a concurrent new run must
			// never be handed that uid while this run's sidecar might still be
			// exiting.
			if sidecar != nil {
				_ = sidecar.Close()
			}
			if sb != nil {
				_ = sb.Close()
			}
			if ahCloser != nil {
				_ = ahCloser.Close()
			}
			if scratchCwd != "" {
				// Via the privileged seam, not os.RemoveAll: the scratch
				// cwd was handed to the sandbox identity at run start, and
				// the run may have left files inside that the post-drop
				// orchestrator cannot unlink itself. Background context —
				// this is teardown; the run's own ctx may be canceled.
				_ = sandbox.RemoveRunTree(context.Background(), scratchCwd)
			}
			if proxies != nil {
				// Detached context so a cancelled run still gets a clean
				// Shutdown drain rather than a torn TCP close that leaks the
				// proxy goroutine.
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutdownCancel()
				if err := proxies.Shutdown(shutdownCtx); err != nil {
					agentprocLog.Warn("proxy shutdown failed", "error", err)
				}
			}
		})
	}

	sdkDir := filepath.Dir(wrapperPath)

	creds, err := resolveCredentials(runCtx, opts.Secrets, opts.OrgID, opts.LLMResolver)
	if err != nil {
		cleanup()
		return nil, cleanup, fmt.Errorf("resolve credentials for org %q: %w", opts.OrgID, err)
	}

	// Some callers (scorer, classifier stage1, profiler) are prompt-only —
	// they send a prompt, get JSON back, and never touch the host filesystem.
	// They have no natural Cwd. The sandbox still needs *something* to
	// bind-mount at /work, so when the caller didn't pass one, materialize a
	// per-run scratch tmpdir. Removed by cleanup.
	workCwd := opts.Cwd
	if workCwd == "" {
		scratch, mkErr := os.MkdirTemp("", "agentproc-scratch-")
		if mkErr != nil {
			cleanup()
			return nil, cleanup, fmt.Errorf("sandbox: scratch cwd: %w", mkErr)
		}
		scratchCwd = scratch
		workCwd = scratch
	}
	// Create the empty mount-point dirs the read-only repo bind-mounts land on
	// BEFORE the chown — so they're owned by the sandbox UID like the rest of
	// /work, and exist as bind targets inside the /work mount when Wrap runs.
	// The shared worktree each one exposes lives outside workCwd and is never
	// chowned here (skipping the per-session chown of the shared tree is the
	// whole point — TFAC-61).
	if err := ensureRepoMountPoints(workCwd, opts.ReadOnlyRepoMounts); err != nil {
		cleanup()
		return nil, cleanup, fmt.Errorf("sandbox: %w", err)
	}
	if err := chownWorktreeForSandbox(runCtx, workCwd); err != nil {
		cleanup()
		return nil, cleanup, fmt.Errorf("sandbox: chown worktree: %w", err)
	}

	// Translate any host-path env values (e.g. TRIAGE_FACTORY_RUN_ROOT) to
	// /work-relative paths before the sandbox sees them.
	sbExtraEnv := translateEnvForSandbox(opts.ExtraEnv, workCwd)
	// The pre-push hook (A·3, TFAC-460) invokes the binary via this env
	// entry; in the sandbox it's the fixed bind-mount path, which is also on
	// PATH but set explicitly so the hook never depends on PATH contents. A
	// literal sandbox path, so it goes in after translateEnvForSandbox.
	sbExtraEnv = append(sbExtraEnv, githooks.BinEnvVar+"="+sandboxTFBinary)
	sbEnv := buildSandboxEnv(sbExtraEnv)

	// Translate AddDirs (host paths under workCwd) into their /work-relative
	// equivalents inside the sandbox. Without this the agent's `--add-dir`
	// flags reference paths that don't exist inside the sandbox rootfs and
	// Claude Code's per-tool path checks reject every write attempt to those
	// subtrees. Build a shallow copy of opts so BuildArgs picks up the
	// translated paths without mutating the caller's struct.
	sandboxOpts := opts
	sandboxOpts.AddDirs = translateAddDirsForSandbox(opts.AddDirs, workCwd)
	// Rewrite the --allowedTools selfBin pattern to point at the in-sandbox
	// path. BuildAllowedTools embeds os.Executable() in its
	// `Bash(<selfBin> exec *)` rule; inside the sandbox that host path
	// doesn't exist, so the agent's per-tool path check would reject every
	// `triagefactory exec` invocation. Re-point to the canonical
	// /usr/local/bin/triagefactory path we bind-mount below.
	hostSelfBin, _ := os.Executable()
	sandboxOpts.AllowedTools = rewriteAllowedToolsForSandbox(opts.AllowedTools, hostSelfBin)

	// Extra bind mounts the sandbox needs:
	//
	//   1. The host TF binary at /usr/local/bin/triagefactory (RO). The
	//      agent's `triagefactory exec ...` invocations exec this path;
	//      without the bind-mount they ENOENT because the host path isn't
	//      visible inside the alpine rootfs.
	//
	//   2. The per-run agenthost unix socket at /run/tf.sock (RW). Started
	//      below when StartAgentHost is supplied. Caller-side hostAgentHost
	//      handles chown/chmod so the sandbox UID can connect.
	extraMounts := []sandbox.Mount{}
	if hostSelfBin != "" {
		extraMounts = append(extraMounts, sandbox.Mount{
			Source:      hostSelfBin,
			Destination: sandboxTFBinary,
			Options:     []string{"ro"},
		})
	}

	// The TF-controlled git hooks dir (F2, TFAC-456), bind-mounted RO at a
	// fixed in-sandbox path. core.hooksPath (set in the GIT_CONFIG_* env by
	// startProxiesForSandbox) points here, so the hooks fire for every repo
	// the agent touches. Mounted unconditionally (cheap, RO) so subdir
	// clones in a repo-less Jira run are covered too. The dir is ensured on
	// the host at startup; the os.Stat guard is purely for paths where that
	// didn't run (e.g. a unit test driving Run directly) — a missing source
	// is skipped rather than failing the run, and core.hooksPath at a
	// non-existent dir is a git no-op anyway.
	hooksDir := githooks.HostDir()
	if _, statErr := os.Stat(hooksDir); statErr == nil {
		extraMounts = append(extraMounts, sandbox.Mount{
			Source:      hooksDir,
			Destination: githooks.SandboxDir,
			Options:     []string{"ro"},
		})
	}

	// Start the per-run agenthost daemon (when wired). The socket must exist
	// on disk before sandbox.Wrap reads the spec, since the spec references
	// the source path of every bind mount. cleanup owns the daemon's Close so
	// a Wrap failure (or run exit) tears it down cleanly.
	if opts.StartAgentHost != nil {
		mount, closer, ahErr := opts.StartAgentHost()
		if ahErr != nil {
			cleanup()
			return nil, cleanup, fmt.Errorf("sandbox: start agenthost daemon: %w", ahErr)
		}
		extraMounts = append(extraMounts, mount)
		ahCloser = closer
	}

	// Shared read-only pinned-repo worktrees (Curator multi-mode). Each is
	// nested under /work — the base spec mounts /work first, then these
	// overlay onto the empty mount points created above, exactly as /dev/pts
	// nests under /dev. The "ro" option is load-bearing: it's what makes an
	// in-jail write to the shared tree fail, so one session can't mutate what
	// another reads.
	extraMounts = append(extraMounts, readOnlyRepoMounts(opts.ReadOnlyRepoMounts)...)

	// The sandbox's argv targets /usr/bin/node (the apk-installed nodejs in
	// the cached alpine rootfs) + /sdk/wrapper.mjs (the SDK bind-mount
	// destination), not host paths. Build the sandbox-side argv from scratch.
	argv := append(
		[]string{"/usr/bin/node", "/sdk/wrapper.mjs"},
		BuildArgs(sandboxOpts)...,
	)

	// Multi-mode credential injection: when the sandbox calls ConfigureProxies
	// after wiring the netns, we start the LLM proxy on the host-side veth IP
	// and return the env entries naming it. The proxy holds the real key on
	// the host side; the sandbox env carries only the proxy URL + placeholder.
	// See proxies.go for the mapping from resolved creds to proxy provider /
	// upstream.
	configureProxies := func(s *sandbox.Sandbox) ([]string, error) {
		// Fold the org commit identity (TFAC-452) into the sandbox's GIT_CONFIG_*
		// block alongside core.hooksPath + the proxy pairs. Empty identity → no
		// pairs added.
		identityPairs := githooks.IdentityConfigPairs(opts.GitUserName, opts.GitUserEmail)
		bundle, proxyEnv, perr := startProxiesForSandbox(runCtx, s.HostIP, creds, opts.GitProxy, opts.LLMCredentialSource, identityPairs...)
		if perr != nil {
			return nil, perr
		}
		// Hand the bundle to the enclosing scope so cleanup tears down the
		// proxy on every exit path (normal, error, ctx cancellation).
		proxies = bundle
		return proxyEnv, nil
	}

	sandboxRun, sboxObj, err := sandbox.Wrap(runCtx, sandbox.Config{
		RunID:            opts.TraceID,
		Worktree:         workCwd,
		SDKDir:           sdkDir,
		Argv:             argv,
		Env:              sbEnv,
		ExtraMounts:      extraMounts,
		ConfigureProxies: configureProxies,
		MemoryLimitMB:    runMemoryLimitMB(),
	})
	if err != nil {
		// Wrap cleaned up its own partial state, but configureProxies may
		// have started the proxies before a later Wrap step failed — cleanup
		// shuts them down (plus the agenthost daemon + scratch dir).
		cleanup()
		return nil, cleanup, fmt.Errorf("sandbox: %w", err)
	}
	sb = sboxObj
	run = sandboxRun

	// Broker-spawn this run's capless credential-sidecar process (an inert
	// skeleton today; nothing here changes the run's behavior). Positioned
	// right after Wrap returns, alongside where
	// StartAgentHost/ConfigureProxies ran above, so it lives beside the
	// other per-run bring-up seams a later phase (proxy + agenthost
	// relocation) will extend. sb.SubnetIdx is the same index Wrap just
	// allocated for the run's own netns, so the sidecar's uid can never
	// collide with a concurrently live run's.
	sidecarHandle, serr := sandbox.LaunchSidecar(runCtx, sandbox.SidecarConfig{
		RunID:     opts.TraceID,
		SubnetIdx: sb.SubnetIdx,
	})
	if serr != nil {
		cleanup()
		return nil, cleanup, fmt.Errorf("sandbox: launch credential sidecar: %w", serr)
	}
	sidecar = sidecarHandle

	// The LaunchedRun satisfies runProc (Start/Stdin/Stdout/Stderr/Wait/
	// OOMKilled); cleanup closes it (kill + cgroup) alongside the rest of
	// the bring-up.
	return sandboxRun, cleanup, nil
}

// newDirectCommand builds the direct (non-sandbox) `node wrapper.mjs`
// command, resolving per-org LLM credentials before spawning Node so
// multi-mode-with-no-creds (in local-mode-but-no-keychain boxes) fails
// fast with a typed error rather than waiting for the SDK to ENOAUTH
// inside the subprocess. The resolver returns an empty map when no
// per-org credentials are configured; mergeEnv detects that and
// preserves the inherited env (subscription / shell-env fallback).
//
// Setpgid + the Cancel hook give the caller hard teardown: cancelling
// runCtx SIGKILLs the whole process group so children the agent spawned
// go down with it. Shared by the one-shot Run and the interactive
// RunInteractive so both spawn identically.
func newDirectCommand(runCtx context.Context, opts RunOptions, nodeArgs []string) (*exec.Cmd, error) {
	creds, err := resolveCredentials(runCtx, opts.Secrets, opts.OrgID, opts.LLMResolver)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials for org %q: %w", opts.OrgID, err)
	}
	cmd := exec.CommandContext(runCtx, "node", nodeArgs...)
	cmd.Dir = opts.Cwd
	// Install the TF-controlled git hooks dir as process-scoped
	// core.hooksPath for this agent (F2, TFAC-456). This is the local /
	// non-sandbox path — the sandbox sets the same key via its GIT_CONFIG_*
	// block (startProxiesForSandbox), so it deliberately lives here, off the
	// sandbox branch, to avoid two GIT_CONFIG_COUNT sources colliding.
	// DirectAgentEnv layers our entry over the assembled env at the next
	// free index (dropping + re-emitting the inherited count), so a
	// pre-existing operator GIT_CONFIG_* set is preserved and the operator's
	// ~/.gitconfig is never touched — env-scoped only.
	//
	// hookEnv carries the binary path the pre-push hook (A·3, TFAC-460)
	// invokes — os.Executable() here, since in local mode the binary is
	// wherever the operator ran it from, not necessarily on PATH. An
	// os.Executable() error is non-fatal: the hook falls back to PATH.
	hookEnv := opts.ExtraEnv
	if selfBin, exeErr := os.Executable(); exeErr == nil {
		hookEnv = append(append([]string(nil), opts.ExtraEnv...), githooks.BinEnvVar+"="+selfBin)
	}
	// Layer the org commit identity (user.name/user.email) into the same
	// GIT_CONFIG_* block as core.hooksPath, at the next free indices. Empty
	// identity → IdentityConfigPairs returns nil → block carries hooks alone
	// (unchanged behavior). TFAC-452.
	identityPairs := githooks.IdentityConfigPairs(opts.GitUserName, opts.GitUserEmail)
	// Engine runtime tuning rides ExtraEnv's lane. Strip any inherited
	// jscJITEnvKey from the parent env first rather than relying on
	// duplicate-key precedence — that resolution order is
	// platform/libc dependent (the same rationale mergeEnv already
	// documents for credentialEnvKeys), so a stale shell export could
	// otherwise defeat either the default-off behavior or the
	// TF_AGENT_JSC_JIT=1 opt-in, depending on direction and platform.
	parentEnv := filterEnv(os.Environ(), []string{jscJITEnvKey})
	hookEnv = append(append([]string(nil), hookEnv...), agentRuntimeEnv()...)
	cmd.Env = githooks.DirectAgentEnv(mergeEnv(parentEnv, hookEnv, creds), identityPairs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Process is non-nil here because the watcher only fires after
		// Start has succeeded. ESRCH is fine — it just means the
		// process group already exited on its own between Wait
		// returning and the cancel watcher reading runCtx.Done(),
		// which is a race exec handles internally.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd, nil
}

// envClaudeBinary is the local-mode override for the Claude binary the Agent
// SDK launches. Honored only on the direct (non-sandbox) spawn path; the
// sandboxed multi-tenant path runs the image-baked binary and ignores it.
const envClaudeBinary = "TF_CLAUDE_BINARY"

// claudeBinaryOverride resolves TF_CLAUDE_BINARY to an absolute path to an
// executable file, or "" when unset (normal binary resolution). A set-but-
// unusable value — missing, a directory, or not executable — is a hard error:
// an explicit override that's wrong should fail the spawn loudly rather than
// silently fall back to the bundled binary and mask the misconfiguration.
func claudeBinaryOverride() (string, error) {
	// Local mode only — independent of the sandbox routing. shouldSandbox() is
	// (ModeMulti && Linux), so a multi-mode process on a non-Linux host would
	// otherwise take the direct spawn path and honor this global override,
	// undercutting the image-baked-binary assumption. Gating on the mode here
	// keeps the behavior matching the docs and survives any change to where the
	// sandbox/direct split is drawn.
	if runmode.Current() != runmode.ModeLocal {
		return "", nil
	}
	raw := strings.TrimSpace(os.Getenv(envClaudeBinary))
	if raw == "" {
		return "", nil
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", envClaudeBinary, raw, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", envClaudeBinary, raw, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s=%q is a directory, not a Claude binary", envClaudeBinary, raw)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s=%q is not executable", envClaudeBinary, raw)
	}
	return abs, nil
}

// maxStreamLineBytes caps a single NDJSON line. Well above any
// legitimate tool_result (Claude truncates Read/Bash output internally
// long before this) but low enough that a wedged or misbehaving
// subprocess that streams without ever emitting a newline gets
// surfaced as a clear stream error instead of growing the heap.
const maxStreamLineBytes = 64 * 1024 * 1024

// consumeStream scans NDJSON output, drives the Sink, and returns the
// first `result` event it sees. Sink errors are logged and skipped so
// a transient DB hiccup on one row doesn't abandon the whole run.
//
// Session id is delivered to the sink the first time it appears, not
// at stream close — any mid-run consumer (the future curator UI, a
// memory-gate retry, a takeover) can read it without waiting for the
// stream to complete.
//
// Reader choice: a bounded readLine loop instead of bufio.Scanner
// because each NDJSON line is one whole stream event, and a single
// tool_result event (a Read of a big file, large Bash output, a fat
// structured artifact) can easily exceed Scanner's old 1 MB per-token
// ceiling. When that ceiling was hit the run aborted with no terminal
// `result` captured, even though the subprocess kept emitting valid
// JSON we just couldn't fit. The new bound (maxStreamLineBytes) is
// generous enough that legitimate events pass through but a runaway /
// newline-less stream still fails fast rather than OOMing the process.
func consumeStream(stdout io.Reader, sink Sink, stream *StreamState, traceID string) (*Result, error) {
	reader := bufio.NewReader(stdout)

	sessionDelivered := false

	for {
		line, readErr := readLine(reader, maxStreamLineBytes)
		// readLine returns whatever bytes it has alongside the error on
		// EOF (or the full line + nil err on a clean newline). Process
		// the bytes before reacting to the error so a final unterminated
		// event isn't dropped.
		if len(line) > 0 {
			messages, result := stream.ParseLine(line, traceID)

			if !sessionDelivered {
				if sid := stream.SessionID(); sid != "" {
					if err := sink.OnSession(sid); err != nil {
						agentprocLog.Warn("sink on-session failed", "error", err)
					}
					sessionDelivered = true
				}
			}

			for _, msg := range messages {
				if err := sink.OnMessage(msg); err != nil {
					agentprocLog.Warn("sink on-message failed", "error", err)
					continue
				}
			}

			if result != nil {
				return result, nil
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return nil, nil
			}
			return nil, readErr
		}
	}
}

// readLine reads up to and including the next '\n', returning the line
// without the trailing newline. If a single line exceeds maxBytes,
// readLine stops reading and returns an error so the caller surfaces
// the stuck-stream case without OOMing on a runaway subprocess.
//
// Implemented over ReadSlice so we can check the accumulated size each
// time bufio's internal buffer fills — bufio.Reader.ReadBytes itself
// has no per-line cap and would grow its buffer until it ran out of
// memory.
func readLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxBytes {
			return nil, fmt.Errorf("stream line exceeded %d bytes; subprocess may be emitting unbounded output", maxBytes)
		}
		// ReadSlice's chunk shares bufio's internal buffer and is
		// invalidated by the next read, so always copy out.
		if err == nil {
			n := len(chunk)
			if n > 0 && chunk[n-1] == '\n' {
				chunk = chunk[:n-1]
			}
			buf = append(buf, chunk...)
			return buf, nil
		}
		if err == bufio.ErrBufferFull {
			buf = append(buf, chunk...)
			continue
		}
		// Real error (EOF or otherwise). Return whatever partial line
		// we have so an EOF-terminated final event still gets parsed.
		if len(chunk) > 0 {
			buf = append(buf, chunk...)
		}
		return buf, err
	}
}
