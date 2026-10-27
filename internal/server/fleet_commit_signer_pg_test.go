package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// signerAccountGitHub serves the deployment signer's account and its signing
// keys, the two public reads its checks make.
type signerAccountGitHub struct {
	mu   sync.Mutex
	keys []string
}

func (g *signerAccountGitHub) serve(w http.ResponseWriter, r *http.Request) {
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

// deploymentSignerForTest configures the rig's server with a deployment signer
// whose account reads go to a fake GitHub, as a control pod's config would.
func deploymentSignerForTest(t *testing.T, r *authRig) (*commitsigner.DeploymentHealth, *signerAccountGitHub) {
	t.Helper()
	key, err := commitsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := commitsign.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		commitsigner.EnvDeploymentSignerLogin: "TriageFactory",
		commitsigner.EnvDeploymentSignerKey:   string(pem),
	}
	get := func(name string) string { return env[name] }
	signer, err := commitsigner.ParseDeploymentSigner(get, get, "https://github.com")
	if err != nil {
		t.Fatalf("ParseDeploymentSigner: %v", err)
	}
	gh := &signerAccountGitHub{keys: []string{signer.PublicKey}}
	ts := httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(ts.Close)
	health := commitsigner.NewDeploymentHealth(noop.NewMeterProvider(), signer, r.srv.allStores, nil)
	health.SetGitHubForTest(t, ts.URL)
	r.srv.SetDeploymentCommitSigner(health)
	return health, gh
}

// TestFleetCommitSigner_Gates pins the operator gate on the three
// deployment-signer routes: no session is a 401, a signed-in caller who is not
// a deployment operator finds no route (404), an operator holding an API
// token is refused (403, since a token is sealed to an org and the routes'
// subject is the deployment), and an operator's session passes, licensed or
// not.
func TestFleetCommitSigner_Gates(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	stores := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey)
	deploymentSignerForTest(t, r)

	const opEmail = "ops@corp.example"
	operator := r.seedUser()
	opOrg, _ := r.seedOrg(operator, "ops")
	if _, err := stores.Operators.Add(t.Context(), opEmail, "test"); err != nil {
		t.Fatalf("add operator: %v", err)
	}
	pgtest.SeedIdentity(t, r.h, operator.String(), opEmail, true)
	member := r.seedUser()
	r.seedOrg(member, "acme")

	opSid := r.sidFromResp(first(r.driveCallback(operator)))
	memberSid := r.sidFromResp(first(r.driveCallback(member)))
	_, token := r.mintToken(operator, opOrg, "ops-token")

	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/fleet/commit-signer"},
		{http.MethodPost, "/api/fleet/commit-signer/verify"},
		{http.MethodGet, "/api/fleet/commit-signer/usage"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			do := func(sid string) int {
				if c.method == http.MethodGet {
					return r.requestWithSid(c.method, c.path, sid).StatusCode
				}
				return r.postJSONWithSid(c.method, c.path, sid, nil).StatusCode
			}
			if got := do(""); got != http.StatusUnauthorized {
				t.Errorf("no session = %d, want 401", got)
			}
			if got := do(memberSid); got != http.StatusNotFound {
				t.Errorf("not an operator = %d, want 404", got)
			}
			if got := r.serve(r.bearerReq(c.method, c.path, token)).Code; got != http.StatusForbidden {
				t.Errorf("an operator's API token = %d, want 403", got)
			}
			if got := do(opSid); got != http.StatusOK {
				t.Errorf("an operator's session = %d, want 200", got)
			}
		})
	}
}

func first[A, B any](a A, _ B) A { return a }

// TestFleetCommitSigner_GetVerifyUsage drives the routes past the gate: the
// signer reads pending until a check, Verify activates it once its key is on
// the account and says which key to add when it is not, a control pod with no
// deployment signer has none to show, and the usage window parses strictly.
func TestFleetCommitSigner_GetVerifyUsage(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	stores := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey)
	const opEmail = "ops@corp.example"
	operator := r.seedUser()
	r.seedOrg(operator, "ops")
	if _, err := stores.Operators.Add(t.Context(), opEmail, "test"); err != nil {
		t.Fatalf("add operator: %v", err)
	}
	pgtest.SeedIdentity(t, r.h, operator.String(), opEmail, true)
	sid := r.sidFromResp(first(r.driveCallback(operator)))

	if got := r.requestWithSid(http.MethodGet, "/api/fleet/commit-signer", sid).StatusCode; got != http.StatusNotFound {
		t.Fatalf("GET with no deployment signer configured = %d, want 404", got)
	}

	health, gh := deploymentSignerForTest(t, r)
	read := func() deploymentSignerJSON {
		t.Helper()
		resp := r.requestWithSid(http.MethodGet, "/api/fleet/commit-signer", sid)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET = %d, want 200", resp.StatusCode)
		}
		var out deploymentSignerJSON
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got.Status != "pending" || got.PublicKey != health.Signer.PublicKey || got.GitHubUserID != nil || got.CommitEmail != nil {
		t.Errorf("before any check the signer reads %+v, want pending with no account", got)
	}

	gh.mu.Lock()
	gh.keys = nil
	gh.mu.Unlock()
	resp := r.postJSONWithSid(http.MethodPost, "/api/fleet/commit-signer/verify", sid, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(readBody(resp), "KEY_NOT_FOUND") {
		t.Errorf("Verify with the key missing = %d, want 409 KEY_NOT_FOUND", resp.StatusCode)
	}

	gh.mu.Lock()
	gh.keys = []string{health.Signer.PublicKey}
	gh.mu.Unlock()
	if resp := r.postJSONWithSid(http.MethodPost, "/api/fleet/commit-signer/verify", sid, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("Verify with the key on the account = %d, want 200: %s", resp.StatusCode, readBody(resp))
	}
	got := read()
	if got.Status != "active" || got.GitHubUserID == nil || *got.GitHubUserID != "90001" ||
		got.CommitEmail == nil || *got.CommitEmail != "90001+TriageFactory@users.noreply.github.com" {
		t.Errorf("after Verify the signer reads %+v, want active as account 90001 with the derived address", got)
	}

	for query, want := range map[string]int{
		"":                                   http.StatusOK,
		"?since=2026-10-01&until=2026-11-01": http.StatusOK,
		"?since=yesterday":                   http.StatusBadRequest,
		"?since=2026-11-01&until=2026-10-01": http.StatusBadRequest,
	} {
		if got := r.requestWithSid(http.MethodGet, "/api/fleet/commit-signer/usage"+query, sid).StatusCode; got != want {
			t.Errorf("usage%s = %d, want %d", query, got, want)
		}
	}
}

// TestCommitIdentity_ManagedWorkspaceSignsAsTheDeploymentSigner: GET
// commit-identity answers a managed workspace with no signer of its own with
// the deployment signer, from the same resolution its runs take.
func TestCommitIdentity_ManagedWorkspaceSignsAsTheDeploymentSigner(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	admin := pgstore.New(r.h.AdminDB, r.h.AdminDB, pgtest.SecretKey)
	founder := r.seedUser()
	org, _ := r.seedOrg(founder, "managed")
	ctx := t.Context()
	if _, err := admin.Orgs.SetSourceBaseURL(ctx, org.String(), eventsource.KindGitHub, "https://github.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Orgs.SetGitHubCredentialClass(ctx, org.String(), domain.GitHubCredentialClassManagedApp); err != nil {
		t.Fatal(err)
	}
	health, _ := deploymentSignerForTest(t, r)
	if _, fault, err := health.Verify(ctx); err != nil || fault != nil {
		t.Fatalf("Verify = %+v, %v", fault, err)
	}

	sid := r.sidFromResp(first(r.driveCallback(founder)))
	resp := r.requestWithSid(http.MethodGet, fmt.Sprintf("/api/orgs/%s/github/commit-identity", org), sid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET commit-identity = %d", resp.StatusCode)
	}
	var got commitIdentityJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Signing != string(domain.CommitSigningDeployment) || got.Identity == nil || got.Identity.Login == nil || *got.Identity.Login != "TriageFactory" {
		t.Errorf("commit-identity = %+v (identity %+v), want the deployment signer, signing as it", got, got.Identity)
	}
}
