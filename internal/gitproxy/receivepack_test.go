package gitproxy

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
	}, pushResponse{status: 200})

	if calls != 1 {
		t.Fatalf("RecordPush called %d times, want 1 (loop must stop once ctx is done)", calls)
	}
}

// TestResponseRecorder_CapturesFirstFinalStatus pins that the 2xx gate keys off
// the upstream's first FINAL status, not a 1xx interim one. ReverseProxy
// forwards a backend 100 Continue (which git elicits via Expect: 100-continue)
// by calling WriteHeader(100) on this writer; latching that would wrongly drop
// the record on a successful push.
func TestResponseRecorder_CapturesFirstFinalStatus(t *testing.T) {
	t.Run("1xx interim does not latch the gate", func(t *testing.T) {
		sr := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
		sr.WriteHeader(http.StatusContinue) // 100 — forwarded interim, must be ignored
		sr.WriteHeader(http.StatusOK)       // 200 — the real final status
		if sr.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a 1xx interim response must not latch the 2xx gate)", sr.status)
		}
	})
	t.Run("first final status wins", func(t *testing.T) {
		sr := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
		sr.WriteHeader(http.StatusForbidden)
		sr.WriteHeader(http.StatusInternalServerError)
		if sr.status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (first final status wins)", sr.status)
		}
	})
	t.Run("implicit 200 via Write", func(t *testing.T) {
		sr := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
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

// band frames s as one side-band pkt-line on band n.
func band(n byte, s string) string { return pktLine(string([]byte{n}) + s) }

// band1 frames data as band-1 pkt-lines, split to fit the pkt-line length
// field the way receive-pack splits a long report.
func band1(data string) string {
	const maxChunk = 65515
	var b strings.Builder
	for len(data) > maxChunk {
		b.WriteString(band(1, data[:maxChunk]))
		data = data[maxChunk:]
	}
	b.WriteString(band(1, data))
	return b.String()
}

// ghProgress, ghGH013 and ghReport are a push GitHub refuses one ref of inside
// an HTTP 200: band-2 progress meters and GH013 rule-violation text, then the
// band-1 report.
const (
	ghProgress = "Resolving deltas:   0% (0/2)\rResolving deltas: 100% (2/2), done.\n"
	ghGH013    = "error: GH013: Repository rule violations found for refs/heads/blocked.\n" +
		"Review all repository rules at https://github.com/octo/repo/rules?ref=refs%2Fheads%2Fblocked\n" +
		"\n" +
		"- Commits must have verified signatures.\n" +
		"  Found 1 violation:\n" +
		"\n" +
		"  aaaa1111\n"
	ghReason = "push declined due to repository rule violations"
)

var ghReport = pktLine("unpack ok\n") +
	pktLine("ok refs/heads/ok\n") +
	pktLine("ng refs/heads/blocked "+ghReason+"\n") +
	"0000"

// wantGH013 is ghGH013 as the remote_message rule renders it: trimmed, the
// progress meter dropped.
var wantGH013 = strings.TrimSpace(ghGH013)

// refWant is one ref's expected outcome under a parsed report.
type refWant struct {
	rejected bool
	reason   string
}

// TestReportCapture is the parser table: a receive-pack response, the
// capabilities the push asked for, and the per-ref outcome and remote message
// read from it. Every case is fed whole, a byte at a time, and in odd-sized
// chunks, so the result never depends on where the writes split it.
func TestReportCapture(t *testing.T) {
	sb := " report-status side-band-64k agent=git/2.43.0"
	overReport := pktLine("unpack ok\n")
	for i := 0; len(overReport) <= maxReportCapture; i++ {
		overReport += pktLine(fmt.Sprintf("ok refs/heads/branch-%05d\n", i))
	}
	overReport += "0000"

	cases := []struct {
		name     string
		caps     string
		body     string
		noReport bool
		refs     map[string]refWant
		message  string
	}{
		{
			name: "side-band-64k: one ok, one ng, band-2 GH013 text",
			caps: sb,
			body: band(2, ghProgress) + band(2, ghGH013) + band1(ghReport) + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: ghReason},
			},
			message: wantGH013,
		},
		{
			name: "side-band (not 64k) reads the same",
			caps: " report-status side-band",
			body: band1(ghReport) + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: ghReason},
			},
		},
		{
			name: "band 1 split across pkt-lines mid-line",
			caps: sb,
			body: band(1, ghReport[:9]) + band(1, ghReport[9:30]) + band(1, ghReport[30:]) + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: ghReason},
			},
		},
		{
			name: "no side-band: the body is the report",
			caps: " report-status",
			body: ghReport,
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: ghReason},
			},
		},
		{
			name: "report-status-v2 option lines after an ok are ignored",
			caps: " report-status-v2 side-band-64k",
			body: band1(pktLine("unpack ok\n")+
				pktLine("ok refs/heads/ok\n")+
				pktLine("option refname refs/heads/ok\n")+
				pktLine("option old-oid "+zeroOID+"\n")+
				pktLine("option new-oid aaaa1111\n")+
				pktLine("option forced-update\n")+
				pktLine("ng refs/heads/blocked "+ghReason+"\n")+
				"0000") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: ghReason},
			},
		},
		{
			name: "unpack failure refuses every ref with its message",
			caps: sb,
			body: band1(pktLine("unpack index-pack abnormal exit\n")+
				pktLine("ng refs/heads/ok unpacker error\n")+
				pktLine("ng refs/heads/blocked unpacker error\n")+
				"0000") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {rejected: true, reason: "unpack index-pack abnormal exit"},
				"refs/heads/blocked": {rejected: true, reason: "unpack index-pack abnormal exit"},
			},
		},
		{
			name: "an over-long reason is cut",
			caps: sb,
			body: band1(pktLine("unpack ok\n")+pktLine("ng refs/heads/blocked "+strings.Repeat("r", 5000)+"\n")+"0000") + "0000",
			refs: map[string]refWant{
				"refs/heads/blocked": {rejected: true, reason: strings.Repeat("r", maxReasonLen)},
			},
		},
		{
			name: "an over-long unpack failure is cut",
			caps: sb,
			body: band1(pktLine("unpack "+strings.Repeat("u", 5000)+"\n")+"0000") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok": {rejected: true, reason: "unpack " + strings.Repeat("u", maxReasonLen-len("unpack "))},
			},
		},
		{
			name: "a ref the report leaves out is refused",
			caps: sb,
			body: band1(pktLine("unpack ok\n")+pktLine("ok refs/heads/ok\n")+"0000") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: "not in report-status"},
			},
		},
		{
			name: "band-3 fatal error in place of a report refuses every ref",
			caps: sb,
			body: band(2, "Resolving deltas: 100% (2/2), done.\n") + band(3, "fatal: internal server error\n") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {rejected: true, reason: "remote fatal error"},
				"refs/heads/blocked": {rejected: true, reason: "remote fatal error"},
			},
			message: "Resolving deltas: 100% (2/2), done.\nfatal: internal server error",
		},
		{
			name: "band-3 fatal error after a truncated report still refuses",
			caps: sb,
			body: band(1, ghReport[:20]) + band(3, "fatal: internal server error\n"),
			refs: map[string]refWant{
				"refs/heads/ok":      {rejected: true, reason: "remote fatal error"},
				"refs/heads/blocked": {rejected: true, reason: "remote fatal error"},
			},
			message: "fatal: internal server error",
		},
		{
			name: "band-3 fatal error refuses even when report-status was not requested",
			caps: " side-band-64k",
			body: band(3, "fatal: internal server error\n") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok": {rejected: true, reason: "remote fatal error"},
			},
			message: "fatal: internal server error",
		},
		{
			name: "a whole report beside a band-3 message is still what happened",
			caps: sb,
			body: band1(ghReport) + band(3, "fatal: error after the update\n") + "0000",
			refs: map[string]refWant{
				"refs/heads/ok":      {},
				"refs/heads/blocked": {rejected: true, reason: ghReason},
			},
			message: "fatal: error after the update",
		},
		{
			name:     "a band-3 byte means nothing without side-band",
			caps:     " report-status",
			body:     band(3, "fatal: internal server error\n") + "0000",
			noReport: true,
		},
		{
			name:     "truncated mid-report",
			caps:     sb,
			body:     band1(ghReport[:len(ghReport)-12]),
			noReport: true,
		},
		{
			name:     "truncated report without side-band",
			caps:     " report-status",
			body:     ghReport[:len(ghReport)-4],
			noReport: true,
		},
		{
			name:     "garbage body",
			caps:     sb,
			body:     "<html>not git</html>",
			noReport: true,
		},
		{
			name:     "report that does not open with unpack",
			caps:     " report-status",
			body:     pktLine("ok refs/heads/ok\n") + "0000",
			noReport: true,
		},
		{
			name:     "report-status not requested",
			caps:     " side-band-64k",
			body:     band1(ghReport) + "0000",
			noReport: true,
		},
		{
			name:     "no capabilities at all",
			caps:     "",
			body:     ghReport,
			noReport: true,
		},
		{
			name:     "side-band report over the 64 KiB bound",
			caps:     sb,
			body:     band1(overReport) + "0000",
			noReport: true,
		},
		{
			name:     "plain report over the 64 KiB bound",
			caps:     " report-status",
			body:     overReport,
			noReport: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := zeroOID + " aaaa1111 refs/heads/ok"
			if c.caps != "" {
				first += "\x00" + c.caps
			}
			block := []byte(pktLine(first) + pktLine(zeroOID+" bbbb2222 refs/heads/blocked") + "0000")
			caps := parsePushCapabilities(block)

			for _, chunk := range []int{len(c.body), 1, 7, 4093} {
				var capture reportCapture
				for b := []byte(c.body); len(b) > 0; {
					n := min(chunk, len(b))
					capture.feed(b[:n])
					b = b[n:]
				}
				report, message, cause := capture.result(caps)
				if message != c.message {
					t.Errorf("chunk %d: remote message = %q, want %q", chunk, message, c.message)
				}
				if c.noReport {
					if report != nil {
						t.Errorf("chunk %d: read a report %+v, want none", chunk, report)
					}
					if cause == "" {
						t.Errorf("chunk %d: no report and no cause", chunk)
					}
					continue
				}
				if report == nil {
					t.Fatalf("chunk %d: no report read (%s)", chunk, cause)
				}
				for ref, want := range c.refs {
					rejected, reason := report.refOutcome(ref)
					if rejected != want.rejected || reason != want.reason {
						t.Errorf("chunk %d: %s = (rejected %v, %q), want (%v, %q)", chunk, ref, rejected, reason, want.rejected, want.reason)
					}
				}
			}
		})
	}
}

func TestParsePushCapabilities(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  pushCapabilities
	}{
		{"both", pktLine(zeroOID+" aaaa refs/heads/x\x00 report-status side-band-64k agent=git/2.43.0") + "0000", pushCapabilities{sideBand: true, reportStatus: true}},
		{"v2 report, old side-band", pktLine(zeroOID+" aaaa refs/heads/x\x00report-status-v2 side-band\n") + "0000", pushCapabilities{sideBand: true, reportStatus: true}},
		{"report only", pktLine(zeroOID+" aaaa refs/heads/x\x00report-status quiet") + "0000", pushCapabilities{reportStatus: true}},
		{"none", pktLine(zeroOID+" aaaa refs/heads/x") + "0000", pushCapabilities{}},
		{"capabilities on a later line are not the first command's", pktLine(zeroOID+" aaaa refs/heads/x") + pktLine("bbbb cccc refs/heads/y") + "0000", pushCapabilities{}},
		{"shallow lines precede the first command", pktLine("shallow dddd") + pktLine(zeroOID+" aaaa refs/heads/x\x00 report-status side-band-64k") + "0000", pushCapabilities{sideBand: true, reportStatus: true}},
		{"a capability name inside another is not it", pktLine(zeroOID+" aaaa refs/heads/x\x00 report-statusx side-band-128k") + "0000", pushCapabilities{}},
	}
	for _, c := range cases {
		if got := parsePushCapabilities([]byte(c.block)); got != c.want {
			t.Errorf("%s: parsePushCapabilities = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// remoteMessageRule is the remote_message rule written the obvious way, over
// the whole text at once: split on newlines, drop any line holding a carriage
// return, keep the last 20, cut from the front to 1 KiB (at a rune boundary),
// trim. messageTail keeps only bounded state as the text streams; it must
// agree with this.
func remoteMessageRule(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "\r") {
			kept = append(kept, line)
		}
	}
	if len(kept) > remoteMessageLines {
		kept = kept[len(kept)-remoteMessageLines:]
	}
	s := strings.Join(kept, "\n")
	if len(s) > remoteMessageMax {
		s = s[len(s)-remoteMessageMax:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return strings.TrimSpace(s)
}

func TestMessageTail(t *testing.T) {
	var many []string
	for i := 1; i <= 30; i++ {
		many = append(many, fmt.Sprintf("line %02d", i))
	}
	cases := []struct {
		name string
		text string
		want string
	}{
		{"progress meters dropped", "Counting: 50%\rCounting: 100%, done.\nkept\n", "kept"},
		{"last 20 lines kept", strings.Join(many, "\n") + "\n", strings.Join(many[11:], "\n")},
		{"cut from the front to 1 KiB", strings.Repeat("x", 3000) + "END", strings.Repeat("x", remoteMessageMax-3) + "END"},
		{"trimmed", "\n\n   message  \n\n", "message"},
		// 2001 bytes: the 1 KiB cut lands on the second byte of an "é".
		{"a cut never splits a rune", strings.Repeat("é", 1000) + "a", strings.Repeat("é", remoteMessageMax/2-1) + "a"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		var m messageTail
		m.write([]byte(c.text))
		if got := m.String(); got != c.want {
			t.Errorf("%s: message = %q, want %q", c.name, got, c.want)
		}
		if ref := remoteMessageRule(c.text); ref != c.want {
			t.Errorf("%s: the rule itself gives %q, want %q (the case is wrong)", c.name, ref, c.want)
		}
	}
}

// TestMessageTail_MatchesTheRule feeds generated text — long lines, carriage
// returns, blank lines, multi-byte runes — through messageTail in random
// chunks, and checks the result against the rule applied to the whole text.
func TestMessageTail_MatchesTheRule(t *testing.T) {
	rng := rand.New(rand.NewPCG(1188, 1))
	pieces := []string{"a", "word ", "é", "\r", strings.Repeat("y", 700), strings.Repeat("é", 300), "GH013 ", "  ", "\n", "\n"}
	for i := 0; i < 1000; i++ {
		// Half the texts are one line, which is where a single over-long line
		// is the whole message.
		from := pieces
		if i%2 == 0 {
			from = pieces[:len(pieces)-2]
		}
		var b strings.Builder
		for n := rng.IntN(200); n > 0; n-- {
			b.WriteString(from[rng.IntN(len(from))])
		}
		text := b.String()

		var m messageTail
		for rest := []byte(text); len(rest) > 0; {
			n := min(1+rng.IntN(64), len(rest))
			m.write(rest[:n])
			rest = rest[n:]
		}
		if got, want := m.String(), remoteMessageRule(text); got != want {
			t.Fatalf("text %q:\n got %q\nwant %q", text, got, want)
		}
	}
}

func TestBoundReason(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short is kept", "hook declined", "hook declined"},
		{"exactly the bound is kept", strings.Repeat("a", maxReasonLen), strings.Repeat("a", maxReasonLen)},
		{"long keeps its start", strings.Repeat("a", maxReasonLen) + "tail", strings.Repeat("a", maxReasonLen)},
		// 255 ASCII bytes then "é": the bound falls inside the rune, so all
		// of it goes.
		{"a cut never splits a rune", strings.Repeat("a", maxReasonLen-1) + "éé", strings.Repeat("a", maxReasonLen-1)},
	}
	for _, c := range cases {
		got := boundReason(c.in)
		if got != c.want {
			t.Errorf("%s: boundReason = %q, want %q", c.name, got, c.want)
		}
		if !utf8.ValidString(got) || len(got) > maxReasonLen {
			t.Errorf("%s: boundReason = %d bytes, valid UTF-8 %v", c.name, len(got), utf8.ValidString(got))
		}
	}
}
