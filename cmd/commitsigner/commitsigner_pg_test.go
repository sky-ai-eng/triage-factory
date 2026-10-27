package commitsigner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	signing "github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// accountGitHub serves the signer account's public reads: the account, by
// login and by id, and its signing keys.
type accountGitHub struct {
	mu   sync.Mutex
	keys []string
}

func (g *accountGitHub) setKeys(keys ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys = keys
}

func (g *accountGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch strings.TrimPrefix(r.URL.Path, "/api/v3") {
	case "/users/TriageFactory", "/user/90001":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 90001, "login": "TriageFactory", "type": "User", "name": "Triage Factory"})
	case "/users/TriageFactory/ssh_signing_keys":
		items := []map[string]any{}
		for i, k := range g.keys {
			items = append(items, map[string]any{"id": i + 1, "key": k})
		}
		_ = json.NewEncoder(w).Encode(items)
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

func healthForTest(t *testing.T) (*signing.DeploymentHealth, *accountGitHub) {
	t.Helper()
	h := pgtest.Shared(t)
	h.Reset(t)
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := commitsign.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		signing.EnvDeploymentSignerLogin: "TriageFactory",
		signing.EnvDeploymentSignerKey:   string(pem),
	}
	get := func(name string) string { return env[name] }
	signer, err := signing.ParseDeploymentSigner(get, get, "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	gh := &accountGitHub{}
	ts := httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(ts.Close)
	health := signing.NewDeploymentHealth(noop.NewMeterProvider(), signer, pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey), nil)
	health.SetGitHubForTest(t, ts.URL)
	return health, gh
}

// TestStatusAndVerify walks an operator's shell session: status before any
// control pod has registered the key reads pending, verify refuses while the
// key is not on the account and names what to do, and passes once it is.
func TestStatusAndVerify(t *testing.T) {
	health, gh := healthForTest(t)
	ctx := t.Context()

	var out strings.Builder
	if err := runStatus(ctx, &out, health); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"TriageFactory on https://github.com (account not read yet)", "status pending", "checked never", "run `triagefactory commit-signer verify`"} {
		if !strings.Contains(flat(&out), want) {
			t.Errorf("status before registration = %q, want it to say %q", flat(&out), want)
		}
	}

	out.Reset()
	err := runVerify(ctx, &out, health)
	if err == nil || !strings.Contains(err.Error(), "not among TriageFactory's signing keys") {
		t.Fatalf("verify with the key missing = %v, want the key-not-found fault", err)
	}

	gh.setKeys(health.Signer.PublicKey)
	out.Reset()
	if err := runVerify(ctx, &out, health); err != nil {
		t.Fatalf("verify with the key on the account: %v", err)
	}
	for _, want := range []string{"The deployment commit signer is active.", "(account 90001)", "status active since",
		"commits as Triage Factory <90001+TriageFactory@users.noreply.github.com>", "public key " + health.Signer.PublicKey} {
		if !strings.Contains(flat(&out), want) {
			t.Errorf("verify = %q, want it to say %q", flat(&out), want)
		}
	}

	out.Reset()
	if err := runStatus(ctx, &out, health); err != nil {
		t.Fatalf("status after verify: %v", err)
	}
	if !strings.Contains(flat(&out), "status active since") || strings.Contains(flat(&out), "commit unsigned") {
		t.Errorf("status after verify = %q, want the active signer and no advice", flat(&out))
	}
}

// flat is out with each run of whitespace one space, so a test reads the
// fields without pinning the column padding.
func flat(out *strings.Builder) string {
	return strings.Join(strings.Fields(out.String()), " ")
}
