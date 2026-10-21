package agentproc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// shouldSandbox decides whether agent execution on this host runs inside
// the gVisor sandbox. Both conditions must hold:
//
//   - runmode.ModeMulti: local-mode users are trusted with their own
//     creds; sandboxing them is friction without isolation benefit
//     (single tenant). The local→multi-style sandbox toggle could
//     land later as defense-in-depth but isn't a v1 concern.
//   - runtime.GOOS == "linux": gVisor only works on Linux. Multi mode
//     on macOS isn't a supported config (the production runner image
//     is alpine Linux).
//
// The one consumer of this gate is the native runtime's resident tool-host
// jail (the cell backend in internal/cell/gvisor) — a multi+Linux deployment
// always runs its delegations that way. Run/RunInteractive (the SDK loop)
// never sandbox: they always take the direct spawn, wrapped on Linux in the
// bubblewrap courtesy isolation instead (opts.LocalSandbox), which is not what
// this predicate answers.
func shouldSandbox() bool {
	return runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"
}

// WillSandbox reports whether agent execution on this host runs inside the
// gVisor sandbox (multi mode + Linux — the native runtime's resident
// tool-host jail; see shouldSandbox). A caller that must pre-stage
// sandbox-only inputs branches on this. Exported form of the internal
// shouldSandbox gate so the predicate stays single-sourced.
func WillSandbox() bool {
	return shouldSandbox()
}

// errSDKLoopInMultiMode is what Run/RunInteractive return when called in
// multi mode. Multi-mode delegations are always runtime='native' — minted
// that way at the dialect-keyed enqueue, never this process's to choose —
// so a call here is a wiring bug (a regression that stamps 'sdk' again, or a
// new call site that forgot the ratchet), not a config this process can
// route around. Refusing loudly is the fail-closed choice: the SDK loop's
// only isolation on Linux is the bubblewrap courtesy sandbox (not a tenant
// boundary — see internal/agentproc/localsandbox's package doc), so silently
// spawning it in multi mode would run a multi-tenant agent on the bare host
// with none of the gVisor/broker/sidecar isolation multi mode requires.
var errSDKLoopInMultiMode = errors.New("agentproc: the SDK loop (Run/RunInteractive) refuses to spawn in multi mode — multi-mode delegations run the native loop against a cell's jail, never node directly on the host")

// refuseMultiModeSDKLoop is the one guard Run and RunInteractive both call
// before touching anything else, so the refusal is single-sourced and can't
// drift between the two entry points.
func refuseMultiModeSDKLoop() error {
	if runmode.Current() == runmode.ModeMulti {
		return errSDKLoopInMultiMode
	}
	return nil
}

// AgentVisibleRoot returns the absolute path the agent observes for hostRoot
// when hostRoot is the run's Cwd (the run-root). In sandbox mode the run-root
// is always presented at cell.WorkDir, so the agent sees that regardless of
// the host path; un-sandboxed the agent runs directly against hostRoot.
//
// Callers interpolate this into the prompts and tool-result messages they
// hand the agent so a concrete absolute memory/scratch path lands where the
// agent's file tools can actually write it. Those tools do no shell env
// expansion, so a bare "$TRIAGE_FACTORY_CONVERSATION_ROOT/..." reference would be
// written verbatim; pre-expanding to this value is what makes the path
// resolve identically whether the run is sandboxed or not.
func AgentVisibleRoot(hostRoot string) string {
	if shouldSandbox() {
		return cell.WorkDir
	}
	return hostRoot
}

// AgentVisibleBinary returns the path the agent must use to invoke the TF
// CLI. Under the sandbox the host binary is presented at cell.TFBinaryPath
// and the host path doesn't exist inside the rootfs, so the agent sees that
// path; un-sandboxed it runs the host binary directly. This is the
// {{BINARY_PATH}} value prompts interpolate.
func AgentVisibleBinary(hostBin string) string {
	if shouldSandbox() {
		return cell.TFBinaryPath
	}
	return hostBin
}

// ChownWorkspaceCheckoutForSandbox chowns a checkout materialized into the run
// root MID-RUN (the agenthost daemon's host-side `workspace add` create,
// TFAC-546) to the jail identity that holds the run root. The launch's
// hand-off already covered the run root, but a later create runs as the host
// process and would otherwise leave the new subtree unwritable to the jailed
// agent (EACCES on its own checkout). Covers the checkout recursively plus the
// owner/repo intermediate directories the create minted between runRoot and
// it — those must at least be traversable-and-ours so the agent can later add
// sibling checkouts' mount paths, remove its tree, etc.
//
// The identity is read off the run root rather than threaded here: the root
// is held by exactly the engagement this checkout is being added for, and the
// broker refuses the chown unless the uid named is that holder. A root no jail
// identity holds — its engagement already ended — has nobody to hand the
// checkout to, and is refused here.
//
// Routed through sandbox.ChownRunTree's subpath form (shallow intermediates,
// recursive final tree). The escape check here is a fast caller-bug guard;
// the privileged side re-validates independently, since this boundary is
// exactly what a compromised orchestrator would try to steer.
func ChownWorkspaceCheckoutForSandbox(ctx context.Context, runRoot, wtDir string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	rel, err := filepath.Rel(runRoot, wtDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("checkout %q is not inside run root %q", wtDir, runRoot)
	}
	jailUID, ok := sandbox.RunTreeJailUID(runRoot)
	if !ok {
		return fmt.Errorf("run root %q is not held by a jail identity", runRoot)
	}
	return sandbox.ChownRunTree(ctx, runRoot, rel, jailUID)
}
