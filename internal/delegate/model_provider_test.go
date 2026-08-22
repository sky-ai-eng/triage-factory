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
// settings refs that are the record of what is bound.
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
	if _, err := store.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("connect providers: %v", err)
	}
}

func restrictTeamProviders(t *testing.T, database *sql.DB, providers ...string) {
	t.Helper()
	if _, err := sqlitestore.New(database).Teams.SetAllowedProvidersSystem(
		context.Background(), runmode.LocalDefaultTeamID, providers,
	); err != nil {
		t.Fatalf("restrict team providers: %v", err)
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
	if err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, models); err != nil {
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
	err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
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

// The restriction is enforced at dispatch, which is what covers a default or a
// step pin stored before an admin imposed it — imposing one rewrites nothing.
func TestCheckModelProviders_RestrictedProvider_Refuses(t *testing.T) {
	database := newCostCapTestDB(t)
	connectProviders(t, database, true, true)
	restrictTeamProviders(t, database, modelcatalog.ProviderAnthropic)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		[]string{modelOn(t, modelcatalog.ProviderBedrock)})
	if !errors.Is(err, modelaccess.ErrProviderRestricted) {
		t.Fatalf("err = %v, want ErrProviderRestricted", err)
	}
	// The provider IS connected for the org, so this must not be reported as a
	// setup gap — the admin's fix is the restriction, not a credential.
	if errors.Is(err, modelaccess.ErrProviderUnconfigured) {
		t.Errorf("err = %v, mis-reported as an unconnected provider", err)
	}
	// A model on the allowed provider still dispatches.
	if err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		[]string{modelOn(t, modelcatalog.ProviderAnthropic)}); err != nil {
		t.Errorf("allowed provider refused: %v", err)
	}
}

// An org that connected nothing is on ambient credentials (local's zero-config
// subscription): there is nothing for a model to select between, so no model is
// refused for want of a credential.
func TestCheckModelProviders_NothingConnected_Allows(t *testing.T) {
	database := newCostCapTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, domain.ModelSonnet)

	models := []string{modelOn(t, modelcatalog.ProviderAnthropic), modelOn(t, modelcatalog.ProviderBedrock)}
	if err := s.checkModelProviders(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, models); err != nil {
		t.Fatalf("an org with no configured provider must keep running, got: %v", err)
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
