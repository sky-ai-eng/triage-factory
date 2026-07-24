package inference

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestClient_StreamAfterCloseReturnsError pins that a closed client fails
// cleanly instead of panicking on the shut-down bifrost, and that Close is
// idempotent.
func TestClient_StreamAfterCloseReturnsError(t *testing.T) {
	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic, APIKey: "sk-test", Models: []string{"claude-sonnet-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	client.Close() // idempotent — must not double-shutdown or panic

	_, err = client.Stream(context.Background(), Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-5",
		Rows: []domain.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Stream after Close must return ErrClientClosed, got %v", err)
	}
}

// TestClient_NilReceiverIsSafe pins that a nil client returns an error rather
// than panicking, and Close on nil is a no-op.
func TestClient_NilReceiverIsSafe(t *testing.T) {
	var client *Client
	_, err := client.Stream(context.Background(), Request{Provider: ProviderAnthropic, Model: "claude-sonnet-5"})
	if !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Stream on a nil client must return ErrClientClosed, got %v", err)
	}
	client.Close() // must not panic
}
