package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// connectProviders records which LLM credentials the local org holds, by the
// settings refs that are the record of what is bound — and the credential
// source alongside them, which is what a real bind writes: an org holding its
// own key is not running on the machine's.
func connectProviders(t *testing.T, database *sql.DB, anthropic, bedrock bool) {
	t.Helper()
	store := sqlitestore.New(database).Orgs
	set, err := store.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("get org settings: %v", err)
	}
	if anthropic {
		set.AnthropicAPIKeyRef = "anthropic_api_key"
	}
	if bedrock {
		set.BedrockCredentialsRef = "aws_bearer_token_bedrock"
	}
	if anthropic || bedrock {
		set.LLMAuthMethod = domain.LLMAuthBYOK
	}
	if _, err := store.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("connect providers: %v", err)
	}
}

// setAuthMethod records the org's credential source without binding anything.
func setAuthMethod(t *testing.T, database *sql.DB, method string) {
	t.Helper()
	store := sqlitestore.New(database).Orgs
	set, err := store.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("get org settings: %v", err)
	}
	set.LLMAuthMethod = method
	if _, err := store.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("set auth method: %v", err)
	}
}

// modelOn returns an offered model served by provider.
func modelOn(t *testing.T, provider string) string {
	t.Helper()
	for _, e := range modelcatalog.Entries() {
		if e.Provider == provider {
			return e.Key
		}
	}
	t.Fatalf("catalog offers no model on %s", provider)
	return ""
}

// An org holding both credentials dispatches on either provider's models — the
// whole point of concurrent providers.
func TestCheckModelProviders_BothConnected_AllowsEither(t *testing.T) {
	database := newCostCapTestDB(t)
	connectProviders(t, database, true, true)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	models := []string{modelOn(t, modelcatalog.ProviderAnthropic), modelOn(t, modelcatalog.ProviderBedrock)}
	if err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, models); err != nil {
		t.Fatalf("both providers connected must allow either model, got: %v", err)
	}
}

// A dispatch on a model whose provider was never connected is refused, and the
// message names the provider so the admin knows what to go connect. Nothing
// retries: the refusal is returned to Delegate's caller before any row is
// written.
func TestCheckModelProviders_ProviderNotConnected_RefusesByName(t *testing.T) {
	database := newCostCapTestDB(t)
	connectProviders(t, database, true, false)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	bedrockModel := modelOn(t, modelcatalog.ProviderBedrock)
	err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID,
		[]string{domain.ModelSonnet, bedrockModel})
	if !errors.Is(err, modelaccess.ErrProviderUnconfigured) {
		t.Fatalf("err = %v, want ErrProviderUnconfigured", err)
	}
	if !strings.Contains(err.Error(), modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock)) {
		t.Errorf("error %q does not name the provider to connect", err)
	}
	if !strings.Contains(err.Error(), bedrockModel) {
		t.Errorf("error %q does not name the offending model", err)
	}
}

// A fresh local install has connected nothing and picked nothing, and it keeps
// running: the SQLite column default puts it on the machine's credentials, so
// there is nothing for a model to select between. Asserted against a database
// nobody configured, because that default arriving already answered is what
// keeps a zero-config install from being asked to choose.
func TestCheckModelProviders_FreshLocalInstall_Allows(t *testing.T) {
	database := newCostCapTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	models := []string{modelOn(t, modelcatalog.ProviderAnthropic), modelOn(t, modelcatalog.ProviderBedrock)}
	if err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, models); err != nil {
		t.Fatalf("a fresh local install must keep running, got: %v", err)
	}
}

// The checked set is the inherited default plus every step pin — the two states
// a step can be in, and nothing at dispatch substitutes a third.
func TestBlueprintModels_CoversTheDefaultAndEveryPin(t *testing.T) {
	got := blueprintModels(domain.ModelSonnet, []domain.BlueprintPlanStep{
		{StepIndex: 0},
		{StepIndex: 1, Model: domain.ModelOpus},
	})
	want := []string{domain.ModelSonnet, "", domain.ModelOpus}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// An org that said it brings its own credentials and bound none dispatches
// nothing. Without this the run would start, hand the agent subprocess an empty
// environment, and let the SDK authenticate from whatever the operator's shell
// holds — billing this org's work to a credential nobody configured and nobody
// can see. Refusing is the same rule the model path already holds: no
// substitution, because spending someone's money on something they did not
// choose is worse than refusing.
func TestCheckModelProviders_OwnCredentialsWithNoneBound_Refuses(t *testing.T) {
	database := newCostCapTestDB(t)
	setAuthMethod(t, database, domain.LLMAuthBYOK)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID,
		[]string{modelOn(t, modelcatalog.ProviderAnthropic)})
	if !errors.Is(err, modelaccess.ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say where to fix it", err)
	}
}

// The same empty credential set on an org that CHOSE the machine's credentials
// dispatches fine — that is the zero-config local install, and the recorded
// source is the whole difference between the two.
func TestCheckModelProviders_HostCredentials_Allows(t *testing.T) {
	database := newCostCapTestDB(t)
	setAuthMethod(t, database, domain.LLMAuthSystem)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	for _, provider := range modelcatalog.SupportedProviders() {
		if err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID,
			[]string{modelOn(t, provider)}); err != nil {
			t.Errorf("%s: an org on the machine's credentials was refused: %v", provider, err)
		}
	}
}
