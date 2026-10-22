//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// jailTmpStateRoot points the state root at a fresh directory for a test that
// makes jail /tmp directories, which takes root to hand them to a jail
// identity and a disk to hold them on.
func jailTmpStateRoot(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("handing a jail its /tmp takes root")
	}
	root := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		t.Fatal(err)
	}
	if fs := memoryBackedFS(&st); fs != "" {
		t.Skipf("the test temp dir is on %s, where the broker refuses to keep a jail's /tmp; set TMPDIR to a disk", fs)
	}
	paths.SetForTest(t, root)
	return root
}

func ownerAndMode(t *testing.T, path string) (uid, gid int, perm os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), info.Mode()
}

// Every network gets an empty /tmp of its jails' own, closed to everyone
// else, whatever a network before it on the same index left there.
func TestPrepareJailTmp_EmptyAndTheJailsAlone(t *testing.T) {
	root := jailTmpStateRoot(t)
	ctx := context.Background()

	dir, err := prepareJailTmp(ctx, 7)
	if err != nil {
		t.Fatalf("prepareJailTmp: %v", err)
	}
	if want := filepath.Join(root, jailTmpDirName, "7"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if uid, gid, mode := ownerAndMode(t, dir); uid != JailUID(7) || gid != JailGID(7) || mode != os.ModeDir|0o700 {
		t.Errorf("jail /tmp is %d:%d %v, want %d:%d drwx------", uid, gid, mode, JailUID(7), JailGID(7))
	}
	if uid, _, mode := ownerAndMode(t, filepath.Dir(dir)); uid != 0 || mode != os.ModeDir|0o700 {
		t.Errorf("the base is uid %d %v, want root drwx------", uid, mode)
	}

	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "left-behind"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareJailTmp(ctx, 7); err != nil {
		t.Fatalf("prepareJailTmp again: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a new network's /tmp holds %v (err %v), want nothing", entries, err)
	}
}

// A jail relaunched into its network keeps what the one before it left in
// /tmp, and gets the directory back as its own whatever that jail did to its
// mode.
func TestJailTmp_KeepsWhatTheJailBeforeLeft(t *testing.T) {
	jailTmpStateRoot(t)
	ctx := context.Background()
	dir, err := prepareJailTmp(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}

	got, err := jailTmp(ctx, 7)
	if err != nil {
		t.Fatalf("jailTmp: %v", err)
	}
	if got != dir {
		t.Errorf("jailTmp = %q, want the network's %q", got, dir)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cmd.log")); err != nil || string(b) != "x" {
		t.Errorf("the relaunched jail's /tmp lost cmd.log: %q, %v", b, err)
	}
	if uid, gid, mode := ownerAndMode(t, dir); uid != JailUID(7) || gid != JailGID(7) || mode != os.ModeDir|0o700 {
		t.Errorf("jail /tmp is %d:%d %v, want %d:%d drwx------", uid, gid, mode, JailUID(7), JailGID(7))
	}

	// One that is missing is made again, empty.
	if err := removeJailTmp(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := jailTmp(ctx, 7); err != nil {
		t.Fatalf("jailTmp with none there: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a remade /tmp holds %v (err %v), want nothing", entries, err)
	}
}

// What sits at a jail's name in the base goes as itself: a symlink planted
// there is removed, never followed out of the base.
func TestPrepareJailTmp_RemovesASymlinkWithoutFollowingIt(t *testing.T) {
	jailTmpStateRoot(t)
	ctx := context.Background()
	if _, err := prepareJailTmp(ctx, 3); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(paths.StateRoot(), jailTmpDirName)
	outside := t.TempDir()
	kept := filepath.Join(outside, "kept")
	if err := os.WriteFile(kept, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(base, "5")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "5")); err != nil {
		t.Fatal(err)
	}

	dir, err := prepareJailTmp(ctx, 5)
	if err != nil {
		t.Fatalf("prepareJailTmp: %v", err)
	}
	if _, _, mode := ownerAndMode(t, dir); mode&os.ModeSymlink != 0 || !mode.IsDir() {
		t.Errorf("jail /tmp is %v, want a real directory", mode)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("a file the planted symlink pointed at is gone: %v", err)
	}
}

// A base the broker did not make is not trusted with a jail's files: one owned
// by another identity, one open to others, and a symlink in its place.
func TestPrepareJailTmp_RefusesABaseItDidNotMake(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, base string){
		"owned by another uid": func(t *testing.T, base string) {
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(base, 12345, 12345); err != nil {
				t.Fatal(err)
			}
		},
		"open to others": func(t *testing.T, base string) {
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(base, 0o777); err != nil {
				t.Fatal(err)
			}
		},
		"a symlink": func(t *testing.T, base string) {
			if err := os.Symlink(t.TempDir(), base); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := jailTmpStateRoot(t)
			plant(t, filepath.Join(root, jailTmpDirName))
			if dir, err := prepareJailTmp(context.Background(), 1); err == nil {
				t.Fatalf("prepareJailTmp made %s in a base it did not make", dir)
			}
		})
	}
}

// A jail's /tmp goes with its network, and a boot removes every one a dead
// process left.
func TestRemoveAndReapJailTmp(t *testing.T) {
	jailTmpStateRoot(t)
	ctx := context.Background()
	var dirs []string
	for _, idx := range []uint8{3, 4} {
		dir, err := prepareJailTmp(ctx, idx)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}

	if err := removeJailTmp(ctx, 3); err != nil {
		t.Fatalf("removeJailTmp: %v", err)
	}
	if _, err := os.Lstat(dirs[0]); !os.IsNotExist(err) {
		t.Errorf("index 3's /tmp survived its removal: %v", err)
	}
	if _, err := os.Lstat(dirs[1]); err != nil {
		t.Errorf("removing index 3's /tmp took index 4's: %v", err)
	}
	if err := removeJailTmp(ctx, 3); err != nil {
		t.Errorf("removing a /tmp already gone: %v", err)
	}

	reapOrphanJailTmp(ctx)
	if entries, err := os.ReadDir(filepath.Dir(dirs[1])); err != nil || len(entries) != 0 {
		t.Errorf("after the reap the base holds %v (err %v), want nothing", entries, err)
	}
}

func TestMemoryBackedFS(t *testing.T) {
	for magic, want := range map[uint32]string{
		unix.TMPFS_MAGIC:      "tmpfs",
		unix.RAMFS_MAGIC:      "ramfs",
		unix.EXT4_SUPER_MAGIC: "",
		unix.XFS_SUPER_MAGIC:  "",
	} {
		st := unix.Statfs_t{Type: int64(magic)}
		if got := memoryBackedFS(&st); got != want {
			t.Errorf("memoryBackedFS(%#x) = %q, want %q", magic, got, want)
		}
	}
}

// runsc's score is lowered only once it has forked its sandbox, so the
// processes the sandbox forks while it boots keep the score they started
// with. The stand-in forks its "sandbox", a child whose argv carries boot,
// after a pause; the score here is a raise, which needs no privilege.
func TestLowerOOMScoreOnceBooted_WaitsForTheSandbox(t *testing.T) {
	cmd := exec.Command("sh", "-c", `sleep 0.5; sh -c 'sleep 30' boot & wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	initial := readScore(t, pid)
	const raised = 300
	if initial >= raised {
		t.Skipf("this process already runs at oom_score_adj %d", initial)
	}

	lowerOOMScoreOnceBooted(pid, raised, "test")
	time.Sleep(200 * time.Millisecond)
	if got := readScore(t, pid); got != initial {
		t.Fatalf("score = %d before the sandbox exists, want it unchanged (%d)", got, initial)
	}

	deadline := time.Now().Add(5 * time.Second)
	for readScore(t, pid) != raised {
		if time.Now().After(deadline) {
			t.Fatalf("score = %d after the sandbox appeared, want %d", readScore(t, pid), raised)
		}
		time.Sleep(20 * time.Millisecond)
	}
	children, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + strconv.Itoa(pid) + "/children")
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range strings.Fields(string(children)) {
		n, _ := strconv.Atoi(child)
		if got := readScore(t, n); got != initial {
			t.Errorf("the sandbox, forked before the write, runs at %d, want %d", got, initial)
		}
	}
}

func readScore(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
