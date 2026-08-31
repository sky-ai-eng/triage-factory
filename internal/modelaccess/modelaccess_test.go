package modelaccess

import (
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

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

func bothConnected() domain.OrgSettings {
	return domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key", BedrockCredentialsRef: "aws_role_arn"}
}

func TestCheck_BothConnected(t *testing.T) {
	for _, provider := range modelcatalog.SupportedProviders() {
		if err := For(bothConnected(), false).Check(modelOn(t, provider)); err != nil {
			t.Errorf("%s: %v", provider, err)
		}
	}
}

func TestCheck_UnconnectedProviderIsNamed(t *testing.T) {
	org := domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key"}
	bedrock := modelOn(t, modelcatalog.ProviderBedrock)

	err := For(org, false).Check(bedrock)
	if !errors.Is(err, ErrProviderUnconfigured) {
		t.Fatalf("err = %v, want ErrProviderUnconfigured", err)
	}
	if !strings.Contains(err.Error(), modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock)) {
		t.Errorf("error %q does not name the provider", err)
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say where to connect it", err)
	}
	if err := For(org, false).Check(modelOn(t, modelcatalog.ProviderAnthropic)); err != nil {
		t.Errorf("the connected provider's model was refused: %v", err)
	}
}

// Ready is the org-level question Check deliberately does not answer: can this
// org authenticate anything at all?
//
// The zero-config local install has chosen the host's credentials and is ready
// with nothing bound — that emptiness is the configuration working. The org that
// said it brings its own and bound none is refused, because the alternative is a
// run authenticating from whatever the operator's environment holds and spending
// against a credential nobody configured. Same empty set, opposite answers, and
// the recorded choice is the whole difference.
func TestReady_TurnsOnTheRecordedCredentialSource(t *testing.T) {
	host := domain.OrgSettings{LLMAuthMethod: domain.LLMAuthSystem}
	if err := For(host, false).Ready(); err != nil {
		t.Errorf("an org on the host's credentials was refused: %v", err)
	}

	own := domain.OrgSettings{LLMAuthMethod: domain.LLMAuthBYOK}
	err := For(own, false).Ready()
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say where to fix it", err)
	}

	// Binding ANY provider is enough. Which one serves a given model is Check's
	// question, and answering it here would report the wrong fault.
	own.BedrockCredentialsRef = "aws_role_arn"
	if err := For(own, false).Ready(); err != nil {
		t.Errorf("an org holding Bedrock material was refused: %v", err)
	}
}

// Multi resolves to BYOK whatever the row says — a multi-mode deployment has no host
// credentials to lend — so a row claiming otherwise is inert rather than a way
// to dispatch against the operator's environment.
func TestReady_MultiIgnoresAStoredHostCredentialSource(t *testing.T) {
	stored := domain.OrgSettings{LLMAuthMethod: domain.LLMAuthSystem}
	if err := For(stored, true).Ready(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials in multi", err)
	}
	if err := For(bothConnected(), true).Ready(); err != nil {
		t.Errorf("a configured multi org was refused: %v", err)
	}
}

// An org that has connected nothing has no model refused for want of a
// credential — whether it may run at all is Ready's question, asked once per
// dispatch rather than once per model — but an explicit restriction still binds.
func TestCheck_OrgWithNothingConnected(t *testing.T) {
	var nothing domain.OrgSettings
	bedrock := modelOn(t, modelcatalog.ProviderBedrock)

	if err := For(nothing, false).Check(bedrock); err != nil {
		t.Errorf("an org with nothing connected refused a model: %v", err)
	}
}

// A model nothing offers is not this predicate's fault to report — the catalog
// validator owns that, and answering here would name the wrong problem.
func TestCheck_UnofferedModelPasses(t *testing.T) {
	if err := For(domain.OrgSettings{AnthropicAPIKeyRef: "k"}, false).Check("vendor-model-nobody-offers"); err != nil {
		t.Errorf("unoffered model reported as a provider fault: %v", err)
	}
	if err := For(bothConnected(), false).Check(""); err != nil {
		t.Errorf("an unset model reported as a provider fault: %v", err)
	}
}

// Role mode stores no Bedrock secret; the ref is the record that it is bound.
func TestOrgProviders_RefsAreTheRecord(t *testing.T) {
	got := boundProviders(domain.OrgSettings{BedrockCredentialsRef: "aws_role_arn"})
	if !got[modelcatalog.ProviderBedrock] || got[modelcatalog.ProviderAnthropic] {
		t.Errorf("OrgProviders = %v, want Bedrock only", got)
	}
	if len(boundProviders(domain.OrgSettings{})) != 0 {
		t.Error("an org with no refs reports a connected provider")
	}
}

// What the org has bound decides whenever it has bound anything, whatever the
// recorded source says. The combination is one the settings PATCH refuses to
// create, so this is about a value assembled some other way — a hand-built
// struct, a row written outside the API — reading as the specific fact rather
// than as a pass for every provider.
func TestHas_BoundRefsOutrankTheRecordedSource(t *testing.T) {
	// Local, so an unset method resolves to the host's credentials.
	c := For(domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key"}, false)
	if !c.Has(modelcatalog.ProviderAnthropic) {
		t.Error("the bound provider was refused")
	}
	if c.Has(modelcatalog.ProviderBedrock) {
		t.Error("an unbound provider passed on the strength of an unset auth method")
	}
}

// Holding nothing falls through to the recorded source, and the two answers
// there are opposite — which is the whole reason the source is stored rather
// than inferred from the same emptiness.
func TestHas_NothingBoundFollowsTheRecordedSource(t *testing.T) {
	host := For(domain.OrgSettings{LLMAuthMethod: domain.LLMAuthSystem}, false)
	own := For(domain.OrgSettings{LLMAuthMethod: domain.LLMAuthBYOK}, false)
	for _, provider := range modelcatalog.SupportedProviders() {
		if !host.Has(provider) {
			t.Errorf("%s: refused for an org on the host's credentials", provider)
		}
		if own.Has(provider) {
			t.Errorf("%s: passed for an org bringing its own and holding none", provider)
		}
	}
}

// Check exempts the org that holds nothing, and the exemption is load-bearing:
// the settings save calls only Check, and multi setup picks a model two steps
// before it binds a credential. Ready is what refuses that org, at dispatch.
func TestCheck_ExemptsTheOrgHoldingNothingSoSetupCanSave(t *testing.T) {
	fresh := domain.OrgSettings{LLMAuthMethod: domain.LLMAuthBYOK}
	for _, provider := range modelcatalog.SupportedProviders() {
		model := modelOn(t, provider)
		if err := For(fresh, true).Check(model); err != nil {
			t.Errorf("%s: a fresh multi org could not save a model: %v", provider, err)
		}
		if For(fresh, true).Has(provider) {
			t.Errorf("%s: Has should report the gap Check declines to", provider)
		}
	}
	if err := For(fresh, true).Ready(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("Ready = %v, want it to refuse what Check let through", err)
	}
}
