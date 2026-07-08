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
// for so cgrouped/uncgrouped/unreadable hosts are all reachable from
// one test binary regardless of what this host actually looks like.
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

// TestAvailableTotal_CgroupLimited pins the container-truthful math:
// an 8 GiB memory.max with 2 GiB current usage and 512 MiB of
// reclaimable inactive file cache reports total=8192 MiB and
// available = 8192 - (2048 - 512) = 6656 MiB, not the host's real
// 64 GiB — this is the whole point of the ticket (§8.2's "--memory=8g
// on a 64 GB host" acceptance case, without needing an actual
// container to run the test in).
func TestAvailableTotal_CgroupLimited(t *testing.T) {
	read := fakeFS(map[string]string{
		cgroupMemMaxPath:     "8589934592",                                          // 8 GiB
		cgroupMemCurrentPath: "2147483648",                                          // 2 GiB
		cgroupMemStatPath:    "anon 1000\ninactive_file 536870912\nactive_file 0\n", // 512 MiB inactive
		procMeminfoPath:      fakeMeminfo,
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
		cgroupMemMaxPath: "max\n",
		procMeminfoPath:  fakeMeminfo,
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
		cgroupMemMaxPath: "8589934592",
		procMeminfoPath:  fakeMeminfo,
	})
	if got := totalMBFrom(read); got != 8192 {
		t.Errorf("totalMBFrom() = %d, want 8192", got)
	}
	if got := availableMBFrom(read); got != Unknown {
		t.Errorf("availableMBFrom() = %d, want Unknown (must not fall back to host-wide meminfo)", got)
	}
}
