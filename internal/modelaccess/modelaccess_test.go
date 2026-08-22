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

func TestCheck_BothConnectedAndUnrestricted(t *testing.T) {
	for _, provider := range modelcatalog.SupportedProviders() {
		if err := Check(modelOn(t, provider), bothConnected(), nil); err != nil {
			t.Errorf("%s: %v", provider, err)
		}
	}
}

func TestCheck_UnconnectedProviderIsNamed(t *testing.T) {
	org := domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key"}
	bedrock := modelOn(t, modelcatalog.ProviderBedrock)

	err := Check(bedrock, org, nil)
	if !errors.Is(err, ErrProviderUnconfigured) {
		t.Fatalf("err = %v, want ErrProviderUnconfigured", err)
	}
	if !strings.Contains(err.Error(), modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock)) {
		t.Errorf("error %q does not name the provider", err)
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say where to connect it", err)
	}
	if err := Check(modelOn(t, modelcatalog.ProviderAnthropic), org, nil); err != nil {
		t.Errorf("the connected provider's model was refused: %v", err)
	}
}

// The restriction outranks the credential: telling a team to connect a provider
// it is not allowed to spend against sends it to do useless work.
func TestCheck_RestrictionOutranksTheCredential(t *testing.T) {
	org := domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key"}
	err := Check(modelOn(t, modelcatalog.ProviderBedrock), org, []string{modelcatalog.ProviderAnthropic})
	if !errors.Is(err, ErrProviderRestricted) {
		t.Fatalf("err = %v, want ErrProviderRestricted", err)
	}
	if errors.Is(err, ErrProviderUnconfigured) {
		t.Errorf("err = %v, reported as both faults at once", err)
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
	if err := Ready(host, false); err != nil {
		t.Errorf("an org on the host's credentials was refused: %v", err)
	}

	own := domain.OrgSettings{LLMAuthMethod: domain.LLMAuthBYOK}
	err := Ready(own, false)
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if !strings.Contains(err.Error(), "Settings") {
		t.Errorf("error %q does not say where to fix it", err)
	}

	// Binding ANY provider is enough. Which one serves a given model is Check's
	// question, and answering it here would report the wrong fault.
	own.BedrockCredentialsRef = "aws_role_arn"
	if err := Ready(own, false); err != nil {
		t.Errorf("an org holding Bedrock material was refused: %v", err)
	}
}

// Multi resolves to BYOK whatever the row says — a hosted deployment has no host
// credentials to lend — so a row claiming otherwise is inert rather than a way
// to dispatch against the operator's environment.
func TestReady_MultiIgnoresAStoredHostCredentialSource(t *testing.T) {
	stored := domain.OrgSettings{LLMAuthMethod: domain.LLMAuthSystem}
	if err := Ready(stored, true); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("err = %v, want ErrNoCredentials in multi", err)
	}
	if err := Ready(bothConnected(), true); err != nil {
		t.Errorf("a configured multi org was refused: %v", err)
	}
}

// An org that has connected nothing has no model refused for want of a
// credential — whether it may run at all is Ready's question, asked once per
// dispatch rather than once per model — but an explicit restriction still binds.
func TestCheck_OrgWithNothingConnected(t *testing.T) {
	var nothing domain.OrgSettings
	bedrock := modelOn(t, modelcatalog.ProviderBedrock)

	if err := Check(bedrock, nothing, nil); err != nil {
		t.Errorf("an org with nothing connected refused a model: %v", err)
	}
	if err := Check(bedrock, nothing, []string{modelcatalog.ProviderAnthropic}); !errors.Is(err, ErrProviderRestricted) {
		t.Errorf("err = %v, want the admin's restriction to bind whatever is connected", err)
	}
}

// A model nothing offers is not this predicate's fault to report — the catalog
// validator owns that, and answering here would name the wrong problem.
func TestCheck_UnofferedModelPasses(t *testing.T) {
	if err := Check("vendor-model-nobody-offers", domain.OrgSettings{AnthropicAPIKeyRef: "k"}, []string{modelcatalog.ProviderAnthropic}); err != nil {
		t.Errorf("unoffered model reported as a provider fault: %v", err)
	}
	if err := Check("", bothConnected(), nil); err != nil {
		t.Errorf("an unset model reported as a provider fault: %v", err)
	}
}

// Role mode stores no Bedrock secret; the ref is the record that it is bound.
func TestOrgProviders_RefsAreTheRecord(t *testing.T) {
	got := OrgProviders(domain.OrgSettings{BedrockCredentialsRef: "aws_role_arn"})
	if !got[modelcatalog.ProviderBedrock] || got[modelcatalog.ProviderAnthropic] {
		t.Errorf("OrgProviders = %v, want Bedrock only", got)
	}
	if len(OrgProviders(domain.OrgSettings{})) != 0 {
		t.Error("an org with no refs reports a connected provider")
	}
}
