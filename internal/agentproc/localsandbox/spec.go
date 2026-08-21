// Package localsandbox builds the bubblewrap invocation that wraps a LOCAL
// delegated agent run in a mount namespace.
//
// It is courtesy isolation, not a security boundary — same uid, shared
// network, shared /proc, and the agent could in principle break out the way
// any same-uid process can. The framing is internal/pushpolicy's: local mode
// is not a security boundary and cannot be made one. What the namespace does
// buy is that a confused or wandering agent cannot read a sibling run's
// worktree, the SQLite database, or the operator's home directory — the three
// things that were one allowlisted `cat` away before it existed. Multi mode's
// gVisor jail is the boundary that backs a multi-tenant promise; this one
// backs no promise, which is why it keeps an escape hatch (TF_LOCAL_SANDBOX
// =off) and the jail does not.
//
// The mount plan is whole-host-read-only, then mask-and-bind-back: everything
// is visible ro, four trees are replaced by empty tmpfs (the operator's home,
// the TF state root, the temp dir, /run), and only the paths this run legibly
// needs are bound back through the masks. Masking /run is what puts the OS
// keychain out of reach — the session D-Bus and Secret Service sockets live
// under /run/user/<uid> — which is a real credential win that falls out of the
// plan rather than being built for.
//
// Two things are deliberately NOT unshared. The network stays the host's,
// because the run's git proxy, gh injector and LLM endpoint are all loopback.
// The pid namespace stays the host's, because agentproc's teardown contract is
// Setpgid plus a kill of the process group, and a nested pid namespace would
// put the agent's children on the other side of it; --die-with-parent is the
// backstop that covers what --unshare-pid would have.
package localsandbox

// Binary is the bubblewrap executable. Resolved from PATH at spawn (and at
// the boot probe), never a pinned absolute path: bubblewrap is the distro's
// package, and its AppArmor profile — the thing that permits unprivileged
// user namespaces on Ubuntu 23.10+ — is attached to the distro's own binary.
const Binary = "bwrap"

// AgentHostSocketDest is where this run's agenthost socket is bound inside
// the namespace. It mirrors cmd/exec/agenthost.DefaultSocketPath, which owns
// the constant for the client that dials it; this package cannot import that
// one (it transitively imports internal/agentproc, which imports this), so a
// drift test in internal/delegate — the package that wires both — cross-checks
// the two spellings agree.
const AgentHostSocketDest = "/run/tf.sock"

// Spec is one run's sandbox plan: the paths that exist because THIS run
// exists. Everything else about the mount plan is a property of the host and
// arrives through Host.
//
// A nil *Spec at the spawn seam means "no sandbox" and is byte-identical to
// the behavior before this package existed.
type Spec struct {
	// RunRoot is the run's own ephemeral tree
	// (<tmpdir>/triagefactory-runs/<rootKey>), bound rw at its real path.
	// It is the only thing that survives the temp-dir mask, which is what
	// hides every concurrent run's worktree from this one. Required.
	RunRoot string

	// GHChannelDir is this conversation's private gh config + trust dir
	// (<StateRoot>/gh-channel/<conversationID>), bound rw. Empty when the run
	// has no real-gh channel, which degrades it to the scoped exec verbs
	// exactly as an unprovisioned channel does outside the sandbox.
	GHChannelDir string

	// AgentHostSocket is the host path of this run's agenthost socket, bound
	// at AgentHostSocketDest. Empty leaves the run with no exec-verb daemon,
	// which the jailed CLI reports as a launch wiring bug rather than
	// silently falling back to a host DB it cannot reach.
	AgentHostSocket string

	// AddDirs are the run's --add-dir grants, bound rw at their real paths.
	// The spawn seam folds RunOptions.AddDirs in on top of whatever the
	// caller set, so the granted list and the bound list cannot drift apart.
	// rw because that is what the grant already means (Claude Code's
	// path-scoped Write/Edit checks consume these), and they are applied
	// after every mask so a grant under $HOME or the temp dir wins over the
	// tmpfs that would otherwise hide it. A grant that no longer exists at
	// spawn fails the run with the path named — never a silent skip, since
	// the agent would then read an empty directory as an answer.
	AddDirs []string

	// ReadOnly are host paths that must survive the masks, bound back ro at
	// their real paths. This is the escape valve for the host binaries a run
	// resolves rather than configures: the node interpreter, this TF binary,
	// the Agent SDK install. Any of them may sit under $HOME (an nvm install,
	// a dev build), and the mount plan cannot know that in advance — the
	// spawn seam computes them from the already-resolved absolute paths.
	// Paths that were never masked are still emitted; the whole host is
	// already ro, so re-binding one is a no-op that keeps the plan
	// independent of where TF_TOOLCHAIN_ROOT happens to point.
	ReadOnly []string
}

// Host is the spawn-time host context the mount plan is computed against —
// the same plan against a different home or state root is a different argv,
// so these are parameters rather than lookups inside the builder. That keeps
// Args pure and every cell of the plan testable without relocating a real
// home directory.
type Host struct {
	// Home is the operator's real home directory, masked with a tmpfs.
	// HOME itself is NOT relocated: the agent still runs with the operator's
	// real value, so subscription OAuth keeps working and the transcript
	// bookkeeping that keys off it stays correct. Required.
	Home string

	// ClaudeDir is ~/.claude, bound back rw through the home mask. It holds
	// the Claude Code credential and this run's transcript, and both need to
	// outlive the namespace — a park-and-resume reads the transcript back
	// from the host. Documented hole: the agent can read the operator's
	// Claude config along with its own transcripts.
	ClaudeDir string

	// StateRoot is <home>/.triagefactory, masked with a tmpfs so the SQLite
	// database, the bare clone cache, the curator projects and every other
	// conversation's gh-channel dir are simply absent.
	StateRoot string

	// HooksDir and GHBinDir are the two state-root children a run needs
	// back read-only: the TF-controlled git hooks core.hooksPath points at,
	// and the directory holding the pinned gh binary that leads PATH.
	HooksDir string
	GHBinDir string

	// TempDir is os.TempDir(), masked with a tmpfs. /tmp is masked
	// alongside it whenever TMPDIR points somewhere else, so a run cannot
	// read the machine's shared temp either way.
	TempDir string

	// Cwd is the agent's working directory, passed to --chdir. It is the
	// real host path (the run tree is bound at its own path, not relocated),
	// so the cwd Claude Code records — and therefore the transcript
	// directory it derives from it — is the same one the host sees.
	// Required.
	Cwd string
}
