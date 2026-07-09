//go:build linux

package capinfo

import (
	"reflect"
	"strings"
	"testing"
)

// 0x201000 = (1<<12)|(1<<21) = CAP_NET_ADMIN | CAP_SYS_ADMIN — the
// cap-broker's documented effective capability set.
const fakeStatus = `Name:	tf-cap-broker
State:	R (running)
Pid:	123
CapInh:	0000000000000000
CapPrm:	0000000000201000
CapEff:	0000000000201000
CapBnd:	0000003fffffffff
CapAmb:	0000000000000000
`

func TestParse_DecodesKnownBits(t *testing.T) {
	got, err := parse(strings.NewReader(fakeStatus), "test", "CapEff")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"cap_net_admin", "cap_sys_admin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse CapEff = %v, want %v", got, want)
	}
}

func TestParse_EmptyMask(t *testing.T) {
	const status = "CapEff:\t0000000000000000\n"
	got, err := parse(strings.NewReader(status), "test", "CapEff")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parse empty CapEff = %v, want empty", got)
	}
}

func TestParse_UnknownBitRendersAsPlaceholder(t *testing.T) {
	// Bit 41 is not in capNames (table stops at 40) — 1<<41 = 0x20000000000.
	const status = "CapEff:\t0000020000000000\n"
	got, err := parse(strings.NewReader(status), "test", "CapEff")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"cap<41>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse unknown-bit CapEff = %v, want %v", got, want)
	}
}

func TestParse_MissingFieldErrors(t *testing.T) {
	const status = "Name:\tfoo\n"
	if _, err := parse(strings.NewReader(status), "test", "CapEff"); err == nil {
		t.Error("parse with no matching field: want an error, got nil")
	}
}

func TestParse_MalformedHexErrors(t *testing.T) {
	const status = "CapEff:\tnot-hex\n"
	if _, err := parse(strings.NewReader(status), "test", "CapEff"); err == nil {
		t.Error("parse with malformed hex: want an error, got nil")
	}
}

func TestNamesFromMask_SysAdminAndNetAdminExactly(t *testing.T) {
	// CAP_NET_ADMIN=12, CAP_SYS_ADMIN=21 -> (1<<12)|(1<<21) = 0x201000,
	// the cap-broker's documented effective capability set.
	mask := uint64(1<<12) | uint64(1<<21)
	got := namesFromMask(mask)
	want := []string{"cap_net_admin", "cap_sys_admin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("namesFromMask(%#x) = %v, want %v", mask, got, want)
	}
}

func TestEffective_ReadsRealProcSelf(t *testing.T) {
	// Smoke test against the real running process: whatever the test
	// binary's own effective set happens to be, this must not error and
	// must return a (possibly empty) slice — it exercises the real
	// /proc/self/status codepath end to end, unlike the synthetic-reader
	// tests above.
	names, err := Effective()
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if names == nil {
		t.Error("Effective() = nil, want a non-nil (possibly empty) slice")
	}
}
