package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// errVaultDown stands in for a transient credential-store failure — a Vault
// blip, an RLS/session fault, a dropped pool connection.
var errVaultDown = errors.New("vault unavailable")

// getFailingSecrets is a SecretStore whose Get always fails; every other
// method delegates to the real store through the embedded interface. That's
// the narrow shape the App staging decision reads through
// (integrations.Load), so a test can fail exactly that read without
// disturbing the resolver reads the same request makes outside the tx.
type getFailingSecrets struct {
	db.SecretStore
}

func (getFailingSecrets) Get(context.Context, string, string) (string, error) {
	return "", errVaultDown
}

// failingSecretsTx wraps a TxRunner and swaps a Get-failing SecretStore into
// every transaction it opens, leaving the rest of the stores real.
type failingSecretsTx struct{ inner db.TxRunner }

func (f failingSecretsTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return f.inner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Secrets = getFailingSecrets{SecretStore: tx.Secrets}
		return fn(tx)
	})
}

func (f failingSecretsTx) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return f.inner.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Secrets = getFailingSecrets{SecretStore: tx.Secrets}
		return fn(tx)
	})
}

// assertNoAppRow fails the test if any org_github_apps row exists. The
// staging decision reads the org's PAT to choose active=true/false; when
// that read fails the request must leave no App row at all, in either
// activation state — an App written active beside a live PAT breaks the
// App-XOR-PAT invariant.
func assertNoAppRow(t *testing.T, s *Server) {
	t.Helper()
	app, err := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read app row: %v", err)
	}
	if app != nil {
		t.Fatalf("app row persisted despite a failed credential read: %+v", app)
	}
}

// TestGitHubAppRegisterCallback_CredentialReadFailure_WritesNothing pins the
// manifest-register half: a failed vault read fails the callback and leaves
// the database exactly as it was.
func TestGitHubAppRegisterCallback_CredentialReadFailure_WritesNothing(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	// Stub GitHub's manifest conversion (plus the best-effort bot-user lookup,
	// which 404s here and is irrelevant to what this test pins).
	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id": 5150,
			"slug": "tf-vault-down",
			"client_id": "Iv1.vault",
			"client_secret": "cs",
			"pem": "-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----",
			"webhook_secret": "whsec"
		}`))
	}))
	t.Cleanup(ghStub.Close)
	setOrgGitHubBase(t, s, ghStub.URL)

	state := appRegisterState{
		OrgID:     runmode.LocalDefaultOrgID,
		OwnerType: "org",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(s.deployCfg.hmacKey)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}

	s.tx = failingSecretsTx{inner: s.tx}

	rec := doJSON(t, s, http.MethodGet,
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/register/callback?code=c&state="+signed, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("callback = %d, want 500 on a failed credential read; body=%s", rec.Code, rec.Body.String())
	}
	assertNoAppRow(t, s)
}

// TestGitHubAppImport_CredentialReadFailure_WritesNothing pins the BYO-App
// half of the same invariant.
func TestGitHubAppImport_CredentialReadFailure_WritesNothing(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 909, slug: "acme-bot", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)

	s.tx = failingSecretsTx{inner: s.tx}

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("909", testRSAPEM(t), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("import = %d, want 500 on a failed credential read; body=%s", rec.Code, rec.Body.String())
	}
	assertNoAppRow(t, s)
}
