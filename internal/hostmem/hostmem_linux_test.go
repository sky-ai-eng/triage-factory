//go:build linux

package hostmem

import (
	"errors"
	"testing"
)

// On any real Linux host both figures are readable and positive, and
// available never exceeds total. This pins the /proc parsing without
// fixture files — the fields have been stable since kernel 3.14. It
// also exercises whichever source (cgroup or meminfo) this host
// actually resolves to, since the invariant holds either way.
func TestMeminfoFigures(t *testing.T) {
	avail := AvailableMB()
	total := TotalMB()
	if avail <= 0 {
		t.Fatalf("AvailableMB() = %d, want > 0 on Linux", avail)
	}
	if total <= 0 {
		t.Fatalf("TotalMB() = %d, want > 0 on Linux", total)
	}
	if avail > total {
		t.Errorf("AvailableMB() %d > TotalMB() %d", avail, total)
	}
}

// errUnreadable is the fake filesystem's stand-in for os.ReadFile's
// error return (permission denied, ENOENT, whatever) — callers only
// branch on err != nil, so a plain sentinel is enough.
var errUnreadable = errors.New("unreadable")

// fakeFS builds a readFileFunc backed by an in-memory path->content
// map; any path not present reports errUnreadable, standing in for a
// missing or permission-denied file without touching the real
// filesystem — the injected-reader seam the acceptance criteria calls
// for so cgrouped/uncgrouped/unreadable hosts, and hosts where our own
// cgroup sits several levels under the mount root, are all reachable
// from one test binary regardless of what this host actually looks
// like.
func fakeFS(files map[string]string) readFileFunc {
	return func(path string) ([]byte, error) {
		content, ok := files[path]
		if !ok {
			return nil, errUnreadable
		}
		return []byte(content), nil
	}
}

const fakeMeminfo = "MemTotal:       67043328 kB\nMemAvailable:   58720256 kB\n"

// TestAvailableTotal_CgroupLimited pins the container-truthful math
// for the common case — a private cgroup namespace (Docker
// 20.10+/containerd/Fly Machines default), where this process's own
// cgroup coincides with the mount root, so /proc/self/cgroup reads
// "0::/": an 8 GiB memory.max with 2 GiB current usage and 512 MiB of
// reclaimable inactive file cache reports total=8192 MiB and
// available = 8192 - (2048 - 512) = 6656 MiB, not the host's real
// 64 GiB — this is the whole point of the ticket (§8.2's "--memory=8g
// on a 64 GB host" acceptance case, without needing an actual
// container to run the test in).
func TestAvailableTotal_CgroupLimited(t *testing.T) {
	read := fakeFS(map[string]string{
		procSelfCgroupPath:                      "0::/\n",
		cgroupRoot + "/" + cgroupMemMaxFile:     "8589934592",                                          // 8 GiB
		cgroupRoot + "/" + cgroupMemCurrentFile: "2147483648",                                          // 2 GiB
		cgroupRoot + "/" + cgroupMemStatFile:    "anon 1000\ninactive_file 536870912\nactive_file 0\n", // 512 MiB inactive
		procMeminfoPath:                         fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 8192 {
		t.Errorf("totalMBFrom() = %d, want 8192", got)
	}
	if got := availableMBFrom(read); got != 6656 {
		t.Errorf("availableMBFrom() = %d, want 6656", got)
	}
}

// TestAvailableTotal_CgroupUnlimited covers memory.max == "max" (no
// real ceiling, e.g. an unrestricted container): both figures must
// fall back to /proc/meminfo rather than treating "max" as a number.
func TestAvailableTotal_CgroupUnlimited(t *testing.T) {
	read := fakeFS(map[string]string{
		cgroupRoot + "/" + cgroupMemMaxFile: "max\n",
		procMeminfoPath:                     fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 65472 {
		t.Errorf("totalMBFrom() = %d, want 65472 (meminfo fallback)", got)
	}
	if got := availableMBFrom(read); got != 57344 {
		t.Errorf("availableMBFrom() = %d, want 57344 (meminfo fallback)", got)
	}
}

// TestAvailableTotal_BareHost covers no cgroup v2 memory files at all
// (bare metal, or a cgroup v1-only host — this sandbox's own shape):
// falls back to /proc/meminfo exactly as before this ticket.
func TestAvailableTotal_BareHost(t *testing.T) {
	read := fakeFS(map[string]string{
		procMeminfoPath: fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 65472 {
		t.Errorf("totalMBFrom() = %d, want 65472", got)
	}
	if got := availableMBFrom(read); got != 57344 {
		t.Errorf("availableMBFrom() = %d, want 57344", got)
	}
}

// TestAvailableTotal_Unreadable covers a filesystem where nothing can
// be read (no cgroup files, /proc/meminfo also fails): both figures
// must fail open to Unknown rather than erroring or panicking.
func TestAvailableTotal_Unreadable(t *testing.T) {
	read := fakeFS(nil)
	if got := totalMBFrom(read); got != Unknown {
		t.Errorf("totalMBFrom() = %d, want Unknown", got)
	}
	if got := availableMBFrom(read); got != Unknown {
		t.Errorf("availableMBFrom() = %d, want Unknown", got)
	}
}

// TestAvailableMB_CgroupLimitedUsageUnreadable pins the fail-open
// boundary within the cgrouped path itself: once memory.max proves
// this process is confined, a broken memory.current/memory.stat read
// must NOT fall back to /proc/meminfo (that would hand back exactly
// the host-wide lie this package exists to avoid) — it reports
// Unknown instead, while TotalMB (which only needs memory.max) still
// succeeds.
func TestAvailableMB_CgroupLimitedUsageUnreadable(t *testing.T) {
	read := fakeFS(map[string]string{
		cgroupRoot + "/" + cgroupMemMaxFile: "8589934592",
		procMeminfoPath:                     fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 8192 {
		t.Errorf("totalMBFrom() = %d, want 8192", got)
	}
	if got := availableMBFrom(read); got != Unknown {
		t.Errorf("availableMBFrom() = %d, want Unknown (must not fall back to host-wide meminfo)", got)
	}
}

// TestAvailableTotal_CgroupHierarchical_SharedNamespace pins the fix
// for a shared/host cgroup namespace: /sys/fs/cgroup is the real
// kernel root here, which has no memory.max of its own (nothing sits
// above it to limit it), and an intermediate slice is unlimited — but
// this process's own leaf cgroup, resolved via /proc/self/cgroup
// rather than assumed to be the mount root, carries the real limit. A
// naive "read /sys/fs/cgroup/memory.max only" implementation would
// see no limit anywhere and leak the host-wide meminfo figures; this
// must resolve the leaf and report the container-truthful numbers.
func TestAvailableTotal_CgroupHierarchical_SharedNamespace(t *testing.T) {
	const leaf = "/sys/fs/cgroup/system.slice/docker-abc123.scope"
	read := fakeFS(map[string]string{
		procSelfCgroupPath: "0::/system.slice/docker-abc123.scope\n",
		"/sys/fs/cgroup/system.slice/" + cgroupMemMaxFile: "max\n",
		leaf + "/" + cgroupMemMaxFile:                     "8589934592",                // 8 GiB
		leaf + "/" + cgroupMemCurrentFile:                 "2147483648",                // 2 GiB
		leaf + "/" + cgroupMemStatFile:                    "inactive_file 536870912\n", // 512 MiB
		procMeminfoPath:                                   fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 8192 {
		t.Errorf("totalMBFrom() = %d, want 8192 (container leaf limit, not host meminfo)", got)
	}
	if got := availableMBFrom(read); got != 6656 {
		t.Errorf("availableMBFrom() = %d, want 6656 (container leaf headroom, not host meminfo)", got)
	}
}

// TestAvailableTotal_CgroupHierarchical_ParentTighter pins the
// hierarchical-min correctness the kernel itself enforces: cgroup v2's
// memory.max on ANY ancestor is a real ceiling, not just the leaf's
// own. When a parent slice is already near its own ceiling — because
// of usage in sibling cgroups this process can't see from its own
// leaf — that parent governs both the effective total and the
// effective headroom, even though the leaf's own limit is more
// generous and the leaf itself looks nearly empty.
func TestAvailableTotal_CgroupHierarchical_ParentTighter(t *testing.T) {
	const parent = "/sys/fs/cgroup/kubepods.slice/podabc.slice"
	const leaf = parent + "/crio-def456.scope"
	read := fakeFS(map[string]string{
		procSelfCgroupPath: "0::/kubepods.slice/podabc.slice/crio-def456.scope\n",
		"/sys/fs/cgroup/kubepods.slice/" + cgroupMemMaxFile: "max\n",
		// Leaf: generous 8 GiB limit, barely used.
		leaf + "/" + cgroupMemMaxFile:     "8589934592",
		leaf + "/" + cgroupMemCurrentFile: "1073741824", // 1 GiB
		leaf + "/" + cgroupMemStatFile:    "inactive_file 0\n",
		// Parent pod slice: tighter 4 GiB limit, already near its own
		// ceiling from sibling containers' usage.
		parent + "/" + cgroupMemMaxFile:     "4294967296",
		parent + "/" + cgroupMemCurrentFile: "3758096384", // 3.5 GiB
		parent + "/" + cgroupMemStatFile:    "inactive_file 0\n",
		procMeminfoPath:                     fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 4096 {
		t.Errorf("totalMBFrom() = %d, want 4096 (parent slice is the binding ceiling)", got)
	}
	if got := availableMBFrom(read); got != 512 {
		t.Errorf("availableMBFrom() = %d, want 512 (parent slice headroom governs, not the leaf's)", got)
	}
}

// TestSelfCgroupPath pins the /proc/self/cgroup parsing: only the
// unified ("0::") line matters, regardless of how many v1 named
// hierarchy lines sit above it in a hybrid mount, and a v1-only host
// (no "0::" line at all) reports ok=false.
func TestSelfCgroupPath(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantOK  bool
	}{
		{"unified root (private namespace)", "0::/\n", "/", true},
		{"unified nested (shared namespace)", "0::/system.slice/docker-abc.scope\n", "/system.slice/docker-abc.scope", true},
		{"hybrid mount, v1 lines above the unified line", "12:pids:/\n11:memory:/foo\n0::/bar\n", "/bar", true},
		{"v1-only host, no unified line", "3:memory:/foo\n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := fakeFS(map[string]string{procSelfCgroupPath: tt.content})
			got, ok := selfCgroupPath(read)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("selfCgroupPath() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestCgroupAncestorDirs pins the path-join logic the shared-namespace
// fix depends on: a nested self path expands into every ancestor
// directory from the mount root down to the leaf, root first.
func TestCgroupAncestorDirs(t *testing.T) {
	tests := []struct {
		name     string
		selfPath string
		want     []string
	}{
		{"private namespace root", "/", []string{"/sys/fs/cgroup"}},
		{"unresolved (self-cgroup read failed)", "", []string{"/sys/fs/cgroup"}},
		{"nested shared-namespace path", "/system.slice/docker-abc.scope", []string{
			"/sys/fs/cgroup",
			"/sys/fs/cgroup/system.slice",
			"/sys/fs/cgroup/system.slice/docker-abc.scope",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cgroupAncestorDirs(tt.selfPath)
			if len(got) != len(tt.want) {
				t.Fatalf("cgroupAncestorDirs(%q) = %v, want %v", tt.selfPath, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("cgroupAncestorDirs(%q)[%d] = %q, want %q", tt.selfPath, i, got[i], tt.want[i])
				}
			}
		})
	}
}
