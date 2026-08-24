// Package modelcatalog is the list of models Triage Factory offers, under each
// execution vocabulary it offers them in, and the names it offers them under.
//
// There are two registries here, not one, because there are two vocabularies. A
// NATIVE id is a bifrost wire id: it names the provider that serves it and
// resolves in the pricing datasheet, so supported_models.json is joined against
// that datasheet at process start and the price a user is shown is the price the
// ledger charges. An SDK ALIAS is a harness's own word for a model family:
// sdk_models.json names those per SDK, with no provider and no join, because the
// harness resolves the alias itself against whichever access path its
// environment selects — the provider is a property of the credential, and the
// cost is settled by the harness rather than interpolated from a price table.
//
// Universe (universe.go) is the one door onto "what may this deployment offer or
// store". Every validator asks it, so none of them has to know which registry
// answered.
//
// It owns exactly the editorial half of the native registry: which datasheet
// keys are supported, and what each is called in a picker. Everything factual
// about such a model — its prices, its context window, whether it caches
// prompts, which vendor serves it — comes from internal/inference's embedded
// datasheet.
//
// The split is not stylistic. scripts/refresh-pricing.sh rewrites the datasheet
// wholesale from upstream every day, so a TF-authored field placed inside it
// would be wiped on the next run. supported_models.json is the file TF owns and
// upstream never touches.
//
// A key is opaque. It is the datasheet key verbatim, and nothing here parses
// one: the same model is spelled a dozen ways across providers, every separator
// means different things in different places, and no substring of a key tells
// you whether two spellings are the same model, an alias and a pin, or unrelated.
//
// Each file is its vocabulary's allowlist: present means supported and named,
// absent means not offered. There is no disabled flag and no ranking — file
// order is display order and nothing more. A capability ordering would need a
// defensible basis for saying one model is better than another, and there is
// none TF is willing to assert.
package modelcatalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

//go:embed supported_models.json
var supportedModelsJSON []byte

// supportedModel is one row of supported_models.json: the two fields TF
// authors. Everything else on an Entry is joined in from the datasheet.
type supportedModel struct {
	Key string `json:"key"`
	// DisplayName renders verbatim, so it is written the way it should read:
	// "Vendor Model Version", with the access path in parentheses where the
	// same model is reachable through more than one provider and the two
	// would otherwise be indistinguishable in a picker.
	DisplayName string `json:"display_name"`
}

// PricesPerMTok is a model's headline rates in dollars per million tokens —
// the unit prices are quoted in, and the unit a spend control would be
// expressed in. They are the base-tier rates: a request over the long-context
// boundary bills higher where the model publishes a long-context tier, and
// inference.CostForUsage is the only authority on what a given request cost.
//
// CacheWrite is the 5-minute-TTL write rate. A zero cache rate is a fact about
// the vendor rather than a gap — some populate a cache for free.
type PricesPerMTok struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Entry is one offered model: TF's editorial fields joined to the datasheet's
// facts.
//
// There is no rank, tier, or capability score here, deliberately. DisplayOrder
// is presentation only — the position of the entry in supported_models.json —
// and carries no claim that one model is better or more capable than another.
type Entry struct {
	Key         string
	DisplayName string
	// Provider is the access path this key is reached through, mapped from the
	// datasheet's vendor label through datasheetProviders. It is the provider
	// vocabulary a credential lookup and a team restriction share, not
	// upstream's label, which spells one path several ways.
	Provider              string
	Prices                PricesPerMTok
	ContextWindow         int
	SupportsPromptCaching bool
	DisplayOrder          int
}

// tokensPerMTok converts the datasheet's per-token rates to the per-million
// unit prices are quoted in.
const tokensPerMTok = 1_000_000

// perMTok restates a per-token rate in dollars per million tokens.
//
// The rounding is the point. A rate like 1e-7 is not exactly representable in
// binary, so scaling it by a million lands at 0.09999999999999999 — a price
// nobody publishes and nobody should be shown. Vendors quote these to a few
// decimal places at most, so rounding to six restores the quoted figure while
// staying orders of magnitude finer than any real price. Billing never comes
// through here: inference.CostForUsage works in per-token rates end to end.
func perMTok(rate float64) float64 {
	const precision = 1e6
	return math.Round(rate*tokensPerMTok*precision) / precision
}

var (
	entries []Entry
	joinErr error
)

func init() {
	entries, joinErr = load(supportedModelsJSON, inference.ModelInfo)
}

// Entries returns the catalog in display order. The slice is freshly copied per
// call: it is read on an API request path, and a shared slice header is one
// careless append away from a caller reordering every other caller's catalog.
func Entries() []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// JoinError names the supported entries whose key the datasheet could not
// resolve. They are absent from Entries; this is how a caller learns they were
// dropped rather than never named.
//
// Both files are compiled in, so the answer is settled when the binary is
// linked: it describes the build, not the deployment. What that should cost is
// therefore a decision for whoever holds the build — this package only reports
// it.
func JoinError() error { return joinErr }

// load parses the supported-models file and joins each entry against the
// datasheet. lookup is a parameter rather than a direct inference call so the
// join's failure mode is testable without a doctored datasheet.
//
// Entries that resolve are returned in file order. Entries that do not are
// omitted, and named in the returned error — every one of them, so a caller
// fixing a stale file sees the whole list rather than one key per build.
func load(fileJSON []byte, lookup func(string) (inference.Info, bool)) ([]Entry, error) {
	var supported []supportedModel
	if err := json.Unmarshal(fileJSON, &supported); err != nil {
		return nil, fmt.Errorf("modelcatalog: parse supported_models.json: %w", err)
	}

	out := make([]Entry, 0, len(supported))
	var problems []error
	seen := make(map[string]bool, len(supported))
	for i, s := range supported {
		switch {
		case s.Key == "":
			problems = append(problems, fmt.Errorf("entry %d: empty key", i))
			continue
		case s.DisplayName == "":
			problems = append(problems, fmt.Errorf("%s: empty display_name", s.Key))
			continue
		case seen[s.Key]:
			// Two rows for one key would give it two display names and two
			// positions, and which one wins would be an accident of order.
			problems = append(problems, fmt.Errorf("%s: duplicate key", s.Key))
			continue
		}
		seen[s.Key] = true

		info, ok := lookup(s.Key)
		if !ok {
			problems = append(problems, fmt.Errorf("%s: no priced row in the pricing datasheet", s.Key))
			continue
		}
		provider, ok := datasheetProviders[info.Provider]
		if !ok {
			// No credential path resolves this key, so offering it would put a
			// model in the picker that no org can authenticate.
			problems = append(problems, fmt.Errorf("%s: datasheet vendor %q maps to no provider TF drives", s.Key, info.Provider))
			continue
		}
		if info.MaxInputTokens <= 0 {
			// A priced row can still lack a window fact; offering it would
			// publish context_window 0 as if it meant something.
			problems = append(problems, fmt.Errorf("%s: datasheet row records no context window", s.Key))
			continue
		}
		out = append(out, Entry{
			Key:         s.Key,
			DisplayName: s.DisplayName,
			Provider:    provider,
			Prices: PricesPerMTok{
				Input:      perMTok(info.InputCostPerToken),
				Output:     perMTok(info.OutputCostPerToken),
				CacheRead:  perMTok(info.CacheReadCostPerToken),
				CacheWrite: perMTok(info.CacheWriteCostPerToken),
			},
			ContextWindow:         info.MaxInputTokens,
			SupportsPromptCaching: info.SupportsPromptCaching,
			// Position in the returned slice, not in the file: a dropped
			// entry must not leave a hole in the ordering the API publishes.
			DisplayOrder: len(out),
		})
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("modelcatalog: %d supported model(s) unusable: %w", len(problems), errors.Join(problems...))
	}
	return out, nil
}
