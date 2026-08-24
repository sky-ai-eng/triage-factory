package domain

import "testing"

func TestEffectiveModel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teamDefault string
		orgMaxTier  string
		wantModel   string
		wantSource  string
		multiMode   bool
	}{
		// The two vocabularies, each resolved in its own deployment. A clamp
		// answers in the vocabulary that deployment stores, so a local team
		// capped at Sonnet lands on the alias rather than on a wire id its own
		// save would refuse.
		{"cap clamps team", ModelOpus, "sonnet", ModelSonnet, "org-cap", true},
		{"team at cap", ModelSonnet, "sonnet", ModelSonnet, "team", true},
		{"both empty fall back", "", "", DefaultModel, "team", true},
		{"no cap keeps team", ModelOpus, "", ModelOpus, "team", true},
		{"team below cap", ModelHaiku, "opus", ModelHaiku, "team", true},
		{"unknown cap ignored", ModelOpus, "llama", ModelOpus, "team", true},
		{"local cap clamps to the alias", ModelAliasOpus, "sonnet", ModelAliasSonnet, "org-cap", false},
		{"local empty falls back to the alias", "", "", LocalDefaultModel, "team", false},
		{"local no cap keeps team", ModelAliasOpus, "", ModelAliasOpus, "team", false},
		{"local alias below cap", ModelAliasHaiku, "opus", ModelAliasHaiku, "team", false},
		{"local fable is unrankable and kept", ModelAliasFable, "haiku", ModelAliasFable, "team", false},
		// A model outside the three-tier ladder is honored as chosen. The cap
		// ranks Anthropic tiers and cannot place it, and the previous behavior
		// — fall back to the shipped default — would answer a deliberate pin
		// with a model nobody picked.
		{"unrankable team default kept, no cap", "claude-fable-5", "", "claude-fable-5", "team", true},
		{"unrankable team default kept, capped", "claude-fable-5", "haiku", "claude-fable-5", "team", true},
		{"surrounding whitespace trimmed", "  " + ModelOpus + " ", "", ModelOpus, "team", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, source := EffectiveModel(tc.teamDefault, tc.orgMaxTier, tc.multiMode)
			if model != tc.wantModel || source != tc.wantSource {
				t.Errorf("EffectiveModel(%q, %q, multi=%v) = (%q, %q), want (%q, %q)",
					tc.teamDefault, tc.orgMaxTier, tc.multiMode, model, source, tc.wantModel, tc.wantSource)
			}
		})
	}
}

func TestParseTierOrdering(t *testing.T) {
	if TierHaiku >= TierSonnet || TierSonnet >= TierOpus {
		t.Fatal("tier ordering must be Haiku < Sonnet < Opus for the cap to clamp correctly")
	}
	if TierUnknown != 0 {
		t.Errorf("TierUnknown should be the zero value, got %d", TierUnknown)
	}
}

func TestModelTierModelID(t *testing.T) {
	for _, tc := range []struct {
		tier      ModelTier
		multiMode bool
		want      string
	}{
		{TierHaiku, true, ModelHaiku},
		{TierSonnet, true, ModelSonnet},
		{TierOpus, true, ModelOpus},
		{TierUnknown, true, ""},
		{TierHaiku, false, ModelAliasHaiku},
		{TierSonnet, false, ModelAliasSonnet},
		{TierOpus, false, ModelAliasOpus},
		{TierUnknown, false, ""},
	} {
		if got := tc.tier.ModelID(tc.multiMode); got != tc.want {
			t.Errorf("ModelTier(%d).ModelID(multi=%v) = %q, want %q", tc.tier, tc.multiMode, got, tc.want)
		}
		// Round-trip: a tier's id parses back to that tier, in either vocabulary.
		if tc.want != "" && ParseTier(tc.want) != tc.tier {
			t.Errorf("ParseTier(%q) did not round-trip to tier %d", tc.want, tc.tier)
		}
	}
}

// The org cap is stored as a bare tier word in either mode, and a local team
// default is stored as the harness alias — the same three strings. ParseTier
// reads them for as long as either exists; this pins that read.
func TestParseTier_ReadsStoredOrgCapVocabulary(t *testing.T) {
	for word, want := range map[string]ModelTier{
		"haiku":  TierHaiku,
		"sonnet": TierSonnet,
		"Opus":   TierOpus,
	} {
		if got := ParseTier(word); got != want {
			t.Errorf("ParseTier(%q) = %d, want %d", word, got, want)
		}
	}
}

// The provisioning default is what an unset team resolves to, and what its
// dialect's team_settings.default_model column DEFAULT materializes. Each has to
// be spelled in the vocabulary that deployment dispatches: a native wire id
// under the native runtime, the harness alias under the SDK one. Getting either
// wrong seeds an install with a model its own save validator refuses.
func TestDefaultModelPerVocabulary(t *testing.T) {
	if DefaultModelFor(true) != ModelSonnet {
		t.Errorf("DefaultModelFor(multi) = %q, want %q", DefaultModelFor(true), ModelSonnet)
	}
	if DefaultModelFor(false) != ModelAliasSonnet {
		t.Errorf("DefaultModelFor(local) = %q, want %q", DefaultModelFor(false), ModelAliasSonnet)
	}
	for _, model := range []string{DefaultModel, LocalDefaultModel} {
		if ParseTier(model) == TierUnknown {
			t.Errorf("provisioning default %q is not one of the three tier ids", model)
		}
	}
}
