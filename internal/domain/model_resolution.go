package domain

import "strings"

// The concrete model ids the three Anthropic tiers name — the NATIVE
// vocabulary, which a multi-mode install stores and sends. They are what
// reaches the provider on that path, so nothing downstream translates a stored
// value.
//
// Haiku is the dated spelling, which is what the vendor publishes it under and
// what the pricing datasheet carries; Sonnet and Opus are the undated current
// ids. A test in internal/modelcatalog pins all three to native registry keys,
// which is what keeps a value the settings UI offers from being one the ledger
// cannot price.
const (
	ModelHaiku  = "claude-haiku-4-5-20251001"
	ModelSonnet = "claude-sonnet-5"
	ModelOpus   = "claude-opus-5"
)

// The Claude Code SDK's family aliases — the vocabulary a LOCAL install stores
// and sends, because the SDK subprocess is what executes a local conversation
// and these are its own words. The harness resolves each to whichever model
// currently heads that family, against whichever access path its environment
// selects, so an alias names no provider and joins no price table.
//
// They are named here, beside the native ids, because the schema's dialect
// defaults are written in one vocabulary or the other and a stored default has
// to be spelled somewhere Go can see. A test in internal/modelcatalog pins each
// to the SDK registry, the same way the native ids are pinned.
const (
	ModelAliasHaiku  = "haiku"
	ModelAliasSonnet = "sonnet"
	ModelAliasOpus   = "opus"
	ModelAliasFable  = "fable"
)

// ModelTier is an ordered rank over three Anthropic model tiers. It exists for
// one consumer, the org max-tier cap: the ordering (Haiku < Sonnet < Opus) is
// what lets that cap clamp a team's default, collapsing a higher-tier default
// to the org's max.
//
// Only those three are in scope — every other model (Fable, a future
// Bedrock-Llama) parses as Unknown and sits outside the ladder, which is why
// nothing but the cap may be built on it. Ranking models in general would need
// a defensible basis for calling one better than another, and there is none TF
// asserts; the catalog deliberately publishes no ordering at all.
type ModelTier int

const (
	TierUnknown ModelTier = iota
	TierHaiku
	TierSonnet
	TierOpus
)

// ParseTier maps a model id to its tier. Unknown for empty or unrecognized
// ids, so callers can apply their own fallback.
//
// Both vocabularies are recognized, and for two separate reasons. The SDK
// aliases are what a local install stores as its team default and its
// per-prompt pins, so the cap has to be able to place them. The same three
// words are also what org_settings.max_llm_model_tier stores the CAP itself as,
// in either mode — the org cap is the one setting the concrete-id rewrite
// deliberately left alone, since its replacement takes the column with it.
func ParseTier(s string) ModelTier {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ModelHaiku, ModelAliasHaiku:
		return TierHaiku
	case ModelSonnet, ModelAliasSonnet:
		return TierSonnet
	case ModelOpus, ModelAliasOpus:
		return TierOpus
	default:
		return TierUnknown
	}
}

// ModelID maps a tier back to the model id it names, in the vocabulary the
// given deployment stores. Empty string for Unknown, which has no id to name.
//
// The vocabulary matters because the answer is written back into configuration
// the deployment then dispatches: clamping a local team's default to a native
// id would store a model the local picker cannot show and the local save would
// refuse.
func (t ModelTier) ModelID(multiMode bool) string {
	switch t {
	case TierHaiku:
		if multiMode {
			return ModelHaiku
		}
		return ModelAliasHaiku
	case TierSonnet:
		if multiMode {
			return ModelSonnet
		}
		return ModelAliasSonnet
	case TierOpus:
		if multiMode {
			return ModelOpus
		}
		return ModelAliasOpus
	default:
		return ""
	}
}

// EffectiveModel resolves the model a team should actually use, given the
// team's configured default and the org's max-tier cap. The team default
// wins unless it exceeds the cap, in which case the cap applies.
//
//   - teamDefault empty → DefaultModelFor(multiMode).
//   - orgMaxTier empty/unknown → no cap.
//   - a teamDefault outside the tier ladder → no cap either. The cap ranks the
//     three Anthropic tiers and can say nothing about a model it cannot place,
//     and clamping one it cannot compare would substitute a model nobody chose.
//
// multiMode selects the vocabulary both the fallback and the clamp are answered
// in, so the model this returns is one the deployment that asked can store and
// dispatch.
//
// source is "org-cap" when the cap clamped the team's choice, else "team" —
// it lets the UI explain "you're on Sonnet because your org caps at Sonnet".
func EffectiveModel(teamDefault, orgMaxTier string, multiMode bool) (model, source string) {
	model = strings.TrimSpace(teamDefault)
	if model == "" {
		model = DefaultModelFor(multiMode)
	}
	capTier := ParseTier(orgMaxTier)
	if capTier == TierUnknown {
		return model, "team"
	}
	team := ParseTier(model)
	if team == TierUnknown || team <= capTier {
		return model, "team"
	}
	return capTier.ModelID(multiMode), "org-cap"
}
