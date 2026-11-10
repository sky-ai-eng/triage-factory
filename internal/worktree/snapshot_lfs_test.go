package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lfsContent is a file's content as Git LFS keeps it: the pointer committed in
// its place, and the object id the store files it under.
type lfsContent struct {
	content, pointer, oid string
}

func newLFSContent(content string) lfsContent {
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	return lfsContent{
		content: content,
		pointer: fmt.Sprintf("%s\noid sha256:%s\nsize %d\n", lfsPointerVersion, oid, len(content)),
		oid:     oid,
	}
}

// gitNoLFS runs git in dir with git-lfs's filter off, so a pointer written
// into the tree is committed as it is whatever the host's config says.
func gitNoLFS(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitAt(t, dir, append(append([]string{}, captureLFSFilterOff...), args...)...)
}

// storeLFSObject writes c's object into the LFS store of the checkout at dir,
// as `git lfs clean` would have.
func storeLFSObject(t *testing.T, dir string, c lfsContent) {
	t.Helper()
	objects, err := lfsObjectsDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := lfsObjectPath(objects, c.oid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeLFSUpstream is an upstream whose main keeps *.bin in Git LFS and holds
// one LFS file, whose object exists nowhere.
func makeLFSUpstream(t *testing.T, pushed lfsContent) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	work := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", upstream).CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "init", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("git init work: %v: %s", err, out)
	}
	writeUnder(t, work, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	writeUnder(t, work, "pushed.bin", pushed.pointer)
	gitNoLFS(t, work, "add", "-A")
	gitNoLFS(t, work, "-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-q", "-m", "seed")
	gitNoLFS(t, work, "push", "-q", "--no-verify", upstream, "main")
	return upstream
}

func TestParseLFSPointer(t *testing.T) {
	c := newLFSContent("hello\n")
	for _, tc := range []struct {
		name, data string
		ok         bool
	}{
		{"pointer", c.pointer, true},
		{"with an extension", fmt.Sprintf("%s\next-0-foo sha256:%s\noid sha256:%s\nsize 6\n", lfsPointerVersion, c.oid, c.oid), true},
		{"other version", strings.Replace(c.pointer, "spec/v1", "spec/v2", 1), false},
		{"short oid", strings.Replace(c.pointer, c.oid, c.oid[:63], 1), false},
		{"uppercase oid", strings.Replace(c.pointer, c.oid, strings.ToUpper(c.oid), 1), false},
		{"another hash", strings.Replace(c.pointer, "sha256:", "sha1:", 1), false},
		{"no size", fmt.Sprintf("%s\noid sha256:%s\n", lfsPointerVersion, c.oid), false},
		{"negative size", strings.Replace(c.pointer, "size 6", "size -6", 1), false},
		{"no final newline", strings.TrimSuffix(c.pointer, "\n"), false},
		{"content", "hello\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := parseLFSPointer([]byte(tc.data))
			if ok != tc.ok {
				t.Fatalf("parseLFSPointer ok = %v, want %v", ok, tc.ok)
			}
			if ok && (p.oid != c.oid || p.size != 6) {
				t.Errorf("parseLFSPointer = %+v, want oid %s size 6", p, c.oid)
			}
		})
	}
}

// TestRestoreCheckout_CarriesUnpushedLFSObjects is a run that committed a new
// LFS file and was parked before it pushed. The capture carries the object
// its unpushed commit names and nothing else: not the object of an LFS file
// already upstream, nor one the checkout does not hold, nor a store file of a
// size its pointer does not name. The restore puts the
// object where git-lfs looks for it in the rebuilt checkout, which for a
// linked worktree is the shared bare it uses; the bare here is a fresh one.
func TestRestoreCheckout_CarriesUnpushedLFSObjects(t *testing.T) {
	for _, mode := range restoreModes {
		t.Run(mode.name, func(t *testing.T) {
			setRestoreMode(t, mode.selfContained)
			ctx := context.Background()
			pushed, added, unheld := newLFSContent("on the remote\n"), newLFSContent("the run's new asset\n"), newLFSContent("held nowhere\n")
			truncated := newLFSContent("cut short in the store\n")
			upstream := makeLFSUpstream(t, pushed)
			const key = "lfs-roundtrip-run"
			root := mustRunRoot(t, key)
			t.Cleanup(func() { RemoveRunRoot(key) })

			wtDir, err := CreateForCheckoutInRoot(ctx, testRepo("acme", "repo"), upstream, "main", "conv-a", root)
			if err != nil {
				t.Fatalf("CreateForCheckoutInRoot: %v", err)
			}
			gitAt(t, wtDir, "checkout", "-q", "-b", "feature")
			storeLFSObject(t, wtDir, pushed)
			storeLFSObject(t, wtDir, added)
			storeLFSObject(t, wtDir, lfsContent{content: "cut short", oid: truncated.oid})
			writeUnder(t, wtDir, "added.bin", added.pointer)
			writeUnder(t, wtDir, "unheld.bin", unheld.pointer)
			writeUnder(t, wtDir, "truncated.bin", truncated.pointer)
			gitNoLFS(t, wtDir, "add", "-A")
			gitNoLFS(t, wtDir, "-c", "user.name=Agent", "-c", "user.email=agent@example.test", "commit", "-q", "-m", "add assets")

			delta, err := CaptureWorkspaceGit(ctx, wtDir)
			if err != nil {
				t.Fatalf("CaptureWorkspaceGit: %v", err)
			}
			if len(delta.LFSObjects) != 1 || delta.LFSObjects[0].OID != added.oid {
				t.Fatalf("captured LFS objects = %+v, want only %s", delta.LFSObjects, added.oid)
			}
			// The capture reads the objects from the tree, which the restore
			// does not have: stage them, as an archive would.
			staged := filepath.Join(t.TempDir(), added.oid)
			if err := os.WriteFile(staged, []byte(added.content), 0o600); err != nil {
				t.Fatal(err)
			}
			delta.LFSObjects[0].Path = staged

			RemoveRunRoot(key)
			bare, _ := repoDir(testRepo("acme", "repo").ID)
			if err := os.RemoveAll(bare); err != nil {
				t.Fatalf("remove bare: %v", err)
			}
			root = mustRunRoot(t, key)
			got := restoreDelta(t, root, key, "acme", "repo", CheckoutRefSlug("main"), upstream, CloneAuth{}, delta, nil)

			objects, err := lfsObjectsDir(ctx, got)
			if err != nil {
				t.Fatal(err)
			}
			if mode.selfContained != strings.HasPrefix(objects, got+string(filepath.Separator)) {
				t.Errorf("LFS store %s; want it inside the checkout exactly when self-contained", objects)
			}
			assertFileContent(t, lfsObjectPath(objects, added.oid), added.content)
			for _, c := range []lfsContent{pushed, unheld, truncated} {
				if _, err := os.Stat(lfsObjectPath(objects, c.oid)); !os.IsNotExist(err) {
					t.Errorf("object %s is in the restored store (stat err %v); want only the run's own", c.oid, err)
				}
			}
			assertFileContent(t, filepath.Join(got, "added.bin"), added.pointer)

			if _, err := exec.LookPath("git-lfs"); err == nil {
				env := gitAt(t, got, "lfs", "env")
				if !strings.Contains(env, "LocalMediaDir="+objects+"\n") {
					t.Errorf("git-lfs does not read objects from %s:\n%s", objects, env)
				}
			}
		})
	}
}

// TestInstallLFSObjects_RefusesContentThatIsNotTheObject pins that a restore
// installs an object only as content hashing to its id, and goes on without
// one that does not.
func TestInstallLFSObjects_RefusesContentThatIsNotTheObject(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitAt(t, dir, "init", "-q")
	good, bad := newLFSContent("good\n"), newLFSContent("bad\n")
	staging := t.TempDir()
	for name, content := range map[string]string{"good": good.content, "bad": "not the bad object\n"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := installLFSObjects(ctx, dir, []LFSObject{
		{OID: bad.oid, Path: filepath.Join(staging, "bad")},
		{OID: good.oid, Path: filepath.Join(staging, "good")},
	})
	if err != nil {
		t.Fatalf("installLFSObjects: %v", err)
	}
	objects := filepath.Join(dir, ".git", "lfs", "objects")
	assertFileContent(t, lfsObjectPath(objects, good.oid), good.content)
	if _, err := os.Stat(lfsObjectPath(objects, bad.oid)); !os.IsNotExist(err) {
		t.Errorf("mismatched object installed (stat err %v)", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(objects, "*", "*", ".restore-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left in the store: %v", leftovers)
	}
}

// TestCaptureUncommitted_FetchedLFSFileStaysOut is a checkout where the run
// fetched an LFS file and its index entry no longer matches it by stat, so the
// capture's git, which runs without git-lfs's filter, reads it as modified. A
// file whose content is the object its HEAD pointer names stays out of the
// patch; a file edited past its pointer and an uncommitted LFS file travel
// whole. The checkout's own config names a filter for lfs that would rewrite
// the files it cleans, and the capture runs none of it. Both stages and both
// environments the capture runs under give the same tree back.
func TestCaptureUncommitted_FetchedLFSFileStaysOut(t *testing.T) {
	for _, env := range []string{"host", "capture-jail"} {
		for _, limit := range []int{changeScopedPathspecLimit, 0} {
			stage := "scoped"
			if limit == 0 {
				stage = "full"
			}
			t.Run(env+"/"+stage, func(t *testing.T) {
				fetched, edited := newLFSContent("fetched asset\n"), newLFSContent("edited asset\n")
				wtDir := captureWorktree(t, sanitizeRootKey(t.Name()))
				writeUnder(t, wtDir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
				writeUnder(t, wtDir, "fetched.bin", fetched.pointer)
				writeUnder(t, wtDir, "edited.bin", edited.pointer)
				gitNoLFS(t, wtDir, "add", "-A")
				gitNoLFS(t, wtDir, "-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-q", "-m", "lfs files")
				head := strings.TrimSpace(gitAt(t, wtDir, "rev-parse", "HEAD"))

				storeLFSObject(t, wtDir, fetched)
				writeUnder(t, wtDir, "fetched.bin", fetched.content)
				writeUnder(t, wtDir, "edited.bin", "edited past its pointer\n")
				writeUnder(t, wtDir, "new.bin", "uncommitted asset\n")
				gitAt(t, wtDir, "config", "filter.lfs.clean", "sed s/^/CLEANED/")
				gitAt(t, wtDir, "config", "filter.lfs.required", "true")

				prev := changeScopedPathspecLimit
				changeScopedPathspecLimit = limit
				t.Cleanup(func() { changeScopedPathspecLimit = prev })
				var delta *GitDelta
				var err error
				capture := func() { delta, err = CaptureWorkspaceGit(context.Background(), wtDir) }
				if env == "capture-jail" {
					withCaptureJailEnv(t, capture)
				} else {
					capture()
				}
				if err != nil {
					t.Fatalf("CaptureWorkspaceGit: %v", err)
				}
				if strings.Contains(string(delta.Patch), "fetched.bin") {
					t.Errorf("patch carries the fetched LFS file:\n%s", delta.Patch)
				}
				for _, name := range []string{"edited.bin", "new.bin"} {
					if !strings.Contains(string(delta.Patch), "b/"+name) {
						t.Errorf("patch does not carry %s:\n%s", name, delta.Patch)
					}
				}

				if err := os.RemoveAll(wtDir); err != nil {
					t.Fatal(err)
				}
				root := filepath.Dir(filepath.Dir(wtDir))
				got := restoreDelta(t, root, filepath.Base(root), "acme", "repo", CheckoutRefSlug("main"), "", CloneAuth{}, delta, nil)
				if h := strings.TrimSpace(gitAt(t, got, "rev-parse", "HEAD")); h != head {
					t.Fatalf("restored HEAD = %s, want %s", h, head)
				}
				assertFileContent(t, filepath.Join(got, "fetched.bin"), fetched.pointer)
				assertFileContent(t, filepath.Join(got, "edited.bin"), "edited past its pointer\n")
				assertFileContent(t, filepath.Join(got, "new.bin"), "uncommitted asset\n")
			})
		}
	}
}

// TestDropFetchedLFSPaths pins that the scoped stage never sees an LFS file
// the run fetched, so the capture neither hashes nor stores it, while an LFS
// file edited past its pointer and every other change stay in.
func TestDropFetchedLFSPaths(t *testing.T) {
	ctx := context.Background()
	fetched, edited := newLFSContent("fetched asset\n"), newLFSContent("edited asset\n")
	wtDir := captureWorktree(t, sanitizeRootKey(t.Name()))
	writeUnder(t, wtDir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	writeUnder(t, wtDir, "fetched.bin", fetched.pointer)
	writeUnder(t, wtDir, "edited.bin", edited.pointer)
	gitNoLFS(t, wtDir, "add", "-A")
	gitNoLFS(t, wtDir, "-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-q", "-m", "lfs files")
	storeLFSObject(t, wtDir, fetched)
	writeUnder(t, wtDir, "fetched.bin", fetched.content)
	writeUnder(t, wtDir, "edited.bin", "edited past its pointer\n")
	writeUnder(t, wtDir, "tracked.txt", "changed\n")

	changed, ok := changedPathsVsHEAD(ctx, wtDir)
	if !ok || !slices.Contains(changed, "fetched.bin") {
		t.Fatalf("pre-pass = %v (ok %v); the fixture needs fetched.bin to read as modified", changed, ok)
	}
	got := dropFetchedLFSPaths(ctx, wtDir, changed)
	slices.Sort(got)
	if want := []string{"edited.bin", "tracked.txt"}; !slices.Equal(got, want) {
		t.Errorf("dropFetchedLFSPaths = %v, want %v", got, want)
	}
}
