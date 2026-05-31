package agentproc

import (
	"context"
	"errors"
	"testing"
)

// fakeSystemGetter records GetSystem calls. It deliberately implements
// ONLY GetSystem (the claims-free door), so a regression that tried to
// route NewSystemSecretsReader through the RLS-checked Get would fail to
// compile against this double — pinning the SKY-389 "system door, not Get"
// decision structurally as well as behaviorally.
type fakeSystemGetter struct {
	gotOrg string
	gotKey string
	val    string
	err    error
	calls  int
}

func (f *fakeSystemGetter) GetSystem(ctx context.Context, orgID, key string) (string, error) {
	f.calls++
	f.gotOrg = orgID
	f.gotKey = key
	return f.val, f.err
}

var _ SystemSecretGetter = (*fakeSystemGetter)(nil)

// TestNewSystemSecretsReader_RoutesThroughGetSystem pins the SKY-389
// invariant: the background run paths' SecretsReader resolves per-org LLM
// credentials via the system/admin door (GetSystem), not the RLS-checked
// Get — these run claims-free, so Get would just trade the nil-Secrets
// error for an RLS denial. SecretsReader.Get(org, key) must forward
// verbatim to GetSystem(org, key).
func TestNewSystemSecretsReader_RoutesThroughGetSystem(t *testing.T) {
	g := &fakeSystemGetter{val: "sk-ant-xyz"}
	r := NewSystemSecretsReader(g)

	got, err := r.Get(context.Background(), "org-7", "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got != "sk-ant-xyz" {
		t.Errorf("Get = %q; want the GetSystem value", got)
	}
	if g.calls != 1 {
		t.Fatalf("GetSystem called %d times; want 1", g.calls)
	}
	if g.gotOrg != "org-7" || g.gotKey != "anthropic_api_key" {
		t.Errorf("GetSystem called with (org=%q, key=%q); want (org-7, anthropic_api_key)", g.gotOrg, g.gotKey)
	}
}

// TestNewSystemSecretsReader_PropagatesError pins that a backend read
// failure surfaces through the adapter unchanged, so resolveCredentials
// can distinguish "fetch failed" from "not configured".
func TestNewSystemSecretsReader_PropagatesError(t *testing.T) {
	sentinel := errors.New("vault unreachable")
	r := NewSystemSecretsReader(&fakeSystemGetter{err: sentinel})

	if _, err := r.Get(context.Background(), "org", "key"); !errors.Is(err, sentinel) {
		t.Errorf("Get error = %v; want the propagated GetSystem error", err)
	}
}
