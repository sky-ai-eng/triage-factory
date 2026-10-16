// Snapshot/rehydrate git primitives for the durable blueprint workspace.
// CaptureWorkspaceGit distills one checkout down to its non-recoverable git
// state (the agent's local-only commits + the uncommitted working tree);
// RestoreCheckout (restore.go) rebuilds a checkout from the persistent bare
// clone and layers that delta back on. The split keeps git knowledge in this
// package while the storage/tar orchestration lives in internal/delegate.

package worktree

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitDelta is the non-recoverable git state of one checkout at a dormancy
// point. Everything recoverable — the bare clone, anything already pushed to a
// remote — is deliberately absent; RestoreCheckout reconstructs those from the
// bare and layers this delta on top. A nil *GitDelta means the path was not a
// git worktree (a run root, which never is), so there is no git state to carry.
//
// SECURITY INVARIANT — the agent's .git/config is NEVER part of this delta,
// and must never be added. Restore rebuilds the worktree from the host-owned
// bare, so the config in effect during the restore-side `git apply` is the
// bare's clean config, not anything the agent authored. That is the only
// reason restore's apply is safe against a hostile checkout: a tracked
// .gitattributes can select a filter driver, but only config defines the
// command that driver runs, and restore's config is ours. Carrying the
// agent's config across a snapshot (or applying it into the rebuilt worktree)
// would arm the restore-side apply the same way an agent-authored config arms
// the capture path — keep config out of the delta.
type GitDelta struct {
	// Branch is the worktree's checked-out branch. Empty when HEAD is
	// detached — restore then checks out Head directly (detached).
	Branch string
	// Head is the HEAD commit SHA, always captured. It's the authoritative
	// tip restore positions the worktree at, and it's what lets restore
	// recreate the branch even when the bundle is empty and the branch was
	// never pushed to a remote (the never-pushed-local-branch case), or check
	// out a detached HEAD (no branch at all).
	Head string
	// Bundle is a `git bundle` of the commits reachable from Head but not
	// from any remote-tracking ref — i.e. only the agent's local-only work.
	// nil when the tip is already on a remote (nothing local to carry); the
	// bare reproduces the committed state by itself in that case, and Head is
	// reachable there.
	Bundle []byte
	// Patch is a single binary diff capturing every uncommitted change
	// (tracked modifications and untracked additions alike). nil when the
	// working tree is clean.
	Patch []byte
}

// CapturedState is one capture's result: a checkout's git delta, or the
// session transcript the local, in-process capture reads. In multi mode the
// delta's large members are staged on disk by the orchestrator, from the
// capture jail's stream, and named by BundlePath and PatchPath; the byte
// fields are the local-mode representation, where capture is in-process and
// has no jail to cross.
type CapturedState struct {
	Delta      *GitDelta
	SessionID  string
	BundlePath string
	PatchPath  string
	Transcript []byte
}

// ReadClaudeSessionTranscript reads the Claude session transcript for sessionID
// under wtPath's project encoding, returning (bytes, true) on success and
// (nil, false) when there is no session id or the file is absent/unreadable.
// The transcript lives under the operator's own ~/.claude and the reader shares
// the agent's uid, so there is no cross-uid boundary. Only the local SDK
// runtime has a session transcript to carry; a native conversation's lives in
// the database.
func ReadClaudeSessionTranscript(wtPath, sessionID string) ([]byte, bool) {
	if sessionID == "" {
		return nil, false
	}
	p, err := ClaudeSessionPath(ResolveClaudeProjectCwd(wtPath), sessionID)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return data, true
}

// IsGitWorktree reports whether wtPath is the root of a git working tree (a
// `.git` file for a linked worktree, or a `.git` directory for a plain
// checkout). False for an empty path and for a run root, which is a plain
// folder — and a run root for which it is true was laid out by an older
// binary, with the PR checkout at the root itself.
func IsGitWorktree(wtPath string) bool {
	if wtPath == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(wtPath, ".git"))
	return err == nil
}

// CaptureWorkspaceGit captures the non-recoverable git delta of the checkout at
// wtPath: the current branch, a bounded bundle of the agent's local-only
// commits, and a single patch covering every uncommitted change. Returns (nil,
// nil) when wtPath is not a git worktree.
func CaptureWorkspaceGit(ctx context.Context, wtPath string) (*GitDelta, error) {
	if !IsGitWorktree(wtPath) {
		return nil, nil
	}
	d := &GitDelta{}

	// HEAD commit SHA — always captured. Restore positions the worktree here,
	// and it's what lets restore recreate a never-pushed branch or check out a
	// detached HEAD when the bundle is empty. A failure here is a real anomaly
	// (a delegated worktree always has commits), so surface it rather than
	// snapshotting a workspace we can't rebuild.
	head, err := gitCapture(ctx, wtPath, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	d.Head = strings.TrimSpace(string(head))

	// rev-parse --abbrev-ref reports "HEAD" for a detached head; treat that as
	// "no branch" so restore checks out d.Head detached instead of trying to
	// check out a branch literally named HEAD.
	if out, err := gitCapture(ctx, wtPath, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(string(out)); b != "HEAD" {
			d.Branch = b
		}
	}

	bundle, err := bundleLocalCommits(ctx, wtPath, d.Branch)
	if err != nil {
		return nil, err
	}
	d.Bundle = bundle

	patch, err := captureUncommitted(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	d.Patch = patch

	return d, nil
}

// CaptureWorkspaceGitToWriters is the multi-mode capture body. It writes the
// two unbounded git members directly to caller-prepared files (handed in as
// writers across exec) and returns only the branch/HEAD metadata.
func CaptureWorkspaceGitToWriters(ctx context.Context, wtPath string, bundle, patch io.Writer) (_ *GitDelta, hasBundle, hasPatch bool, err error) {
	if !IsGitWorktree(wtPath) {
		return nil, false, false, nil
	}
	d, err := captureWorkspaceGitMetadata(ctx, wtPath)
	if err != nil {
		return nil, false, false, err
	}
	hasBundle, err = bundleLocalCommitsTo(ctx, wtPath, d.Branch, bundle)
	if err != nil {
		return nil, false, false, err
	}
	hasPatch, err = captureUncommittedTo(ctx, wtPath, patch)
	if err != nil {
		return nil, false, false, err
	}
	return d, hasBundle, hasPatch, nil
}

func captureWorkspaceGitMetadata(ctx context.Context, wtPath string) (*GitDelta, error) {
	d := &GitDelta{}
	head, err := gitCapture(ctx, wtPath, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	d.Head = strings.TrimSpace(string(head))
	if out, err := gitCapture(ctx, wtPath, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(string(out)); b != "HEAD" {
			d.Branch = b
		}
	}
	return d, nil
}

// bundleLocalCommits writes a `git bundle` of <rev> --not --remotes — the
// commits the agent made that aren't on any remote-tracking ref yet — and
// returns its bytes. Returns (nil, nil) when git refuses to create an empty
// bundle: the tip is already published, so there are no local-only commits to
// carry and the bare reproduces the committed state unaided.
func bundleLocalCommits(ctx context.Context, wtPath, branch string) ([]byte, error) {
	var out bytes.Buffer
	ok, err := bundleLocalCommitsTo(ctx, wtPath, branch, &out)
	if err != nil || !ok {
		return nil, err
	}
	return out.Bytes(), nil
}

func bundleLocalCommitsTo(ctx context.Context, wtPath, branch string, w io.Writer) (bool, error) {
	rev := branch
	if rev == "" {
		rev = "HEAD"
	}
	if err := gitCaptureTo(ctx, wtPath, nil, w, "bundle", "create", "-", rev, "--not", "--remotes"); err != nil {
		if strings.Contains(err.Error(), "empty bundle") {
			return false, nil
		}
		return false, fmt.Errorf("git bundle: %w", err)
	}
	return true, nil
}

// BundleHeader reads a git bundle's header from r: the signature line, any
// capability lines, the prerequisite commits and the refs, through the blank
// line where the pack begins. It is the bundle's identity. The refs name the
// commits it carries and the prerequisites the commits it stops at, and object
// ids are hashes of content, so two bundles with one header carry the same
// objects. Their bytes need not match: git writes the pack on several threads
// and chooses its deltas differently from one run to the next.
func BundleHeader(r io.Reader) ([]byte, error) {
	return readBundleHeader(bufio.NewReader(r))
}

// readBundleHeader is BundleHeader over a reader the caller keeps: on success
// br is positioned at the first byte of the bundle's pack.
func readBundleHeader(br *bufio.Reader) ([]byte, error) {
	var header bytes.Buffer
	for first := true; ; first = false {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read bundle header: %w", err)
		}
		if first && !bytes.HasPrefix(line, []byte("# v2 git bundle")) && !bytes.HasPrefix(line, []byte("# v3 git bundle")) {
			return nil, fmt.Errorf("not a git bundle: signature %q", bytes.TrimSpace(line))
		}
		header.Write(line)
		if len(line) == 1 {
			return header.Bytes(), nil
		}
	}
}

// changeScopedPathspecLimit caps how many changed paths the scoped stage in
// captureUncommitted will name before falling back to the full `add -A`.
// Pathspec matching is a per-index-entry scan over the pathspec set, so a
// change set in the thousands against a large index turns the scoped stage
// quadratic — and a tree where thousands of paths changed is one where the
// full re-hash was never the wasteful part. A var, not a const, so the
// fallback arm is testable without minting a thousand files.
var changeScopedPathspecLimit = 1000

// changedPathsVsHEAD is the read-only pre-pass that makes captureUncommitted
// change-scoped: `git status` against the worktree's REAL index, whose stat
// cache answers "what differs?" from lstat alone — the throwaway index below
// has no stat cache, so staging through it re-reads and re-hashes every file
// in the tree, a fixed O(tree) cost per park even when nothing changed.
//
// Returns the union of paths from both status columns (index-vs-HEAD and
// worktree-vs-index, both sides of a rename record, untracked entries). That
// union is a superset of every path where the worktree differs from HEAD:
// worktree≠HEAD implies worktree≠index or index≠HEAD, so a path in neither
// column cannot contribute to the patch. Staging only these paths into the
// HEAD-seeded temp index therefore yields the same diff the full stage
// would.
//
// ok=false means the pre-pass is unusable — status failed, produced a record
// this parser does not recognize, or the change set is past
// changeScopedPathspecLimit — and the caller takes the full `add -A` path,
// which remains the semantics of record. ok=true with no paths means the
// tree matches HEAD exactly: nothing uncommitted to capture.
//
// Two flags keep the read honest. --no-optional-locks stops status from
// opportunistically refreshing the real index, so the agent's staging (which
// a warm resume reads back) is untouched even at the stat-cache level. An
// explicit --untracked-files=normal overrides any status.showUntrackedFiles
// config in the checkout — the repo's (or agent's) display preference must
// not hide untracked files from the capture.
func changedPathsVsHEAD(ctx context.Context, wtPath string) (paths []string, ok bool) {
	out, err := gitCapture(ctx, wtPath, nil, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, false
	}
	tokens := strings.Split(string(out), "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "" {
			continue // the trailing terminator
		}
		if len(tok) < 4 || tok[2] != ' ' {
			// Not a record this parser recognizes — refuse to guess what a
			// future porcelain emits and take the full path instead.
			return nil, false
		}
		xy := tok[:2]
		// A collapsed untracked-directory record ends in "/". The pathspec is
		// written under :(literal) magic, which keeps that slash literally and
		// then matches no path at all (nothing ends in a slash) — strip it, so
		// the pathspec names the directory and directory-prefix matching
		// covers its contents.
		paths = append(paths, strings.TrimSuffix(tok[3:], "/"))
		// A rename/copy record carries its second pathname as the following
		// token. Both sides belong in the set (one holds the content, the
		// other the deletion), which also makes the parse indifferent to
		// which of the two git printed first.
		if strings.ContainsAny(xy, "RC") && i+1 < len(tokens) {
			i++
			if other := tokens[i]; other != "" {
				paths = append(paths, strings.TrimSuffix(other, "/"))
			}
		}
		if len(paths) > changeScopedPathspecLimit {
			return nil, false
		}
	}
	return paths, true
}

// stageChangedPaths stages the pre-pass's changed paths into the temp index.
// The pathspecs travel by file, NUL-separated — never argv, so the set can't
// hit a command-length limit and a path is arbitrary bytes. Each changed path
// is written under `:(literal)` magic, because these are filenames, not
// patterns: a name containing a glob metacharacter must match itself, and
// must not be re-interpreted into a wildcard that could explicitly name an
// ignored file (which `git add` refuses outright, where recursing past one is
// silent).
func stageChangedPaths(ctx context.Context, wtPath string, env []string, paths []string) error {
	f, err := os.CreateTemp("", "tf-pathspec-*")
	if err != nil {
		return fmt.Errorf("pathspec tempfile: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	for _, p := range paths {
		if _, err := f.WriteString(":(literal)" + p + "\x00"); err != nil {
			_ = f.Close()
			return fmt.Errorf("write pathspec: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("flush pathspec: %w", err)
	}
	_, err = gitCapture(ctx, wtPath, env, "add", "-A", "--pathspec-from-file="+name, "--pathspec-file-nul")
	return err
}

// captureUncommitted produces one binary patch of every uncommitted change in
// wtPath via a throwaway index seeded from HEAD: stage everything (add -A
// records modifications, deletions, and untracked additions alike), then diff
// that index against HEAD. Using
// GIT_INDEX_FILE keeps the worktree's real index untouched so a warm-path
// resume still sees the agent's staging exactly as it left it. Returns
// (nil, nil) for a clean tree.
//
// The stage is change-scoped when changedPathsVsHEAD can say what changed: a
// clean tree skips the temp index entirely (the common idle-park case), and a
// dirty one stages only the changed paths, so the capture's cost follows the
// size of the delta rather than the size of the repository. The full `add -A`
// survives as the fallback and the semantics of record — the scoped stage
// must produce a byte-identical patch or not run at all.
//
// Every diff below passes --no-ext-diff --no-textconv. `git diff` is porcelain,
// so by default it execs whatever external diff program (`diff.external`, or a
// `.gitattributes`-mapped `diff.<driver>.command`) and whatever
// `diff.<driver>.textconv` the repo names — and in a checkout the config and the
// attributes are both the agent's to write. The flags are the guarantee, because
// they outrank config; no value of `diff.external` is one, git having none that
// means "disabled". They are equally a correctness requirement: textconv output
// is explicitly not appliable, so a textconv-mapped path would otherwise
// snapshot a patch that lies at restore time. A name-only diff invokes no driver
// either way, but carries them so no diff here reads as license for a flag-less
// one.
func captureUncommitted(ctx context.Context, wtPath string) ([]byte, error) {
	var out bytes.Buffer
	ok, err := captureUncommittedTo(ctx, wtPath, &out)
	if err != nil || !ok {
		return nil, err
	}
	return out.Bytes(), nil
}

func captureUncommittedTo(ctx context.Context, wtPath string, w io.Writer) (bool, error) {
	changed, scoped := changedPathsVsHEAD(ctx, wtPath)
	if scoped && len(changed) == 0 {
		// The real index's stat cache says the tree matches HEAD exactly —
		// nothing uncommitted to carry, and no temp index to build.
		return false, nil
	}

	idx, err := os.CreateTemp("", "tf-index-*")
	if err != nil {
		return false, fmt.Errorf("temp index: %w", err)
	}
	idxName := idx.Name()
	_ = idx.Close()
	defer func() { _ = os.Remove(idxName) }()

	// The objects the stage writes go to a throwaway directory too, with the
	// checkout's own object store behind it as an alternate. Staging a change
	// hashes and stores every changed file, and the capture runs where the
	// checkout is mounted read-only: it must not need to write the thing it
	// reads, and a capture that left loose objects in the repository on every
	// run would grow it by the size of the uncommitted work each time.
	objs, err := os.MkdirTemp("", "tf-objs-*")
	if err != nil {
		return false, fmt.Errorf("temp object dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(objs) }()
	shared, err := gitCapture(ctx, wtPath, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return false, fmt.Errorf("locate object store: %w", err)
	}

	// Seed from gitBaseEnv() (not os.Environ()) so this throwaway-index env still
	// carries GIT_TERMINAL_PROMPT=0, keeping every capture on the package's one
	// base environment.
	env := append(gitBaseEnv(),
		"GIT_INDEX_FILE="+idxName,
		"GIT_OBJECT_DIRECTORY="+objs,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+strings.TrimSpace(string(shared)),
	)
	if _, err := gitCapture(ctx, wtPath, env, "read-tree", "HEAD"); err != nil {
		return false, fmt.Errorf("seed temp index: %w", err)
	}
	if scoped {
		// A scoped stage can fail legitimately: a pathspec can match nothing
		// in the temp-index world — a file staged in the REAL index and then
		// deleted from the worktree exists in neither HEAD nor the tree — and
		// `git add` treats an unmatched pathspec as fatal. Any failure here
		// demotes to the full stage rather than failing the capture, after
		// re-seeding the index: the failed add may have staged part of the
		// set before it stopped.
		if sErr := stageChangedPaths(ctx, wtPath, env, changed); sErr != nil {
			scoped = false
			if _, err := gitCapture(ctx, wtPath, env, "read-tree", "HEAD"); err != nil {
				return false, fmt.Errorf("re-seed temp index: %w", err)
			}
		}
	}
	if !scoped {
		if _, err := gitCapture(ctx, wtPath, env, "add", "-A"); err != nil {
			return false, fmt.Errorf("stage worktree: %w", err)
		}
	}
	cw := &captureCountingWriter{w: w}
	if err := gitCaptureTo(ctx, wtPath, env, cw, "diff", "--no-ext-diff", "--no-textconv", "--cached", "--binary", "HEAD"); err != nil {
		return false, fmt.Errorf("diff temp index: %w", err)
	}
	return cw.n > 0, nil
}

// applyPatch replays an uncommitted-changes patch file onto the freshly rebuilt
// checkout. --whitespace=nowarn keeps trailing-whitespace edits the agent made
// from tripping apply; --binary round-trips binary hunks. The changes land
// unstaged (file content is what matters for resume; the agent re-stages as it
// continues).
func applyPatch(ctx context.Context, wtDir, patchPath string) error {
	return gitRunCtx(ctx, wtDir, "apply", "--whitespace=nowarn", "--binary", patchPath)
}

// ClaudeSessionPath returns the absolute path of the Claude Code session
// transcript (`<sessionID>.jsonl`) for an agent whose symlink-resolved cwd is
// resolvedCwd. Snapshot reads the transcript from it; rehydrate writes the
// carried transcript to the equivalent path under the new cwd so
// `claude --resume <sessionID>` reconnects after landing on a different
// worktree path. resolvedCwd must already be EvalSymlinks'd (see
// ResolveClaudeProjectCwd) because Claude Code keys the dir name off the
// resolved cwd.
func ClaudeSessionPath(resolvedCwd, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("claude session path: empty session id")
	}
	dir, err := ClaudeProjectDir(resolvedCwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID+".jsonl"), nil
}

// gitCapture runs git and returns its stdout (only) so a `git diff` /
// `git bundle` payload isn't corrupted by progress text on stderr — unlike
// gitOutputCtx, which combines the two. env, when non-nil, replaces the
// child's environment (used to point GIT_INDEX_FILE at a throwaway index).
//
// captureUncommitted's `git status` / `git add -A` / `git diff` go through
// here. Unlike the push-gate's metadata reads (a byte read of HEAD, and config
// read from OUTSIDE any repository — neither of which lets an agent-writable
// config run anything), status/add/diff DO consult the repository's own
// (agent-writable, once chowned to the sandbox uid) `.gitattributes` +
// `.git/config` to decide whether to invoke an external clean/smudge filter
// or diff driver (status runs clean filters when a racy-stat file forces a
// content compare).
//
// In multi mode these run inside the capture jail (sandbox.LaunchCaptureJail):
// a one-shot gVisor sandbox with no network, the checkout mounted read-only,
// and nothing of any run but the files it reads. So a clean/smudge/diff filter
// a hostile `.gitattributes` + `.git/config` pair could trigger executes there,
// where it can reach neither the host nor another run. This path must still NOT
// be made ownership-tolerant against a chowned checkout in-process: doing so
// would move filter execution back to whatever privilege the in-process caller
// holds (a local-mode operator, or a dev running the binary directly). Keep it
// strict; the isolation is the jail, not any check here.
func gitCapture(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	if err := gitCaptureTo(ctx, dir, env, &out, args...); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type captureCountingWriter struct {
	w io.Writer
	n int64
}

func (c *captureCountingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func gitCaptureTo(ctx context.Context, dir string, env []string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Capture paths are local-only (rev-parse, bundle, diff against the
	// bare/worktree), but still run with prompts disabled for consistency: a
	// supplied env already starts from gitBaseEnv() (see captureUncommitted), and
	// a nil env falls back to it here.
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = gitBaseEnv()
	}
	var errb bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return nil
}
