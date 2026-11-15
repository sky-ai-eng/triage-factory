package worktree

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// privateOrigin is an upstream that serves blobless clones and answers only
// with a credential, as a private GitHub repository does. Every fetch a test's
// git makes without the credential fails, so a capture or an import that
// succeeds without one made no fetch.
//
// Its history, every commit one step past its parent:
//
//	main:             canary.txt, then big.txt
//	refs/pull/7/head: main + pr.txt
//	refs/pull/8/head: main + two commits that each edit big.txt
//	topic:            main + topic.txt
type privateOrigin struct {
	path string // the upstream's bare repository on disk
	work string // a full clone the fixture commits in and pushes from
	url  string
	auth CloneAuth

	main, pr7, pr8First, pr8, topic string
}

// bigFile is a file long enough that git stores a one-line edit of it as a
// delta, which is what makes a `git bundle create` pack of that edit thin.
func bigFile(edit string) string {
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "line %03d of a file long enough to be stored as a delta\n", i)
	}
	return b.String() + edit + "\n"
}

func newPrivateOrigin(t *testing.T) *privateOrigin {
	t.Helper()
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // accept the httptest self-signed cert
	reposRoot := t.TempDir()
	const token = "ghs_bundle_token"
	path, _ := seedBloblessUpstream(t, reposRoot, "repo")
	o := &privateOrigin{path: path, work: filepath.Join(t.TempDir(), "work")}
	runGit(t, "clone", "-q", path, o.work)
	gitAt(t, o.work, "config", "user.email", "test@example.com")
	gitAt(t, o.work, "config", "user.name", "Test")

	o.main = o.commit(t, "origin/main", "big.txt", bigFile("base"))
	o.push(t, o.main, "refs/heads/main")
	o.pr7 = o.commit(t, o.main, "pr.txt", "pr change\n")
	o.push(t, o.pr7, "refs/pull/7/head")
	o.pr8First = o.commit(t, o.main, "big.txt", bigFile("first pull request edit"))
	o.pr8 = o.commit(t, o.pr8First, "big.txt", bigFile("second pull request edit"))
	o.push(t, o.pr8, "refs/pull/8/head")
	o.topic = o.commit(t, o.main, "topic.txt", "topic change\n")
	o.push(t, o.topic, "refs/heads/topic")

	o.url = startAuthedGitServer(t, reposRoot, "x-access-token", token) + "/repo.git"
	o.auth = CloneAuthFor(o.url, token)
	return o
}

// commit makes a commit on top of onto that writes content to file, and
// returns its id. Nothing is pushed.
func (o *privateOrigin) commit(t *testing.T, onto, file, content string) string {
	t.Helper()
	gitAt(t, o.work, "checkout", "-q", "--detach", onto)
	writeUnder(t, o.work, file, content)
	gitAt(t, o.work, "add", file)
	gitAt(t, o.work, "commit", "-q", "-m", "write "+file)
	return strings.TrimSpace(gitAt(t, o.work, "rev-parse", "HEAD"))
}

// push moves the upstream's ref to commit, as a force-push does.
func (o *privateOrigin) push(t *testing.T, commit, ref string) {
	t.Helper()
	gitAt(t, o.work, "push", "-q", "-f", o.path, commit+":"+ref)
}

// forget drops from the upstream every object no ref reaches, so it answers a
// fetch of commit by id as GitHub answers for a commit it has collected.
func (o *privateOrigin) forget(t *testing.T, commit string) {
	t.Helper()
	gitAt(t, o.path, "gc", "-q", "--prune=now")
	if exec.Command("git", "-C", o.path, "cat-file", "-e", commit).Run() == nil {
		t.Fatalf("fixture: the upstream still holds %s after it was pruned", commit)
	}
}

// pr is the pull request detail a restore of one of the fixture's pull request
// checkouts is handed.
func (o *privateOrigin) pr() *PRCheckout {
	return &PRCheckout{HeadRef: "feature", HeadCloneURL: o.url, BaseRef: "main"}
}

// prCheckout builds pull request n's checkout under root with its base branch
// known, as a pull request run's setup does.
func (o *privateOrigin) prCheckout(t *testing.T, root string, n int) string {
	t.Helper()
	wtDir, err := CreateForPRInRoot(context.Background(), testRepo("acme", "repo"), o.url, o.url, "feature", n, "conv-a", root,
		WithCloneAuth(o.auth), WithBaseBranch("main"))
	if err != nil {
		t.Fatalf("CreateForPRInRoot: %v", err)
	}
	return wtDir
}

// refCheckout builds a checkout of the upstream's branch ref under root.
func (o *privateOrigin) refCheckout(t *testing.T, root, ref string) string {
	t.Helper()
	wtDir, err := CreateForCheckoutInRoot(context.Background(), testRepo("acme", "repo"), o.url, ref, "conv-a", root, WithCloneAuth(o.auth))
	if err != nil {
		t.Fatalf("CreateForCheckoutInRoot: %v", err)
	}
	return wtDir
}

// gitAuthed runs git in dir with the origin's credential, as a run's git does
// through its proxy.
func (o *privateOrigin) gitAuthed(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var withAuth []string
	for _, kv := range o.auth.GitConfigEntries() {
		withAuth = append(withAuth, "-c", kv[0]+"="+kv[1])
	}
	return gitAt(t, dir, append(withAuth, args...)...)
}

// commitIn commits content to file in the checkout at wtDir, as the agent
// would, and returns the commit's id.
func commitIn(t *testing.T, wtDir, file, content string) string {
	t.Helper()
	gitAt(t, wtDir, "config", "user.email", "agent@example.com")
	gitAt(t, wtDir, "config", "user.name", "Agent")
	writeUnder(t, wtDir, file, content)
	gitAt(t, wtDir, "add", file)
	gitAt(t, wtDir, "commit", "-q", "-m", "agent writes "+file)
	return strings.TrimSpace(gitAt(t, wtDir, "rev-parse", "HEAD"))
}

// captureJailed captures the checkout under the capture jail's environment,
// which holds no credential and refuses a lazy fetch.
func captureJailed(t *testing.T, wtDir string) *GitDelta {
	t.Helper()
	var delta *GitDelta
	var err error
	withCaptureJailEnv(t, func() { delta, err = CaptureWorkspaceGit(context.Background(), wtDir) })
	if err != nil {
		t.Fatalf("CaptureWorkspaceGit: %v", err)
	}
	return delta
}

// loseHost removes the run root and the shared bare, leaving what a fresh
// executor starts a restore with, and returns a new run root.
func loseHost(t *testing.T, key string) string {
	t.Helper()
	RemoveRunRoot(key)
	bare, err := repoDir(testRepo("acme", "repo").ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(bare); err != nil {
		t.Fatalf("remove bare: %v", err)
	}
	return mustRunRoot(t, key)
}

// bundleView is a bundle read back: the commits it stops at, its ref lines,
// and the type of every object in its pack.
type bundleView struct {
	prerequisites []string
	tips          []string
	objects       map[string]string
}

// viewBundle parses bundle. Its pack is indexed on its own, outside any
// repository, which git refuses for a thin pack: a view is also the check that
// the pack is self-contained.
func viewBundle(t *testing.T, bundle []byte) bundleView {
	t.Helper()
	v, err := readBundle(t, bundle)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return v
}

func readBundle(t *testing.T, bundle []byte) (bundleView, error) {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader(bundle))
	header, err := readBundleHeader(br)
	if err != nil {
		return bundleView{}, err
	}
	v := bundleView{objects: map[string]string{}}
	if v.prerequisites, err = bundlePrerequisites(header); err != nil {
		return bundleView{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(header)), "\n") {
		if !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "-") {
			v.tips = append(v.tips, line)
		}
	}
	dir := t.TempDir()
	pack := filepath.Join(dir, "bundle.pack")
	f, err := os.Create(pack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := br.WriteTo(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "index-pack", pack).CombinedOutput(); err != nil {
		return bundleView{}, fmt.Errorf("index-pack: %w: %s", err, out)
	}
	out, err := exec.Command("git", "verify-pack", "-v", filepath.Join(dir, "bundle.idx")).Output()
	if err != nil {
		t.Fatalf("verify-pack: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && commitSHAPattern.MatchString(f[0]) {
			v.objects[f[0]] = f[1]
		}
	}
	return v, nil
}

// blobAt is the id of the blob at path in commit, read from the fixture's
// full clone.
func (o *privateOrigin) blobAt(t *testing.T, commit, path string) string {
	t.Helper()
	return strings.TrimSpace(gitAt(t, o.work, "rev-parse", commit+":"+path))
}

// TestRestoreCheckout_EditedFileRoundTripsThroughFreshBares: a run commits an
// edit to a file the repository already had, and its workspace is rebuilt from
// the snapshot on a fresh executor, twice. The bundle carries the edited file
// whole, so the import asks the origin for nothing (it is run here without
// the credential), and so does the capture of the rebuilt checkout, which
// never fetched the file's earlier version.
func TestRestoreCheckout_EditedFileRoundTripsThroughFreshBares(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "edited-file-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir := o.prCheckout(t, root, 7)
			edited := bigFile("the agent's edit")
			head := commitIn(t, wtDir, "big.txt", edited)
			delta := captureJailed(t, wtDir)
			if delta.Head != head || len(delta.Bundle) == 0 {
				t.Fatalf("capture = head %s with %d bundle bytes, want %s bundled", delta.Head, len(delta.Bundle), head)
			}

			root = loseHost(t, key)
			bare, err := EnsureBareClone(context.Background(), testRepo("acme", "repo"), o.url, WithCloneAuth(o.auth))
			if err != nil {
				t.Fatalf("EnsureBareClone: %v", err)
			}
			staged := filepath.Join(t.TempDir(), "bundle")
			if err := os.WriteFile(staged, delta.Bundle, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := importBundle(context.Background(), bare, staged, CloneAuth{}); err != nil {
				t.Fatalf("import with no credential: %v; the import fetched from the origin", err)
			}

			got := restoreDelta(t, root, key, "acme", "repo", PRRefSlug(7), o.url, o.auth, delta, o.pr())
			assertFileContent(t, filepath.Join(got, "big.txt"), edited)
			again := captureJailed(t, got)
			if again.Head != head || len(again.Bundle) == 0 {
				t.Fatalf("capture of the restored checkout = head %s with %d bundle bytes, want %s bundled", again.Head, len(again.Bundle), head)
			}

			root = loseHost(t, key)
			got = restoreDelta(t, root, key, "acme", "repo", PRRefSlug(7), o.url, o.auth, again, o.pr())
			assertFileContent(t, filepath.Join(got, "big.txt"), edited)
			if b := CurrentBranch(got); b != delta.Branch {
				t.Errorf("restored branch = %q, want %q", b, delta.Branch)
			}
		})
	}
}

// TestCaptureWorkspaceGit_PRBundleStopsAtTheBaseBranch: a pull request
// checkout the run has not committed in still writes a bundle. It stops at the
// merge base with the base branch and carries the pull request's commits, with
// the pull request's head as its tip.
func TestCaptureWorkspaceGit_PRBundleStopsAtTheBaseBranch(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "pr-bundle-range-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			delta := captureJailed(t, o.prCheckout(t, root, 7))
			if len(delta.Bundle) == 0 {
				t.Fatal("a pull request checkout with no local commit wrote no bundle")
			}
			v := viewBundle(t, delta.Bundle)
			if !slices.Equal(v.prerequisites, []string{o.main}) {
				t.Errorf("prerequisites = %v, want the merge base with the base branch %s", v.prerequisites, o.main)
			}
			if want := []string{o.pr7 + " refs/heads/" + prLocalBranch("conv-a", 7)}; !slices.Equal(v.tips, want) {
				t.Errorf("tips = %v, want %v", v.tips, want)
			}
			if v.objects[o.pr7] != "commit" {
				t.Errorf("the bundle does not hold the pull request's commit %s", o.pr7)
			}
		})
	}
}

// TestRestoreCheckout_ForcePushedPRRestoresFromTheBundle: a pull request is
// force-pushed while its run is parked, and the origin no longer has the head
// the snapshot captured. The bundle holds the pull request's commits back to
// the base branch, so the restore on a fresh executor needs no commit by id.
func TestRestoreCheckout_ForcePushedPRRestoresFromTheBundle(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "force-pushed-pr-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			delta := captureJailed(t, o.prCheckout(t, root, 7))
			o.push(t, o.commit(t, o.main, "pr.txt", "rewritten pr change\n"), "refs/pull/7/head")
			o.forget(t, o.pr7)

			root = loseHost(t, key)
			got := restoreDelta(t, root, key, "acme", "repo", PRRefSlug(7), o.url, o.auth, delta, o.pr())
			if head := strings.TrimSpace(gitAt(t, got, "rev-parse", "HEAD")); head != o.pr7 {
				t.Errorf("restored HEAD = %s, want the captured head %s", head, o.pr7)
			}
			assertFileContent(t, filepath.Join(got, "pr.txt"), "pr change\n")
		})
	}
}

// TestCaptureWorkspaceGit_MergedBaseCommitsStayOutOfTheBundle: a run fetches
// its pull request's base branch and merges it, as the conflict-resolution
// prompt asks. The fetch moves origin/<base> in a fresh checkout, so the bundle
// holds the merge commit and stops at the base branch's new tip.
func TestCaptureWorkspaceGit_MergedBaseCommitsStayOutOfTheBundle(t *testing.T) {
	setRestoreMode(t, true)
	o := newPrivateOrigin(t)
	const key = "merged-base-run"
	root := mustRunRoot(t, key)
	t.Cleanup(func() { RemoveRunRoot(key) })

	wtDir := o.prCheckout(t, root, 7)
	moved := o.commit(t, o.main, "base.txt", "landed on the base branch\n")
	o.push(t, moved, "refs/heads/main")
	gitAt(t, wtDir, "config", "user.email", "agent@example.com")
	gitAt(t, wtDir, "config", "user.name", "Agent")
	o.gitAuthed(t, wtDir, "fetch", "-q", "origin", "main")
	o.gitAuthed(t, wtDir, "merge", "-q", "--no-edit", "origin/main")
	merge := strings.TrimSpace(gitAt(t, wtDir, "rev-parse", "HEAD"))

	v := viewBundle(t, captureJailed(t, wtDir).Bundle)
	if v.objects[merge] != "commit" || v.objects[o.pr7] != "commit" {
		t.Errorf("the bundle lacks the merge commit %s or the pull request's commit %s", merge, o.pr7)
	}
	for _, base := range []string{moved, o.main} {
		if _, ok := v.objects[base]; ok {
			t.Errorf("the bundle holds the base branch's commit %s", base)
		}
	}
	if !slices.Contains(v.prerequisites, moved) {
		t.Errorf("prerequisites = %v, want the base branch's new tip %s among them", v.prerequisites, moved)
	}
}

// TestCaptureWorkspaceGit_PRWithoutBaseRefStopsAtThePRHead: a pull request
// checkout whose base branch's ref never landed has only the refs that record
// the pull request's head. The bundle stops at them rather than at nothing,
// which would bundle the whole history.
func TestCaptureWorkspaceGit_PRWithoutBaseRefStopsAtThePRHead(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "pr-no-base-ref-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir, err := CreateForPRInRoot(context.Background(), testRepo("acme", "repo"), o.url, o.url, "feature", 7, "conv-a", root, WithCloneAuth(o.auth))
			if err != nil {
				t.Fatalf("CreateForPRInRoot: %v", err)
			}
			if exec.Command("git", "-C", wtDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main").Run() == nil {
				t.Fatal("fixture: the checkout has an origin/main ref")
			}
			head := commitIn(t, wtDir, "local.txt", "the agent's file\n")

			v := viewBundle(t, captureJailed(t, wtDir).Bundle)
			if !slices.Equal(v.prerequisites, []string{o.pr7}) {
				t.Errorf("prerequisites = %v, want the pull request's head %s", v.prerequisites, o.pr7)
			}
			if _, ok := v.objects[o.pr7]; ok || v.objects[head] != "commit" {
				t.Errorf("bundle objects = %v, want the agent's commit and not the pull request's", v.objects)
			}
		})
	}
}

// TestCaptureWorkspaceGit_RefCheckoutBundleStopsAtItsRef: a checkout of a
// branch bundles the run's commits past that branch, under the name of the
// branch the run made, and writes no bundle before the run commits.
func TestCaptureWorkspaceGit_RefCheckoutBundleStopsAtItsRef(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "ref-bundle-range-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir := o.refCheckout(t, root, "topic")
			if delta := captureJailed(t, wtDir); len(delta.Bundle) != 0 {
				t.Fatalf("a checkout at its ref's tip wrote a %d-byte bundle", len(delta.Bundle))
			}
			gitAt(t, wtDir, "checkout", "-q", "-b", "fix/thing")
			head := commitIn(t, wtDir, "local.txt", "the agent's file\n")

			v := viewBundle(t, captureJailed(t, wtDir).Bundle)
			if !slices.Equal(v.prerequisites, []string{o.topic}) {
				t.Errorf("prerequisites = %v, want the ref's tip %s", v.prerequisites, o.topic)
			}
			if want := []string{head + " refs/heads/fix/thing"}; !slices.Equal(v.tips, want) {
				t.Errorf("tips = %v, want %v", v.tips, want)
			}
		})
	}
}

// TestRestoreCheckout_BundleWithoutAPromisedBlobSurvivesRestores: a pull
// request edits a file twice, so the checkout never fetched the first edit's
// blob and the bundle leaves it out. The pull request is then force-pushed, so
// a rebuilt bare gets the first edit's commit from the bundle alone. Each
// capture of each rebuilt checkout has to leave the blob out again, which git
// allows only while the imported pack is marked as one whose missing
// references the origin supplies.
func TestRestoreCheckout_BundleWithoutAPromisedBlobSurvivesRestores(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "promised-blob-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			promised := o.blobAt(t, o.pr8First, "big.txt")
			wtDir := o.prCheckout(t, root, 8)
			edited := bigFile("the agent's edit")
			head := commitIn(t, wtDir, "big.txt", edited)
			delta := captureJailed(t, wtDir)
			o.push(t, o.commit(t, o.main, "big.txt", bigFile("rewritten pull request")), "refs/pull/8/head")

			for round := 1; round <= 2; round++ {
				v := viewBundle(t, delta.Bundle)
				if _, ok := v.objects[promised]; ok || v.objects[o.pr8First] != "commit" {
					t.Fatalf("capture %d: the bundle must hold commit %s and leave out its blob %s", round, o.pr8First, promised)
				}
				root = loseHost(t, key)
				got := restoreDelta(t, root, key, "acme", "repo", PRRefSlug(8), o.url, o.auth, delta, o.pr())
				assertFileContent(t, filepath.Join(got, "big.txt"), edited)
				delta = captureJailed(t, got)
				if delta.Head != head || len(delta.Bundle) == 0 {
					t.Fatalf("capture %d = head %s with %d bundle bytes, want %s bundled", round+1, delta.Head, len(delta.Bundle), head)
				}
			}
		})
	}
}

// TestRestoreCheckout_StoredThinBundleRestoresWithTheCredential: a snapshot
// stored before the capture wrote its own bundles carries one from `git bundle
// create`, whose pack holds the edited file as a delta against a version a
// fresh blobless bare does not have. The import completes the pack by
// fetching that version, which takes the run's credential.
func TestRestoreCheckout_StoredThinBundleRestoresWithTheCredential(t *testing.T) {
	setRestoreMode(t, true)
	o := newPrivateOrigin(t)
	const key = "thin-bundle-run"
	root := mustRunRoot(t, key)
	t.Cleanup(func() { RemoveRunRoot(key) })

	wtDir := o.prCheckout(t, root, 7)
	edited := bigFile("the agent's edit")
	head := commitIn(t, wtDir, "big.txt", edited)
	branch := prLocalBranch("conv-a", 7)
	staged := filepath.Join(t.TempDir(), "bundle")
	gitAt(t, wtDir, "bundle", "create", "-q", staged, branch, "--not", "--remotes", "--")
	thin, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readBundle(t, thin); err == nil {
		t.Fatal("fixture: the pack `git bundle create` wrote is not thin")
	}

	root = loseHost(t, key)
	bare, err := EnsureBareClone(context.Background(), testRepo("acme", "repo"), o.url, WithCloneAuth(o.auth))
	if err != nil {
		t.Fatalf("EnsureBareClone: %v", err)
	}
	gitAt(t, bare, "fetch", "-q", o.path, "+refs/pull/7/head:refs/remotes/origin/"+branch)
	if err := importBundle(context.Background(), bare, staged, CloneAuth{}); err == nil {
		t.Fatal("fixture: the thin bundle imported with no credential, so it needed no fetch")
	}

	root = loseHost(t, key)
	got := restoreDelta(t, root, key, "acme", "repo", PRRefSlug(7), o.url, o.auth, &GitDelta{Head: head, Branch: branch, Bundle: thin}, o.pr())
	assertFileContent(t, filepath.Join(got, "big.txt"), edited)
}

// rewrittenTopic is a checkout of the fixture's topic branch, captured, whose
// branch the upstream then moved off the commit the snapshot needs: the
// bundle's prerequisite when the run committed, the captured head when it did
// not. It returns the capture and a fresh executor's run root.
func rewrittenTopic(t *testing.T, o *privateOrigin, key string, committed bool) (*GitDelta, string) {
	t.Helper()
	root := mustRunRoot(t, key)
	t.Cleanup(func() { RemoveRunRoot(key) })
	wtDir := o.refCheckout(t, root, "topic")
	if committed {
		commitIn(t, wtDir, "local.txt", "the agent's file\n")
	}
	delta := captureJailed(t, wtDir)
	if committed != (len(delta.Bundle) != 0) {
		t.Fatalf("fixture: capture carries %d bundle bytes with committed=%v", len(delta.Bundle), committed)
	}
	o.push(t, o.main, "refs/heads/topic")
	return delta, loseHost(t, key)
}

// TestRestoreCheckout_FetchesAMissingPrerequisiteByID: the branch a checkout
// was made from is rewritten upstream, so no ref reaches the commit the bundle
// stops at and a fresh bare does not get it from a ref's fetch. The restore
// fetches it by id.
func TestRestoreCheckout_FetchesAMissingPrerequisiteByID(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "missing-prerequisite-run"
			delta, root := rewrittenTopic(t, o, key, true)

			got := restoreDelta(t, root, key, "acme", "repo", CheckoutRefSlug("topic"), o.url, o.auth, delta, nil)
			if head := strings.TrimSpace(gitAt(t, got, "rev-parse", "HEAD")); head != delta.Head {
				t.Errorf("restored HEAD = %s, want %s", head, delta.Head)
			}
			assertFileContent(t, filepath.Join(got, "topic.txt"), "topic change\n")
			assertFileContent(t, filepath.Join(got, "local.txt"), "the agent's file\n")
		})
	}
}

// TestRestoreCheckout_FetchesAMissingHeadByID: a snapshot with no bundle whose
// captured head no ref names any more. The restore fetches the head by id.
func TestRestoreCheckout_FetchesAMissingHeadByID(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			o := newPrivateOrigin(t)
			const key = "missing-head-run"
			delta, root := rewrittenTopic(t, o, key, false)

			got := restoreDelta(t, root, key, "acme", "repo", CheckoutRefSlug("topic"), o.url, o.auth, delta, nil)
			if head := strings.TrimSpace(gitAt(t, got, "rev-parse", "HEAD")); head != o.topic {
				t.Errorf("restored HEAD = %s, want %s", head, o.topic)
			}
			assertFileContent(t, filepath.Join(got, "topic.txt"), "topic change\n")
		})
	}
}

// TestRestoreCheckout_CommitTheOriginRefusesFailsByKind: the origin no longer
// has the commit a snapshot needs. The restore fails with an error that says
// which commit it was: the captured head, with the message that case has
// always had, or the bundle's prerequisite.
func TestRestoreCheckout_CommitTheOriginRefusesFailsByKind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed bool
		kind      MissingCommitKind
		message   string
	}{
		{"head", false, MissingHead, "is not in the bare after refreshing and importing the bundle"},
		{"prerequisite", true, MissingPrerequisite, "bundle prerequisite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRestoreMode(t, true)
			o := newPrivateOrigin(t)
			const key = "refused-commit-run"
			delta, root := rewrittenTopic(t, o, key, tc.committed)
			o.forget(t, o.topic)

			_, err := restoreDeltaErr(t, root, key, "acme", "repo", CheckoutRefSlug("topic"), o.url, o.auth, delta, nil)
			var missing *MissingCommitError
			if !errors.As(err, &missing) {
				t.Fatalf("restore = %v, want a MissingCommitError", err)
			}
			if missing.Kind != tc.kind || missing.OID != o.topic {
				t.Errorf("missing commit = %s %s, want %s %s", missing.Kind, missing.OID, tc.kind, o.topic)
			}
			if !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), o.topic) {
				t.Errorf("error = %q, want it to say %q of %s", err, tc.message, o.topic)
			}
		})
	}
}
