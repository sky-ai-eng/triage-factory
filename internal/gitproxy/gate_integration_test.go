package gitproxy_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// startGatedProxy boots a proxy with an Authorize gate (and optional denial
// capture) in front of upstream. Mirrors startProxy but exercises the gated
// path the multi-mode wiring uses.
func startGatedProxy(
	t *testing.T,
	ts gitproxy.TokenSource,
	upstream string,
	authorize func(context.Context, string, string) (gitproxy.Decision, error),
	onDeny func(gitproxy.DeniedGitOp),
) (*gitproxy.Server, string) {
	t.Helper()
	cfg := gitproxy.Config{TokenSource: ts, Upstream: upstream, Authorize: authorize}
	if onDeny != nil {
		cfg.RecordDenial = func(_ context.Context, d gitproxy.DeniedGitOp) { onDeny(d) }
	}
	srv, err := gitproxy.New(cfg)
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, "http://" + addr
}

func allowAllAuthorize(_ context.Context, _, _ string) (gitproxy.Decision, error) {
	return gitproxy.Decision{Allowed: true}, nil
}

// directClient avoids the process HTTP_PROXY (CI runners set it) so requests
// reach the loopback test proxy directly.
func directClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// TestGatedProxy_PathShapeRejectsNonGit: a GHES-style API path is rejected by
// the path-shape allowlist (403 + denial) before any credential is injected,
// even though the Authorize hook would allow the repo. This is the control
// that makes "the git proxy can only do git" hold on GHES.
func TestGatedProxy_PathShapeRejectsNonGit(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	var denials []gitproxy.DeniedGitOp
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, allowAllAuthorize, func(d gitproxy.DeniedGitOp) {
		denials = append(denials, d)
	})

	req, _ := http.NewRequest("GET", proxyURL+"/api/v3/repos/octo/repo", nil)
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a non-git path", resp.StatusCode)
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hit on a non-git path; want fail-closed at the proxy")
	}
	if len(denials) != 1 || denials[0].Reason != "non-git-path" {
		t.Errorf("denials = %+v, want one non-git-path denial", denials)
	}
}

// TestGatedProxy_AuthorizeDenyReturns403 pins that a repo the gate refuses is
// 403'd with a recorded denial and never reaches the upstream credential path.
func TestGatedProxy_AuthorizeDenyReturns403(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	deny := func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
		return gitproxy.Decision{Allowed: false}, nil
	}
	var denials []gitproxy.DeniedGitOp
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, deny, func(d gitproxy.DeniedGitOp) {
		denials = append(denials, d)
	})

	req, _ := http.NewRequest("GET", proxyURL+"/octo/repo/info/refs", nil)
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an unauthorized repo", resp.StatusCode)
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hit on an unauthorized repo; want fail-closed")
	}
	if len(denials) != 1 || denials[0].Reason != "repo-not-authorized" {
		t.Errorf("denials = %+v, want one repo-not-authorized denial", denials)
	}
}

// TestGatedProxy_NilAuthorizeAllowsAll pins that an unset Authorize is allow-all
// (the loopback/test path) — a fetch on any repo forwards.
func TestGatedProxy_NilAuthorizeAllowsAll(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, nil, nil)

	req, _ := http.NewRequest("GET", proxyURL+"/octo/repo/info/refs", nil)
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || rec.hits.Load() != 1 {
		t.Errorf("status=%d hits=%d, want 200 + 1 hit (nil Authorize = allow-all)", resp.StatusCode, rec.hits.Load())
	}
}

// TestGatedProxy_RefEnforcement is the Layer-3 control: on a push, every
// ref-update is checked against the repo's allowed set BEFORE forwarding —
// a foreign ref, a delete (even of an allowed ref), or one bad ref in a
// multi-ref push rejects the whole push with no upstream call; an allowed ref
// forwards; an oversize command block fails closed; an empty command list
// (capability probe) is allowed.
func TestGatedProxy_RefEnforcement(t *testing.T) {
	authorize := func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
		return gitproxy.Decision{Allowed: true, AllowedRefs: []string{"refs/heads/feature/x"}}, nil
	}
	oversize := strings.Repeat(pkt(zeroOID+" "+zeroOID+" refs/heads/x\n"), 800) // ~79 KiB, no flush

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantHit    bool
		wantReason string
	}{
		{"allowed ref forwards", pkt(zeroOID+" aaaa refs/heads/feature/x\n") + "0000PACKDATA", http.StatusOK, true, ""},
		{"force-push to allowed ref forwards", pkt("bbbb cccc refs/heads/feature/x\n") + "0000PACKDATA", http.StatusOK, true, ""},
		{"foreign ref denied", pkt(zeroOID+" aaaa refs/heads/main\n") + "0000PACKDATA", http.StatusForbidden, false, "ref-not-allowed"},
		{"delete of allowed ref denied", pkt("aaaa "+zeroOID+" refs/heads/feature/x\n") + "0000PACKDATA", http.StatusForbidden, false, "ref-delete"},
		{"multi-ref one bad rejects whole push", pkt(zeroOID+" aaaa refs/heads/feature/x\n") + pkt("bbbb cccc refs/heads/main\n") + "0000PACKDATA", http.StatusForbidden, false, "ref-not-allowed"},
		{"empty commands allowed (capability probe)", "0000", http.StatusOK, true, ""},
		{"oversize command block fails closed", oversize, http.StatusForbidden, false, "command-block-too-large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &fakeUpstreamRecord{}
			upstream := fakeGitHub(rec)
			defer upstream.Close()
			ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
			var denials []gitproxy.DeniedGitOp
			_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, authorize, func(d gitproxy.DeniedGitOp) {
				denials = append(denials, d)
			})

			req, _ := http.NewRequest("POST", proxyURL+"/octo/repo/git-receive-pack", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
			resp, err := directClient().Do(req)
			if err != nil {
				t.Fatalf("roundtrip: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != c.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			if gotHit := rec.hits.Load() > 0; gotHit != c.wantHit {
				t.Errorf("upstream hit = %v, want %v", gotHit, c.wantHit)
			}
			if c.wantReason != "" {
				if len(denials) == 0 || denials[len(denials)-1].Reason != c.wantReason {
					t.Errorf("denials = %+v, want a %q denial", denials, c.wantReason)
				}
			}
		})
	}
}

// TestGatedProxy_PerRepoTokenCache pins the per-repo token cache: two requests
// for the same repo mint once; a different repo mints again.
func TestGatedProxy_PerRepoTokenCache(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()

	var mu sync.Mutex
	mints := map[string]int{}
	ts := func(_ context.Context, owner, repo string) (gitproxy.Token, error) {
		mu.Lock()
		mints[owner+"/"+repo]++
		mu.Unlock()
		return gitproxy.Token{Value: "ghs", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	srv, proxyURL := startGatedProxy(t, ts, upstream.URL, allowAllAuthorize, nil)

	for _, p := range []string{"/o/a/info/refs", "/o/a/info/refs", "/o/b/info/refs"} {
		req, _ := http.NewRequest("GET", proxyURL+p, nil)
		resp, err := directClient().Do(req)
		if err != nil {
			t.Fatalf("roundtrip %s: %v", p, err)
		}
		_ = resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if mints["o/a"] != 1 {
		t.Errorf("repo a mints = %d, want 1 (cached across two requests)", mints["o/a"])
	}
	if mints["o/b"] != 1 {
		t.Errorf("repo b mints = %d, want 1", mints["o/b"])
	}
	if got := srv.MintCount(); got != 2 {
		t.Errorf("MintCount = %d, want 2 (one per distinct repo)", got)
	}
}
