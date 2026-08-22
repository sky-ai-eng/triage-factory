package llmcred

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeSecrets is a single-org in-memory SecretsReader.
type fakeSecrets map[string]string

func (f fakeSecrets) Get(_ context.Context, _ string, key string) (string, error) {
	return f[key], nil
}

// fakeMinter records the last AssumeRole input (so tests can inspect the
// session policy) and returns scripted credentials / errors.
type fakeMinter struct {
	lastInput AssumeRoleInput
	calls     int
	creds     MintedCredentials
	err       error
	arn       string
	arnErr    error
}

func (m *fakeMinter) AssumeRole(_ context.Context, in AssumeRoleInput) (MintedCredentials, error) {
	m.lastInput = in
	m.calls++
	if m.err != nil {
		return MintedCredentials{}, m.err
	}
	return m.creds, nil
}

func (m *fakeMinter) CallerIdentityARN(_ context.Context) (string, error) {
	return m.arn, m.arnErr
}

func mintedAt(base time.Time, ttl time.Duration) MintedCredentials {
	return MintedCredentials{
		AccessKeyID:     "ASIAEXAMPLE",
		SecretAccessKey: "secret",
		SessionToken:    "session-tok",
		Expiry:          base.Add(ttl),
	}
}

// TestPassthrough_MatchesResolveCredentialsForBundle pins that a
// non-role org resolves byte-for-byte identically to what agentproc
// injects — decision 7's "existing configs resolve byte-for-byte" pin.
func TestPassthrough_MatchesResolveCredentialsForBundle(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	org := "org-1"

	cases := []fakeSecrets{
		{integrations.KeyAnthropicAPIKey: "sk-ant-xyz"},
		{integrations.KeyAWSBearerTokenBedrock: "bdrk-tok", integrations.KeyAWSRegion: "us-west-2", integrations.KeyBedrockModelID: "anthropic.claude-x"},
		{integrations.KeyAWSAccessKeyID: "AKIA1", integrations.KeyAWSSecretAccessKey: "sk", integrations.KeyAWSSessionToken: "st", integrations.KeyAWSRegion: "eu-central-1"},
	}
	for i, sec := range cases {
		want, err := agentproc.ResolveCredentialsForBundle(ctx, sec, org, "")
		if err != nil {
			t.Fatalf("case %d: baseline resolve: %v", i, err)
		}
		r := NewResolver(sec, &fakeMinter{}, DefaultTTL, NetworkBinding{})
		got, err := r.ResolveForSystem(ctx, org, "tf-test", "")
		if err != nil {
			t.Fatalf("case %d: ResolveForSystem: %v", i, err)
		}
		if !got.Expiry.IsZero() {
			t.Errorf("case %d: passthrough must have zero expiry, got %v", i, got.Expiry)
		}
		if !mapsEqual(got.Env, want) {
			t.Errorf("case %d: env drift\n got=%v\nwant=%v", i, got.Env, want)
		}
	}
}

// TestRoleMint_EnvShapeAndExpiry checks a role org mints the SigV4 triple
// env plus region/model/endpoint, and carries the credential expiry.
func TestRoleMint_EnvShapeAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	sec := fakeSecrets{
		integrations.KeyAWSRoleARN:     "arn:aws:iam::123456789012:role/tf-bedrock",
		integrations.KeyAWSExternalID:  "ext-abc",
		integrations.KeyAWSRegion:      "us-east-1",
		integrations.KeyBedrockModelID: "arn:aws:bedrock:us-east-1:123456789012:inference-profile/x",
		integrations.KeyBedrockBaseURL: "https://vpce-1.bedrock.example/",
	}
	m := &fakeMinter{creds: mintedAt(now, time.Hour)}
	r := NewResolver(sec, m, time.Hour, NetworkBinding{})
	r.now = func() time.Time { return now }

	mat, err := r.ResolveForBundle(ctx, "org-1", "conv-42", "")
	if err != nil {
		t.Fatalf("ResolveForBundle: %v", err)
	}
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":          "ASIAEXAMPLE",
		"AWS_SECRET_ACCESS_KEY":      "secret",
		"AWS_SESSION_TOKEN":          "session-tok",
		"CLAUDE_CODE_USE_BEDROCK":    "1",
		"AWS_REGION":                 "us-east-1",
		"ANTHROPIC_MODEL":            "arn:aws:bedrock:us-east-1:123456789012:inference-profile/x",
		"ANTHROPIC_BEDROCK_BASE_URL": "https://vpce-1.bedrock.example",
	}
	if !mapsEqual(mat.Env, want) {
		t.Errorf("env mismatch\n got=%v\nwant=%v", mat.Env, want)
	}
	if !mat.Expiry.Equal(now.Add(time.Hour)) {
		t.Errorf("expiry=%v want %v", mat.Expiry, now.Add(time.Hour))
	}
	if m.lastInput.RoleSessionName != "tf-conv-42" {
		t.Errorf("session name=%q want tf-conv-42", m.lastInput.RoleSessionName)
	}
	if m.lastInput.ExternalID != "ext-abc" {
		t.Errorf("external id=%q want ext-abc", m.lastInput.ExternalID)
	}
}

// TestBrainBoundMint_NoNetworkCondition is the load-bearing pin from
// decision 5: with TF_EXECUTOR_EGRESS_CIDRS set, a brain-bound mint's
// session policy contains no IP condition, while a bundle-bound mint's does.
func TestBrainBoundMint_NoNetworkCondition(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	sec := fakeSecrets{
		integrations.KeyAWSRoleARN:    "arn:aws:iam::1:role/r",
		integrations.KeyAWSExternalID: "ext",
	}
	egress := NetworkBinding{SourceCIDRs: []string{"203.0.113.0/24"}, VPCEIDs: []string{"vpce-abc"}}

	// Brain-bound (system): NO condition despite egress being set.
	mSys := &fakeMinter{creds: mintedAt(now, time.Hour)}
	rSys := NewResolver(sec, mSys, time.Hour, egress)
	if _, err := rSys.ResolveForSystem(ctx, "org", "tf-scorer", ""); err != nil {
		t.Fatalf("system mint: %v", err)
	}
	if hasNetworkCondition(t, mSys.lastInput.Policy) {
		t.Errorf("brain-bound session policy must NOT contain a network condition:\n%s", mSys.lastInput.Policy)
	}

	// Executor-bound (bundle): condition IS present.
	mExec := &fakeMinter{creds: mintedAt(now, time.Hour)}
	rExec := NewResolver(sec, mExec, time.Hour, egress)
	if _, err := rExec.ResolveForBundle(ctx, "org", "conv-1", ""); err != nil {
		t.Fatalf("bundle mint: %v", err)
	}
	if !hasNetworkCondition(t, mExec.lastInput.Policy) {
		t.Errorf("executor-bound session policy MUST contain a network condition:\n%s", mExec.lastInput.Policy)
	}
}

// TestMintCache_UntilHalfLife checks the resolver mints once, serves the
// cached credential until half-life, then re-mints past it.
func TestMintCache_UntilHalfLife(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	cur := now
	sec := fakeSecrets{integrations.KeyAWSRoleARN: "arn:aws:iam::1:role/r", integrations.KeyAWSExternalID: "ext"}
	m := &fakeMinter{}
	r := NewResolver(sec, m, time.Hour, NetworkBinding{})
	r.now = func() time.Time { return cur }

	m.creds = mintedAt(cur, time.Hour) // expiry = now+1h, half-life now+30m
	if _, err := r.ResolveForSystem(ctx, "org", "tf-scorer", ""); err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 {
		t.Fatalf("first resolve: calls=%d want 1", m.calls)
	}
	// Within half-life: cached, no new mint.
	cur = now.Add(20 * time.Minute)
	if _, err := r.ResolveForSystem(ctx, "org", "tf-scorer", ""); err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 {
		t.Fatalf("cached resolve: calls=%d want 1", m.calls)
	}
	// Past half-life: re-mint.
	cur = now.Add(31 * time.Minute)
	m.creds = mintedAt(cur, time.Hour)
	if _, err := r.ResolveForSystem(ctx, "org", "tf-scorer", ""); err != nil {
		t.Fatal(err)
	}
	if m.calls != 2 {
		t.Fatalf("post-half-life resolve: calls=%d want 2", m.calls)
	}
}

// TestPerConversationMintKeying: two different conversations of the same org
// mint separately (per-conversation RoleSessionName), so CloudTrail attribution
// stays per-conversation.
func TestPerConversationMintKeying(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	sec := fakeSecrets{integrations.KeyAWSRoleARN: "arn:aws:iam::1:role/r", integrations.KeyAWSExternalID: "ext"}
	m := &fakeMinter{creds: mintedAt(now, time.Hour)}
	r := NewResolver(sec, m, time.Hour, NetworkBinding{})
	r.now = func() time.Time { return now }

	if _, err := r.ResolveForBundle(ctx, "org", "conv-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveForBundle(ctx, "org", "conv-b", ""); err != nil {
		t.Fatal(err)
	}
	if m.calls != 2 {
		t.Fatalf("distinct conversations: calls=%d want 2", m.calls)
	}
}

// TestRoleMint_MissingExternalID surfaces a config error, not a mint.
func TestRoleMint_MissingExternalID(t *testing.T) {
	ctx := context.Background()
	sec := fakeSecrets{integrations.KeyAWSRoleARN: "arn:aws:iam::1:role/r"}
	r := NewResolver(sec, &fakeMinter{}, time.Hour, NetworkBinding{})
	_, err := r.ResolveForSystem(ctx, "org", "tf-scorer", "")
	if !errors.Is(err, ErrRoleMisconfigured) {
		t.Fatalf("err=%v want ErrRoleMisconfigured", err)
	}
}

// TestRoleMint_DeniedPropagates keeps the AssumeRole denial class intact.
func TestRoleMint_DeniedPropagates(t *testing.T) {
	ctx := context.Background()
	sec := fakeSecrets{integrations.KeyAWSRoleARN: "arn:aws:iam::1:role/r", integrations.KeyAWSExternalID: "ext"}
	m := &fakeMinter{err: ErrAssumeRoleDenied}
	r := NewResolver(sec, m, time.Hour, NetworkBinding{})
	_, err := r.ResolveForBundle(ctx, "org", "conv-1", "")
	if !errors.Is(err, ErrAssumeRoleDenied) {
		t.Fatalf("err=%v want ErrAssumeRoleDenied", err)
	}
}

// TestSessionPolicy_ResourceScoping: an ARN model pins the resource; a bare
// model id (or none) falls back to "*" but stays action-scoped.
func TestSessionPolicy_ResourceScoping(t *testing.T) {
	arnPol := buildSessionPolicy("arn:aws:bedrock:us-east-1:1:foundation-model/x", nil)
	if !strings.Contains(arnPol, "foundation-model/x") {
		t.Errorf("arn model should pin resource: %s", arnPol)
	}
	barePol := buildSessionPolicy("anthropic.claude-x", nil)
	if !policyResourceIsWildcard(t, barePol) {
		t.Errorf("bare model id should fall back to * resource: %s", barePol)
	}
	// Both must be action-scoped to the two invoke actions only.
	for _, p := range []string{arnPol, barePol} {
		if !strings.Contains(p, actionInvokeModel) || !strings.Contains(p, actionInvokeModelWithStream) {
			t.Errorf("policy missing invoke actions: %s", p)
		}
	}
}

func TestSessionNameForConversation_Sanitizes(t *testing.T) {
	if got := sessionNameForConversation("abc/def ghi"); got != "tf-abc-def-ghi" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := sessionNameForConversation(long); len(got) != 64 {
		t.Errorf("session name not clamped to 64: len=%d", len(got))
	}
}

// helpers

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func parsePolicy(t *testing.T, raw string) sessionPolicy {
	t.Helper()
	var p sessionPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("parse policy %q: %v", raw, err)
	}
	return p
}

func hasNetworkCondition(t *testing.T, raw string) bool {
	p := parsePolicy(t, raw)
	for _, s := range p.Statement {
		if len(s.Condition) > 0 {
			return true
		}
	}
	return false
}

func policyResourceIsWildcard(t *testing.T, raw string) bool {
	p := parsePolicy(t, raw)
	for _, s := range p.Statement {
		for _, res := range s.Resource {
			if res == "*" {
				return true
			}
		}
	}
	return false
}

// TestProbeRole_PassesExternalIDAndNoNetworkCondition: the connect probe is a
// brain-bound AssumeRole (no network condition) presenting the external ID.
func TestProbeRole_PassesExternalIDAndNoNetworkCondition(t *testing.T) {
	m := &fakeMinter{creds: mintedAt(time.Now(), time.Hour)}
	r := NewResolver(fakeSecrets{}, m, time.Hour, NetworkBinding{SourceCIDRs: []string{"10.0.0.0/8"}})
	if err := r.ProbeRole(context.Background(), "arn:aws:iam::1:role/r", "ext-xyz"); err != nil {
		t.Fatalf("ProbeRole: %v", err)
	}
	if m.lastInput.ExternalID != "ext-xyz" {
		t.Errorf("probe external id = %q", m.lastInput.ExternalID)
	}
	if hasNetworkCondition(t, m.lastInput.Policy) {
		t.Errorf("connect probe must not be network-bound:\n%s", m.lastInput.Policy)
	}
}

// TestProbeRole_NilMinter surfaces the no-ambient-identity class.
func TestProbeRole_NilMinter(t *testing.T) {
	r := NewResolver(fakeSecrets{}, nil, time.Hour, NetworkBinding{})
	if err := r.ProbeRole(context.Background(), "arn", "ext"); !errors.Is(err, ErrNoAmbientIdentity) {
		t.Fatalf("err=%v want ErrNoAmbientIdentity", err)
	}
	if _, err := r.CallerIdentityARN(context.Background()); !errors.Is(err, ErrNoAmbientIdentity) {
		t.Fatalf("CallerIdentityARN err=%v want ErrNoAmbientIdentity", err)
	}
}

// TestSystemEnvResolver_NilResolver returns nil so the consumer keeps Run's
// built-in resolution (local/ambient).
func TestSystemEnvResolver_NilResolver(t *testing.T) {
	if SystemEnvResolver(nil, "tf-x") != nil {
		t.Error("SystemEnvResolver(nil) must return nil")
	}
}
