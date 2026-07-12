package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// fakeRoleResolver stands in for *llmcred.Resolver so the role-setup + connect
// probe handlers can be exercised without real AWS.
type fakeRoleResolver struct {
	arn      string
	arnErr   error
	probeErr error

	probedARN string
	probedExt string
	probes    int
}

func (f *fakeRoleResolver) CallerIdentityARN(context.Context) (string, error) {
	return f.arn, f.arnErr
}

func (f *fakeRoleResolver) ProbeRole(_ context.Context, roleARN, externalID string) error {
	f.probedARN = roleARN
	f.probedExt = externalID
	f.probes++
	return f.probeErr
}

func newRoleTestServer(t *testing.T, resolver bedrockRoleResolver) *Server {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	if resolver != nil {
		s.SetBedrockRoleResolver(resolver)
	}
	return s
}

// TestBedrockRoleSetup_ReturnsCallerARNAndExternalID: the setup endpoint
// returns the caller ARN + a stable generated External ID and a filled
// trust-policy snippet carrying both.
func TestBedrockRoleSetup_ReturnsCallerARNAndExternalID(t *testing.T) {
	s := newRoleTestServer(t, &fakeRoleResolver{arn: "arn:aws:iam::111122223333:role/tf-control"})

	rec := doJSON(t, s, "POST", "/api/bedrock/role-setup", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		CallerARN   string `json:"caller_identity_arn"`
		ExternalID  string `json:"external_id"`
		TrustPolicy string `json:"trust_policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.CallerARN != "arn:aws:iam::111122223333:role/tf-control" {
		t.Errorf("caller ARN = %q", out.CallerARN)
	}
	if !strings.HasPrefix(out.ExternalID, "tf-") {
		t.Errorf("external id = %q, want a tf- prefixed value", out.ExternalID)
	}
	if !strings.Contains(out.TrustPolicy, out.ExternalID) || !strings.Contains(out.TrustPolicy, out.CallerARN) {
		t.Errorf("trust policy missing caller ARN / external id:\n%s", out.TrustPolicy)
	}
	if !strings.Contains(out.TrustPolicy, "sts:ExternalId") {
		t.Errorf("trust policy missing the ExternalId condition:\n%s", out.TrustPolicy)
	}

	// External ID is stable across calls (persisted).
	rec2 := doJSON(t, s, "POST", "/api/bedrock/role-setup", nil)
	var out2 struct {
		ExternalID string `json:"external_id"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &out2)
	if out2.ExternalID != out.ExternalID {
		t.Errorf("external id changed between calls: %q vs %q", out.ExternalID, out2.ExternalID)
	}
}

// TestBedrockRoleSetup_NoAmbientIdentity surfaces the operator-remediation
// message when the control service has no AWS identity.
func TestBedrockRoleSetup_NoAmbientIdentity(t *testing.T) {
	s := newRoleTestServer(t, &fakeRoleResolver{arnErr: llmcred.ErrNoAmbientIdentity})

	rec := doJSON(t, s, "POST", "/api/bedrock/role-setup", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no AWS identity") {
		t.Errorf("body should carry the operator remediation: %s", rec.Body.String())
	}
}

// TestBedrockRoleConnect_Success: a passing probe stores the role config (no
// secret), flips the presence flag + method marker, and the probe saw the
// generated External ID.
func TestBedrockRoleConnect_Success(t *testing.T) {
	fake := &fakeRoleResolver{arn: "arn:aws:iam::111122223333:role/tf-control"}
	s := newRoleTestServer(t, fake)

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "role",
		"role_arn":    "arn:aws:iam::444455556666:role/tf-bedrock",
		"region":      "us-east-1",
		"model_id":    "us.anthropic.claude-sonnet-4-6",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := mustSecret(t, s, integrations.KeyAWSRoleARN); got != "arn:aws:iam::444455556666:role/tf-bedrock" {
		t.Errorf("stored role ARN = %q", got)
	}
	if got := mustSecret(t, s, integrations.KeyAWSExternalID); !strings.HasPrefix(got, "tf-") {
		t.Errorf("external id = %q", got)
	}
	if fake.probes != 1 || fake.probedARN != "arn:aws:iam::444455556666:role/tf-bedrock" {
		t.Errorf("probe not called with the role ARN: probes=%d arn=%q", fake.probes, fake.probedARN)
	}
	if fake.probedExt == "" || fake.probedExt != mustSecret(t, s, integrations.KeyAWSExternalID) {
		t.Errorf("probe external id %q must equal the persisted one", fake.probedExt)
	}

	view := getOrgBedrockView(t, s)
	if !view.Has || view.Method != "role" {
		t.Errorf("GET should report role mode: has=%v method=%q", view.Has, view.Method)
	}
	if view.HasAnthr {
		t.Errorf("Anthropic key should be cleared on switch to role mode")
	}
}

// TestBedrockRoleConnect_Denied: an AssumeRole denial produces the
// trust-policy / External-ID guidance and stores nothing.
func TestBedrockRoleConnect_Denied(t *testing.T) {
	s := newRoleTestServer(t, &fakeRoleResolver{probeErr: llmcred.ErrAssumeRoleDenied})

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "role",
		"role_arn":    "arn:aws:iam::444455556666:role/tf-bedrock",
		"region":      "us-east-1",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "trust policy") && !strings.Contains(rec.Body.String(), "External ID") {
		t.Errorf("denial should guide on trust policy / external id: %s", rec.Body.String())
	}
	// Nothing persisted.
	if got := mustSecret(t, s, integrations.KeyAWSRoleARN); got != "" {
		t.Errorf("role ARN should not persist on a denied probe, got %q", got)
	}
	view := getOrgBedrockView(t, s)
	if view.Has {
		t.Errorf("no credential should be marked present after a denied probe")
	}
}

// TestBedrockRoleConnect_NoAmbient: the no-ambient-identity failure is the
// operator message, distinct from the trust-policy denial.
func TestBedrockRoleConnect_NoAmbient(t *testing.T) {
	s := newRoleTestServer(t, &fakeRoleResolver{probeErr: llmcred.ErrNoAmbientIdentity})

	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "role",
		"role_arn":    "arn:aws:iam::444455556666:role/tf-bedrock",
		"region":      "us-east-1",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no AWS identity") {
		t.Errorf("no-ambient should carry the operator message, got: %s", rec.Body.String())
	}
}

// TestBedrockRoleConnect_ClearsOtherProviders: switching from access_keys to
// role removes the stale IAM key material.
func TestBedrockRoleConnect_ClearsOtherProviders(t *testing.T) {
	fake := &fakeRoleResolver{arn: "arn:aws:iam::1:role/c"}
	s := newRoleTestServer(t, fake)

	// Seed an access-keys config.
	rec := doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":       "access_keys",
		"access_key_id":     "AKIAOLD",
		"secret_access_key": "secretold",
		"region":            "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed access_keys: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Switch to role mode.
	rec = doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method": "role",
		"role_arn":    "arn:aws:iam::444455556666:role/tf-bedrock",
		"region":      "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("switch to role: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, k := range []string{integrations.KeyAWSAccessKeyID, integrations.KeyAWSSecretAccessKey} {
		if got := mustSecret(t, s, k); got != "" {
			t.Errorf("%s should be cleared on switch to role mode, got %q", k, got)
		}
	}
	// And switching BACK to access_keys clears the role ARN (resolver detects
	// role by aws_role_arn presence).
	rec = doJSON(t, s, "POST", "/api/bedrock/connect", map[string]any{
		"auth_method":       "access_keys",
		"access_key_id":     "AKIANEW",
		"secret_access_key": "secretnew",
		"region":            "us-east-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("switch back: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := mustSecret(t, s, integrations.KeyAWSRoleARN); got != "" {
		t.Errorf("role ARN should be cleared on switch back to access_keys, got %q", got)
	}
}

// TestTrustPolicyPrincipalARN converts an STS assumed-role session ARN (what
// GetCallerIdentity returns when the control service runs under an instance
// role / IRSA — the recommended setup) to the underlying IAM role ARN, which
// is what a trust policy Principal must name. IAM user/role ARNs pass through.
func TestTrustPolicyPrincipalARN(t *testing.T) {
	cases := []struct{ in, want string }{
		// assumed-role session ARN → role ARN (drops the session name).
		{"arn:aws:sts::123456789012:assumed-role/tf-control/i-0abc123", "arn:aws:iam::123456789012:role/tf-control"},
		// GovCloud partition preserved.
		{"arn:aws-us-gov:sts::123456789012:assumed-role/r/s", "arn:aws-us-gov:iam::123456789012:role/r"},
		// already a role ARN — unchanged.
		{"arn:aws:iam::123456789012:role/tf-control", "arn:aws:iam::123456789012:role/tf-control"},
		// an IAM user ARN — unchanged (already a valid Principal).
		{"arn:aws:iam::123456789012:user/ci", "arn:aws:iam::123456789012:user/ci"},
	}
	for _, c := range cases {
		if got := trustPolicyPrincipalARN(c.in); got != c.want {
			t.Errorf("trustPolicyPrincipalARN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBedrockRoleSetup_TrustPolicyUsesRoleARNForAssumedRoleCaller pins that
// the rendered trust policy names the ROLE ARN, not the (invalid-as-Principal)
// STS session ARN, when the control service runs under an assumed role.
func TestBedrockRoleSetup_TrustPolicyUsesRoleARNForAssumedRoleCaller(t *testing.T) {
	s := newRoleTestServer(t, &fakeRoleResolver{arn: "arn:aws:sts::111122223333:assumed-role/tf-control/i-0abc"})
	rec := doJSON(t, s, "POST", "/api/bedrock/role-setup", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		CallerARN   string `json:"caller_identity_arn"`
		TrustPolicy string `json:"trust_policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// caller_identity_arn stays the raw session ARN (accurate — that's who we are).
	if out.CallerARN != "arn:aws:sts::111122223333:assumed-role/tf-control/i-0abc" {
		t.Errorf("caller ARN should be the raw session ARN, got %q", out.CallerARN)
	}
	// The trust policy must use the ROLE ARN as Principal, not the session ARN.
	if !strings.Contains(out.TrustPolicy, "arn:aws:iam::111122223333:role/tf-control") {
		t.Errorf("trust policy must name the IAM role ARN:\n%s", out.TrustPolicy)
	}
	if strings.Contains(out.TrustPolicy, "assumed-role") {
		t.Errorf("trust policy must NOT contain the STS assumed-role session ARN:\n%s", out.TrustPolicy)
	}
}
