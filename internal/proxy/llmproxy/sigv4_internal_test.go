package llmproxy

import "testing"

// TestSigV4AccessKeyID pins the fail-closed parsing the token gate
// relies on for ProviderBedrockSigV4: anything that isn't a well-formed
// SigV4 Authorization header yields "", which then fails the
// constant-time compare against the per-run token.
func TestSigV4AccessKeyID(t *testing.T) {
	cases := []struct {
		name  string
		authz string
		want  string
	}{
		{
			"well_formed",
			"AWS4-HMAC-SHA256 Credential=AKIARUNTOKEN/20260707/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123",
			"AKIARUNTOKEN",
		},
		{"empty", "", ""},
		{"bearer_shaped", "Bearer AKIARUNTOKEN", ""},
		{"missing_algorithm_prefix", "Credential=AKIARUNTOKEN/20260707/us-east-1/bedrock/aws4_request", ""},
		{"missing_credential_component", "AWS4-HMAC-SHA256 SignedHeaders=host, Signature=abc", ""},
		{"credential_without_scope_slash", "AWS4-HMAC-SHA256 Credential=AKIARUNTOKEN", ""},
		{
			// A different algorithm token must not pass the prefix check —
			// SigV4-A headers (AWS4-ECDSA-P256-SHA256) are not this path.
			"sigv4a_algorithm",
			"AWS4-ECDSA-P256-SHA256 Credential=AKIARUNTOKEN/20260707/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=abc",
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sigV4AccessKeyID(c.authz); got != c.want {
				t.Errorf("sigV4AccessKeyID(%q) = %q, want %q", c.authz, got, c.want)
			}
		})
	}
}
