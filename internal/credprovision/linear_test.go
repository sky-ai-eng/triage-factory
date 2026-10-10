package credprovision

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
)

// linearSecrets is the org secret scope the Linear resolver reads: a missing
// key reads as "" with no error, as the real store answers.
type linearSecrets struct {
	db.SecretStore
	values map[string]string
}

func (f *linearSecrets) GetSystem(_ context.Context, _, key string) (string, error) {
	return f.values[key], nil
}

// installTokens stands in for linearoauth.TokenCache: the access token an
// app-installed org's refresh would mint now, or the error it would fail with.
type installTokens struct {
	token     string
	expiresAt time.Time
	err       error
	calls     int
}

func (f *installTokens) AccessTokenForOrg(context.Context, string) (string, time.Time, error) {
	f.calls++
	return f.token, f.expiresAt, f.err
}

// linearResolverFor builds the production resolver over a fake secret scope
// and token source, so the tests below exercise its marker dispatch as well
// as the provisioner's mapping of its answer.
func linearResolverFor(values map[string]string, tokens *installTokens) linear.Resolver {
	return linear.NewResolverWithInstall(&linearSecrets{values: values}, nil, tokens)
}

func TestManager_resolveLinear(t *testing.T) {
	ctx := context.Background()
	appInstall := map[string]string{integrations.KeyLinearAuthMethod: string(linear.AuthMethodAppInstall)}

	t.Run("app install seals the cache's access token and its expiry", func(t *testing.T) {
		expires := time.Now().Add(24 * time.Hour).Truncate(time.Second)
		tokens := &installTokens{token: "lin_oauth_minted", expiresAt: expires}
		m := &Manager{linearResolver: linearResolverFor(appInstall, tokens)}

		got, err := m.resolveLinear(ctx, "org-1")
		if err != nil {
			t.Fatalf("resolveLinear: %v", err)
		}
		want := credbundle.LinearCreds{
			AuthMethod:  string(linear.AuthMethodAppInstall),
			AccessToken: "lin_oauth_minted",
			ExpiresUnix: expires.Unix(),
		}
		if got == nil || *got != want {
			t.Fatalf("resolveLinear = %+v, want %+v", got, want)
		}
		if tokens.calls != 1 {
			t.Errorf("token source calls = %d, want 1", tokens.calls)
		}
	})

	t.Run("api key seals the key alone", func(t *testing.T) {
		m := &Manager{linearResolver: linearResolverFor(map[string]string{
			integrations.KeyLinearAuthMethod: string(linear.AuthMethodAPIKey),
			integrations.KeyLinearAPIKey:     "lin_api_key",
		}, &installTokens{})}

		got, err := m.resolveLinear(ctx, "org-1")
		if err != nil {
			t.Fatalf("resolveLinear: %v", err)
		}
		want := credbundle.LinearCreds{AuthMethod: string(linear.AuthMethodAPIKey), APIKey: "lin_api_key"}
		if got == nil || *got != want {
			t.Fatalf("resolveLinear = %+v, want %+v", got, want)
		}
	})

	t.Run("unconfigured org seals nothing", func(t *testing.T) {
		m := &Manager{linearResolver: linearResolverFor(nil, &installTokens{})}
		got, err := m.resolveLinear(ctx, "org-1")
		if err != nil || got != nil {
			t.Fatalf("resolveLinear = (%+v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("revoked install seals nothing", func(t *testing.T) {
		tokens := &installTokens{err: linear.ErrNoLinearSystemCredential}
		m := &Manager{linearResolver: linearResolverFor(appInstall, tokens)}
		got, err := m.resolveLinear(ctx, "org-1")
		if err != nil || got != nil {
			t.Fatalf("resolveLinear = (%+v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("a failed mint fails the provision", func(t *testing.T) {
		mintErr := errors.New("linear token endpoint: 503")
		m := &Manager{linearResolver: linearResolverFor(appInstall, &installTokens{err: mintErr})}
		got, err := m.resolveLinear(ctx, "org-1")
		if !errors.Is(err, mintErr) || got != nil {
			t.Fatalf("resolveLinear = (%+v, %v), want the mint error", got, err)
		}
	})

	t.Run("no resolver wired seals nothing", func(t *testing.T) {
		got, err := (&Manager{}).resolveLinear(ctx, "org-1")
		if err != nil || got != nil {
			t.Fatalf("resolveLinear = (%+v, %v), want (nil, nil)", got, err)
		}
	})
}

// --- the stores a whole provision reads and writes ---

type provisionQueue struct {
	db.ConversationQueueStore
	claim db.AwaitingCredentialsConversation
}

func (f *provisionQueue) GetClaim(context.Context, string, string) (db.AwaitingCredentialsConversation, bool, error) {
	return f.claim, true, nil
}

type provisionInstances struct {
	db.InstanceStore
	inst *domain.Instance
}

func (f *provisionInstances) Get(context.Context, string) (*domain.Instance, error) {
	return f.inst, nil
}

type provisionConversations struct {
	db.ConversationStore
}

func (provisionConversations) GetSystem(_ context.Context, _, conversationID string) (*domain.Conversation, error) {
	return &domain.Conversation{ID: conversationID}, nil
}

type provisionClaimCredentials struct {
	db.ClaimCredentialsStore
	sealed []byte
}

func (f *provisionClaimCredentials) Put(_ context.Context, _, _, _ string, _ int64, sealed []byte, _ []string) error {
	f.sealed = sealed
	return nil
}

type noLLM struct{}

func (noLLM) ResolveForBundle(context.Context, string, string, string) (llmcred.Material, error) {
	return llmcred.Material{}, nil
}

// TestProvisionForConversation_SealsLinear runs a whole provision and opens
// what it wrote with the claim's own key, the way the run's sidecar does: an
// app-installed org's bundle carries an access token that is still live, and
// an org with no Linear credential carries no Linear half at all.
func TestProvisionForConversation_SealsLinear(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		tokens *installTokens
		want   func(t *testing.T, got *credbundle.LinearCreds)
	}{
		{
			name:   "app install",
			values: map[string]string{integrations.KeyLinearAuthMethod: string(linear.AuthMethodAppInstall)},
			tokens: &installTokens{token: "lin_oauth_minted", expiresAt: time.Now().Add(24 * time.Hour)},
			want: func(t *testing.T, got *credbundle.LinearCreds) {
				if got == nil {
					t.Fatal("bundle carries no Linear credential")
				}
				if got.AuthMethod != string(linear.AuthMethodAppInstall) || got.AccessToken != "lin_oauth_minted" || got.APIKey != "" {
					t.Errorf("Linear = %+v, want the minted app-install access token alone", got)
				}
				if got.ExpiresUnix <= time.Now().Unix() {
					t.Errorf("ExpiresUnix = %d, want a time after now", got.ExpiresUnix)
				}
			},
		},
		{
			name:   "unconfigured",
			tokens: &installTokens{},
			want: func(t *testing.T, got *credbundle.LinearCreds) {
				if got != nil {
					t.Errorf("Linear = %+v, want nil for an org with no Linear credential", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kp, err := credseal.GenerateKeyPair()
			if err != nil {
				t.Fatalf("GenerateKeyPair: %v", err)
			}
			claims := &provisionClaimCredentials{}
			secrets := &linearSecrets{values: tc.values}
			m := &Manager{
				stores: db.Stores{
					Secrets: secrets,
					ConversationQueue: &provisionQueue{claim: db.AwaitingCredentialsConversation{
						ConversationID: "conv-1", OrgID: "org-1", ExecutorID: "exec-1", BootEpoch: 4,
						CredPubKey: base64.StdEncoding.EncodeToString(kp.Public[:]),
					}},
					Instances:        &provisionInstances{inst: &domain.Instance{ID: "exec-1", BootEpoch: 4}},
					Conversations:    provisionConversations{},
					ClaimCredentials: claims,
				},
				ghResolver:     &fakeScopedResolver{},
				linearResolver: linear.NewResolverWithInstall(secrets, nil, tc.tokens),
				llm:            noLLM{},
			}

			if err := m.ProvisionForConversation(context.Background(), "org-1", "conv-1"); err != nil {
				t.Fatalf("ProvisionForConversation: %v", err)
			}
			if claims.sealed == nil {
				t.Fatal("no bundle was written")
			}
			plaintext, err := kp.Open(claims.sealed)
			if err != nil {
				t.Fatalf("open the written bundle with the claim's key: %v", err)
			}
			bundle, err := credbundle.Unmarshal(plaintext)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			tc.want(t, bundle.Linear)
		})
	}
}
