package sandbox

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// writeMembers frames members in order, each at its mode in modes or at 0600.
func writeMembers(t *testing.T, members map[string]string, modes map[string]os.FileMode, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewCaptureStreamWriter(&buf)
	for _, name := range order {
		perm, ok := modes[name]
		if !ok {
			perm = 0o600
		}
		m, err := w.Member(name, perm)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(m, members[name]); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCaptureStream_RoundTrip(t *testing.T) {
	big := strings.Repeat("x", 3*captureStreamChunk+17)
	members := map[string]string{"a": "alpha", "empty": "", "big": big, "scratch/n/f": "nested"}
	modes := map[string]os.FileMode{"a": 0o600, "empty": 0, "big": 0o644, "scratch/n/f": 0o777}
	order := []string{"a", "empty", "big", "scratch/n/f"}
	r := NewCaptureStreamReader(bytes.NewReader(writeMembers(t, members, modes, order)))
	for _, want := range order {
		name, perm, body, err := r.Next()
		if err != nil || name != want {
			t.Fatalf("Next = %q, %v; want %q", name, err, want)
		}
		if perm != modes[want] {
			t.Errorf("member %q mode = %v, want %v", want, perm, modes[want])
		}
		got, err := io.ReadAll(body)
		if err != nil || string(got) != members[want] {
			t.Fatalf("member %q = %d bytes, %v; want %d", want, len(got), err, len(members[want]))
		}
	}
	if _, _, _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last member: %v, want EOF", err)
	}
}

// TestCaptureStream_SkippedMemberIsDrained: a reader that abandons a member
// still lands on the next one.
func TestCaptureStream_SkippedMemberIsDrained(t *testing.T) {
	r := NewCaptureStreamReader(bytes.NewReader(writeMembers(t, map[string]string{"a": "alpha", "b": "beta"}, nil, []string{"a", "b"})))
	if _, _, _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	name, _, body, err := r.Next()
	if err != nil || name != "b" {
		t.Fatalf("Next = %q, %v", name, err)
	}
	if got, _ := io.ReadAll(body); string(got) != "beta" {
		t.Errorf("b = %q", got)
	}
}

// TestCaptureStream_TruncationIsAnError: a stream cut anywhere inside a member
// is never read as a shorter member.
func TestCaptureStream_TruncationIsAnError(t *testing.T) {
	full := writeMembers(t, map[string]string{"a": "alpha-bytes"}, nil, []string{"a"})
	for cut := 1; cut < len(full); cut++ {
		r := NewCaptureStreamReader(bytes.NewReader(full[:cut]))
		name, _, body, err := r.Next()
		if err != nil {
			continue
		}
		if _, err := io.ReadAll(body); err == nil {
			t.Fatalf("a stream cut at %d of %d read cleanly as member %q", cut, len(full), name)
		}
	}
}

func TestCaptureStream_HostileLengths(t *testing.T) {
	// A chunk header larger than any writer emits.
	huge := []byte{0, 1, 'a', 0, 0, 0x7f, 0xff, 0xff, 0xff}
	_, _, body, err := NewCaptureStreamReader(bytes.NewReader(huge)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Error("accepted a chunk larger than the chunk bound")
	}
	// A zero-length name.
	if _, _, _, err := NewCaptureStreamReader(bytes.NewReader([]byte{0, 0})).Next(); err == nil {
		t.Error("accepted an empty member name")
	}
	// A mode with bits beyond the permission bits (setuid here).
	if _, _, _, err := NewCaptureStreamReader(bytes.NewReader([]byte{0, 1, 'a', 0x09, 0xed})).Next(); err == nil {
		t.Error("accepted a member mode that is not permission bits")
	}
	// The total bound.
	orig := CaptureStreamMaxBytes
	CaptureStreamMaxBytes = 64
	t.Cleanup(func() { CaptureStreamMaxBytes = orig })
	_, _, body, err = NewCaptureStreamReader(bytes.NewReader(writeMembers(t, map[string]string{"a": strings.Repeat("x", 1000)}, nil, []string{"a"}))).Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Error("read past the stream's total bound")
	}
}

func TestCaptureStreamWriter_RefusesBadUse(t *testing.T) {
	w := NewCaptureStreamWriter(io.Discard)
	if _, err := w.Member("", 0o600); err == nil {
		t.Error("accepted an empty name")
	}
	if _, err := w.Member(strings.Repeat("n", CaptureMemberNameMax+1), 0o600); err == nil {
		t.Error("accepted an over-long name")
	}
	if _, err := w.Member("a", os.ModeSetuid|0o755); err == nil {
		t.Error("accepted a mode that is not permission bits")
	}
	if _, err := w.Member("a", 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Member("b", 0o600); err == nil {
		t.Error("opened a second member before closing the first")
	}
}
