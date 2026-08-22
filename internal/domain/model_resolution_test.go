package domain

import "testing"

func TestEffectiveModel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teamDefault string
		orgMaxTier  string
		wantModel   string
		wantSource  string
	}{
		{"cap clamps team", ModelOpus, "sonnet", ModelSonnet, "org-cap"},
		{"team at cap", ModelSonnet, "sonnet", ModelSonnet, "team"},
		{"both empty fall back", "", "", DefaultModel, "team"},
		{"no cap keeps team", ModelOpus, "", ModelOpus, "team"},
		{"team below cap", ModelHaiku, "opus", ModelHaiku, "team"},
		{"unknown cap ignored", ModelOpus, "llama", ModelOpus, "team"},
		// A model outside the three-tier ladder is honored as chosen. The cap
		// ranks Anthropic tiers and cannot place it, and the previous behavior
		// — fall back to the shipped default — would answer a deliberate pin
		// with a model nobody picked.
		{"unrankable team default kept, no cap", "claude-fable-5", "", "claude-fable-5", "team"},
		{"unrankable team default kept, capped", "claude-fable-5", "haiku", "claude-fable-5", "team"},
		{"surrounding whitespace trimmed", "  " + ModelOpus + " ", "", ModelOpus, "team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, source := EffectiveModel(tc.teamDefault, tc.orgMaxTier)
			if model != tc.wantModel || source != tc.wantSource {
				t.Errorf("EffectiveModel(%q, %q) = (%q, %q), want (%q, %q)",
					tc.teamDefault, tc.orgMaxTier, model, source, tc.wantModel, tc.wantSource)
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
		tier ModelTier
		want string
	}{
		{TierHaiku, ModelHaiku},
		{TierSonnet, ModelSonnet},
		{TierOpus, ModelOpus},
		{TierUnknown, ""},
	} {
		if got := tc.tier.ModelID(); got != tc.want {
			t.Errorf("ModelTier(%d).ModelID() = %q, want %q", tc.tier, got, tc.want)
		}
		// Round-trip: a tier's id parses back to that tier.
		if tc.want != "" && ParseTier(tc.want) != tc.tier {
			t.Errorf("ParseTier(%q) did not round-trip to tier %d", tc.want, tc.tier)
		}
	}
}

// The org cap is the one setting still stored as a bare tier word, so ParseTier
// has to keep reading them for as long as the column exists. Nothing writes
// them; this pins the read.
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

// DefaultModel is what an unset team resolves to, and what the
// team_settings.default_model column default materializes. It must be a
// concrete id — the vocabulary migration's whole point — so a fresh install
// never stores a word the provider would reject.
func TestDefaultModelIsConcrete(t *testing.T) {
	if ParseTier(DefaultModel) == TierUnknown {
		t.Fatalf("DefaultModel %q is not one of the three tier ids", DefaultModel)
	}
	if DefaultModel != ModelSonnet {
		t.Errorf("DefaultModel = %q, want %q", DefaultModel, ModelSonnet)
	}
}
