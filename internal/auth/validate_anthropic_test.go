package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestValidateAnthropicAPIKey_OK pins the happy path: a 200 from the models
// endpoint is a valid key; the request hits the configured /v1/models path
// (not some rewritten endpoint) with GET and the required headers (x-api-key +
// anthropic-version) sent exactly as Anthropic documents.
func TestValidateAnthropicAPIKey_OK(t *testing.T) {
	var gotKey, gotVersion, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		// The validator keys only on the status code (it drains and discards the
		// body), so an empty list is enough — no need to embed any model id.
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	// Include the real endpoint path so the request-path assertion below proves
	// the validator hits /v1/models verbatim, catching a regression that points
	// it at the wrong endpoint.
	SetAnthropicModelsURLForTest(t, srv.URL+"/v1/models")

	if err := ValidateAnthropicAPIKey(context.Background(), "sk-ant-good"); err != nil {
		t.Fatalf("ValidateAnthropicAPIKey returned %v, want nil", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/v1/models" {
		t.Errorf("request path = %q, want /v1/models", gotPath)
	}
	if gotKey != "sk-ant-good" {
		t.Errorf("x-api-key = %q, want the supplied key", gotKey)
	}
	if gotVersion != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicAPIVersion)
	}
}

// TestValidateAnthropicAPIKey_Invalid pins that the host rejecting the key
// (401 or 403) maps to ErrAnthropicKeyInvalid — the "your key is wrong" state,
// distinct from "we couldn't reach Anthropic".
func TestValidateAnthropicAPIKey_Invalid(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"type":"authentication_error"}}`, code)
		}))
		t.Cleanup(srv.Close)
		SetAnthropicModelsURLForTest(t, srv.URL)

		err := ValidateAnthropicAPIKey(context.Background(), "sk-ant-bad")
		if !errors.Is(err, ErrAnthropicKeyInvalid) {
			t.Errorf("status %d: err = %v, want ErrAnthropicKeyInvalid", code, err)
		}
	}
}

// TestValidateAnthropicAPIKey_UnexpectedStatus pins that an unexpected non-2xx
// (e.g. a 500) is the unreachable/transport class, not "bad key" — we never
// tell the user their key is wrong on a server-side hiccup.
func TestValidateAnthropicAPIKey_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	SetAnthropicModelsURLForTest(t, srv.URL)

	err := ValidateAnthropicAPIKey(context.Background(), "sk-ant-key")
	if !errors.Is(err, ErrAnthropicUnreachable) {
		t.Errorf("err = %v, want ErrAnthropicUnreachable", err)
	}
	if errors.Is(err, ErrAnthropicKeyInvalid) {
		t.Errorf("a 500 was misclassified as an invalid key: %v", err)
	}
}

// TestValidateAnthropicAPIKey_Unreachable pins the transport-failure path:
// when the host can't be dialed at all, the result is ErrAnthropicUnreachable.
func TestValidateAnthropicAPIKey_Unreachable(t *testing.T) {
	// Stand a stub up to claim a port, then close it so dials are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	SetAnthropicModelsURLForTest(t, deadURL)

	err := ValidateAnthropicAPIKey(context.Background(), "sk-ant-key")
	if !errors.Is(err, ErrAnthropicUnreachable) {
		t.Errorf("err = %v, want ErrAnthropicUnreachable", err)
	}
}
