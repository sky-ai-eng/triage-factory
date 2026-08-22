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
)

// ErrProviderUnconfigured is a model served by a provider the org holds no
// credential for. The message names the provider and where to connect it,
// because that is the whole fix.
var ErrProviderUnconfigured = errors.New("model's provider is not connected")

// ErrProviderRestricted is a model served by a provider an org admin has
// restricted this team from spending against. Distinct from unconfigured
// because the remedies differ — one is a credential nobody bound, the other a
// decision somebody made — and a caller told the wrong one goes to the wrong
// settings page.
var ErrProviderRestricted = errors.New("model's provider is restricted for this team")

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
// An org that has connected NO provider passes the credential check for every
// model — the same rule credential resolution holds. Such an org is on ambient
// credentials (local mode's zero-config Claude Code subscription), where the
// host env is the credential and there is nothing for a model to select
// between. The restriction still applies there, because it is an explicit
// decision somebody made rather than an inference from what is bound.
func Check(model string, org domain.OrgSettings, allowedProviders []string) error {
	provider, ok := modelcatalog.ProviderFor(model)
	if !ok {
		return nil
	}
	name := modelcatalog.ProviderDisplayName(provider)
	if !modelcatalog.AllowedProviders(allowedProviders).Has(provider) {
		return fmt.Errorf("%w: %s is served by %s, which this team is not allowed to spend against", ErrProviderRestricted, model, name)
	}
	connected := OrgProviders(org)
	if len(connected) > 0 && !connected[provider] {
		return fmt.Errorf("%w: %s is served by %s, which this organization has not connected — connect it in Settings → Claude credentials", ErrProviderUnconfigured, model, name)
	}
	return nil
}

// OrgProviders reports which providers the org holds credentials for, read off
// the settings row's two refs rather than by probing the vault: the refs are the
// record of what is actually bound, and they are already loaded wherever this is
// asked. The Bedrock ref covers all three of its shapes, role mode included —
// that one stores no secret at all, and a caller looking for a key would read it
// as unconfigured.
func OrgProviders(org domain.OrgSettings) map[string]bool {
	out := map[string]bool{}
	if org.AnthropicAPIKeyRef != "" {
		out[modelcatalog.ProviderAnthropic] = true
	}
	if org.BedrockCredentialsRef != "" {
		out[modelcatalog.ProviderBedrock] = true
	}
	return out
}
