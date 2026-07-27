package gitproxy

import (
	"context"
	"fmt"
	"net/http"
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

// TestDispatchPushes_StopsWhenContextDone proves the per-ref loop bails out the
// moment its bounded context is done, rather than calling RecordPush for every
// remaining ref (each of which would observe ctx.Err() and fail). Here the
// first callback cancels the context — standing in for an upsert that exhausts
// the window — so only that first ref is dispatched.
func TestDispatchPushes_StopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	s := &Server{cfg: Config{RecordPush: func(context.Context, PushedRef) {
		calls++
		cancel()
	}}}

	s.dispatchPushes(ctx, "octo/repo", []refUpdate{
		{ref: "refs/heads/a", newSHA: "1"},
		{ref: "refs/heads/b", newSHA: "2"},
		{ref: "refs/heads/c", newSHA: "3"},
	}, 200)

	if calls != 1 {
		t.Fatalf("RecordPush called %d times, want 1 (loop must stop once ctx is done)", calls)
	}
}

// TestStatusRecorder_CapturesFirstFinalStatus pins that the 2xx gate keys off
// the upstream's first FINAL status, not a 1xx interim one. ReverseProxy
// forwards a backend 100 Continue (which git elicits via Expect: 100-continue)
// by calling WriteHeader(100) on this writer; latching that would wrongly drop
// the record on a successful push.
func TestStatusRecorder_CapturesFirstFinalStatus(t *testing.T) {
	t.Run("1xx interim does not latch the gate", func(t *testing.T) {
		sr := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		sr.WriteHeader(http.StatusContinue) // 100 — forwarded interim, must be ignored
		sr.WriteHeader(http.StatusOK)       // 200 — the real final status
		if sr.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a 1xx interim response must not latch the 2xx gate)", sr.status)
		}
	})
	t.Run("first final status wins", func(t *testing.T) {
		sr := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		sr.WriteHeader(http.StatusForbidden)
		sr.WriteHeader(http.StatusInternalServerError)
		if sr.status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (first final status wins)", sr.status)
		}
	})
	t.Run("implicit 200 via Write", func(t *testing.T) {
		sr := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
		if _, err := sr.Write([]byte("ok")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if sr.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a bare Write implies 200)", sr.status)
		}
	})
}

// TestRefDenial covers the three situations a rejected ref can be in, which
// used to collapse to one flat "ref not allowed" string. The point of the
// split is the remedy: a protected ref means "open a PR (or ask an admin to
// change the policy)", an empty allowlist means "your worktree isn't recorded
// yet", and a foreign ref means "push from the checkout you have".
func TestRefDenial(t *testing.T) {
	cases := []struct {
		name          string
		ref           string
		allowedRefs   []string
		protectedRefs []string
		wantReason    string
		// wantContains are substrings the message must carry: what was refused,
		// why, and what to do about it.
		wantContains []string
	}{
		{
			name:          "protected ref names the branch, the reason and both remedies",
			ref:           "refs/heads/main",
			allowedRefs:   []string{"refs/heads/agent/x"},
			protectedRefs: []string{"refs/heads/main", "refs/heads/master"},
			wantReason:    "ref-protected",
			wantContains:  []string{"refs/heads/main", "protected branch", "acme/api", "pull request", "team admin"},
		},
		{
			name:         "no pushable branch points at the missing worktree",
			ref:          "refs/heads/agent/x",
			allowedRefs:  nil,
			wantReason:   "ref-no-pushable-branch",
			wantContains: []string{"acme/api", "workspace add"},
		},
		{
			name:         "foreign ref names the branch this run may actually push",
			ref:          "refs/heads/somebody-elses",
			allowedRefs:  []string{"refs/heads/agent/x"},
			wantReason:   "ref-not-allowed",
			wantContains: []string{"refs/heads/somebody-elses", "refs/heads/agent/x"},
		},
		{
			name:          "a protected ref wins over the empty-allowlist case",
			ref:           "refs/heads/main",
			allowedRefs:   nil,
			protectedRefs: []string{"refs/heads/main"},
			wantReason:    "ref-protected",
			wantContains:  []string{"protected branch"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, msg := refDenial(c.ref, "acme/api", c.allowedRefs, c.protectedRefs)
			if reason != c.wantReason {
				t.Errorf("reason = %q, want %q", reason, c.wantReason)
			}
			for _, want := range c.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not mention %q", msg, want)
				}
			}
		})
	}
}
