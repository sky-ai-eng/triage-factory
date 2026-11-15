package inference

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestClient_BedrockDialsTheEndpointOverride sends a Bedrock call through a
// client whose credentials carry a base URL, and checks the call lands on that
// host rather than the public regional one, and that the error a refusal
// produces names the host that answered.
//
// A unit test on the account cannot stand in for this: the account carried
// the base URL before, in a field bifrost's Bedrock provider never read, and
// only a request reaching the server shows which one it reads.
func TestClient_BedrockDialsTheEndpointOverride(t *testing.T) {
	const model = "us.anthropic.claude-sonnet-5"

	var (
		mu    sync.Mutex
		paths []string
		auth  string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"refused by the fake endpoint"}`))
	}))
	defer srv.Close()

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderBedrock,
		APIKey:   "bedrock-bearer",
		BaseURL:  srv.URL,
		Models:   []string{model},
		Bedrock:  &BedrockCredentials{Region: "us-west-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// bifrost dials the endpoint over https, so it has to trust the test
	// server's certificate. This is the only change to what NewAccount built.
	cfg, err := acct.GetConfigForProvider(ProviderBedrock)
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	cfg.NetworkConfig.CACertPEM = &schemas.SecretVar{Val: ca, SecretType: schemas.SecretTypePlainText}

	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = client.Stream(ctx, Request{
		Provider: ProviderBedrock,
		Model:    model,
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("the fake endpoint refuses every call; the stream must fail")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatalf("the endpoint override received no request; the call went elsewhere: %v", err)
	}
	if !strings.HasPrefix(paths[0], "/model/") {
		t.Errorf("path = %q, want a Bedrock runtime /model/… call", paths[0])
	}
	if auth != "Bearer bedrock-bearer" {
		t.Errorf("Authorization = %q, want the Bedrock bearer", auth)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	if !strings.Contains(err.Error(), "[endpoint: https://"+host+"]") {
		t.Errorf("the error must name the host that was dialled (%s): %v", host, err)
	}
	if strings.Contains(err.Error(), "amazonaws.com") {
		t.Errorf("the error names the public host, which was never dialled: %v", err)
	}
}
