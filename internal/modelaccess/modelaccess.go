// Package modelaccess answers one question, for one org and one team: may this
// model be dispatched?
//
// Two facts decide it, and they fail differently. A provider the org never
// connected is a setup gap an admin fixes by connecting it; a provider a team is
// restricted from is a decision an admin already made about that team. Both
// refuse, and neither substitutes: TF ships no fallback model, so a model whose
// provider is unavailable is a refusal, never a quiet switch to one the caller
// did not choose.
//
// It is its own package because both enforcement points need it and neither can
// import the other. The settings handler asks at save time, so a team's default
// cannot be set to something its next run would refuse; the delegate asks at
// dispatch, which is what catches a configuration saved before the restriction —
// restricting a team rewrites nothing it already stored.
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

// ErrProviderRestricted is a model served by a provider an org admin has
// restricted this team from spending against. Distinct from unconfigured
// because the remedies differ — one is a credential nobody bound, the other a
// decision somebody made — and a caller told the wrong one goes to the wrong
// settings page.
var ErrProviderRestricted = errors.New("model's provider is restricted for this team")

// Credentials is an org's resolved credential position: which providers TF can
// put a credential behind for it. Resolve it once — per request, per dispatch —
// and ask it everything.
//
// It exists because that one question had grown two answers. The catalog read
// derived it one way to badge a model, the dispatch gate derived it another to
// refuse one, and for an org that brings its own credentials and has bound none
// the two disagreed: the badge said unconfigured while the gate allowed. Only
// their composition was correct, and nothing made them compose. A resolved value
// with the methods on it means a caller cannot pick the wrong derivation,
// because there is only one.
type Credentials struct {
	// host is the org running on whatever credential the host supplies, so it
	// is not selecting between providers at all.
	host bool
	// bound is what it holds of its own, read off the settings refs.
	bound map[string]bool
}

// For resolves an org's position. multiMode is a parameter rather than a
// runmode read so both arms are reachable in a test without touching process
// state; ForOrg is the production spelling.
func For(org domain.OrgSettings, multiMode bool) Credentials {
	return Credentials{
		host:  domain.EffectiveLLMAuthMethod(org.LLMAuthMethod, multiMode) == domain.LLMAuthSystem,
		bound: boundProviders(org),
	}
}

// ForOrg resolves against this process's mode. Every production caller wants
// this one; it exists so the mode read lives here once instead of at each of
// them, which is how a caller ends up passing the wrong answer.
func ForOrg(org domain.OrgSettings) Credentials {
	return For(org, runmode.Current() == runmode.ModeMulti)
}

// Has reports whether TF can put a credential behind a call to provider.
//
// What it has bound decides whenever it has bound anything: those refs name
// exactly which providers it can reach, and a provider missing from them is
// missing however the org authenticates. Only an org holding NOTHING of its own
// falls through to the recorded source, and then the answer is that source's:
// on the host's credentials every provider passes, because there is no
// per-provider binding to be missing and the environment the runtime inherits
// is the credential; bringing its own and having bound none, every provider
// fails, because the alternative is spending against a credential nobody
// configured.
//
// Reading the refs first also settles a combination the API refuses to create —
// host credentials alongside a bound one, which the settings PATCH 422s — in
// the direction of the more specific fact, rather than letting an unset column
// on a hand-built value read as a pass for everything.
func (c Credentials) Has(provider string) bool {
	if len(c.bound) > 0 {
		return c.bound[provider]
	}
	return c.host
}

// Ready reports whether the org holds credentials any run could authenticate
// with. Ask it before dispatching work, and only before dispatching: a save is
// allowed to name a model the org cannot run yet, because setup picks a model
// before it binds a credential and a settings form that refused would be
// unusable in the order it is presented.
//
// Defined as "no provider satisfies Has", so it cannot drift from the
// per-provider answer — an org ready to run is exactly one with somewhere to
// run, and an org on the host's credentials satisfies it at the first provider
// asked.
//
// TODO(TFAC-888): "has bound anything" is as far as this can go in local,
// where a bound credential is not the one a run actually authenticates with.
// Once local reads the org's own material, ready and used are the same
// credential in both modes and this doc loses its asterisk.
func (c Credentials) Ready() error {
	for _, provider := range modelcatalog.SupportedProviders() {
		if c.Has(provider) {
			return nil
		}
	}
	return fmt.Errorf("%w: connect a provider in Settings → Claude credentials, or switch to the credentials already on this machine", ErrNoCredentials)
}

// Check reports whether model may be dispatched for a team in this org.
// allowedProviders is the team's stored restriction (empty = unrestricted).
//
// A model the catalog does not offer passes. Whether an unoffered model is
// acceptable is the catalog validator's question, asked wherever a model is
// stored; answering it again here would report the wrong fault for a value
// whose real problem is that nothing offers it.
//
// The restriction is checked before the credential: a team told to go connect a
// provider it is not allowed to use has been sent to do useless work.
//
// An org holding NOTHING of its own is exempt from the credential half, and
// the exemption is the reason this is not simply !Has. Whether such an org may
// run at all is Ready's question, asked once per dispatch; this one is also
// asked by the settings save, which must let setup name a model two steps
// before it binds a credential. Answering here would make a fresh org's first
// save fail on a gap it is on its way to filling. The restriction still binds,
// because it is a decision somebody made rather than an inference from what is
// bound.
//
// So a badge and this gate can differ for exactly that org — the catalog read
// calls it unconfigured, a save allows it — and that is the division of labour,
// not a drift: one reports capability, the other permits setup. They agree
// everywhere a credential actually exists to disagree about, because both read
// Has.
func (c Credentials) Check(model string, allowedProviders []string) error {
	provider, ok := modelcatalog.ProviderFor(model)
	if !ok {
		return nil
	}
	name := modelcatalog.ProviderDisplayName(provider)
	if !modelcatalog.AllowedProviders(allowedProviders).Has(provider) {
		return fmt.Errorf("%w: %s is served by %s, which this team is not allowed to spend against", ErrProviderRestricted, model, name)
	}
	if c.anyBound() && !c.Has(provider) {
		return fmt.Errorf("%w: %s is served by %s, which this organization has not connected — connect it in Settings → Claude credentials", ErrProviderUnconfigured, model, name)
	}
	return nil
}

// anyBound reports whether the org holds any credential of its own. It names
// the one condition Check exempts, so that exemption reads as the decision it
// is rather than as a bare emptiness test.
func (c Credentials) anyBound() bool { return len(c.bound) > 0 }

// boundProviders reports which providers the org holds credentials of its own
// for, read off the settings row's two refs rather than by probing the vault:
// the refs are the record of what is actually bound, and they are already
// loaded wherever this is asked. The Bedrock ref covers all three of its
// shapes, role mode included — that one stores no secret at all, and a caller
// looking for a key would read it as unconfigured.
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
