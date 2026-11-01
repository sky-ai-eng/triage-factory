package gitproxy_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
)

const zeroOID = "0000000000000000000000000000000000000000"

// pkt frames s as a git pkt-line (4-hex-digit length, the 4 bytes included).
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushResult is what runReceivePack observed: the client-facing status and
// body, the exact body the upstream received (to prove the tee never alters
// it), the Authorization and Accept-Encoding the upstream saw (to prove the
// backstop doesn't break credential injection, and asks for a readable
// response), and the refs handed to RecordPush.
type pushResult struct {
	clientStatus           int
	clientBody             string
	upstreamBody           string
	upstreamAuth           string
	upstreamAcceptEncoding string
	pushes                 []gitproxy.PushedRef
}

// pushRig is the upstream a push is run against, and which receive-pack path
// the proxy takes to it.
type pushRig struct {
	// status and response are what the fake upstream answers, with
	// contentEncoding as its Content-Encoding when set.
	status          int
	response        string
	contentEncoding string
	// allowedRefs, when non-nil, wires an Authorize gate allowing them, so the
	// push takes the enforcing path rather than the observe one.
	allowedRefs []string
}

// runReceivePack boots a proxy in front of a fake upstream that returns
// upstreamStatus with a bare flush-pkt, sends one request, then Shuts the
// proxy down. Shutdown waits for the in-flight handler — including the
// synchronous, post-response recordReceivePack — so the returned pushes are
// deterministic, not timing-dependent.
func runReceivePack(t *testing.T, upstreamStatus int, method, path, body string) pushResult {
	t.Helper()
	return runPush(t, pushRig{status: upstreamStatus, response: "0000"}, method, path, body)
}

// runPush is runReceivePack against the upstream rig describes.
func runPush(t *testing.T, rig pushRig, method, path, body string) pushResult {
	t.Helper()

	var (
		mu         sync.Mutex
		gotBody    []byte
		gotAuth    string
		gotEncoded string
		upstream   = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody = b
			gotAuth = r.Header.Get("Authorization")
			gotEncoded = r.Header.Get("Accept-Encoding")
			mu.Unlock()
			if rig.contentEncoding != "" {
				w.Header().Set("Content-Encoding", rig.contentEncoding)
			}
			w.WriteHeader(rig.status)
			_, _ = w.Write([]byte(rig.response))
		}))
	)
	t.Cleanup(upstream.Close)

	var res pushResult
	cfg := gitproxy.Config{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_tok"}, nil
		},
		Upstream: upstream.URL,
		RecordPush: func(_ context.Context, p gitproxy.PushedRef) {
			mu.Lock()
			res.pushes = append(res.pushes, p)
			mu.Unlock()
		},
	}
	if rig.allowedRefs != nil {
		cfg.Authorize = func(context.Context, string, string) (gitproxy.Decision, error) {
			return gitproxy.Decision{Allowed: true, AllowedRefs: rig.allowedRefs}, nil
		}
	}
	srv, err := gitproxy.New(cfg)
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}

	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	// What git's curl client advertises.
	req.Header.Set("Accept-Encoding", "deflate, gzip")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	clientBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	res.clientStatus = resp.StatusCode
	res.clientBody = string(clientBody)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	mu.Lock()
	res.upstreamBody = string(gotBody)
	res.upstreamAuth = gotAuth
	res.upstreamAcceptEncoding = gotEncoded
	mu.Unlock()
	return res
}

func wantBasicXAccessToken(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// TestReceivePackBackstop_RecordsNonDeleteRefs is the core assertion: a push
// transiting the proxy reports a push per non-delete ref (a delete
// is dropped; a tag is forwarded — the branch filter is downstream), and the
// upstream still receives the body byte-for-byte with the injected credential.
func TestReceivePackBackstop_RecordsNonDeleteRefs(t *testing.T) {
	body := pkt(zeroOID+" aaaa1111 refs/heads/main\x00report-status side-band-64k\n") +
		pkt("bbbb2222 cccc3333 refs/heads/feature/x\n") +
		pkt("dddd4444 "+zeroOID+" refs/heads/stale\n") + // delete -> not recorded
		pkt("eeee5555 ffff6666 refs/tags/v1.0\n") + // tag -> forwarded
		"0000" +
		"PACK\x00\x00\x00\x02binary\x00pack\x00data"

	res := runReceivePack(t, http.StatusOK, http.MethodPost, "/octo/repo/git-receive-pack", body)

	if res.clientStatus != http.StatusOK {
		t.Errorf("client status = %d, want 200", res.clientStatus)
	}
	if res.upstreamBody != body {
		t.Errorf("upstream body was altered by the tee:\n got %q\nwant %q", res.upstreamBody, body)
	}
	if want := wantBasicXAccessToken("ghs_tok"); res.upstreamAuth != want {
		t.Errorf("upstream auth = %q, want %q (backstop must not break credential injection)", res.upstreamAuth, want)
	}

	want := []gitproxy.PushedRef{
		{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaaa1111", Created: true, Status: http.StatusOK},
		{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "cccc3333", Created: false, Status: http.StatusOK},
		{Repo: "octo/repo", Ref: "refs/tags/v1.0", NewSHA: "ffff6666", Created: false, Status: http.StatusOK},
	}
	if !equalPushes(res.pushes, want) {
		t.Errorf("recorded pushes =\n  %+v\nwant\n  %+v", res.pushes, want)
	}
}

// TestReceivePackBackstop_ReportsRefusedPushWithStatus proves a push the
// upstream refused (here 403 — nothing landed) is still reported to
// RecordPush, carrying the failure status (Succeeded()==false) so the wiring
// records an audit failure row and skips the deliverable. The body still reaches
// the upstream untouched.
func TestReceivePackBackstop_ReportsRefusedPushWithStatus(t *testing.T) {
	body := pkt(zeroOID+" aaaa refs/heads/main\n") + "0000" + "PACKDATA"
	res := runReceivePack(t, http.StatusForbidden, http.MethodPost, "/octo/repo/git-receive-pack", body)

	if res.clientStatus != http.StatusForbidden {
		t.Errorf("client status = %d, want 403", res.clientStatus)
	}
	if res.upstreamBody != body {
		t.Errorf("upstream body altered: got %q want %q", res.upstreamBody, body)
	}
	want := []gitproxy.PushedRef{{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaaa", Created: true, Status: http.StatusForbidden}}
	if !equalPushes(res.pushes, want) {
		t.Errorf("recorded pushes = %+v, want %+v (a refused push is audit material, never dropped)", res.pushes, want)
	}
	if len(res.pushes) == 1 && res.pushes[0].Succeeded() {
		t.Error("Succeeded() = true for a 403, want false")
	}
}

// TestReceivePackBackstop_IgnoresNonPush proves the GET ref advertisement (and
// anything that isn't the receive-pack POST) is proxied untouched and records
// nothing.
func TestReceivePackBackstop_IgnoresNonPush(t *testing.T) {
	res := runReceivePack(t, http.StatusOK, http.MethodGet, "/octo/repo/info/refs?service=git-receive-pack", "")
	if res.clientStatus != http.StatusOK {
		t.Errorf("client status = %d, want 200", res.clientStatus)
	}
	if len(res.pushes) != 0 {
		t.Errorf("recorded %d pushes for a GET advertisement, want 0", len(res.pushes))
	}
}

// TestReceivePackBackstop_LargePackForwardedIntact proves the body-capture cap
// (which bounds only how much is buffered for parsing) never truncates the
// stream sent upstream: a multi-hundred-KiB packfile arrives whole, and the
// command list before it is still parsed.
func TestReceivePackBackstop_LargePackForwardedIntact(t *testing.T) {
	pack := "PACK" + strings.Repeat("\xab\xcd\xef\x01", 80*1024) // ~320 KiB, past the 64 KiB cap
	body := pkt(zeroOID+" aaaabbbb refs/heads/big\n") + "0000" + pack

	res := runReceivePack(t, http.StatusOK, http.MethodPost, "/octo/repo.git/git-receive-pack", body)

	if res.upstreamBody != body {
		t.Errorf("large body truncated/altered by the tee: got %d bytes, want %d", len(res.upstreamBody), len(body))
	}
	want := []gitproxy.PushedRef{{Repo: "octo/repo", Ref: "refs/heads/big", NewSHA: "aaaabbbb", Created: true, Status: http.StatusOK}}
	if !equalPushes(res.pushes, want) {
		t.Errorf("recorded pushes = %+v, want %+v", res.pushes, want)
	}
}

// TestReceivePackBackstop_InterimResponseStillRecords drives a real push through
// the proxy against an upstream that sends a 1xx interim response (103 Early
// Hints) before its final 200. httputil.ReverseProxy forwards that 1xx by
// calling WriteHeader(103) on the wrapped responseRecorder (verified on Go 1.26),
// so without the recorder's "first FINAL status" guard the 103 would latch the
// gate and the branch would be silently dropped on a successful push. This is
// the full-path companion to the responseRecorder unit test.
func TestReceivePackBackstop_InterimResponseStillRecords(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusEarlyHints) // 103 interim — forwarded before the final status
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0000"))
	}))
	defer upstream.Close()

	var mu sync.Mutex
	var pushes []gitproxy.PushedRef
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs"}, nil
		},
		Upstream: upstream.URL,
		RecordPush: func(_ context.Context, p gitproxy.PushedRef) {
			mu.Lock()
			pushes = append(pushes, p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}

	body := pkt(zeroOID+" aaaa refs/heads/main\n") + "0000" + "PACKDATA"
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/octo/repo/git-receive-pack", strings.NewReader(body))
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("client status = %d, want 200", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []gitproxy.PushedRef{{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaaa", Created: true, Status: http.StatusOK}}
	if !equalPushes(pushes, want) {
		t.Errorf("recorded pushes = %+v, want %+v (a 1xx interim must not latch the recorded status)", pushes, want)
	}
}

// TestReceivePackBackstop_NilRecordPushDisablesBackstop proves a proxy with no
// RecordPush wired (local mode / no stores) leaves the push completely
// untouched — the Handler guard skips serveReceivePack, so the body still
// reaches the upstream and nothing is recorded.
func TestReceivePackBackstop_NilRecordPushDisablesBackstop(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0000"))
	}))
	defer upstream.Close()

	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs"}, nil
		},
		Upstream: upstream.URL,
		// RecordPush deliberately nil.
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	body := pkt(zeroOID+" aaaa refs/heads/main\n") + "0000" + "PACKDATA"
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/octo/repo/git-receive-pack", strings.NewReader(body))
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("client status = %d, want 200", resp.StatusCode)
	}
	if string(gotBody) != body {
		t.Errorf("upstream body = %q, want the push forwarded untouched %q", gotBody, body)
	}
}

func equalPushes(a, b []gitproxy.PushedRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// band frames s as one side-band pkt-line on band n.
func band(n byte, s string) string { return pkt(string([]byte{n}) + s) }

// TestReceivePackBackstop_ReadsRefusalInsideA200 is the case the status code
// cannot tell apart: GitHub accepts one ref, refuses the other under a
// repository ruleset, and answers the whole POST with a 200. RecordPush must
// see one accepted ref and one rejected ref carrying GitHub's reason and its
// rule-violation text, on the enforcing path and the observe path alike — and
// the client must receive GitHub's response untouched.
func TestReceivePackBackstop_ReadsRefusalInsideA200(t *testing.T) {
	const (
		reason = "push declined due to repository rule violations"
		gh013  = "error: GH013: Repository rule violations found for refs/heads/blocked.\n" +
			"- Commits must have verified signatures.\n"
	)
	response := band(2, "Resolving deltas: 100% (1/1)\rResolving deltas: 100% (2/2), done.\n") +
		band(2, gh013) +
		band(1, pkt("unpack ok\n")+pkt("ok refs/heads/ok\n")+pkt("ng refs/heads/blocked "+reason+"\n")+"0000") +
		"0000"
	body := pkt(zeroOID+" aaaa1111 refs/heads/ok\x00 report-status side-band-64k agent=git/2.43.0") +
		pkt(zeroOID+" bbbb2222 refs/heads/blocked") +
		"0000" + "PACKDATA"

	for _, path := range []struct {
		name        string
		allowedRefs []string
	}{
		{"gated", []string{"refs/heads/ok", "refs/heads/blocked"}},
		{"observe", nil},
	} {
		t.Run(path.name, func(t *testing.T) {
			res := runPush(t, pushRig{status: http.StatusOK, response: response, allowedRefs: path.allowedRefs},
				http.MethodPost, "/octo/repo/git-receive-pack", body)

			if res.clientStatus != http.StatusOK || res.clientBody != response {
				t.Errorf("client got %d %q, want 200 and the upstream's response unchanged", res.clientStatus, res.clientBody)
			}
			if res.upstreamBody != body {
				t.Errorf("upstream body altered: got %q want %q", res.upstreamBody, body)
			}
			if res.upstreamAcceptEncoding != "identity" {
				t.Errorf("upstream Accept-Encoding = %q, want identity (the report must arrive readable)", res.upstreamAcceptEncoding)
			}
			want := []gitproxy.PushedRef{
				{Repo: "octo/repo", Ref: "refs/heads/ok", NewSHA: "aaaa1111", Created: true, Status: http.StatusOK,
					RemoteMessage: strings.TrimSpace(gh013)},
				{Repo: "octo/repo", Ref: "refs/heads/blocked", NewSHA: "bbbb2222", Created: true, Status: http.StatusOK,
					Rejected: true, Reason: reason, RemoteMessage: strings.TrimSpace(gh013)},
			}
			if !equalPushes(res.pushes, want) {
				t.Fatalf("recorded pushes =\n  %+v\nwant\n  %+v", res.pushes, want)
			}
			if !res.pushes[0].Succeeded() || res.pushes[1].Succeeded() {
				t.Errorf("Succeeded = (%v, %v), want (true, false)", res.pushes[0].Succeeded(), res.pushes[1].Succeeded())
			}
		})
	}
}

// TestReceivePackBackstop_IdentityOnlyForPushes pins that only the push POST
// has its Accept-Encoding rewritten: a fetch keeps what the client asked for.
func TestReceivePackBackstop_IdentityOnlyForPushes(t *testing.T) {
	res := runReceivePack(t, http.StatusOK, http.MethodPost, "/octo/repo/git-upload-pack", "0000")
	if res.upstreamAcceptEncoding != "deflate, gzip" {
		t.Errorf("upload-pack Accept-Encoding = %q, want the client's own", res.upstreamAcceptEncoding)
	}
}

// TestReceivePackBackstop_FatalErrorInPlaceOfAReport is a 200 whose body is a
// band-3 fatal error and no report-status. git reports that push to the agent
// as failed, and nothing in the response says a ref landed, so every ref must
// record refused, with the error as its remote message, on both paths.
func TestReceivePackBackstop_FatalErrorInPlaceOfAReport(t *testing.T) {
	response := band(3, "fatal: internal server error\n") + "0000"
	body := pkt(zeroOID+" aaaa1111 refs/heads/feature\x00 report-status side-band-64k agent=git/2.43.0") +
		"0000" + "PACKDATA"

	for _, path := range []struct {
		name        string
		allowedRefs []string
	}{
		{"gated", []string{"refs/heads/feature"}},
		{"observe", nil},
	} {
		t.Run(path.name, func(t *testing.T) {
			res := runPush(t, pushRig{status: http.StatusOK, response: response, allowedRefs: path.allowedRefs},
				http.MethodPost, "/octo/repo/git-receive-pack", body)

			if res.clientBody != response {
				t.Errorf("client body = %q, want the upstream's response unchanged", res.clientBody)
			}
			want := []gitproxy.PushedRef{{
				Repo: "octo/repo", Ref: "refs/heads/feature", NewSHA: "aaaa1111", Created: true, Status: http.StatusOK,
				Rejected: true, Reason: "remote fatal error", RemoteMessage: "fatal: internal server error",
			}}
			if !equalPushes(res.pushes, want) {
				t.Fatalf("recorded pushes =\n  %+v\nwant\n  %+v", res.pushes, want)
			}
		})
	}
}

// TestReceivePackBackstop_CompressedResponseFallsBack covers an upstream that
// compresses the response despite the identity request, as a front end in
// the path might. The body cannot be read as a report, so the push keeps the
// status rule — the ref the compressed report refuses still records as
// accepted — and the WARN names the compression rather than a parse failure.
// The client gets the compressed response exactly as sent.
func TestReceivePackBackstop_CompressedResponseFallsBack(t *testing.T) {
	var buf bytes.Buffer
	defer logging.SetOutput(&buf)()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(band(1, pkt("unpack ok\n")+pkt("ng refs/heads/feature push declined due to repository rule violations\n")+"0000") + "0000"))
	_ = zw.Close()
	body := pkt(zeroOID+" aaaa1111 refs/heads/feature\x00 report-status side-band-64k") + "0000" + "PACKDATA"

	res := runPush(t, pushRig{status: http.StatusOK, response: gz.String(), contentEncoding: "gzip"},
		http.MethodPost, "/octo/repo/git-receive-pack", body)

	if res.clientBody != gz.String() {
		t.Errorf("client body altered: got %d bytes, want the %d compressed bytes sent", len(res.clientBody), gz.Len())
	}
	want := []gitproxy.PushedRef{{Repo: "octo/repo", Ref: "refs/heads/feature", NewSHA: "aaaa1111", Created: true, Status: http.StatusOK}}
	if !equalPushes(res.pushes, want) {
		t.Errorf("recorded pushes = %+v, want %+v (an unreadable 2xx keeps the status rule)", res.pushes, want)
	}
	if got := buf.String(); !strings.Contains(got, "no readable report-status") || !strings.Contains(got, "response compressed (gzip)") {
		t.Errorf("log does not name the compression as the cause; got:\n%s", got)
	}
}

// TestReceivePackBackstop_RefusedStatusDoesNotWarn pins that a non-2xx push
// logs nothing: its status is the whole answer, and the WARN is for the 2xx
// whose report could not be read, where recording it accepted may be wrong.
func TestReceivePackBackstop_RefusedStatusDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	defer logging.SetOutput(&buf)()

	body := pkt(zeroOID+" aaaa refs/heads/main\x00 report-status side-band-64k") + "0000" + "PACKDATA"
	res := runReceivePack(t, http.StatusForbidden, http.MethodPost, "/octo/repo/git-receive-pack", body)

	if len(res.pushes) != 1 || res.pushes[0].Succeeded() {
		t.Fatalf("recorded pushes = %+v, want one refused ref", res.pushes)
	}
	if got := buf.String(); strings.Contains(got, "report-status") {
		t.Errorf("a 403 push logged a report-status WARN:\n%s", got)
	}
}
