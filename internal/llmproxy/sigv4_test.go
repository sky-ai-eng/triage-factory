// Phase 2 validation tiers 1 and 2 (both network-free).
//
// Tier 1 exercises aws-sdk-go-v2's SignHTTP directly — the proxy's
// signing primitive — and cross-checks it two ways: against the
// official AWS SigV4 test-suite vector (pinning both the SDK and this
// package's independent verifier to the published spec), and against a
// canned Bedrock InvokeModel request recomputed manually.
//
// Tier 2 drives placeholder-signed requests through the proxy at a
// mock upstream that validates the re-signed result the way AWS would:
// reconstruct the canonical request from the wire, re-derive the
// signature from the real secret, compare. Tier 3 (real Bedrock) lives
// in sigv4_live_test.go behind the "live" build tag.
package llmproxy_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// emptySHA256 is the hex SHA-256 of the empty payload, the value SigV4
// mandates for bodyless requests.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// --- Tier 1: the signing primitive ------------------------------------

// TestSigV4SignerMatchesOfficialTestVector pins SignHTTP — and,
// transitively, this package's independent verifier — to the official
// AWS SigV4 test suite's "get-vanilla" case: GET / on
// example.amazonaws.com at 2015-08-30T12:36:00Z with the published
// example credentials must produce exactly the published signature. If
// either implementation misread the spec (canonicalization order,
// key-derivation chain, scope format), this catches it against ground
// truth rather than one implementation validating the other.
func TestSigV4SignerMatchesOfficialTestVector(t *testing.T) {
	const (
		accessKeyID = "AKIDEXAMPLE"
		secretKey   = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		wantSig     = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	)
	signingTime := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	req, err := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	creds := aws.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secretKey}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, req, emptySHA256, "service", "us-east-1", signingTime); err != nil {
		t.Fatalf("SignHTTP: %v", err)
	}

	wantAuthz := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=" + wantSig
	if got := req.Header.Get("Authorization"); got != wantAuthz {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, wantAuthz)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q, want 20150830T123600Z", got)
	}

	// Now the independent verifier must reach the same signature.
	auth := parseSigV4Authorization(t, req.Header.Get("Authorization"))
	_, _, recomputed := recomputeSigV4(t, clientSigV4Input(req, emptySHA256), auth, "20150830T123600Z", secretKey)
	if recomputed != wantSig {
		t.Errorf("independent verifier computed %s, want the official vector %s", recomputed, wantSig)
	}
}

// TestSigV4SignerBedrockRequestShape signs a canned Bedrock InvokeModel
// request — the exact shape the proxy re-signs — and asserts the
// Authorization header's format, the credential scope's components, the
// signed-header set, and that the signature matches a full manual
// recompute of canonical request → string-to-sign → HMAC chain. The
// model-ID path segment contains an encoded ':' (%3A) to pin the
// double-encoding rule for canonical URIs.
func TestSigV4SignerBedrockRequestShape(t *testing.T) {
	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	signingTime := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	const path = "/model/us.anthropic.claude-3-5-haiku-20241022-v1%3A0/invoke"
	req, err := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	creds := aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "test-secret-key"}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, req, payloadHash, "bedrock", "us-east-1", signingTime); err != nil {
		t.Fatalf("SignHTTP: %v", err)
	}

	// The wire path must still carry the once-encoded model ID — the
	// signer must not have re-written the URL path.
	if got := req.URL.EscapedPath(); got != path {
		t.Errorf("EscapedPath = %q, want %q (signer must not rewrite the request path)", got, path)
	}

	authz := req.Header.Get("Authorization")
	shape := regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260415/us-east-1/bedrock/aws4_request, SignedHeaders=[a-z0-9;-]+, Signature=[0-9a-f]{64}$`)
	if !shape.MatchString(authz) {
		t.Errorf("Authorization %q does not match the SigV4 spec shape", authz)
	}

	auth := parseSigV4Authorization(t, authz)
	wantSigned := []string{"content-length", "content-type", "host", "x-amz-content-sha256", "x-amz-date"}
	if got := strings.Join(auth.SignedHeaders, ";"); got != strings.Join(wantSigned, ";") {
		t.Errorf("SignedHeaders = %q, want %q (sorted, lowercase)", got, strings.Join(wantSigned, ";"))
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != payloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q, want SHA256(body) %q", got, payloadHash)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20260415T120000Z" {
		t.Errorf("X-Amz-Date = %q, want 20260415T120000Z", got)
	}

	// Full manual recompute: canonical request → string-to-sign →
	// derived key → signature.
	_, _, recomputed := recomputeSigV4(t, clientSigV4Input(req, payloadHash), auth, "20260415T120000Z", "test-secret-key")
	if recomputed != auth.Signature {
		t.Errorf("manual recompute = %s, signer produced %s", recomputed, auth.Signature)
	}
}

// TestSigV4SignerSessionToken pins the STS-credential path of the
// primitive: a session token on the credentials must surface as a
// signed X-Amz-Security-Token header, and the signature must still
// verify by manual recompute.
func TestSigV4SignerSessionToken(t *testing.T) {
	signingTime := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	req, err := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	body := []byte("{}")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	creds := aws.Credentials{
		AccessKeyID:     "ASIATEMPKEY",
		SecretAccessKey: "temp-secret",
		SessionToken:    "FwoGZXIvYXdzEXAMPLETOKEN",
	}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, req, payloadHash, "bedrock", "us-east-1", signingTime); err != nil {
		t.Fatalf("SignHTTP: %v", err)
	}

	if got := req.Header.Get("X-Amz-Security-Token"); got != creds.SessionToken {
		t.Errorf("X-Amz-Security-Token = %q, want the session token", got)
	}
	auth := parseSigV4Authorization(t, req.Header.Get("Authorization"))
	found := false
	for _, h := range auth.SignedHeaders {
		if h == "x-amz-security-token" {
			found = true
		}
	}
	if !found {
		t.Errorf("SignedHeaders %v does not include x-amz-security-token", auth.SignedHeaders)
	}
	_, _, recomputed := recomputeSigV4(t, clientSigV4Input(req, payloadHash), auth, "20260415T120000Z", "temp-secret")
	if recomputed != auth.Signature {
		t.Errorf("manual recompute = %s, signer produced %s", recomputed, auth.Signature)
	}
}

// --- Tier 2: through the proxy ----------------------------------------

// sigV4Capture records what the fake Bedrock upstream received, in the
// form the independent verifier consumes.
type sigV4Capture struct {
	hits  atomic.Int64
	input sigV4Input
	body  []byte
}

// fakeBedrockUpstream captures each request for later verification and
// returns a canned InvokeModel-ish response.
func fakeBedrockUpstream(rec *sigV4Capture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		rec.body = body
		rec.input = sigV4Input{
			method:        r.Method,
			host:          r.Host,
			escapedPath:   r.URL.EscapedPath(),
			rawQuery:      r.URL.RawQuery,
			header:        r.Header.Clone(),
			contentLength: r.ContentLength,
			payloadHash:   hex.EncodeToString(sum[:]),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
}

// signAsSandboxSDK plays the role of the in-sandbox agent SDK: sign the
// request with the throwaway placeholder credentials injected into the
// sandbox env. The proxy must strip every trace of this signature.
func signAsSandboxSDK(t *testing.T, req *http.Request, body []byte, placeholder aws.Credentials) {
	t.Helper()
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := v4.NewSigner().SignHTTP(context.Background(), placeholder, req, payloadHash, "bedrock", "us-east-1", time.Now()); err != nil {
		t.Fatalf("placeholder SignHTTP: %v", err)
	}
}

// startSigV4Proxy boots a ProviderBedrockSigV4 proxy at upstream with
// the given real triple (and optional per-run token), returning its
// base URL.
func startSigV4Proxy(t *testing.T, upstream string, creds llmproxy.SigV4Credentials, incomingToken string) (*llmproxy.Server, string) {
	t.Helper()
	srv, err := llmproxy.New(llmproxy.Config{
		Provider:         llmproxy.ProviderBedrockSigV4,
		Upstream:         upstream,
		SigV4Credentials: &creds,
		IncomingToken:    incomingToken,
	})
	if err != nil {
		t.Fatalf("llmproxy.New: %v", err)
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

// TestProxySigV4ResignsForUpstream is the load-bearing Tier 2 test: a
// request signed with placeholder credentials goes in; what reaches the
// upstream must carry a valid signature under the REAL secret, over the
// exact bytes and headers that arrived — verified by independently
// recomputing the signature the way AWS would. The body must be
// byte-identical and no trace of the placeholder may survive.
func TestProxySigV4ResignsForUpstream(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIAREALKEY0000001",
		SecretAccessKey: "real-secret-key-that-never-enters-the-sandbox",
		Region:          "us-west-2",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, "")

	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: PROXY_OK"}]}`)
	const modelPath = "/model/us.anthropic.claude-3-5-haiku-20241022-v1%3A0/invoke"
	req, err := http.NewRequest("POST", proxyURL+modelPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	placeholder := aws.Credentials{AccessKeyID: "AKIA-PROXY-PLACEHOLDER", SecretAccessKey: "secret-placeholder"}
	signAsSandboxSDK(t, req, body, placeholder)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rec.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", rec.hits.Load())
	}

	// Body byte-identical.
	if !bytes.Equal(rec.body, body) {
		t.Errorf("upstream body differs from what the client sent:\n got %q\nwant %q", rec.body, body)
	}
	// Path preserved verbatim, including the once-encoded model ID.
	if rec.input.escapedPath != modelPath {
		t.Errorf("upstream path = %q, want %q", rec.input.escapedPath, modelPath)
	}
	// Content framing consistent with the signed content-length.
	if rec.input.contentLength != int64(len(body)) {
		t.Errorf("upstream Content-Length = %d, want %d", rec.input.contentLength, len(body))
	}
	// Recomputed body hash matches the forwarded header.
	if got := rec.input.header.Get("X-Amz-Content-Sha256"); got != rec.input.payloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q, want SHA256 of the received body %q", got, rec.input.payloadHash)
	}

	// The signature must verify under the REAL secret over what
	// actually arrived — the verifier recomputes it like AWS would.
	auth := verifyReceivedSigV4(t, rec.input, real.SecretAccessKey)
	if auth.AccessKeyID != real.AccessKeyID {
		t.Errorf("Credential access key = %q, want the real %q", auth.AccessKeyID, real.AccessKeyID)
	}
	if auth.Region != "us-west-2" || auth.Service != "bedrock" || auth.Terminator != "aws4_request" {
		t.Errorf("credential scope = %s/%s/%s, want us-west-2/bedrock/aws4_request", auth.Region, auth.Service, auth.Terminator)
	}

	// Fresh signing time, not the client's.
	amzDate, err := time.Parse("20060102T150405Z", rec.input.header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatalf("parse X-Amz-Date %q: %v", rec.input.header.Get("X-Amz-Date"), err)
	}
	if d := time.Since(amzDate); d < -time.Minute || d > 5*time.Minute {
		t.Errorf("X-Amz-Date %v is not fresh (delta %v)", amzDate, d)
	}

	// No trace of the placeholder credential anywhere in the headers.
	for name, values := range rec.input.header {
		for _, v := range values {
			if strings.Contains(v, placeholder.AccessKeyID) || strings.Contains(v, placeholder.SecretAccessKey) {
				t.Errorf("placeholder credential leaked upstream in header %s: %q", name, v)
			}
		}
	}
	// And no session token was invented (the real triple has none).
	if got := rec.input.header.Get("X-Amz-Security-Token"); got != "" {
		t.Errorf("X-Amz-Security-Token = %q, want absent (real creds carry no session token)", got)
	}
}

// TestProxySigV4InjectsSessionToken pins the STS path end to end: real
// credentials with a session token must surface upstream as a signed
// X-Amz-Security-Token that verifies under the real secret.
func TestProxySigV4InjectsSessionToken(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "ASIAREALTEMPKEY001",
		SecretAccessKey: "real-temp-secret",
		SessionToken:    "real-session-token-FwoGZXIvYXdz",
		Region:          "us-east-1",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, "")

	body := []byte(`{"messages":[]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke", bytes.NewReader(body))
	signAsSandboxSDK(t, req, body, aws.Credentials{AccessKeyID: "AKIA-PLACEHOLDER", SecretAccessKey: "x"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()

	if got := rec.input.header.Get("X-Amz-Security-Token"); got != real.SessionToken {
		t.Errorf("X-Amz-Security-Token = %q, want the real session token", got)
	}
	auth := verifyReceivedSigV4(t, rec.input, real.SecretAccessKey)
	found := false
	for _, h := range auth.SignedHeaders {
		if h == "x-amz-security-token" {
			found = true
		}
	}
	if !found {
		t.Errorf("SignedHeaders %v missing x-amz-security-token (AWS requires the token be signed)", auth.SignedHeaders)
	}
}

// TestProxySigV4StripsStalePlaceholderSessionToken covers the inverse
// mismatch: the sandbox's placeholder creds carried a session token
// (so the incoming request has X-Amz-Security-Token) but the real
// triple has none. The stale token must be stripped — if it were
// forwarded, it would be signed as-is and AWS would reject the unknown
// token even though the signature itself verified.
func TestProxySigV4StripsStalePlaceholderSessionToken(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIAREALKEY0000002",
		SecretAccessKey: "real-secret-2",
		Region:          "us-east-1",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, "")

	body := []byte(`{"messages":[]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke", bytes.NewReader(body))
	signAsSandboxSDK(t, req, body, aws.Credentials{
		AccessKeyID:     "AKIA-PLACEHOLDER",
		SecretAccessKey: "x",
		SessionToken:    "stale-placeholder-session-token",
	})
	if req.Header.Get("X-Amz-Security-Token") == "" {
		t.Fatal("test setup: placeholder signing should have set X-Amz-Security-Token")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()

	if got := rec.input.header.Get("X-Amz-Security-Token"); got != "" {
		t.Errorf("X-Amz-Security-Token = %q leaked upstream, want stripped", got)
	}
	verifyReceivedSigV4(t, rec.input, real.SecretAccessKey)
}

// TestProxySigV4TokenGate pins caller auth for the SigV4 path:
// the per-run token is the placeholder access-key ID in the incoming
// Authorization header's Credential scope. Only a request signed with
// this run's placeholder AKID is forwarded; a sibling run's AKID, a
// bearer-shaped header, or no header at all gets 401 with the upstream
// untouched.
func TestProxySigV4TokenGate(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	const runToken = "AKIA1234THISRUNSTOKEN"
	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIAREALKEY0000003",
		SecretAccessKey: "real-secret-3",
		Region:          "us-east-1",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, runToken)

	body := []byte(`{"messages":[]}`)
	send := func(mutate func(*http.Request)) int {
		req, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke", bytes.NewReader(body))
		if mutate != nil {
			mutate(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("roundtrip: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// A sibling run's placeholder AKID → 401.
	if got := send(func(r *http.Request) {
		signAsSandboxSDK(t, r, body, aws.Credentials{AccessKeyID: "AKIA9999SIBLINGRUNS", SecretAccessKey: "x"})
	}); got != http.StatusUnauthorized {
		t.Errorf("sibling AKID: status = %d, want 401", got)
	}
	// No Authorization at all → 401.
	if got := send(nil); got != http.StatusUnauthorized {
		t.Errorf("missing Authorization: status = %d, want 401", got)
	}
	// Bearer-shaped header (not SigV4) → 401.
	if got := send(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+runToken)
	}); got != http.StatusUnauthorized {
		t.Errorf("bearer-shaped Authorization: status = %d, want 401", got)
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("upstream reached %d time(s) by unauthorized requests; want 0", rec.hits.Load())
	}

	// This run's AKID → forwarded, re-signed with the real key.
	if got := send(func(r *http.Request) {
		signAsSandboxSDK(t, r, body, aws.Credentials{AccessKeyID: runToken, SecretAccessKey: "placeholder"})
	}); got != 200 {
		t.Errorf("this run's AKID: status = %d, want 200", got)
	}
	if rec.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", rec.hits.Load())
	}
	auth := verifyReceivedSigV4(t, rec.input, real.SecretAccessKey)
	if auth.AccessKeyID != real.AccessKeyID {
		t.Errorf("forwarded Credential AKID = %q, want the real %q", auth.AccessKeyID, real.AccessKeyID)
	}
}

// TestProxySigV4EmptyBody pins the bodyless edge: the payload hash is
// the SHA-256 of the empty string and the signature still verifies.
func TestProxySigV4EmptyBody(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIAREALKEY0000004",
		SecretAccessKey: "real-secret-4",
		Region:          "us-east-1",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, "")

	req, _ := http.NewRequest("GET", proxyURL+"/foundation-models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if got := rec.input.header.Get("X-Amz-Content-Sha256"); got != emptySHA256 {
		t.Errorf("X-Amz-Content-Sha256 = %q, want the empty-payload hash %q", got, emptySHA256)
	}
	if len(rec.body) != 0 {
		t.Errorf("upstream received %d body bytes, want 0", len(rec.body))
	}
	verifyReceivedSigV4(t, rec.input, real.SecretAccessKey)
}

// TestProxySigV4BodyTooLarge pins the signing-buffer cap: a body past
// 32 MiB gets 413 without touching the upstream. The cap is what keeps
// a hostile sandbox from driving unbounded host-side allocations
// through the buffering middleware.
func TestProxySigV4BodyTooLarge(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIAREALKEY0000005",
		SecretAccessKey: "real-secret-5",
		Region:          "us-east-1",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, "")

	// One byte past the 32 MiB buffer cap.
	oversize := bytes.NewReader(make([]byte, 32<<20+1))
	req, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke", oversize)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if rec.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0 (oversize body must be rejected before forwarding)", rec.hits.Load())
	}
}

// TestProxySigV4StreamingResponsePassesThrough pins that buffering the
// REQUEST for signing does not buffer the RESPONSE. This is what makes
// InvokeModelWithResponseStream work: the request is one bounded POST,
// but the response streams — the first chunk must reach the client
// while the upstream is still holding the connection open. Mirrors the
// Phase 1 streaming test's timing discrimination.
func TestProxySigV4StreamingResponsePassesThrough(t *testing.T) {
	const interChunkSleep = 600 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`{"chunk":0}`))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(interChunkSleep)
		_, _ = w.Write([]byte(`{"chunk":1}`))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	real := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIAREALKEY0000006",
		SecretAccessKey: "real-secret-6",
		Region:          "us-east-1",
	}
	_, proxyURL := startSigV4Proxy(t, upstream.URL, real, "")

	body := []byte(`{"messages":[],"stream":true}`)
	req, _ := http.NewRequest("POST", proxyURL+"/model/m/invoke-with-response-stream", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()

	start := time.Now()
	buf := make([]byte, 256)
	n, err := resp.Body.Read(buf)
	elapsed := time.Since(start)
	if err != nil && err != io.EOF {
		t.Fatalf("first read: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("first chunk took %v (upstream sleeps %v between chunks); the proxy is buffering the response", elapsed, interChunkSleep)
	}
	if n == 0 || !strings.Contains(string(buf[:n]), `"chunk":0`) {
		t.Errorf("first read got %q, expected chunk 0", string(buf[:n]))
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), `"chunk":1`) {
		t.Errorf("second chunk missing from drained body: %q", rest)
	}
}

// TestProxySigV4RejectsInvalidConfig pins construction-time validation
// for the new provider: the triple must be present and complete, with a
// region for the credential scope. APIKey, unused on this path, may be
// empty.
func TestProxySigV4RejectsInvalidConfig(t *testing.T) {
	valid := llmproxy.SigV4Credentials{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "s",
		Region:          "us-east-1",
	}
	cases := []struct {
		name  string
		creds *llmproxy.SigV4Credentials
	}{
		{"nil_credentials", nil},
		{"missing_access_key", &llmproxy.SigV4Credentials{SecretAccessKey: "s", Region: "us-east-1"}},
		{"missing_secret", &llmproxy.SigV4Credentials{AccessKeyID: "AKIA", Region: "us-east-1"}},
		{"missing_region", &llmproxy.SigV4Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := llmproxy.New(llmproxy.Config{
				Provider:         llmproxy.ProviderBedrockSigV4,
				Upstream:         "https://bedrock-runtime.us-east-1.amazonaws.com",
				SigV4Credentials: c.creds,
			})
			if err == nil {
				t.Errorf("New accepted %+v; want validation error", c.creds)
			}
		})
	}

	// Positive: no APIKey needed on the SigV4 path.
	if _, err := llmproxy.New(llmproxy.Config{
		Provider:         llmproxy.ProviderBedrockSigV4,
		Upstream:         "https://bedrock-runtime.us-east-1.amazonaws.com",
		SigV4Credentials: &valid,
	}); err != nil {
		t.Errorf("New with a complete triple and empty APIKey: %v; want success", err)
	}
}

// TestProxySigV4LiveCredentialSource_MidRunSwap pins the TokenSource
// pattern (TFAC-616): a SigV4CredentialSource that returns near-expiry
// material forces the proxy to re-read it per request, and swapping the
// source's triple mid-run must change what the proxy signs upstream — the
// executor picking up a re-minted STS session credential without a proxy
// restart.
func TestProxySigV4LiveCredentialSource_MidRunSwap(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	credA := llmproxy.SigV4Credentials{AccessKeyID: "ASIAAAAAAAAAAAAAAAA1", SecretAccessKey: "secret-A", Region: "us-east-1"}
	credB := llmproxy.SigV4Credentials{AccessKeyID: "ASIABBBBBBBBBBBBBBB2", SecretAccessKey: "secret-B", Region: "us-east-1"}

	var current atomic.Value
	current.Store(credA)
	var calls atomic.Int64
	source := func(ctx context.Context) (llmproxy.SigV4Material, error) {
		calls.Add(1)
		c := current.Load().(llmproxy.SigV4Credentials)
		// Expiry inside the refresh threshold → the proxy re-reads on every
		// request (models a short-TTL role session partway through its life).
		return llmproxy.SigV4Material{SigV4Credentials: c, Expiry: time.Now().Add(1 * time.Minute)}, nil
	}

	srv, err := llmproxy.New(llmproxy.Config{
		Provider:              llmproxy.ProviderBedrockSigV4,
		Upstream:              upstream.URL,
		SigV4CredentialSource: source,
	})
	if err != nil {
		t.Fatalf("llmproxy.New: %v", err)
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
	proxyURL := "http://" + addr

	send := func() {
		body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":8,"messages":[]}`)
		req, err := http.NewRequest("POST", proxyURL+"/model/x/invoke", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		signAsSandboxSDK(t, req, body, aws.Credentials{AccessKeyID: "AKIA-PLACEHOLDER", SecretAccessKey: "placeholder"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("roundtrip: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}

	// Request 1 → signed with A.
	send()
	if got := verifyReceivedSigV4(t, rec.input, credA.SecretAccessKey); got.AccessKeyID != credA.AccessKeyID {
		t.Fatalf("request 1 signed with %q, want the A key %q", got.AccessKeyID, credA.AccessKeyID)
	}

	// Swap the source's triple mid-run (the brain re-sealed a fresh bundle).
	current.Store(credB)

	// Request 2 → must be signed with B (the proxy re-read the source).
	send()
	if got := verifyReceivedSigV4(t, rec.input, credB.SecretAccessKey); got.AccessKeyID != credB.AccessKeyID {
		t.Fatalf("request 2 signed with %q, want the swapped B key %q — the proxy did not re-read the source", got.AccessKeyID, credB.AccessKeyID)
	}
	if calls.Load() < 2 {
		t.Fatalf("source called %d times, want >= 2 (per-request re-read on near-expiry material)", calls.Load())
	}
}

// TestProxySigV4LiveCredentialSource_CachesUntilExpiry pins that
// non-expiring (or far-future) material from the source is minted once and
// cached, not re-read per request.
func TestProxySigV4LiveCredentialSource_CachesUntilExpiry(t *testing.T) {
	rec := &sigV4Capture{}
	upstream := fakeBedrockUpstream(rec)
	defer upstream.Close()

	var calls atomic.Int64
	source := func(ctx context.Context) (llmproxy.SigV4Material, error) {
		calls.Add(1)
		return llmproxy.SigV4Material{
			SigV4Credentials: llmproxy.SigV4Credentials{AccessKeyID: "ASIACACHED000000001", SecretAccessKey: "s", Region: "us-east-1"},
			Expiry:           time.Now().Add(1 * time.Hour),
		}, nil
	}
	srv, err := llmproxy.New(llmproxy.Config{
		Provider:              llmproxy.ProviderBedrockSigV4,
		Upstream:              upstream.URL,
		SigV4CredentialSource: source,
	})
	if err != nil {
		t.Fatalf("llmproxy.New: %v", err)
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
	proxyURL := "http://" + addr

	for i := 0; i < 3; i++ {
		body := []byte(`{"messages":[]}`)
		req, _ := http.NewRequest("POST", proxyURL+"/model/x/invoke", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		signAsSandboxSDK(t, req, body, aws.Credentials{AccessKeyID: "AKIA-PH", SecretAccessKey: "ph"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("roundtrip %d: %v", i, err)
		}
		resp.Body.Close()
	}
	if calls.Load() != 1 {
		t.Fatalf("source called %d times over 3 requests, want 1 (cached until near expiry)", calls.Load())
	}
}
