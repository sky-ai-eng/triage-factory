// Snapshot/rehydrate git primitives for the durable blueprint workspace.
// CaptureWorkspaceGit distills one checkout down to the git state a rebuild
// cannot count on getting back from the remote (its commits past the branch it
// was made from + the uncommitted working tree); RestoreCheckout (restore.go)
// rebuilds a checkout from the persistent bare clone and layers that delta
// back on. The split keeps git knowledge in this
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
	"slices"
	"strings"
)

// GitDelta is the git state of one checkout at a dormancy point that a rebuild
// cannot count on getting back from the remote. What a branch on the remote
// holds — the history up to the branch the checkout was made from — is
// deliberately absent; RestoreCheckout reconstructs that from the bare and
// layers this delta on top. A pull request's own commits are carried even when
// pushed: a force-push leaves them on no branch. A nil *GitDelta means the path was not a
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
	// Bundle is a git bundle of the commits reachable from Head and not from
	// the refs the capture stops at (bundleStopArgs): the base branch in a
	// pull request checkout, so the pull request's commits and the run's own;
	// the checked-out ref in any other, so the run's commits alone. nil when
	// no commit is in that range, and Head is then one the stop refs reach.
	Bundle []byte
	// Patch is a single binary diff capturing every uncommitted change
	// (tracked modifications and untracked additions alike). nil when the
	// working tree is clean.
	Patch []byte
	// LFSObjects are the Git LFS objects the checkout's unpushed commits name
	// that the checkout holds (unpushedLFSObjects). Never encoded: the capture jail
	// names them in its manifest by id and streams their bytes, and a path
	// the jail wrote is not one this process reads.
	LFSObjects []LFSObject `json:"-"`
}

// CapturedState is one capture's result: a checkout's git delta. In multi
// mode the delta's large members are staged on disk by the orchestrator, from
// the capture jail's stream, and named by BundlePath and PatchPath; the
// delta's byte fields are the local-mode representation, where capture is
// in-process and has no jail to cross.
type CapturedState struct {
	Delta      *GitDelta
	BundlePath string
	PatchPath  string
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
// wtPath: the current branch, a bundle of the commits the checkout holds past
// the refs it stops at, and a single patch covering every uncommitted change.
// Returns (nil, nil) when wtPath is not a git worktree.
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

	bundle, err := bundleLocalCommits(ctx, wtPath, d.Head, d.Branch)
	if err != nil {
		return nil, err
	}
	d.Bundle = bundle
	// A bundle is the cheap sign the checkout holds commits at all past its
	// stop refs; the LFS walk decides on its own range which are unpushed.
	if bundle != nil {
		if d.LFSObjects, err = unpushedLFSObjects(ctx, wtPath, localRev(d.Branch)); err != nil {
			return nil, err
		}
	}

	patch, err := captureUncommitted(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	d.Patch = patch

	return d, nil
}

// CaptureWorkspaceGitToWriters is the multi-mode capture body. It writes the
// two unbounded git members directly to caller-prepared files (handed in as
// writers across exec) and returns the branch/HEAD metadata, with the Git LFS
// objects the checkout's unpushed commits name for the caller to stream.
func CaptureWorkspaceGitToWriters(ctx context.Context, wtPath string, bundle, patch io.Writer) (_ *GitDelta, hasBundle, hasPatch bool, err error) {
	if !IsGitWorktree(wtPath) {
		return nil, false, false, nil
	}
	d, err := captureWorkspaceGitMetadata(ctx, wtPath)
	if err != nil {
		return nil, false, false, err
	}
	hasBundle, err = bundleLocalCommitsTo(ctx, wtPath, d.Head, d.Branch, bundle)
	if err != nil {
		return nil, false, false, err
	}
	if hasBundle {
		if d.LFSObjects, err = unpushedLFSObjects(ctx, wtPath, localRev(d.Branch)); err != nil {
			return nil, false, false, err
		}
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

// bundleLocalCommits returns a bundle of the commits reachable from head and
// not from the refs the capture stops at (bundleStopArgs), or (nil, nil) when
// there are none: the stop refs then reach head, so the bare reproduces the
// committed state unaided.
func bundleLocalCommits(ctx context.Context, wtPath, head, branch string) ([]byte, error) {
	var out bytes.Buffer
	ok, err := bundleLocalCommitsTo(ctx, wtPath, head, branch, &out)
	if err != nil || !ok {
		return nil, err
	}
	return out.Bytes(), nil
}

// bundleLocalCommitsTo is bundleLocalCommits writing to w, reporting whether
// it wrote a bundle. head is the checkout's HEAD commit and branch its branch,
// "" when detached.
//
// The bundle is written here rather than by `git bundle create`, whose pack is
// always thin: it stores a changed file as a delta against a version outside
// the pack and reads that version while it packs. A checkout is a blobless
// partial clone that usually lacks it, the capture has no route to fetch it,
// and neither has the import on the other side. The pack written here is
// self-contained: every object of the range the checkout holds, each whole or
// as a delta against another object in the pack.
//
// --missing=allow-promisor leaves out an object the checkout lacks only when a
// promisor pack says the remote has it, which is a blob the remote sent a tree
// for and the checkout never read. The restore's bare gets such a blob from
// the remote when something reads it. A missing object no promisor pack
// accounts for is one the run made and lost, and still fails the capture;
// allow-any would write a bundle past it.
//
// Nothing is written before the range is known to hold a commit: on a
// streaming w, bytes written are already a capture member.
func bundleLocalCommitsTo(ctx context.Context, wtPath, head, branch string, w io.Writer) (bool, error) {
	stop, err := bundleStopArgs(ctx, wtPath)
	if err != nil {
		return false, err
	}
	// The trailing "--" ends the revisions, so nothing after the stop refs is
	// read as a path.
	out, err := gitCapture(ctx, wtPath, nil, slices.Concat([]string{"rev-list", "--boundary", head, "--not"}, stop, []string{"--"})...)
	if err != nil {
		return false, fmt.Errorf("git rev-list: %w", err)
	}
	// --boundary prints each commit the walk stopped at with a leading "-":
	// the bundle's prerequisites, the commits an importer must already hold.
	var prerequisites []string
	local := false
	for _, line := range strings.Fields(string(out)) {
		if oid, ok := strings.CutPrefix(line, "-"); ok {
			prerequisites = append(prerequisites, oid)
		} else {
			local = true
		}
	}
	if !local {
		return false, nil
	}
	// Sorted, and with no subject text after the id, so the header is a
	// function of the commits alone: it is what a checkpoint fingerprints.
	slices.Sort(prerequisites)

	var header, revs strings.Builder
	header.WriteString("# v2 git bundle\n")
	revs.WriteString(head + "\n")
	for _, oid := range prerequisites {
		header.WriteString("-" + oid + "\n")
		revs.WriteString("^" + oid + "\n")
	}
	ref := "HEAD"
	if branch != "" {
		ref = "refs/heads/" + branch
	}
	header.WriteString(head + " " + ref + "\n\n")
	if _, err := io.WriteString(w, header.String()); err != nil {
		return false, fmt.Errorf("write bundle header: %w", err)
	}
	if err := gitCaptureIO(ctx, wtPath, nil, strings.NewReader(revs.String()), w,
		"pack-objects", "--stdout", "--revs", "--delta-base-offset", "--missing=allow-promisor"); err != nil {
		return false, fmt.Errorf("git pack-objects: %w", err)
	}
	return true, nil
}

// prHeadRefExcludes are the remote-tracking refs that record a pull request's
// own head, as rev-list --exclude patterns (relative to refs/remotes/, with *
// matching across slashes): the run's branch mirror
// refs/remotes/origin/triagefactory/<key>/pr-<N> (prLocalBranch) and the push
// remote's refs refs/remotes/tfpush-<key>-<N>/* (prPushRemoteName).
var prHeadRefExcludes = []string{"origin/triagefactory/*", "tfpush-*/*"}

// recordsPRHead reports whether the remote-tracking ref named name, relative
// to refs/remotes/, matches one of prHeadRefExcludes.
func recordsPRHead(name string) bool {
	if strings.HasPrefix(name, "origin/triagefactory/") {
		return true
	}
	rest, ok := strings.CutPrefix(name, "tfpush-")
	return ok && strings.Contains(rest, "/")
}

// bundleStopArgs is the revisions a checkout's bundle stops at, as the
// arguments that follow --not: every remote-tracking ref except the ones
// recording a pull request's own head. In a pull request checkout that leaves
// the base branch, so the bundle carries the pull request's commits as well as
// the run's, and a pull request force-pushed while the run is parked restores
// from the bundle rather than from a commit the remote may no longer serve.
// A checkout of a ref holds only that ref's remote-tracking ref, which the
// exclusion does not touch.
//
// When the exclusion would leave no ref at all (a pull request checkout whose
// base ref never landed) the bundle stops at every remote-tracking ref, the
// pull request's head among them: an empty stop set would bundle the whole
// history.
func bundleStopArgs(ctx context.Context, wtPath string) ([]string, error) {
	out, err := gitCapture(ctx, wtPath, nil, "for-each-ref", "--format=%(refname)", "refs/remotes/")
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %w", err)
	}
	for _, ref := range strings.Fields(string(out)) {
		if !recordsPRHead(strings.TrimPrefix(ref, "refs/remotes/")) {
			args := make([]string, 0, len(prHeadRefExcludes)+1)
			for _, pattern := range prHeadRefExcludes {
				args = append(args, "--exclude="+pattern)
			}
			return append(args, "--remotes"), nil
		}
	}
	return []string{"--remotes"}, nil
}

// localRev names the tip of a checkout's local work: its branch, or HEAD when
// it is detached.
func localRev(branch string) string {
	if branch == "" {
		return "HEAD"
	}
	return branch
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
	if scoped {
		changed = dropFetchedLFSPaths(ctx, wtPath, changed)
	}
	if scoped && len(changed) == 0 {
		// The real index's stat cache says the tree matches HEAD exactly, or
		// differs only in LFS files the run fetched — nothing uncommitted to
		// carry, and no temp index to build.
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
		// The full stage cannot leave fetched LFS files out the way the
		// scoped one does, so it takes them back out of the index after.
		unstageFetchedLFSFiles(ctx, wtPath, env)
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

// gitCapture runs git and returns its stdout (only) so a `git diff` or
// `git pack-objects` payload isn't corrupted by progress text on stderr — unlike
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
	return gitCaptureIO(ctx, dir, env, nil, stdout, args...)
}

// captureLFSFilterOff turns git-lfs's filter driver off for every capture git
// runs, whatever the host's or the checkout's config says, so the capture
// reads LFS files the same way in both modes. A file git-lfs would clean into
// a pointer is then staged as its content, which is what lets an uncommitted
// LFS file travel in the patch. The command line outranks every config file,
// and a driver whose commands are empty runs nothing.
var captureLFSFilterOff = []string{
	"-c", "filter.lfs.process=",
	"-c", "filter.lfs.clean=",
	"-c", "filter.lfs.smudge=",
	"-c", "filter.lfs.required=false",
}

// gitCaptureIO is gitCaptureTo with stdin, nil for none.
func gitCaptureIO(ctx context.Context, dir string, env []string, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(captureLFSFilterOff), args...)...)
	cmd.Stdin = stdin
	if dir != "" {
		cmd.Dir = dir
	}
	// Capture paths are local-only (rev-parse, pack-objects, diff against the
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
