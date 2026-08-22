package domain

import "strings"

// The concrete model ids the three Anthropic tiers name. Stored configuration
// carries these verbatim — a team default, a per-prompt override — and they are
// what reaches the provider, so nothing downstream translates a stored value.
//
// Haiku is the dated spelling, which is what the vendor publishes it under and
// what the pricing datasheet carries; Sonnet and Opus are the undated current
// ids. A test in internal/modelcatalog pins all three to catalog keys, which is
// what keeps a value the settings UI offers from being one the ledger cannot
// price.
const (
	ModelHaiku  = "claude-haiku-4-5-20251001"
	ModelSonnet = "claude-sonnet-5"
	ModelOpus   = "claude-opus-5"
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
// The three bare tier words are recognized because org_settings
// .max_llm_model_tier still stores them: the org cap is the one setting the
// concrete-id migration deliberately left alone, since its replacement takes
// the column with it. Nothing writes those words — this reads what is already
// there.
func ParseTier(s string) ModelTier {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ModelHaiku, "haiku":
		return TierHaiku
	case ModelSonnet, "sonnet":
		return TierSonnet
	case ModelOpus, "opus":
		return TierOpus
	default:
		return TierUnknown
	}
}

// ModelID maps a tier back to the concrete model id it names. Empty string for
// Unknown, which has no id to name.
func (t ModelTier) ModelID() string {
	switch t {
	case TierHaiku:
		return ModelHaiku
	case TierSonnet:
		return ModelSonnet
	case TierOpus:
		return ModelOpus
	default:
		return ""
	}
}

// EffectiveModel resolves the model a team should actually use, given the
// team's configured default and the org's max-tier cap. The team default
// wins unless it exceeds the cap, in which case the cap applies.
//
//   - teamDefault empty → DefaultModel.
//   - orgMaxTier empty/unknown → no cap.
//   - a teamDefault outside the tier ladder → no cap either. The cap ranks the
//     three Anthropic tiers and can say nothing about a model it cannot place,
//     and clamping one it cannot compare would substitute a model nobody chose.
//
// source is "org-cap" when the cap clamped the team's choice, else "team" —
// it lets the UI explain "you're on Sonnet because your org caps at Sonnet".
func EffectiveModel(teamDefault, orgMaxTier string) (model, source string) {
	model = strings.TrimSpace(teamDefault)
	if model == "" {
		model = DefaultModel
	}
	capTier := ParseTier(orgMaxTier)
	if capTier == TierUnknown {
		return model, "team"
	}
	team := ParseTier(model)
	if team == TierUnknown || team <= capTier {
		return model, "team"
	}
	return capTier.ModelID(), "org-cap"
}
