//go:build linux

package capinfo

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// capNames is the standard Linux capability bit -> name table
// (linux/capability.h), current through CAP_CHECKPOINT_RESTORE (40, added
// in kernel 5.9). This list has been stable for years — new capabilities
// are rare and ABI-sensitive — but an unknown bit still renders as
// "cap<N>" rather than being silently swallowed, so a future kernel
// addition shows up in the boot line instead of vanishing from it.
var capNames = map[uint]string{
	0:  "cap_chown",
	1:  "cap_dac_override",
	2:  "cap_dac_read_search",
	3:  "cap_fowner",
	4:  "cap_fsetid",
	5:  "cap_kill",
	6:  "cap_setgid",
	7:  "cap_setuid",
	8:  "cap_setpcap",
	9:  "cap_linux_immutable",
	10: "cap_net_bind_service",
	11: "cap_net_broadcast",
	12: "cap_net_admin",
	13: "cap_net_raw",
	14: "cap_ipc_lock",
	15: "cap_ipc_owner",
	16: "cap_sys_module",
	17: "cap_sys_rawio",
	18: "cap_sys_chroot",
	19: "cap_sys_ptrace",
	20: "cap_sys_pacct",
	21: "cap_sys_admin",
	22: "cap_sys_boot",
	23: "cap_sys_nice",
	24: "cap_sys_resource",
	25: "cap_sys_time",
	26: "cap_sys_tty_config",
	27: "cap_mknod",
	28: "cap_lease",
	29: "cap_audit_write",
	30: "cap_audit_control",
	31: "cap_setfcap",
	32: "cap_mac_override",
	33: "cap_mac_admin",
	34: "cap_syslog",
	35: "cap_wake_alarm",
	36: "cap_block_suspend",
	37: "cap_audit_read",
	38: "cap_perfmon",
	39: "cap_bpf",
	40: "cap_checkpoint_restore",
}

// statusPath is the proc file every lookup below reads. A var (not a
// const) purely so this package's own tests can point it at a fixture
// instead of the real /proc/self/status.
var statusPath = "/proc/self/status"

// Effective returns this process's own effective capability set — the one
// the kernel actually checks at syscall time — decoded to names and
// sorted. Empty (never nil) when the process holds none, which is the
// orchestrator's expected state after the exec-time drop.
func Effective() ([]string, error) { return read(statusPath, "CapEff") }

// Bounding returns this process's own capability bounding set the same
// way: the ceiling nothing it execs can ever regain, even via a
// setuid-root helper — the property the exec-time drop's
// --bounding-set=-all clears for good.
func Bounding() ([]string, error) { return read(statusPath, "CapBnd") }

func read(path, field string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("capinfo: open %s: %w", path, err)
	}
	defer f.Close()
	return parse(f, path, field)
}

// parse scans r (the contents of a /proc/<pid>/status file) for field's
// hex bitmask line and decodes it to sorted capability names. Split out
// from read so it's testable against a synthetic reader without touching
// the real filesystem.
func parse(r io.Reader, sourceName, field string) ([]string, error) {
	prefix := field + ":"
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		hex := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		mask, err := strconv.ParseUint(hex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("capinfo: parse %s %q in %s: %w", field, hex, sourceName, err)
		}
		return namesFromMask(mask), nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("capinfo: read %s: %w", sourceName, err)
	}
	return nil, fmt.Errorf("capinfo: %s has no %s line", sourceName, field)
}

func namesFromMask(mask uint64) []string {
	names := make([]string, 0, 8)
	for bit := uint(0); bit < 64; bit++ {
		if mask&(1<<bit) == 0 {
			continue
		}
		if name, ok := capNames[bit]; ok {
			names = append(names, name)
		} else {
			names = append(names, fmt.Sprintf("cap<%d>", bit))
		}
	}
	sort.Strings(names)
	return names
}
