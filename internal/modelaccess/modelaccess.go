// Package modelaccess answers one question, for one org: can this model be
// authenticated?
//
// It is about CREDENTIALS and nothing else. A provider the org never connected
// is a setup gap an admin fixes by connecting it, and an org that brings its own
// and has bound nothing can authenticate nothing at all. Both refuse, and
// neither substitutes: TF ships no fallback model, so a model whose provider is
// unavailable is a refusal, never a quiet switch to one the caller did not
// choose.
//
// Which models a tenant may PICK is a separate control with a separate storage
// and a separate failure — the org and team enable-sets (domain.ModelSet) — and
// the two are deliberately not fused: one is about what the deployment can
// reach, the other about what an admin decided, and a caller told the wrong one
// goes to the wrong settings page.
//
// It is its own package because both enforcement points need it and neither can
// import the other. The settings handler asks at save time, so a team's default
// cannot be set to something its next run would refuse; the delegate asks at
// dispatch, which is what catches a configuration saved before a credential was
// unbound.
package modelaccess

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ErrProviderUnconfigured is a model served by a provider the org holds no
// credential for. The message names the provider and where to connect it,
// because that is the whole fix.
var ErrProviderUnconfigured = errors.New("model's provider is not connected")

// ErrNoCredentials is an org that can authenticate nothing at all: it brings
// its own Claude credentials and has bound none. Org-level rather than
// per-model, which is why Ready answers it and Check does not — no choice of
// model fixes it.
//
// It is a REFUSAL, not a fallback, and that is the whole point of the sentinel.
// A local run whose org has bound nothing hands the agent subprocess an empty
// environment and lets the SDK authenticate from whatever the operator's shell
// holds. For an org on domain.LLMAuthSystem that is the configuration working
// as chosen. For an org that said it brings its own, it is TF quietly spending
// on a credential nobody configured and nobody can see — the same fault as
// substituting a model the caller did not choose, and worse, because the payer
// changes rather than the price.
var ErrNoCredentials = errors.New("organization has no Claude credentials of its own")

// Credentials says which providers this org can actually reach. Build it once
// per request or dispatch and ask it every credential question, so the badge a
// reader sees and the refusal a run hits come from the same answer.
type Credentials struct {
	// host: the org runs on whatever credential the machine supplies, so it is
	// not choosing between providers at all.
	host bool
	// bound: the providers it holds its own credentials for.
	bound map[string]bool
}

// For builds an org's Credentials. Takes multiMode as an argument so a test can
// exercise either mode directly; production calls ForOrg.
func For(org domain.OrgSettings, multiMode bool) Credentials {
	return Credentials{
		host:  domain.EffectiveLLMAuthMethod(org.LLMAuthMethod, multiMode) == domain.LLMAuthSystem,
		bound: boundProviders(org),
	}
}

// ForOrg builds an org's Credentials for the mode this process runs in. Use it
// everywhere in production: it keeps the mode read in one place instead of at
// every call site.
func ForOrg(org domain.OrgSettings) Credentials {
	return For(org, runmode.Current() == runmode.ModeMulti)
}

// Has reports whether this org can reach provider.
//
// If it holds credentials of its own, those decide: a provider it did not bind
// is one it cannot reach. Otherwise the org's recorded source decides — on the
// machine's credentials everything is reachable, and bringing its own with none
// bound nothing is, since the alternative is spending on a credential nobody
// chose.
//
// Bound credentials are checked first so that an org somehow holding both — a
// combination the settings PATCH rejects — is read by what it bound rather than
// waved through for every provider.
func (c Credentials) Has(provider string) bool {
	if len(c.bound) > 0 {
		return c.bound[provider]
	}
	return c.host
}

// Ready reports whether the org can reach any provider at all.
//
// Call it before dispatching work, and only then. A settings save must still
// accept a model the org cannot run yet, because setup picks one before it binds
// a credential.
//
// It is "no provider passes Has" rather than its own rule, so it can never
// disagree with the per-provider answer.
//
// TODO(TFAC-888): in local mode a bound credential is not the one a run
// authenticates with, so this can only report that something is bound. Once
// local reads the org's own material, bound and used are the same credential.
func (c Credentials) Ready() error {
	for _, provider := range modelcatalog.SupportedProviders() {
		if c.Has(provider) {
			return nil
		}
	}
	return fmt.Errorf("%w: connect a provider in Settings → Claude credentials, or switch to the credentials already on this machine", ErrNoCredentials)
}

// Check reports whether this org can authenticate model.
//
// Two rules:
//
//   - A model the catalog does not offer passes. That fault belongs to the
//     catalog validator wherever the model is stored; reporting it here would
//     name the wrong problem.
//   - The credential is checked only if the org holds some. An org holding none
//     passes every model: Ready refuses it at dispatch, and the settings save —
//     which calls Check and not Ready — has to accept a model chosen before any
//     credential is bound.
//
// That second rule is why this is not simply !Has, and why the catalog read can
// badge a model unconfigured while a save of it succeeds.
func (c Credentials) Check(model string) error {
	provider, ok := modelcatalog.ProviderFor(model)
	if !ok {
		return nil
	}
	if c.anyBound() && !c.Has(provider) {
		return fmt.Errorf("%w: %s is served by %s, which this organization has not connected — connect it in Settings → Claude credentials",
			ErrProviderUnconfigured, model, modelcatalog.ProviderDisplayName(provider))
	}
	return nil
}

// anyBound reports whether the org holds any credential of its own — the
// condition Check exempts from the credential rule.
func (c Credentials) anyBound() bool { return len(c.bound) > 0 }

// boundProviders reads the org's own providers off the two settings refs rather
// than the vault: the refs record what is bound and are already loaded wherever
// this is called. The Bedrock ref covers all three of its shapes including role
// mode, which stores no secret — a caller looking for a key would call it
// unconfigured.
func boundProviders(org domain.OrgSettings) map[string]bool {
	out := map[string]bool{}
	if org.AnthropicAPIKeyRef != "" {
		out[modelcatalog.ProviderAnthropic] = true
	}
	if org.BedrockCredentialsRef != "" {
		out[modelcatalog.ProviderBedrock] = true
	}
	return out
}
