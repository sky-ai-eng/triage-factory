package agentloop

import (
	"errors"
	"testing"
)

func TestIsTransient(t *testing.T) {
	transient := []string{
		"inference: provider error: 429 rate limit",
		"inference: provider error: 503 service unavailable",
		"dial tcp: i/o timeout",
		"connection reset by peer",
		"overloaded_error",
		"unexpected EOF",
		"EOF",
	}
	for _, msg := range transient {
		if !isTransient(errors.New(msg)) {
			t.Errorf("%q must be retried", msg)
		}
	}
	permanent := []string{
		"inference: provider error: 401 invalid api key",
		"inference: provider error: model not found",
		"inference: request has no model",
		// eofPattern must not match "eof" embedded in a longer token: a
		// field name or a base64 fragment that happens to contain it is not
		// evidence of a truncated stream.
		"invalid geofence parameter",
		"inference: provider error: bad request field 'eof_marker'",
	}
	for _, msg := range permanent {
		if isTransient(errors.New(msg)) {
			t.Errorf("%q must NOT burn the retry budget", msg)
		}
	}
}

// TestIsTransient_BifrostTransportFailure pins the coupling between this
// classifier and how internal/inference renders a provider error. Every
// transport failure in every bifrost provider carries the same fixed message,
// which matches no marker here — so a network blip reads as permanent and
// ends the engagement on the first attempt unless the wrapped cause and the
// status code travel with it. That rendering is the contract; this is the
// half of it that has to hold for a retryable failure to be retried.
func TestIsTransient_BifrostTransportFailure(t *testing.T) {
	rendered := "inference: provider error: failed to execute HTTP request to provider API: " +
		"dial tcp 10.42.7.1:41231: connect: connection refused (HTTP 502) " +
		"[provider_connection_failed] [endpoint: http://10.42.7.1:41231]"
	if !isTransient(errors.New(rendered)) {
		t.Fatalf("a dial failure must be retried: %q", rendered)
	}
	bare := "inference: provider error: failed to execute HTTP request to provider API"
	if isTransient(errors.New(bare)) {
		t.Fatal("this test's premise is gone: the bare message now matches a marker")
	}
}
