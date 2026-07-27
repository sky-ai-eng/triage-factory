package gitproxy_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// denialSink collects denials the (async, detached-goroutine) RecordDenial hook
// reports, so tests can wait for one rather than race the goroutine.
type denialSink struct{ ch chan gitproxy.DeniedGitOp }

func newDenialSink() *denialSink { return &denialSink{ch: make(chan gitproxy.DeniedGitOp, 16)} }

func (s *denialSink) record(d gitproxy.DeniedGitOp) { s.ch <- d }

// next waits up to 2s for one denial.
func (s *denialSink) next(t *testing.T) gitproxy.DeniedGitOp {
	t.Helper()
	select {
	case d := <-s.ch:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a denial to be recorded")
		return gitproxy.DeniedGitOp{}
	}
}

// startGatedProxy boots a proxy with an Authorize gate (and optional denial
// sink) in front of upstream. Mirrors startProxy but exercises the gated path
// the multi-mode wiring uses.
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
	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, allowAllAuthorize, sink.record)

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
	if d := sink.next(t); d.Reason != "non-git-path" {
		t.Errorf("denial reason = %q, want non-git-path", d.Reason)
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
	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, deny, sink.record)

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
	if d := sink.next(t); d.Reason != "repo-not-authorized" {
		t.Errorf("denial reason = %q, want repo-not-authorized", d.Reason)
	}
}

// TestGatedProxy_DenyReasonAndMessagePropagate pins that a Decision carrying a
// specific DenyReason/DenyMessage surfaces both: the reason lands in the audit
// row and the message becomes the 403 body the agent reads in git's remote
// output — the plumbing that lets a tracked-but-unmaterialized clone tell the
// agent to run `workspace add` instead of a flat "not authorized".
func TestGatedProxy_DenyReasonAndMessagePropagate(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	deny := func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
		return gitproxy.Decision{
			Allowed:     false,
			DenyReason:  "repo-not-materialized",
			DenyMessage: "gitproxy: repo octo/repo is tracked by this team but not yet materialized in this run; run 'workspace add octo/repo' to persist it, then retry",
		}, nil
	}
	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, deny, sink.record)

	req, _ := http.NewRequest("GET", proxyURL+"/octo/repo/info/refs", nil)
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "workspace add octo/repo") {
		t.Errorf("403 body = %q, want the workspace-add recovery hint", string(body))
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hit on a denied repo; want fail-closed")
	}
	if d := sink.next(t); d.Reason != "repo-not-materialized" {
		t.Errorf("denial reason = %q, want repo-not-materialized", d.Reason)
	}
}

// TestGatedProxy_AuthorizeErrorFailsClosedAndAudits pins that an authz-backend
// error fails closed (502, no upstream) AND still records a denial, so an
// outage/misconfig leaves an audit trail of blocked git activity.
func TestGatedProxy_AuthorizeErrorFailsClosedAndAudits(t *testing.T) {
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	errAuthorize := func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
		return gitproxy.Decision{}, errors.New("db is down")
	}
	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, errAuthorize, sink.record)

	req, _ := http.NewRequest("GET", proxyURL+"/octo/repo/info/refs", nil)
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on an authz-backend error", resp.StatusCode)
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hit despite a failed authz check; want fail-closed")
	}
	if d := sink.next(t); d.Reason != "authorize-error" || d.Repo != "repo" {
		t.Errorf("denial = %+v, want an authorize-error denial for the repo", d)
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
			sink := newDenialSink()
			_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, authorize, sink.record)

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
				if d := sink.next(t); d.Reason != c.wantReason {
					t.Errorf("denial reason = %q, want %q", d.Reason, c.wantReason)
				}
			}
		})
	}
}

// TestGatedProxy_ProtectedRefDenialExplainsItself is acceptance criterion 4 of
// the base-branch push policy: a push refused because the ref is the repo's
// base branch must reach the agent as a message that names the ref, says why,
// and says what to do — not the flat "ref not allowed" every ref-level refusal
// used to share. The gate is the only side that knows the ref was excluded by
// policy rather than by "that isn't your branch", so it carries ProtectedRefs
// on the decision to tell the receive-pack path apart.
func TestGatedProxy_ProtectedRefDenialExplainsItself(t *testing.T) {
	authorize := func(_ context.Context, _, _ string) (gitproxy.Decision, error) {
		return gitproxy.Decision{
			Allowed:       true,
			AllowedRefs:   []string{"refs/heads/agent/x"},
			ProtectedRefs: []string{"refs/heads/main", "refs/heads/master"},
		}, nil
	}
	rec := &fakeUpstreamRecord{}
	upstream := fakeGitHub(rec)
	defer upstream.Close()
	ts := &constantTokenSource{value: "ghs", expiresAt: time.Now().Add(time.Hour)}
	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t, ts.source, upstream.URL, authorize, sink.record)

	body := pkt(zeroOID+" aaaa refs/heads/main\n") + "0000PACKDATA"
	req, _ := http.NewRequest("POST", proxyURL+"/octo/repo/git-receive-pack", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := directClient().Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden || rec.hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d, want 403 with no upstream call", resp.StatusCode, rec.hits.Load())
	}
	for _, want := range []string{"refs/heads/main", "protected branch", "octo/repo", "pull request", "team admin"} {
		if !strings.Contains(string(msg), want) {
			t.Errorf("403 body %q does not mention %q", strings.TrimSpace(string(msg)), want)
		}
	}
	if d := sink.next(t); d.Reason != "ref-protected" || d.Ref != "refs/heads/main" {
		t.Errorf("denial = %+v, want a ref-protected denial naming refs/heads/main", d)
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
