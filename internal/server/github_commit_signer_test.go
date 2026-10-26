package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// signerGitHubUser is one account the fake GitHub serves.
type signerGitHubUser struct {
	ID    int64
	Login string
	Type  string
	Name  string
}

// signerGitHub is a fake GitHub serving what the commit-signer routes read:
// accounts by login and by id, each account's signing-key listing, and one
// commit's verification. Tests mutate it between requests to play the admin
// adding a key, GitHub renaming an account, or a fault being fixed.
type signerGitHub struct {
	mu    sync.Mutex
	users map[string]signerGitHubUser // by login
	keys  map[int64][]string          // by account id
	// noSigningKeys makes the listing 404 for every account, as a GitHub
	// that does not serve SSH signing keys answers.
	noSigningKeys bool
	// commit is the verification the one commit the fake knows reads as,
	// nil for a commit GitHub no longer has.
	commit      *ghclient.CommitVerification
	commitReads int
}

func newSignerGitHub(t *testing.T) (*signerGitHub, string) {
	t.Helper()
	gh := &signerGitHub{
		users: map[string]signerGitHubUser{
			"octocat":   {ID: 583231, Login: "octocat", Type: "User", Name: "The Octocat"},
			"plainuser": {ID: 1001, Login: "plainuser", Type: "User"},
			"helper":    {ID: 49699333, Login: "helper[bot]", Type: "Bot"},
			"acme":      {ID: 77, Login: "acme", Type: "Organization"},
		},
		keys: map[int64][]string{},
	}
	ts := httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(ts.Close)
	return gh, ts.URL
}

func (g *signerGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	notFound := func() { http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) }
	writeUser := func(u signerGitHubUser) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": u.ID, "login": u.Login, "type": u.Type, "name": u.Name})
	}
	switch {
	case strings.HasPrefix(path, "/users/") && strings.HasSuffix(path, "/ssh_signing_keys"):
		u, ok := g.users[strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/ssh_signing_keys")]
		if !ok || g.noSigningKeys {
			notFound()
			return
		}
		items := []map[string]any{}
		for i, k := range g.keys[u.ID] {
			items = append(items, map[string]any{"id": i + 1, "key": k, "title": commitSignerKeyTitle})
		}
		_ = json.NewEncoder(w).Encode(items)
	case path == "/users/broken":
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	case strings.HasPrefix(path, "/users/"):
		if u, ok := g.users[strings.TrimPrefix(path, "/users/")]; ok {
			writeUser(u)
			return
		}
		notFound()
	case strings.HasPrefix(path, "/user/"):
		for _, u := range g.users {
			if fmt.Sprint(u.ID) == strings.TrimPrefix(path, "/user/") {
				writeUser(u)
				return
			}
		}
		notFound()
	case strings.HasPrefix(path, "/repos/acme/app/commits/"):
		g.commitReads++
		if g.commit == nil {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{
			"committer":    map[string]any{"email": g.commit.CommitterEmail},
			"verification": map[string]any{"verified": g.commit.Verified, "reason": g.commit.Reason},
		}})
	default:
		notFound()
	}
}

// addKey plays the admin adding key to the account as a signing key.
func (g *signerGitHub) addKey(id int64, key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[id] = append(g.keys[id], key)
}

// rename plays GitHub renaming an account: the old login answers nothing.
func (g *signerGitHub) rename(from, to string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.users[from]
	delete(g.users, from)
	u.Login = to
	g.users[to] = u
}

func (g *signerGitHub) dropSigningKeys() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.noSigningKeys = true
}

func (g *signerGitHub) setCommit(v *ghclient.CommitVerification) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commit = v
}

func (g *signerGitHub) reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commitReads
}

// signerResolver routes every GitHub client at the fake and reports the
// identity the org commits as without a signer, the App's bot.
type signerResolver struct{ repositoriesByIDResolver }

func (signerResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "acme-tf[bot]", "9+acme-tf[bot]@users.noreply.github.com", true
}

// newSignerServer is a local-mode server on host whose GitHub is a fresh fake.
func newSignerServer(t *testing.T, host string) (*Server, *signerGitHub) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	seedSourceHost(t, s, eventsource.KindGitHub, host)
	gh, url := newSignerGitHub(t)
	s.ghResolver = signerResolver{repositoriesByIDResolver{url: url}}
	return s, gh
}

func commitSignerPath(suffix string) string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/commit-signer" + suffix
}

func decodeSigner(t *testing.T, rec *httptest.ResponseRecorder) commitSignerJSON {
	t.Helper()
	var out commitSignerJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode signer: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// mustSigner sends a signer request that must answer status, and decodes it.
func mustSigner(t *testing.T, s *Server, method, suffix string, body any, status int) commitSignerJSON {
	t.Helper()
	rec := doJSON(t, s, method, commitSignerPath(suffix), body)
	if rec.Code != status {
		t.Fatalf("%s %s = %d, want %d; body=%s", method, suffix, rec.Code, status, rec.Body.String())
	}
	return decodeSigner(t, rec)
}

func localSecret(t *testing.T, s *Server, key string) string {
	t.Helper()
	v, err := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return v
}

// publicHalf is the public key of a stored private key, as GitHub lists it.
func publicHalf(t *testing.T, privatePEM string) string {
	t.Helper()
	key, err := commitsign.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		t.Fatalf("parse stored signing key: %v", err)
	}
	pub, err := commitsign.PublicKey(key)
	if err != nil {
		t.Fatalf("public half: %v", err)
	}
	return pub
}

// signerAuditRows reads the org's commit-signer access-log rows, oldest first,
// as "action change" pairs ("credential_set registered").
func signerAuditRows(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT action, COALESCE(detail_json, '') FROM access_change_log
		WHERE org_id = ? ORDER BY created_at, rowid`, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read access log: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatalf("scan access log: %v", err)
		}
		d := parseAccessDetail(detail)
		if d.Kind != domain.CredentialKindGitHubCommitSigner {
			continue
		}
		out = append(out, strings.TrimSpace(action+" "+d.Change))
	}
	return out
}

// signerChecks points the checks counter at a manual reader for the test and
// returns a reader of its series, "check result" → count.
func signerChecks(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	commitsigner.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "commit_signer.checks" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					check, _ := dp.Attributes.Value(attribute.Key("check"))
					result, _ := dp.Attributes.Value(attribute.Key("result"))
					out[check.AsString()+" "+result.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

// TestCommitSigner_RegisterAndVerify walks a github.com signer from nothing to
// active: registration stores a pending signer and its private key and answers
// the public half to add, Verify refuses until the key is on the account, and
// the identity a run commits as follows the signer through it.
func TestCommitSigner_RegisterAndVerify(t *testing.T) {
	s, gh := newSignerServer(t, "https://github.com")
	checks := signerChecks(t)

	assertOneFault(t, doJSON(t, s, http.MethodGet, commitSignerPath(""), nil), http.StatusNotFound, "NOT_FOUND", "")
	if got := commitIdentity(t, s); got.Signing != "none" || got.Identity == nil || got.Identity.Name != "acme-tf[bot]" {
		t.Errorf("identity with no signer = %+v, want the bot, unsigned", got)
	}

	created := mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)
	if created.Status != "pending" || created.Login != "octocat" || created.GitHubUserID != "583231" || created.Host != "https://github.com" {
		t.Errorf("created = %+v, want a pending signer for octocat (583231) on github.com", created)
	}
	if created.CommitName != "The Octocat" || created.CommitEmail != "583231+octocat@users.noreply.github.com" || created.Email != nil {
		t.Errorf("commit identity = %q <%q> (override %v), want the profile name and the derived noreply address",
			created.CommitName, created.CommitEmail, created.Email)
	}
	if created.KeyTitle != "Triage Factory commit signing" || created.PendingPublicKey != nil {
		t.Errorf("key title %q, pending %v; want the suggested title and no rotation", created.KeyTitle, created.PendingPublicKey)
	}
	if got := publicHalf(t, localSecret(t, s, integrations.KeyGitHubCommitSigningKey)); got != created.PublicKey {
		t.Errorf("stored private key's public half = %q, want the answered %q", got, created.PublicKey)
	}
	if got := mustSigner(t, s, http.MethodGet, "", nil, http.StatusOK); got.PublicKey != created.PublicKey {
		t.Errorf("GET = %+v, want the registered signer", got)
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""), map[string]any{"login": "plainuser"}),
		http.StatusConflict, "CONFLICT", "")
	if got := commitIdentity(t, s); got.Signing != "none" || got.Identity == nil || got.Identity.Name != "The Octocat" ||
		got.Identity.GitHubUserID == nil || *got.Identity.GitHubUserID != "583231" {
		t.Errorf("identity with a pending signer = %+v, want the signer, unsigned", got)
	}

	rec := doJSON(t, s, http.MethodPost, commitSignerPath("/verify"), nil)
	assertOneFault(t, rec, http.StatusConflict, "KEY_NOT_FOUND", "")
	if msg := decodeErrorItems(t, rec)[0].Message; !strings.Contains(msg, "a minute") {
		t.Errorf("KEY_NOT_FOUND message %q does not say the listing can lag", msg)
	}

	gh.addKey(583231, "ssh-ed25519 AAAAsomeoneelse")
	gh.addKey(583231, created.PublicKey)
	verified := mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK)
	if verified.Status != "active" || verified.CheckedAt == nil {
		t.Errorf("verified = %+v, want an active signer with a check time", verified)
	}
	if got := commitIdentity(t, s); got.Signing != "org" {
		t.Errorf("identity with an active signer signs %q, want org", got.Signing)
	}

	// The account is read by id, so a rename on GitHub does not lose it.
	gh.rename("octocat", "octocat-renamed")
	mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK)

	if got, want := checks(), map[string]int64{"verify key_missing": 1, "verify valid": 2}; !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
	if got, want := signerAuditRows(t, s), []string{"credential_set registered", "credential_set verified", "credential_set verified"}; !slices.Equal(got, want) {
		t.Errorf("access log = %v, want %v", got, want)
	}
}

// commitIdentity reads GET commit-identity.
func commitIdentity(t *testing.T, s *Server) commitIdentityJSON {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/commit-identity", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET commit-identity = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out commitIdentityJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	return out
}

// TestCommitSigner_RegisterRefusals: every refusal answers before anything is
// written, so the org is left with no signer and no key.
func TestCommitSigner_RegisterRefusals(t *testing.T) {
	// An org with no GitHub credential is told GitHub is not connected.
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	bare := newTestServer(t)
	assertOneFault(t, doJSON(t, bare, http.MethodPost, commitSignerPath(""), map[string]any{"login": "octocat"}),
		http.StatusConflict, "NOT_CONFIGURED", "")

	s, gh := newSignerServer(t, "https://github.com")
	for _, tc := range []struct {
		name   string
		body   any
		status int
		reason string
		field  string
	}{
		{"no login", map[string]any{}, http.StatusBadRequest, "MISSING_FIELD", "login"},
		{"not a login", map[string]any{"login": "not a login"}, http.StatusBadRequest, "INVALID_FIELD", "login"},
		{"an unknown field", map[string]any{"login": "octocat", "key": "x"}, http.StatusBadRequest, "UNKNOWN_FIELD", "key"},
		{"an empty email", map[string]any{"login": "octocat", "email": ""}, http.StatusBadRequest, "INVALID_FIELD", "email"},
		{"an email with a name", map[string]any{"login": "octocat", "email": "Bot <bot@acme.example>"}, http.StatusBadRequest, "INVALID_FIELD", "email"},
		{"an email with space", map[string]any{"login": "octocat", "email": " bot@acme.example"}, http.StatusBadRequest, "INVALID_FIELD", "email"},
		{"an email git would rewrite", map[string]any{"login": "octocat", "email": "'bot@acme.example"}, http.StatusBadRequest, "INVALID_FIELD", "email"},
		{"no such account", map[string]any{"login": "nobody"}, http.StatusUnprocessableEntity, "INVALID_FIELD", "login"},
		{"a bot", map[string]any{"login": "helper"}, http.StatusUnprocessableEntity, "INVALID_FIELD", "login"},
		{"an organization", map[string]any{"login": "acme"}, http.StatusUnprocessableEntity, "INVALID_FIELD", "login"},
		{"GitHub failing", map[string]any{"login": "broken"}, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""), tc.body), tc.status, tc.reason, tc.field)
		})
	}

	// A GitHub that answers 404 for the signing-key listing of an account
	// that exists does not serve SSH signing keys.
	gh.dropSigningKeys()
	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""), map[string]any{"login": "octocat"}),
		http.StatusUnprocessableEntity, "SSH_SIGNING_UNSUPPORTED", "")

	// An org with no GitHub URL has no GitHub to resolve the login on.
	seedSourceHost(t, s, eventsource.KindGitHub, "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""), map[string]any{"login": "octocat"}),
		http.StatusConflict, "NOT_CONFIGURED", "")

	assertOneFault(t, doJSON(t, s, http.MethodGet, commitSignerPath(""), nil), http.StatusNotFound, "NOT_FOUND", "")
	if k := localSecret(t, s, integrations.KeyGitHubCommitSigningKey); k != "" {
		t.Error("a refused registration left a signing key behind")
	}
	if rows := signerAuditRows(t, s); len(rows) != 0 {
		t.Errorf("a refused registration recorded %v", rows)
	}
}

// TestCommitSigner_GHESWithoutSigningKeys: a GHES that does not serve the
// signing-key listing is detected from the listing, not from its version.
func TestCommitSigner_GHESWithoutSigningKeys(t *testing.T) {
	s, gh := newSignerServer(t, "https://ghe.example.com")
	gh.dropSigningKeys()
	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""),
		map[string]any{"login": "octocat", "email": "octocat@ghe.example.com"}),
		http.StatusUnprocessableEntity, "SSH_SIGNING_UNSUPPORTED", "")
}

// TestCommitSigner_EmailRulesByHost: only github.com has a noreply address to
// derive, so a signer anywhere else is registered with an address and cannot
// have it cleared.
func TestCommitSigner_EmailRulesByHost(t *testing.T) {
	t.Run("github.com", func(t *testing.T) {
		s, _ := newSignerServer(t, "https://github.com")
		created := mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "plainuser", "email": "bot@acme.example"}, http.StatusCreated)
		if created.Email == nil || *created.Email != "bot@acme.example" || created.CommitEmail != "bot@acme.example" || created.CommitName != "plainuser" {
			t.Errorf("created = %+v, want the address it was given and the login as the name", created)
		}
		cleared := mustSigner(t, s, http.MethodPatch, "", map[string]any{"email": nil}, http.StatusOK)
		if cleared.Email != nil || cleared.CommitEmail != "1001+plainuser@users.noreply.github.com" {
			t.Errorf("cleared = %+v, want the derived noreply address", cleared)
		}
		set := mustSigner(t, s, http.MethodPatch, "", map[string]any{"email": "agent@acme.example"}, http.StatusOK)
		if set.Email == nil || *set.Email != "agent@acme.example" {
			t.Errorf("set = %+v, want the new address", set)
		}
		// The same address again changes nothing and records nothing.
		mustSigner(t, s, http.MethodPatch, "", map[string]any{"email": "agent@acme.example"}, http.StatusOK)

		for _, tc := range []struct {
			body   any
			reason string
			field  string
		}{
			{map[string]any{}, "MISSING_FIELD", ""},
			{map[string]any{"email": 5}, "INVALID_FIELD", "email"},
			{map[string]any{"email": "not an address"}, "INVALID_FIELD", "email"},
			{map[string]any{"email": ""}, "INVALID_FIELD", "email"},
			{map[string]any{"login": "x"}, "UNKNOWN_FIELD", "login"},
		} {
			assertOneFault(t, doJSON(t, s, http.MethodPatch, commitSignerPath(""), tc.body), http.StatusBadRequest, tc.reason, tc.field)
		}
		want := []string{"credential_set registered", "credential_set email_changed", "credential_set email_changed"}
		if got := signerAuditRows(t, s); !slices.Equal(got, want) {
			t.Errorf("access log = %v, want %v", got, want)
		}
	})

	t.Run("GHES", func(t *testing.T) {
		s, _ := newSignerServer(t, "https://ghe.example.com")
		assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""), map[string]any{"login": "octocat"}),
			http.StatusBadRequest, "MISSING_FIELD", "email")
		assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath(""), map[string]any{"login": "octocat", "email": nil}),
			http.StatusBadRequest, "MISSING_FIELD", "email")
		created := mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat", "email": "octocat@ghe.example.com"}, http.StatusCreated)
		if created.Host != "https://ghe.example.com" || created.CommitEmail != "octocat@ghe.example.com" {
			t.Errorf("created = %+v, want a GHES signer committing under its address", created)
		}
		assertOneFault(t, doJSON(t, s, http.MethodPatch, commitSignerPath(""), map[string]any{"email": nil}),
			http.StatusUnprocessableEntity, "INVALID_FIELD", "email")
		if got := mustSigner(t, s, http.MethodPatch, "", map[string]any{"email": "bot@ghe.example.com"}, http.StatusOK); got.CommitEmail != "bot@ghe.example.com" {
			t.Errorf("after the change commits carry %q, want the new address", got.CommitEmail)
		}
	})

	t.Run("no signer", func(t *testing.T) {
		s, _ := newSignerServer(t, "https://github.com")
		assertOneFault(t, doJSON(t, s, http.MethodPatch, commitSignerPath(""), map[string]any{"email": "a@b.example"}),
			http.StatusNotFound, "NOT_FOUND", "")
	})
}

// registerActiveSigner registers octocat and verifies it, returning the
// active signer.
func registerActiveSigner(t *testing.T, s *Server, gh *signerGitHub) commitSignerJSON {
	t.Helper()
	created := mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)
	gh.addKey(583231, created.PublicKey)
	return mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK)
}

// TestCommitSigner_RotationPromotesOnVerify: a rotation stages a new key while
// the current one keeps signing; Verify promotes the new key only once it is on
// the account, and the old private half is destroyed.
func TestCommitSigner_RotationPromotesOnVerify(t *testing.T) {
	s, gh := newSignerServer(t, "https://github.com")
	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath("/rotate"), nil), http.StatusNotFound, "NOT_FOUND", "")

	active := registerActiveSigner(t, s, gh)
	oldPrivate := localSecret(t, s, integrations.KeyGitHubCommitSigningKey)

	first := mustSigner(t, s, http.MethodPost, "/rotate", nil, http.StatusOK)
	rotated := mustSigner(t, s, http.MethodPost, "/rotate", nil, http.StatusOK)
	if rotated.PendingPublicKey == nil || *rotated.PendingPublicKey == *first.PendingPublicKey {
		t.Fatalf("second rotation = %+v, want it to replace the first one's pending key", rotated)
	}
	newKey := *rotated.PendingPublicKey
	if rotated.Status != "active" || rotated.PublicKey != active.PublicKey {
		t.Errorf("rotated = %+v, want the current key still active", rotated)
	}
	if got := publicHalf(t, localSecret(t, s, integrations.KeyGitHubCommitSigningKeyPending)); got != newKey {
		t.Errorf("pending secret's public half = %q, want the pending key %q", got, newKey)
	}
	if localSecret(t, s, integrations.KeyGitHubCommitSigningKey) != oldPrivate {
		t.Error("staging a rotation replaced the key that signs")
	}

	// Only the old key is on the account: Verify is looking for the new one.
	rec := doJSON(t, s, http.MethodPost, commitSignerPath("/verify"), nil)
	assertOneFault(t, rec, http.StatusConflict, "KEY_NOT_FOUND", "")
	if msg := decodeErrorItems(t, rec)[0].Message; !strings.Contains(msg, "new key") {
		t.Errorf("KEY_NOT_FOUND message %q does not say it is the new key that is missing", msg)
	}
	if got := mustSigner(t, s, http.MethodGet, "", nil, http.StatusOK); got.PublicKey != active.PublicKey || got.PendingPublicKey == nil {
		t.Errorf("after a failed verify = %+v, want the rotation still pending", got)
	}

	gh.addKey(583231, newKey)
	promoted := mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK)
	if promoted.PublicKey != newKey || promoted.PendingPublicKey != nil || promoted.Status != "active" {
		t.Errorf("promoted = %+v, want the new key signing and no rotation", promoted)
	}
	signing := localSecret(t, s, integrations.KeyGitHubCommitSigningKey)
	if signing == oldPrivate || publicHalf(t, signing) != newKey {
		t.Error("the signing secret is not the new key's private half")
	}
	if pending := localSecret(t, s, integrations.KeyGitHubCommitSigningKeyPending); pending != "" {
		t.Error("the pending secret outlived the promotion")
	}
	want := []string{"credential_set registered", "credential_set verified", "credential_set rotated",
		"credential_set rotated", "credential_set verified"}
	if got := signerAuditRows(t, s); !slices.Equal(got, want) {
		t.Errorf("access log = %v, want %v", got, want)
	}
}

// breakSigner marks the signer broken as the post-push check does, and records
// the failing commit that broke it, signed with key in acme/app.
func breakSigner(t *testing.T, s *Server, reason, key string) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `UPDATE org_commit_signers SET status = 'broken', status_reason = ? WHERE org_id = ?`,
		reason, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("break signer: %v", err)
	}
	repo, err := s.allStores.Repos.FindOrCreateByRefSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRefFromSlug("https://github.com", "acme/app"))
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO commit_signatures (id, org_id, boot_epoch, signer, signer_host,
			signer_user_id, public_key, digest, state, signature, signed_at, commit_sha, repository_id, pushed_at,
			verification_reason, checked_at, created_at)
		VALUES (?, ?, 1, 'org', 'https://github.com', 583231, ?, 'ab', 'signed', 'sig', ?, ?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), runmode.LocalDefaultOrgID, key, now, "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		repo.ID, now, reason, now, now); err != nil {
		t.Fatalf("seed failing signature: %v", err)
	}
}

// TestCommitSigner_VerifyRereadsTheFailingCommit: for a signer the post-push
// check broke, Verify also re-reads the commit that failed, which GitHub
// evaluates again on every read, and the signer stays broken while it still
// fails.
func TestCommitSigner_VerifyRereadsTheFailingCommit(t *testing.T) {
	s, gh := newSignerServer(t, "https://github.com")
	checks := signerChecks(t)
	active := registerActiveSigner(t, s, gh)
	breakSigner(t, s, "unverified_email", active.PublicKey)

	gh.setCommit(&ghclient.CommitVerification{Reason: "unverified_email", CommitterEmail: active.CommitEmail})
	rec := doJSON(t, s, http.MethodPost, commitSignerPath("/verify"), nil)
	assertOneFault(t, rec, http.StatusConflict, "COMMIT_UNVERIFIED", "")
	if msg := decodeErrorItems(t, rec)[0].Message; !strings.Contains(msg, "unverified_email") {
		t.Errorf("COMMIT_UNVERIFIED message %q does not carry GitHub's reason", msg)
	}
	if got := mustSigner(t, s, http.MethodGet, "", nil, http.StatusOK); got.Status != "broken" {
		t.Errorf("status = %q after a failing re-read, want broken", got.Status)
	}

	// The admin verifies the address on the account; GitHub now reads the
	// same commit as valid.
	gh.setCommit(&ghclient.CommitVerification{Verified: true, Reason: "valid", CommitterEmail: active.CommitEmail})
	if got := mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK); got.Status != "active" || got.StatusReason != nil {
		t.Errorf("verified = %+v, want active with no reason", got)
	}
	if got, want := checks(), map[string]int64{"verify valid": 2, "verify unverified_email": 1}; !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
}

// TestCommitSigner_VerifySkipsAFailureAlreadyReplaced: a failing commit that
// cannot say anything about the signer as it is now does not hold it broken —
// one committed under an address the signer no longer uses, one signed with a
// key a rotation replaced, one GitHub no longer has — and a signer broken for
// a missing key has no commit to re-read.
func TestCommitSigner_VerifySkipsAFailureAlreadyReplaced(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange breaks the signer and returns how many commit reads Verify
		// should make.
		arrange func(t *testing.T, s *Server, gh *signerGitHub, active commitSignerJSON) int
	}{
		{"the address changed", func(t *testing.T, s *Server, gh *signerGitHub, active commitSignerJSON) int {
			breakSigner(t, s, "no_user", active.PublicKey)
			gh.setCommit(&ghclient.CommitVerification{Reason: "no_user", CommitterEmail: active.CommitEmail})
			mustSigner(t, s, http.MethodPatch, "", map[string]any{"email": "bot@acme.example"}, http.StatusOK)
			return 1
		}},
		{"the key was rotated", func(t *testing.T, s *Server, gh *signerGitHub, active commitSignerJSON) int {
			breakSigner(t, s, "unknown_key", active.PublicKey)
			gh.setCommit(&ghclient.CommitVerification{Reason: "unknown_key", CommitterEmail: active.CommitEmail})
			rotated := mustSigner(t, s, http.MethodPost, "/rotate", nil, http.StatusOK)
			gh.addKey(583231, *rotated.PendingPublicKey)
			return 0
		}},
		{"the commit is gone", func(t *testing.T, s *Server, gh *signerGitHub, active commitSignerJSON) int {
			breakSigner(t, s, "no_user", active.PublicKey)
			gh.setCommit(nil)
			return 1
		}},
		{"the key went missing", func(t *testing.T, s *Server, gh *signerGitHub, active commitSignerJSON) int {
			breakSigner(t, s, domain.CommitSignerReasonKeyMissing, active.PublicKey)
			gh.setCommit(&ghclient.CommitVerification{Reason: "unknown_key", CommitterEmail: active.CommitEmail})
			return 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, gh := newSignerServer(t, "https://github.com")
			active := registerActiveSigner(t, s, gh)
			wantReads := tc.arrange(t, s, gh, active)
			if got := mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK); got.Status != "active" {
				t.Errorf("status = %q, want active", got.Status)
			}
			if got := gh.reads(); got != wantReads {
				t.Errorf("commit reads = %d, want %d", got, wantReads)
			}
		})
	}
}

// TestCommitSigner_PostPushBreakClearedByVerify follows a signer from the
// post-push check to Verify: a pushed commit GitHub reads as unverified_email
// breaks the signer, Verify refuses while that commit still fails, and once
// the address is verified on the account, Verify's re-read of the same commit
// clears it.
func TestCommitSigner_PostPushBreakClearedByVerify(t *testing.T) {
	s, gh := newSignerServer(t, "https://github.com")
	checks := signerChecks(t)
	active := registerActiveSigner(t, s, gh)
	ctx := t.Context()
	orgID := runmode.LocalDefaultOrgID

	conversationID := uuid.New().String()
	dbtest.SeedConversation(t, s.db, domain.Conversation{ID: conversationID})
	claimID := dbtest.SeedActiveClaim(t, s.db, conversationID, "executor-1", 1)
	at := time.Now().UTC()
	sig, err := s.allStores.CommitSignatures.InsertSystem(ctx, domain.CommitSignature{
		OrgID: orgID, ConversationID: conversationID, ClaimID: claimID, BootEpoch: 1,
		Signer: domain.CommitSigningOrg, SignerHost: "https://github.com", SignerUserID: 583231,
		PublicKey: active.PublicKey, Digest: "ab", State: domain.CommitSignatureSigned,
		Signature: "sig", SignedAt: &at, CommitSHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
	})
	if err != nil {
		t.Fatalf("InsertSystem: %v", err)
	}
	repo, err := s.allStores.Repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRefFromSlug("https://github.com", "acme/app"))
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if _, err := s.allStores.CommitSignatures.MarkPushedSystem(ctx, orgID, sig.ID, repo.ID); err != nil {
		t.Fatalf("MarkPushedSystem: %v", err)
	}

	gh.setCommit(&ghclient.CommitVerification{Reason: "unverified_email", CommitterEmail: active.CommitEmail})
	(&commitsigner.HealthChecker{Stores: s.allStores, GitHub: s.ghResolver}).Check(ctx, orgID)
	broken := mustSigner(t, s, http.MethodGet, "", nil, http.StatusOK)
	if broken.Status != "broken" || broken.StatusReason == nil || *broken.StatusReason != "unverified_email" {
		t.Fatalf("after the post-push check = %+v, want broken for unverified_email", broken)
	}

	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath("/verify"), nil), http.StatusConflict, "COMMIT_UNVERIFIED", "")

	// GitHub's verification service failing is no verdict: Verify asks for a
	// retry and the signer stays broken.
	gh.setCommit(&ghclient.CommitVerification{Reason: "gpgverify_unavailable", CommitterEmail: active.CommitEmail})
	assertOneFault(t, doJSON(t, s, http.MethodPost, commitSignerPath("/verify"), nil), http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
	if got := mustSigner(t, s, http.MethodGet, "", nil, http.StatusOK); got.Status != "broken" {
		t.Errorf("after an unavailable verification the signer is %q, want broken", got.Status)
	}

	gh.setCommit(&ghclient.CommitVerification{Verified: true, Reason: "valid", CommitterEmail: active.CommitEmail})
	if got := mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK); got.Status != "active" || got.StatusReason != nil {
		t.Errorf("verified = %+v, want active with no reason", got)
	}
	want := map[string]int64{"verify valid": 2, "post_push unverified_email": 1, "verify unverified_email": 1, "verify error": 1}
	if got := checks(); !maps.Equal(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
}

// TestCommitSigner_Delete removes the signer and both private keys, and the
// org can register again.
func TestCommitSigner_Delete(t *testing.T) {
	s, gh := newSignerServer(t, "https://github.com")
	// The org's own credential, which keeps its host once the signer goes.
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubPAT: "ghp_bound"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	assertOneFault(t, doJSON(t, s, http.MethodDelete, commitSignerPath(""), nil), http.StatusNotFound, "NOT_FOUND", "")

	registerActiveSigner(t, s, gh)
	mustSigner(t, s, http.MethodPost, "/rotate", nil, http.StatusOK)
	if rec := doJSON(t, s, http.MethodDelete, commitSignerPath(""), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, commitSignerPath(""), nil), http.StatusNotFound, "NOT_FOUND", "")
	for _, key := range commitSigningKeys {
		if localSecret(t, s, key) != "" {
			t.Errorf("%s outlived the signer", key)
		}
	}
	if got := commitIdentity(t, s); got.Signing != "none" || got.Identity == nil || got.Identity.Name != "acme-tf[bot]" {
		t.Errorf("identity after removal = %+v, want the bot again", got)
	}
	rows := signerAuditRows(t, s)
	if len(rows) == 0 || rows[len(rows)-1] != "credential_removed" {
		t.Errorf("access log = %v, want it to end with the removal", rows)
	}
	mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)
}

// TestCommitSigner_HoldsTheGitHubHost: a signer is a connected credential for
// the org's host, so a settings save cannot move it, and disconnecting the
// org's token leaves it in place.
func TestCommitSigner_HoldsTheGitHubHost(t *testing.T) {
	s, _ := newSignerServer(t, "https://github.com")
	mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)

	rec := patchOrgSettings(t, s, map[string]any{"github_base_url": "https://ghe.example.com"})
	assertOneFault(t, rec, http.StatusConflict, "CONFLICT", "github_base_url")
	if msg := decodeErrorItems(t, rec)[0].Message; !strings.Contains(msg, "commit signer") {
		t.Errorf("refusal %q does not name the commit signer", msg)
	}

	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubPAT: "ghp_bound"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	if rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat", nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE pat = %d; body=%s", rec.Code, rec.Body.String())
	}
	if host := seedRepoHost(t, s); host != "https://github.com" {
		t.Errorf("host after the token's disconnect = %q, want it kept for the signer", host)
	}

	// The signer was the last thing holding the host, so removing it clears
	// the host the token's disconnect left: the same state as removing the
	// two in the other order.
	if rec := doJSON(t, s, http.MethodDelete, commitSignerPath(""), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE signer = %d; body=%s", rec.Code, rec.Body.String())
	}
	if host := seedRepoHost(t, s); host != "" {
		t.Errorf("host after the signer's removal = %q, want it cleared: nothing is connected", host)
	}
}

// TestCommitSigner_DeleteKeepsAHostStillInUse: removing the signer clears the
// host only when nothing else is connected on it.
func TestCommitSigner_DeleteKeepsAHostStillInUse(t *testing.T) {
	s, _ := newSignerServer(t, "https://github.com")
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{GitHubPAT: "ghp_bound"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)
	if rec := doJSON(t, s, http.MethodDelete, commitSignerPath(""), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE signer = %d; body=%s", rec.Code, rec.Body.String())
	}
	if host := seedRepoHost(t, s); host != "https://github.com" {
		t.Errorf("host after the signer's removal = %q, want it kept for the token", host)
	}
}

// TestCommitSigner_AppDisconnectKeepsTheHost: disconnecting the org's own App
// clears the host only when nothing else uses it, and a signer does.
func TestCommitSigner_AppDisconnectKeepsTheHost(t *testing.T) {
	s, _ := newSignerServer(t, "https://github.com")
	mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)
	seedLocalApp(t, s, true)

	if rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/disconnect", nil); rec.Code != http.StatusOK {
		t.Fatalf("POST github/app/disconnect = %d; body=%s", rec.Code, rec.Body.String())
	}
	if host := seedRepoHost(t, s); host != "https://github.com" {
		t.Errorf("host after the App's disconnect = %q, want it kept for the signer", host)
	}
	mustSigner(t, s, http.MethodGet, "", nil, http.StatusOK)
}

// A local run signs with the host's git, which signs with an SSH key only from
// 2.34. With an older one, an org whose signer is active commits as the
// signer, unsigned, and commit-identity says why.
func TestCommitIdentity_LocalGitTooOldToSign(t *testing.T) {
	s, gh := newSignerServer(t, "https://github.com")
	created := mustSigner(t, s, http.MethodPost, "", map[string]any{"login": "octocat"}, http.StatusCreated)
	gh.addKey(583231, created.PublicKey)
	mustSigner(t, s, http.MethodPost, "/verify", nil, http.StatusOK)

	if got := commitIdentity(t, s); got.Signing != "org" || got.UnsignedReason != nil || got.GitVersion != nil {
		t.Errorf("identity with a git that signs = %+v, want org with no unsigned reason", got)
	}

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.33.8'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	got := commitIdentity(t, s)
	if got.Signing != "none" || got.Identity == nil || got.Identity.Name != "The Octocat" {
		t.Errorf("identity with git 2.33 = %+v, want the signer, unsigned", got)
	}
	if got.UnsignedReason == nil || *got.UnsignedReason != "git_too_old" || got.GitVersion == nil || *got.GitVersion != "2.33.8" {
		t.Errorf("unsigned reason = %v, git version = %v; want git_too_old and 2.33.8", got.UnsignedReason, got.GitVersion)
	}
}
