package gitproxy_test

import (
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

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

const zeroOID = "0000000000000000000000000000000000000000"

// pkt frames s as a git pkt-line (4-hex-digit length, the 4 bytes included).
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// pushResult is what runReceivePack observed: the client-facing status, the
// exact body the upstream received (to prove the tee never alters it), the
// Authorization the upstream saw (to prove the backstop doesn't break
// credential injection), and the refs handed to RecordPush.
type pushResult struct {
	clientStatus int
	upstreamBody string
	upstreamAuth string
	pushes       []gitproxy.PushedRef
}

// runReceivePack boots a proxy in front of a fake upstream that returns
// upstreamStatus, sends one request, then Shuts the proxy down. Shutdown waits
// for the in-flight handler — including the synchronous, post-response
// recordReceivePack — so the returned pushes are deterministic, not
// timing-dependent.
func runReceivePack(t *testing.T, upstreamStatus int, method, path, body string) pushResult {
	t.Helper()

	var (
		mu       sync.Mutex
		gotBody  []byte
		gotAuth  string
		upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody = b
			gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
			w.WriteHeader(upstreamStatus)
			_, _ = w.Write([]byte("0000")) // a minimal report-status-shaped body
		}))
	)
	t.Cleanup(upstream.Close)

	var res pushResult
	srv, err := gitproxy.New(gitproxy.Config{
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_tok"}, nil
		},
		Upstream: upstream.URL,
		RecordPush: func(_ context.Context, p gitproxy.PushedRef) {
			mu.Lock()
			res.pushes = append(res.pushes, p)
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

	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	res.clientStatus = resp.StatusCode

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	mu.Lock()
	res.upstreamBody = string(gotBody)
	res.upstreamAuth = gotAuth
	mu.Unlock()
	return res
}

func wantBasicXAccessToken(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// TestReceivePackBackstop_RecordsNonDeleteRefs is the core assertion: a push
// transiting the proxy records a branch artifact per non-delete ref (a delete
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
		{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaaa1111", Created: true},
		{Repo: "octo/repo", Ref: "refs/heads/feature/x", NewSHA: "cccc3333", Created: false},
		{Repo: "octo/repo", Ref: "refs/tags/v1.0", NewSHA: "ffff6666", Created: false},
	}
	if !equalPushes(res.pushes, want) {
		t.Errorf("recorded pushes =\n  %+v\nwant\n  %+v", res.pushes, want)
	}
}

// TestReceivePackBackstop_GatesOnSuccess proves a push the upstream refused
// (here 403 — nothing landed) records nothing, while the body still reaches the
// upstream untouched.
func TestReceivePackBackstop_GatesOnSuccess(t *testing.T) {
	body := pkt(zeroOID+" aaaa refs/heads/main\n") + "0000" + "PACKDATA"
	res := runReceivePack(t, http.StatusForbidden, http.MethodPost, "/octo/repo/git-receive-pack", body)

	if res.clientStatus != http.StatusForbidden {
		t.Errorf("client status = %d, want 403", res.clientStatus)
	}
	if res.upstreamBody != body {
		t.Errorf("upstream body altered: got %q want %q", res.upstreamBody, body)
	}
	if len(res.pushes) != 0 {
		t.Errorf("recorded %d pushes on a 403, want 0", len(res.pushes))
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
	want := []gitproxy.PushedRef{{Repo: "octo/repo", Ref: "refs/heads/big", NewSHA: "aaaabbbb", Created: true}}
	if !equalPushes(res.pushes, want) {
		t.Errorf("recorded pushes = %+v, want %+v", res.pushes, want)
	}
}

// TestReceivePackBackstop_InterimResponseStillRecords drives a real push through
// the proxy against an upstream that sends a 1xx interim response (103 Early
// Hints) before its final 200. httputil.ReverseProxy forwards that 1xx by
// calling WriteHeader(103) on the wrapped statusRecorder (verified on Go 1.26),
// so without the recorder's "first FINAL status" guard the 103 would latch the
// gate and the branch would be silently dropped on a successful push. This is
// the full-path companion to the statusRecorder unit test.
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
	want := []gitproxy.PushedRef{{Repo: "octo/repo", Ref: "refs/heads/main", NewSHA: "aaaa", Created: true}}
	if !equalPushes(pushes, want) {
		t.Errorf("recorded pushes = %+v, want %+v (a 1xx interim must not drop the record)", pushes, want)
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
