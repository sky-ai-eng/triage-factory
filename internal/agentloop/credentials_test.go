package agentloop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/llmproxy"
)

// TestEnvCredentials_ACancelledCallReleasesWithoutWaiting is the shape of a
// provider that went silent behind a run's credential proxy: the proxy took
// the request and the provider never answered it. The watchdog cancels the
// call, and the call returns, but closing its client waits for the request
// the client still has in flight. The release must not hold the engagement
// for that.
func TestEnvCredentials_ACancelledCallReleasesWithoutWaiting(t *testing.T) {
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	proxy, err := llmproxy.New(llmproxy.Config{
		Provider:      llmproxy.ProviderAnthropic,
		APIKey:        "sk-ant-real",
		Upstream:      upstream.URL,
		IncomingToken: "run-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := proxy.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	}()
	// Deferred after the proxy's shutdown so it runs first: the shutdown waits
	// for the request the upstream is holding.
	defer close(hold)

	creds := &EnvCredentials{
		Resolve: func(context.Context) (map[string]string, error) {
			return map[string]string{"ANTHROPIC_API_KEY": "run-token", "ANTHROPIC_BASE_URL": "http://" + addr}, nil
		},
		Models: []string{"claude-sonnet-5"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	provider, client, release, err := creds.ForCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stream(ctx, inference.Request{
		Provider: provider,
		Model:    "claude-sonnet-5",
		Rows:     []domain.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("a call the provider never answered succeeded")
	}

	released := make(chan struct{})
	go func() {
		release()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the cancelled call's client waited on the request it was cancelled to stop waiting for")
	}
}

// TestEnvCredentials_ReleaseClosesTheClient: the close runs after release
// returns, but it runs.
func TestEnvCredentials_ReleaseClosesTheClient(t *testing.T) {
	closing := make(chan struct{})
	closed := make(chan struct{})
	creds := &EnvCredentials{
		Resolve: func(context.Context) (map[string]string, error) {
			return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}, nil
		},
		Models: []string{"claude-sonnet-5"},
		NewClient: func(schemas.Account) (Provider, func(), error) {
			return nil, func() {
				<-closing
				close(closed)
			}, nil
		},
	}
	_, _, release, err := creds.ForCall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	close(closing)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the client was never closed")
	}
}
