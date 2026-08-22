package agentproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// directArgv returns the argv the direct spawn execs, given the `node
// wrapper.mjs …` command it was going to run.
//
// Without a Spec it returns exactly that command — argv[0] "node", resolved
// by exec against the parent's PATH — which is what the spawn did before the
// local sandbox existed and what TF_LOCAL_SANDBOX=off keeps.
//
// With one it returns the bubblewrap invocation with the same command after
// it, and node is named by an absolute path rather than left to a PATH lookup:
// the plan masks the operator's home, an nvm-installed node lives there, and a
// lookup inside the namespace would find nothing. Resolving it here is also
// what lets the resolved path be bound back through the mask.
func directArgv(opts RunOptions, nodeArgs []string) ([]string, error) {
	if opts.LocalSandbox == nil {
		return append([]string{"node"}, nodeArgs...), nil
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("local sandbox: resolve node: %w", err)
	}
	// LookPath joins the matching PATH entry with the name, so a relative entry
	// (PATH=bin:$PATH) yields a relative path. It normally reports ErrDot for
	// that too — caught above, and the unsandboxed spawn refuses the same shape
	// — but GODEBUG=execerrdot=0 suppresses the error and hands the relative
	// path back regardless. Resolve it against this process's cwd, which is the
	// directory the lookup was relative TO: the namespace chdirs to the run
	// root, where the same relative path names something else entirely, and the
	// mount plan has no real location to bind without this.
	nodeAbs, err := filepath.Abs(nodePath)
	if err != nil {
		return nil, fmt.Errorf("local sandbox: resolve node path %q: %w", nodePath, err)
	}
	nodePath = nodeAbs
	host, err := sandboxHostLayout(opts.Cwd)
	if err != nil {
		return nil, err
	}
	// The same resolution the boot probe validated through, so a host with
	// more than one bubblewrap installed runs the one that was proven to
	// work rather than whichever happens to lead PATH at spawn.
	if host.Bwrap, err = localsandbox.Resolve(); err != nil {
		return nil, fmt.Errorf("local sandbox: %w", err)
	}
	// A copy, because a run that is retried or resumed hands the same *Spec
	// back and must not accumulate the last spawn's resolved host paths.
	spec := *opts.LocalSandbox
	spec.ReadOnly = append(append([]string(nil), spec.ReadOnly...), hostToolPaths(nodePath)...)
	// The resolver files, which the /run mask would otherwise take with it —
	// see localsandbox.DNSPaths. They are resolved there rather than here
	// because the boot probe builds the same bind-backs into its smoke argv,
	// and a second copy of the resolution is how the plan the probe validates
	// drifts from the plan a run gets.
	spec.ReadOnly = append(spec.ReadOnly, localsandbox.DNSPaths()...)
	// Fold in the run's --add-dir flags here rather than at the call site, so
	// the directories the agent is GRANTED and the directories the namespace
	// BINDS are the same list by construction. A caller that adds a grant and
	// forgets the bind would hand the agent a flag pointing at a tmpfs.
	spec.AddDirs = append(append([]string(nil), spec.AddDirs...), opts.AddDirs...)
	if err := localsandbox.Preflight(spec, host); err != nil {
		return nil, err
	}
	args, err := localsandbox.Args(spec, host)
	if err != nil {
		return nil, err
	}
	return append(append(args, nodePath), nodeArgs...), nil
}

// sandboxHostLayout resolves the host half of the mount plan: what gets
// masked, and which of TF's own directories come back through the masks.
//
// The home directory is read through paths.ExpandHome rather than
// os.UserHomeDir because this is not a TF state path — it is the tree the
// plan replaces with a tmpfs — and ExpandHome is the resolver that exists for
// exactly that kind of one-off home-relative lookup.
func sandboxHostLayout(cwd string) (localsandbox.Host, error) {
	home, err := paths.ExpandHome("~")
	if err != nil {
		return localsandbox.Host{}, fmt.Errorf("local sandbox: resolve home: %w", err)
	}
	stateRoot, err := paths.StateRootErr()
	if err != nil {
		return localsandbox.Host{}, fmt.Errorf("local sandbox: resolve state root: %w", err)
	}
	claudeDir, err := paths.ExpandHome("~/.claude")
	if err != nil {
		return localsandbox.Host{}, fmt.Errorf("local sandbox: resolve claude dir: %w", err)
	}
	return localsandbox.Host{
		Home:      home,
		ClaudeDir: claudeDir,
		StateRoot: stateRoot,
		HooksDir:  paths.HooksDir(),
		GHBinDir:  paths.GHBinDir(),
		TempDir:   os.TempDir(),
		Cwd:       cwd,
	}, nil
}

// hostToolPaths are the executables and toolchain trees a run RESOLVES rather
// than configures, which the masks would otherwise take with them. Each may
// legitimately live under $HOME — an nvm node, a dev build of this binary, an
// SDK install under the state root — and none of them is knowable to the
// mount plan in advance, so they are computed here from the already-resolved
// absolute paths and bound back read-only.
//
// The TF binary matters twice over: the agent execs it for every `exec` verb,
// and the allowlist pattern names it by its host path, so a masked binary
// would fail Claude Code's per-tool path check before it ever failed to exec.
// An os.Executable error is non-fatal in the same way it is for the git-hooks
// env — the bind is simply not emitted, and a binary outside the masks needed
// none.
func hostToolPaths(nodePath string) []string {
	tools := []string{nodePath, paths.SDKDir()}
	if selfBin, err := os.Executable(); err == nil {
		tools = append(tools, selfBin)
	}
	// TF_CLAUDE_BINARY is local-mode only and points wherever the operator
	// says — frequently a checkout under $HOME, which is the whole reason it
	// belongs in this list. A malformed value is left to the spawn to report;
	// this is not the place that validates it.
	if bin, err := claudeBinaryOverride(); err == nil && bin != "" {
		tools = append(tools, bin)
	}
	return tools
}
