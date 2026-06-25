package gitproxy

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// pktLine frames s as a git pkt-line: a 4-hex-digit length (counting the 4
// length bytes) followed by the payload.
func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+pktLineLenWidth, s)
}

const zeroOID = "0000000000000000000000000000000000000000"

func TestParseReceivePackCommands(t *testing.T) {
	// A realistic receive-pack request: the first command carries the
	// capabilities after a NUL; a delete (new-sha all zeros) is dropped; a tag
	// is returned (the branch filter lives downstream in NewBranchArtifact);
	// then the flush-pkt and the packfile, which must not be parsed as commands.
	body := []byte(
		pktLine(zeroOID+" aaaa1111 refs/heads/main\x00report-status side-band-64k\n") +
			pktLine("bbbb2222 cccc3333 refs/heads/feature/x\n") +
			pktLine("dddd4444 "+zeroOID+" refs/heads/stale\n") + // delete -> skipped
			pktLine("eeee5555 ffff6666 refs/tags/v1.0\n") + // tag -> returned
			"0000" +
			"PACK\x00\x00\x00\x02\x91\x0achunk-of-binary-\x00\x00pack-data",
	)

	got := parseReceivePackCommands(body)
	want := []refUpdate{
		{ref: "refs/heads/main", newSHA: "aaaa1111", created: true},
		{ref: "refs/heads/feature/x", newSHA: "cccc3333", created: false},
		{ref: "refs/tags/v1.0", newSHA: "ffff6666", created: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseReceivePackCommands =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestParseReceivePackCommands_EmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"flush only", "0000"},
		{"too short for a length", "00"},
		{"non-hex length", "zzzzhello"},
		// A declared length longer than the remaining bytes — truncated framing.
		{"truncated line", "0099" + zeroOID + " aaaa refs/heads/main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseReceivePackCommands([]byte(c.body)); len(got) != 0 {
				t.Errorf("parseReceivePackCommands(%q) = %+v, want none", c.body, got)
			}
		})
	}
}

// TestParseReceivePackCommands_StopsAtFirstFlush proves the packfile after the
// flush-pkt is never mined for commands even if it happens to contain bytes
// that frame like a pkt-line.
func TestParseReceivePackCommands_StopsAtFirstFlush(t *testing.T) {
	body := []byte(
		pktLine(zeroOID+" aaaa refs/heads/main\n") +
			"0000" +
			// 0030... would parse as a 48-byte pkt-line if scanned, but it's pack data.
			"0030deadbeef cafebabe refs/heads/injected\n",
	)
	got := parseReceivePackCommands(body)
	if len(got) != 1 || got[0].ref != "refs/heads/main" {
		t.Fatalf("parseReceivePackCommands = %+v, want exactly refs/heads/main", got)
	}
}

func TestReceivePackRepoPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/octo/repo/git-receive-pack", "octo/repo"},
		{"/octo/repo.git/git-receive-pack", "octo/repo"},
		// Multi-segment paths pass through here; NewBranchArtifact rejects them.
		{"/scm/octo/repo/git-receive-pack", "scm/octo/repo"},
	}
	for _, c := range cases {
		if got := receivePackRepoPath(c.path); got != c.want {
			t.Errorf("receivePackRepoPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestIsZeroOID(t *testing.T) {
	cases := map[string]bool{
		zeroOID: true,
		"0000000000000000000000000000000000000000000000000000000000000000": true, // SHA-256 zero
		"":       true,
		"0000a":  false,
		"abc123": false,
	}
	for in, want := range cases {
		if got := isZeroOID(in); got != want {
			t.Errorf("isZeroOID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsReceivePackPush(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{"POST", "/octo/repo/git-receive-pack", true},
		{"POST", "/octo/repo.git/git-receive-pack", true},
		{"GET", "/octo/repo/git-receive-pack", false}, // not a POST
		{"POST", "/octo/repo/git-upload-pack", false}, // a fetch, not a push
		{"GET", "/octo/repo/info/refs", false},        // the advertisement
		{"POST", "/octo/repo/info/refs", false},       // not the push endpoint
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := isReceivePackPush(r); got != c.want {
			t.Errorf("isReceivePackPush(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestParseRefUpdate_TrailingAndCaps(t *testing.T) {
	// No trailing LF, capabilities present.
	u, ok := parseRefUpdate([]byte(zeroOID + " aaaa refs/heads/main\x00report-status"))
	if !ok || u.ref != "refs/heads/main" || u.newSHA != "aaaa" || !u.created {
		t.Fatalf("parseRefUpdate = %+v ok=%v, want main/aaaa/created", u, ok)
	}
	// Not three fields.
	if _, ok := parseRefUpdate([]byte("only two")); ok {
		t.Error("parseRefUpdate accepted a 2-field line")
	}
	if _, ok := parseRefUpdate([]byte(strings.Repeat(" ", 3))); ok {
		t.Error("parseRefUpdate accepted a blank line")
	}
}
