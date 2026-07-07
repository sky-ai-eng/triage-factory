//go:build live

// Tier 3 of the Phase 2 validation plan: real Bedrock calls through the
// SigV4 re-signing proxy. Build-tagged so CI never runs it and no AWS
// spend happens on a default `go test ./...`.
//
// To run:
//
//	AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... [AWS_SESSION_TOKEN=...] \
//	  go test -tags live ./internal/llmproxy/ -v -run TestLive_SigV4
//
// Cost: ~$0.01 (one small Haiku call; the negative test 403s before any
// billing). Mint short-lived credentials for this, e.g.
// `aws sts get-session-token --duration-seconds 900`, and let them
// expire after the run — same hygiene as the Phase 1 Anthropic probe.
//
// Optional env:
//
//	AWS_REGION                 signing + endpoint region (default us-east-1)
//	TF_LIVE_BEDROCK_MODEL_ID   model to invoke (default the us. cross-region
//	                           Haiku 3.5 inference profile; older accounts
//	                           may need anthropic.claude-3-haiku-20240307-v1:0)
package llmproxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// liveAWSCredentials reads the real AWS triple from the test process's
// env (never the agent's), skipping the test when absent so machines
// without AWS credentials pass cleanly.
func liveAWSCredentials(t *testing.T) llmproxy.SigV4Credentials {
	t.Helper()
	akid := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if akid == "" || secret == "" {
		t.Skip("AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY not set; skipping live SigV4 test")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return llmproxy.SigV4Credentials{
		AccessKeyID:     akid,
		SecretAccessKey: secret,
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		Region:          region,
	}
}

func liveBedrockModelID() string {
	if m := os.Getenv("TF_LIVE_BEDROCK_MODEL_ID"); m != "" {
		return m
	}
	return "us.anthropic.claude-3-5-haiku-20241022-v1:0"
}

// invokeThroughProxy sends one InvokeModel request — signed with
// placeholder creds carrying runToken as the access-key ID, exactly as
// the sandbox SDK would — through a freshly started SigV4 proxy backed
// by real. Returns the upstream's status code and body.
func invokeThroughProxy(t *testing.T, real llmproxy.SigV4Credentials, runToken string) (int, []byte) {
	t.Helper()
	upstream := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", real.Region)
	_, proxyURL := startSigV4Proxy(t, upstream, real, runToken)

	body := []byte(`{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens": 16,
		"messages": [{"role": "user", "content": "Reply with the single word: PROXY_OK"}]
	}`)
	path := "/model/" + url.PathEscape(liveBedrockModelID()) + "/invoke"
	req, err := http.NewRequest("POST", proxyURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The placeholder triple the sandbox would sign with: the AKID is
	// the per-run token; the secret is throwaway (never verified).
	signAsSandboxSDK(t, req, body, aws.Credentials{
		AccessKeyID:     runToken,
		SecretAccessKey: "secret-placeholder",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// TestLive_SigV4ProxyForwardsRealBedrockRequest is the Tier 3
// acceptance test: a placeholder-signed InvokeModel request re-signed
// by the proxy must get a 200 and model output from real Bedrock. This
// validates the whole re-signing chain against AWS's actual verifier —
// canonical-request construction, service/region scope, body hash,
// header stripping.
func TestLive_SigV4ProxyForwardsRealBedrockRequest(t *testing.T) {
	real := liveAWSCredentials(t)

	status, body := invokeThroughProxy(t, real, "AKIA-PROXY-LIVE-RUN-TOKEN")
	if status != 200 {
		t.Fatalf("Bedrock status = %d, want 200. body = %s", status, body)
	}
	var parsed struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal Bedrock response: %v\nbody: %s", err, body)
	}
	if len(parsed.Content) == 0 {
		t.Fatalf("Bedrock response has no content blocks: %s", body)
	}
	if !strings.Contains(parsed.Content[0].Text, "PROXY_OK") {
		t.Logf("model output %q does not contain PROXY_OK (models don't always comply); 200 + content is the load-bearing assertion", parsed.Content[0].Text)
	}
	t.Logf("Bedrock 200 via proxy; model said: %s", parsed.Content[0].Text)
}

// TestLive_SigV4BadSignatureRejected is the sanity check on the
// positive test: a proxy holding a corrupted secret must be rejected by
// Bedrock (403, before any billing). If this ever passed with a 200,
// AWS would not actually be validating signatures and the positive
// test's result would prove nothing.
func TestLive_SigV4BadSignatureRejected(t *testing.T) {
	real := liveAWSCredentials(t)
	real.SecretAccessKey += "-corrupted"

	status, body := invokeThroughProxy(t, real, "AKIA-PROXY-LIVE-RUN-TOKEN")
	if status != http.StatusForbidden {
		t.Fatalf("Bedrock status = %d for a corrupted signing secret, want 403. body = %s", status, body)
	}
	if !strings.Contains(string(body), "signature") && !strings.Contains(string(body), "Signature") {
		t.Logf("403 body does not mention the signature (may be a generic auth error): %s", body)
	}
	t.Logf("Bedrock rejected the corrupted signature as expected: %s", body)
}
