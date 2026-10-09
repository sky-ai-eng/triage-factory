package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The Anthropic and Bedrock credential routes write their key ref through
// OrgsStore.SetAnthropicKeyRef / SetBedrockCredentialsRef and nothing else on
// the settings row. The tests below pin, for each of them: a settings save
// loaded before the credential write conflicts instead of landing on top of
// it, the credential route itself never conflicts, a write that races the
// other provider's leaves that provider's ref alone, and in local mode a write
// that fails after its keychain writes puts every key back.

// failingLLMRefTx fails the ref write, the last write each of these handlers
// makes before its audit row, so every keychain write before it has already
// landed when the transaction rolls back.
type failingLLMRefTx struct{ db.TxRunner }

func (w failingLLMRefTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.TxRunner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Orgs = failingLLMRefOrgs{OrgsStore: tx.Orgs}
		return fn(tx)
	})
}

type failingLLMRefOrgs struct{ db.OrgsStore }

func (failingLLMRefOrgs) SetAnthropicKeyRef(context.Context, string, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("ref write failed")
}

func (failingLLMRefOrgs) SetBedrockCredentialsRef(context.Context, string, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, errors.New("ref write failed")
}

// racingSettingsTx runs *race once, inside the next transaction that reads the
// org's settings and right after that read: the write a concurrent request
// would commit between this request's read and its write. A nil *race runs
// nothing.
type racingSettingsTx struct {
	db.TxRunner
	race *func(ctx context.Context, orgs db.OrgsStore) error
}

func (w racingSettingsTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.TxRunner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.Orgs = racingSettingsOrgs{OrgsStore: tx.Orgs, race: w.race}
		return fn(tx)
	})
}

type racingSettingsOrgs struct {
	db.OrgsStore
	race *func(ctx context.Context, orgs db.OrgsStore) error
}

func (o racingSettingsOrgs) GetSettings(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	set, err := o.OrgsStore.GetSettings(ctx, orgID)
	if err != nil || *o.race == nil {
		return set, err
	}
	race := *o.race
	*o.race = nil
	return set, race(ctx, o.OrgsStore)
}

// llmWriteCase is one credential route that writes an LLM key ref.
type llmWriteCase struct {
	name string
	// seed binds the material the call replaces or removes, writing straight
	// to the stores so it works on a server whose transactions fail.
	seed func(t *testing.T, s *Server)
	call func(t *testing.T, s *Server) *httptest.ResponseRecorder
	// keys is every key the call writes in the keychain.
	keys []string
	// ref reads the ref the call writes; wantRef is that ref after the call.
	ref     func(domain.OrgSettings) string
	wantRef string
}

func anthropicRef(o domain.OrgSettings) string { return o.AnthropicAPIKeyRef }

func bedrockRef(o domain.OrgSettings) string { return o.BedrockCredentialsRef }

func seedSecrets(t *testing.T, s *Server, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		if err := s.secrets.Put(t.Context(), runmode.LocalDefaultOrgID, k, v, ""); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
}

func seedAnthropicBound(t *testing.T, s *Server) {
	t.Helper()
	seedSecrets(t, s, map[string]string{secretKeyAnthropicAPIKey: "sk-ant-old"})
	if _, err := s.orgs.SetAnthropicKeyRef(t.Context(), runmode.LocalDefaultOrgID, secretKeyAnthropicAPIKey); err != nil {
		t.Fatalf("seed anthropic ref: %v", err)
	}
}

// seedBedrockBound binds one Bedrock shape: ref is its marker key, kv its
// stored keys.
func seedBedrockBound(t *testing.T, s *Server, ref string, kv map[string]string) {
	t.Helper()
	seedSecrets(t, s, kv)
	if _, err := s.orgs.SetBedrockCredentialsRef(t.Context(), runmode.LocalDefaultOrgID, ref); err != nil {
		t.Fatalf("seed bedrock ref: %v", err)
	}
}

func seedBedrockBearer(t *testing.T, s *Server) {
	seedBedrockBound(t, s, integrations.KeyAWSBearerTokenBedrock, map[string]string{
		integrations.KeyAWSBearerTokenBedrock: "bdrk-old",
		integrations.KeyAWSRegion:             "us-west-2",
		integrations.KeyBedrockModelID:        "old-model",
	})
}

func seedBedrockAccessKeys(t *testing.T, s *Server) {
	seedBedrockBound(t, s, integrations.KeyAWSAccessKeyID, map[string]string{
		integrations.KeyAWSAccessKeyID:     "AKIAOLD",
		integrations.KeyAWSSecretAccessKey: "old-secret",
		integrations.KeyAWSSessionToken:    "old-session",
		integrations.KeyAWSRegion:          "us-west-2",
	})
}

func seedBedrockRole(t *testing.T, s *Server) {
	seedBedrockBound(t, s, integrations.KeyAWSRoleARN, map[string]string{
		integrations.KeyAWSRoleARN:     "arn:aws:iam::111122223333:role/old",
		integrations.KeyAWSExternalID:  "tf-old",
		integrations.KeyAWSRegion:      "us-west-2",
		integrations.KeyBedrockBaseURL: "https://bedrock.example.com",
	})
}

func llmWriteCases() []llmWriteCase {
	return []llmWriteCase{
		{
			name: "anthropic bind",
			// The org holds Bedrock material, which the bind leaves alone.
			seed: seedBedrockBearer,
			call: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
				return doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": "sk-ant-new"})
			},
			keys:    []string{secretKeyAnthropicAPIKey},
			ref:     anthropicRef,
			wantRef: secretKeyAnthropicAPIKey,
		},
		{
			name: "anthropic unbind",
			seed: seedAnthropicBound,
			call: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return doJSON(t, s, http.MethodDelete, llmPath("anthropic"), nil)
			},
			keys:    []string{secretKeyAnthropicAPIKey},
			ref:     anthropicRef,
			wantRef: "",
		},
		{
			name: "bedrock access-keys bind",
			seed: seedBedrockBearer,
			call: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return putBedrock(t, s, "access-keys", map[string]any{
					"access_key_id": "AKIANEW", "secret_access_key": "new-secret",
					"session_token": "new-session", "region": "us-east-1",
				})
			},
			keys:    integrations.BedrockKeys(),
			ref:     bedrockRef,
			wantRef: integrations.KeyAWSAccessKeyID,
		},
		{
			name: "bedrock bearer bind",
			seed: seedBedrockAccessKeys,
			call: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return putBedrock(t, s, "bearer", map[string]any{
					"bearer_token": "bdrk-new", "region": "us-east-1", "base_url": "https://vpce.example.com",
				})
			},
			keys:    integrations.BedrockKeys(),
			ref:     bedrockRef,
			wantRef: integrations.KeyAWSBearerTokenBedrock,
		},
		{
			name: "bedrock role bind",
			seed: func(t *testing.T, s *Server) {
				s.SetBedrockRoleResolver(&fakeRoleResolver{})
				seedBedrockBearer(t, s)
				// Stored before the call, so the External ID the role bind
				// ensures ahead of its transaction is one this test already
				// counts as prior state.
				seedSecrets(t, s, map[string]string{integrations.KeyAWSExternalID: "tf-seeded"})
			},
			call: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return putBedrock(t, s, "role", map[string]any{
					"role_arn": "arn:aws:iam::111122223333:role/tf-bedrock", "region": "us-east-1",
				})
			},
			keys:    integrations.BedrockKeys(),
			ref:     bedrockRef,
			wantRef: integrations.KeyAWSRoleARN,
		},
		{
			name: "bedrock unbind",
			seed: seedBedrockRole,
			call: func(t *testing.T, s *Server) *httptest.ResponseRecorder {
				return doJSON(t, s, http.MethodDelete, llmPath("bedrock"), nil)
			},
			keys:    integrations.BedrockKeys(),
			ref:     bedrockRef,
			wantRef: "",
		},
	}
}

func localOrgSettings(t *testing.T, s *Server) domain.OrgSettings {
	t.Helper()
	set, err := s.orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	return set
}

// TestLLMCredentialWrite_StaleSettingsSaveConflicts: a settings save loaded
// before the credential write answers 409 VERSION_CONFLICT and writes nothing,
// while the credential route, which takes no version, succeeds over a settings
// save it never saw.
func TestLLMCredentialWrite_StaleSettingsSaveConflicts(t *testing.T) {
	for _, c := range llmWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			c.seed(t, s)

			// Another admin's settings save lands first.
			if rec := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
				"version": orgSettingsVersion(t, s), "max_concurrent_runs": 3,
			}); rec.Code != http.StatusOK {
				t.Fatalf("settings save: %d: %s", rec.Code, rec.Body.String())
			}
			loaded := orgSettingsVersion(t, s)

			rec := c.call(t, s)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), httpx.ReasonVersionConflict) {
				t.Fatalf("credential route = %d %s, want 200", rec.Code, rec.Body.String())
			}

			stale := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
				"version": loaded, "max_concurrent_runs": 9,
			})
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), httpx.ReasonVersionConflict) {
				t.Errorf("settings save loaded before the credential write = %d %s, want 409 %s", stale.Code, stale.Body.String(), httpx.ReasonVersionConflict)
			}
			set := localOrgSettings(t, s)
			if set.MaxConcurrentRuns != 3 {
				t.Errorf("max_concurrent_runs = %d, want the earlier save's 3 and not the refused save's 9", set.MaxConcurrentRuns)
			}
			if got := c.ref(set); got != c.wantRef {
				t.Errorf("ref = %q after the credential write, want %q", got, c.wantRef)
			}
		})
	}
}

// TestLLMCredentialBind_StaleHostCredentialsSaveConflicts: the settings save
// refuses the host's credentials while a ref is set, and it checks that
// against the row it read. A bind that lands between that read and the save's
// write moves the version, so the save answers 409 rather than storing
// "system" beside the credential the bind just stored.
func TestLLMCredentialBind_StaleHostCredentialsSaveConflicts(t *testing.T) {
	for _, c := range []struct {
		name string
		bind func(ctx context.Context, orgs db.OrgsStore) error
	}{
		{"anthropic", func(ctx context.Context, orgs db.OrgsStore) error {
			_, err := orgs.SetAnthropicKeyRef(ctx, runmode.LocalDefaultOrgID, secretKeyAnthropicAPIKey)
			return err
		}},
		{"bedrock", func(ctx context.Context, orgs db.OrgsStore) error {
			_, err := orgs.SetBedrockCredentialsRef(ctx, runmode.LocalDefaultOrgID, integrations.KeyAWSBearerTokenBedrock)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			var race func(ctx context.Context, orgs db.OrgsStore) error
			s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner { return racingSettingsTx{TxRunner: tx, race: &race} })
			// Bring-your-own selected and nothing bound yet, so the save's
			// move to the host's credentials passes the check it reads.
			patchOrgSettingsOK(t, s, map[string]any{"llm_auth_method": domain.LLMAuthBYOK})

			loaded := orgSettingsVersion(t, s)
			race = c.bind
			rec := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
				"version": loaded, "llm_auth_method": domain.LLMAuthSystem,
			})
			if race != nil {
				t.Fatal("the settings save never read the row the bind raced")
			}
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), httpx.ReasonVersionConflict) {
				t.Errorf("settings save raced by a bind = %d %s, want 409 %s", rec.Code, rec.Body.String(), httpx.ReasonVersionConflict)
			}
			// The simulated bind rides the save's own transaction, which the
			// conflict rolls back with it, so only the save's own write is
			// checked here.
			if got := localOrgSettings(t, s).LLMAuthMethod; got != domain.LLMAuthBYOK {
				t.Errorf("llm_auth_method = %q after the refused save, want %q", got, domain.LLMAuthBYOK)
			}
		})
	}
}

// TestLLMCredentialRotation_UnchangedRefLeavesSettingsVersion: rotating a key
// under the shape the org already holds changes no ref and no auth method, so
// a settings save loaded before the rotation still lands.
func TestLLMCredentialRotation_UnchangedRefLeavesSettingsVersion(t *testing.T) {
	for _, c := range []struct {
		name  string
		bind  func(t *testing.T, s *Server, secret string) *httptest.ResponseRecorder
		key   string
		setup func(t *testing.T)
	}{
		{"anthropic", func(t *testing.T, s *Server, secret string) *httptest.ResponseRecorder {
			return doJSON(t, s, http.MethodPut, llmPath("anthropic"), map[string]any{"api_key": secret})
		}, secretKeyAnthropicAPIKey, func(t *testing.T) {
			auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
		}},
		{"bedrock bearer", func(t *testing.T, s *Server, secret string) *httptest.ResponseRecorder {
			return putBedrock(t, s, "bearer", map[string]any{"bearer_token": secret, "region": "us-east-1"})
		}, integrations.KeyAWSBearerTokenBedrock, func(*testing.T) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			c.setup(t)
			if rec := c.bind(t, s, "first"); rec.Code != http.StatusOK {
				t.Fatalf("bind: %d: %s", rec.Code, rec.Body.String())
			}
			loaded := orgSettingsVersion(t, s)

			if rec := c.bind(t, s, "rotated"); rec.Code != http.StatusOK {
				t.Fatalf("rotation: %d: %s", rec.Code, rec.Body.String())
			}

			if v := orgSettingsVersion(t, s); v != loaded {
				t.Errorf("settings version = %d after a rotation under the same ref, want %d unchanged", v, loaded)
			}
			if rec := doJSON(t, s, http.MethodPatch, orgSettingsPath(), map[string]any{
				"version": loaded, "max_concurrent_runs": 3,
			}); rec.Code != http.StatusOK {
				t.Errorf("settings save loaded before the rotation = %d %s, want 200", rec.Code, rec.Body.String())
			}
			if v := getSecret(t, s, c.key); v != "rotated" {
				t.Errorf("stored key = %q, want the rotated one", v)
			}
		})
	}
}

// TestLLMCredentialWrite_RacingProviderRefsSurvive: each provider's route
// writes only its own ref, so one that lands between the other's read and its
// write survives it. An Anthropic unbind that read the row before a Bedrock
// bind committed does not clear the Bedrock ref, and a Bedrock bind that read
// the row before an Anthropic unbind committed does not put the Anthropic ref
// back.
func TestLLMCredentialWrite_RacingProviderRefsSurvive(t *testing.T) {
	newRacingServer := func(t *testing.T) (*Server, *func(ctx context.Context, orgs db.OrgsStore) error) {
		t.Helper()
		runmode.SetForTest(t, runmode.ModeLocal)
		keyring.MockInit()
		var race func(ctx context.Context, orgs db.OrgsStore) error
		s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner { return racingSettingsTx{TxRunner: tx, race: &race} })
		return s, &race
	}

	t.Run("anthropic unbind raced by a bedrock bind", func(t *testing.T) {
		s, race := newRacingServer(t)
		seedAnthropicBound(t, s)
		*race = func(ctx context.Context, orgs db.OrgsStore) error {
			_, err := orgs.SetBedrockCredentialsRef(ctx, runmode.LocalDefaultOrgID, integrations.KeyAWSBearerTokenBedrock)
			return err
		}
		if rec := doJSON(t, s, http.MethodDelete, llmPath("anthropic"), nil); rec.Code != http.StatusOK {
			t.Fatalf("anthropic unbind: %d: %s", rec.Code, rec.Body.String())
		}
		if *race != nil {
			t.Fatal("the unbind never read the row the bind raced")
		}
		set := localOrgSettings(t, s)
		if set.AnthropicAPIKeyRef != "" || set.BedrockCredentialsRef != integrations.KeyAWSBearerTokenBedrock {
			t.Errorf("refs = (anthropic %q, bedrock %q), want the anthropic ref cleared and the bedrock bind's kept", set.AnthropicAPIKeyRef, set.BedrockCredentialsRef)
		}
		if set.LLMAuthMethod != domain.LLMAuthBYOK {
			t.Errorf("llm_auth_method = %q, want the bedrock bind's %q", set.LLMAuthMethod, domain.LLMAuthBYOK)
		}
	})

	t.Run("bedrock bind raced by an anthropic unbind", func(t *testing.T) {
		s, race := newRacingServer(t)
		seedAnthropicBound(t, s)
		*race = func(ctx context.Context, orgs db.OrgsStore) error {
			_, err := orgs.SetAnthropicKeyRef(ctx, runmode.LocalDefaultOrgID, "")
			return err
		}
		if rec := putBedrock(t, s, "bearer", map[string]any{"bearer_token": "bdrk-1", "region": "us-east-1"}); rec.Code != http.StatusOK {
			t.Fatalf("bedrock bind: %d: %s", rec.Code, rec.Body.String())
		}
		if *race != nil {
			t.Fatal("the bind never read the row the unbind raced")
		}
		set := localOrgSettings(t, s)
		if set.AnthropicAPIKeyRef != "" || set.BedrockCredentialsRef != integrations.KeyAWSBearerTokenBedrock {
			t.Errorf("refs = (anthropic %q, bedrock %q), want the anthropic unbind's clear kept and the bedrock ref set", set.AnthropicAPIKeyRef, set.BedrockCredentialsRef)
		}
	})
}

// TestLLMCredentialWrite_FailedWriteRestoresKeys: in local mode, a write whose
// transaction fails after its keychain writes answers 500 and leaves the org
// as it was — every key put back, the ref unchanged, and no audit row.
func TestLLMCredentialWrite_FailedWriteRestoresKeys(t *testing.T) {
	for _, c := range llmWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner { return failingLLMRefTx{TxRunner: tx} })
			c.seed(t, s)
			before := storedKeys(t, s, c.keys)
			ref := c.ref(localOrgSettings(t, s))
			rows := len(credAuditRows(t, s))

			rec := c.call(t, s)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("credential route with a failing ref write = %d %s, want 500", rec.Code, rec.Body.String())
			}
			for k, v := range storedKeys(t, s, c.keys) {
				if v != before[k] {
					t.Errorf("%s = %q after the failed write, want %q", k, v, before[k])
				}
			}
			if got := c.ref(localOrgSettings(t, s)); got != ref {
				t.Errorf("ref = %q after the failed write, want %q", got, ref)
			}
			if got := len(credAuditRows(t, s)); got != rows {
				t.Errorf("credential audit rows = %d after the failed write, want still %d", got, rows)
			}
		})
	}
}

// TestLLMCredentialWrite_UnreadableSnapshotRefuses: in local mode a key the
// snapshot cannot read is one a failed write could not put back, so the route
// refuses before touching anything.
func TestLLMCredentialWrite_UnreadableSnapshotRefuses(t *testing.T) {
	for _, c := range llmWriteCases() {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			c.seed(t, s)
			before := storedKeys(t, s, c.keys)
			ref := c.ref(localOrgSettings(t, s))
			secrets := s.secrets
			s.secrets = getFailingSecrets{SecretStore: secrets}

			rec := c.call(t, s)
			s.secrets = secrets
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("credential route with an unreadable snapshot = %d %s, want 500", rec.Code, rec.Body.String())
			}
			for k, v := range storedKeys(t, s, c.keys) {
				if v != before[k] {
					t.Errorf("%s = %q after a refused write, want %q", k, v, before[k])
				}
			}
			if got := c.ref(localOrgSettings(t, s)); got != ref {
				t.Errorf("ref = %q after a refused write, want %q", got, ref)
			}
		})
	}
}

// TestLLMCredentialWrite_MultiMode_StaleSettingsSaveConflicts runs the version
// property on Postgres, through each route's own claims-bound transaction:
// the ref write lands under RLS, and a settings save loaded before it
// conflicts rather than landing on top of it.
func TestLLMCredentialWrite_MultiMode_StaleSettingsSaveConflicts(t *testing.T) {
	for _, c := range []struct {
		name, method, route string
		body                map[string]any
		// bound seeds the material the call replaces or removes.
		bound   func(ctx context.Context, tx db.TxStores, org string) error
		refKey  string
		wantRef string
	}{
		{"anthropic bind", http.MethodPut, "/llm/anthropic", map[string]any{"api_key": "sk-ant-new"},
			nil, "anthropic_api_key_ref", secretKeyAnthropicAPIKey},
		{"anthropic unbind", http.MethodDelete, "/llm/anthropic", nil,
			func(ctx context.Context, tx db.TxStores, org string) error {
				if err := tx.Secrets.Put(ctx, org, secretKeyAnthropicAPIKey, "sk-ant-old", ""); err != nil {
					return err
				}
				_, err := tx.Orgs.SetAnthropicKeyRef(ctx, org, secretKeyAnthropicAPIKey)
				return err
			}, "anthropic_api_key_ref", ""},
		{"bedrock bearer bind", http.MethodPut, "/llm/bedrock/bearer", map[string]any{"bearer_token": "bdrk-new", "region": "us-east-1"},
			nil, "bedrock_credentials_ref", integrations.KeyAWSBearerTokenBedrock},
		{"bedrock unbind", http.MethodDelete, "/llm/bedrock", nil,
			func(ctx context.Context, tx db.TxStores, org string) error {
				for k, v := range map[string]string{integrations.KeyAWSBearerTokenBedrock: "bdrk-old", integrations.KeyAWSRegion: "us-west-2"} {
					if err := tx.Secrets.Put(ctx, org, k, v, ""); err != nil {
						return err
					}
				}
				_, err := tx.Orgs.SetBedrockCredentialsRef(ctx, org, integrations.KeyAWSBearerTokenBedrock)
				return err
			}, "bedrock_credentials_ref", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newAuthRig(t)
			auth.SetAnthropicModelsURLForTest(t, anthropicModelsStub(t, http.StatusOK).URL)
			founder := r.seedUser()
			orgUUID, _ := r.seedOrg(founder, "acme")
			org := orgUUID.String()
			sid := r.signIn(founder)
			ctx := t.Context()
			if c.bound != nil {
				if err := r.srv.tx.WithTx(ctx, org, founder.String(), func(tx db.TxStores) error {
					return c.bound(ctx, tx, org)
				}); err != nil {
					t.Fatalf("seed credential: %v", err)
				}
			}

			settingsPath := "/api/orgs/" + org + "/settings"
			readSettings := func() map[string]any {
				t.Helper()
				rec := r.tokensJSON(http.MethodGet, settingsPath, nil, sid, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("GET settings: %d: %s", rec.Code, rec.Body.String())
				}
				var out map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode settings: %v", err)
				}
				return out
			}
			loaded := readSettings()["version"]

			rec := r.tokensJSON(c.method, "/api/orgs/"+org+c.route, c.body, sid, "")
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), httpx.ReasonVersionConflict) {
				t.Fatalf("%s %s = %d %s, want 200", c.method, c.route, rec.Code, rec.Body.String())
			}
			stale := r.tokensJSON(http.MethodPatch, settingsPath, map[string]any{
				"version": loaded, "max_concurrent_runs": 9,
			}, sid, "")
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), httpx.ReasonVersionConflict) {
				t.Errorf("settings save loaded before the credential write = %d %s, want 409 %s", stale.Code, stale.Body.String(), httpx.ReasonVersionConflict)
			}

			var ref string
			var concurrent sql.NullInt64
			if err := r.h.AdminDB.QueryRowContext(ctx,
				`SELECT COALESCE(`+c.refKey+`, ''), max_concurrent_runs FROM org_settings WHERE org_id = $1`,
				org).Scan(&ref, &concurrent); err != nil {
				t.Fatalf("read org_settings: %v", err)
			}
			if ref != c.wantRef {
				t.Errorf("%s = %q after the credential write, want %q", c.refKey, ref, c.wantRef)
			}
			if concurrent.Valid {
				t.Errorf("the refused save landed: max_concurrent_runs = %d", concurrent.Int64)
			}
		})
	}
}
