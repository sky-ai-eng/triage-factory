package modelcatalog

import (
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// The providers TF can reach a model through. The values are bifrost's own
// vocabulary — derived from it rather than restated, so a stored restriction, a
// wire field and the account TF hands bifrost cannot spell the same provider two
// ways.
//
// Two, and adding a third is an execution-path decision, not a naming one: a
// provider is offered when the LLM proxy, the sandbox egress allowlist and the
// local SDK can all drive it.
const (
	ProviderAnthropic = string(inference.ProviderAnthropic)
	ProviderBedrock   = string(inference.ProviderBedrock)
)

// datasheetProviders maps the pricing datasheet's vendor label to the provider
// the model on it is reached through.
//
// It exists because upstream's labels are not an access-path vocabulary: one
// Bedrock is spelled two ways there (`bedrock` for the older invoke rows,
// `bedrock_converse` for the Converse-API ones), and the same split will appear
// for any provider whose API surface upstream versions. Collapsing them here is
// what lets a team restriction, a picker and a credential lookup all name one
// "bedrock".
//
// The map is also the gate: a supported entry whose label is absent is dropped
// from the catalog and named in JoinError, because TF has no credential path for
// it — offering a model nothing can authenticate is a picker that lies.
var datasheetProviders = map[string]string{
	"anthropic":        ProviderAnthropic,
	"bedrock":          ProviderBedrock,
	"bedrock_converse": ProviderBedrock,
}

// providerDisplayNames is how each provider is written for a person. TF-authored
// rather than passed through, because upstream's labels are neither complete
// (one path, several labels) nor correctly cased.
var providerDisplayNames = map[string]string{
	ProviderAnthropic: "Anthropic",
	ProviderBedrock:   "Amazon Bedrock",
}

// SupportedProviders returns the providers this build can drive, sorted so the
// set reads the same everywhere it is rendered or serialized.
func SupportedProviders() []string {
	out := make([]string, 0, len(providerDisplayNames))
	for p := range providerDisplayNames {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// KnownProvider reports whether p names a provider this build drives — the
// accepted set for any stored provider value, the way Offers is for a model.
func KnownProvider(p string) bool {
	_, ok := providerDisplayNames[p]
	return ok
}

// ProviderDisplayName is how p is written for a person, or p itself when the
// build does not know it. A stored value that outlived its provider still has
// to render as something.
func ProviderDisplayName(p string) string {
	if name, ok := providerDisplayNames[p]; ok {
		return name
	}
	return p
}

// ProviderFor reports which provider serves the offered model key. Not-ok for a
// key this build does not offer — the answer is a property of the catalog entry,
// so a key with no entry has no provider rather than a guessed one.
//
// This is the selection rule for a run: the model names the provider, and
// nothing else does. An org holding credentials for both is not asked which one
// it prefers, because the question has an answer already.
func ProviderFor(key string) (string, bool) {
	for _, e := range entries {
		if e.Key == key {
			return e.Provider, true
		}
	}
	return "", false
}

// ProviderSet answers whether one team may spend against a given provider.
type ProviderSet struct {
	keys map[string]bool
}

// AllowedProviders resolves a team's stored provider restriction into something
// to ask questions of. A nil or empty stored value means the org admin has
// expressed no restriction and resolves to every provider — never to "none",
// which would leave a team unable to run the moment the column exists.
//
// Callers pass what the team stored and never branch on whether it was empty;
// that decision belongs here, in one place, so every surface answers the same.
func AllowedProviders(stored []string) ProviderSet {
	if len(stored) == 0 {
		return ProviderSet{}
	}
	set := ProviderSet{keys: make(map[string]bool, len(stored))}
	for _, p := range stored {
		set.keys[p] = true
	}
	return set
}

// Has reports whether the team may spend against p. An unrestricted set answers
// true for everything, including a provider this build does not know — the
// restriction is a narrowing, and something it never narrowed is not its call.
func (s ProviderSet) Has(p string) bool {
	if s.keys == nil {
		return true
	}
	return s.keys[p]
}

// Restricted reports whether an org admin narrowed this team at all. The read
// surfaces use it to distinguish "allowed because the admin said so" from
// "allowed because nobody has said anything".
func (s ProviderSet) Restricted() bool { return s.keys != nil }
