package modelcatalog

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// Every offered key resolves to a provider TF drives, and the two providers
// v1 ships are both represented — an org connecting Bedrock has to find
// something in the catalog to run on it.
func TestProviderFor_EveryEntryNamesADrivenProvider(t *testing.T) {
	seen := map[string]int{}
	for _, e := range Entries() {
		p, ok := ProviderFor(e.Key)
		if !ok {
			t.Errorf("%s: catalog entry has no provider", e.Key)
			continue
		}
		if !KnownProvider(p) {
			t.Errorf("%s: provider %q is not one this build drives", e.Key, p)
		}
		seen[p]++
	}
	for _, p := range []string{ProviderAnthropic, ProviderBedrock} {
		if seen[p] == 0 {
			t.Errorf("no catalog entry is served by %s; that provider is unreachable from a picker", p)
		}
	}
}

func TestProviderFor_UnofferedKeyHasNoProvider(t *testing.T) {
	if p, ok := ProviderFor("vendor-model-nobody-offers"); ok {
		t.Errorf("an unoffered key resolved to provider %q; want no answer", p)
	}
}

// The provider map is what makes upstream's two Bedrock labels one access path.
func TestDatasheetProviders_CollapsesUpstreamBedrockLabels(t *testing.T) {
	for _, label := range []string{"bedrock", "bedrock_converse"} {
		if got := datasheetProviders[label]; got != ProviderBedrock {
			t.Errorf("datasheet label %q maps to %q, want %q", label, got, ProviderBedrock)
		}
	}
}

// A supported key whose datasheet vendor maps to no provider is dropped and
// named, the same treatment an unpriced key gets: TF has no credential path
// for it, so offering it would be a picker entry nothing can authenticate.
func TestLoad_DropsAKeyWhoseVendorTFCannotDrive(t *testing.T) {
	const key = "priced-but-undrivable-model"
	vertex := func(k string) (inference.Info, bool) {
		if k != key {
			return inference.Info{}, false
		}
		return inference.Info{
			Provider:           "vertex_ai-anthropic_models",
			InputCostPerToken:  1e-6,
			OutputCostPerToken: 5e-6,
			MaxInputTokens:     200_000,
		}, true
	}

	got, err := load([]byte(`[{"key": "`+key+`", "display_name": "Undrivable"}]`), vertex)
	if err == nil {
		t.Fatalf("load accepted a key on a provider TF cannot drive; entries = %+v", got)
	}
	if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "vertex_ai-anthropic_models") {
		t.Errorf("error %q names neither the key nor the vendor", err)
	}
	if len(got) != 0 {
		t.Errorf("entries = %+v, want none", got)
	}
}

// The provider vocabulary is bifrost's, not a restatement of it: a stored
// restriction, the wire field and the account TF hands bifrost are one set of
// spellings.
func TestProviderIDs_AreBifrostVocabulary(t *testing.T) {
	if ProviderAnthropic != string(inference.ProviderAnthropic) {
		t.Errorf("ProviderAnthropic = %q, want %q", ProviderAnthropic, inference.ProviderAnthropic)
	}
	if ProviderBedrock != string(inference.ProviderBedrock) {
		t.Errorf("ProviderBedrock = %q, want %q", ProviderBedrock, inference.ProviderBedrock)
	}
}

// Every provider the map can produce must be nameable, so enabling one forces
// the naming decision rather than leaking a raw label into a picker.
func TestProviderDisplayNames_CoverEveryMappedProvider(t *testing.T) {
	for label, p := range datasheetProviders {
		if !KnownProvider(p) {
			t.Errorf("datasheet label %q maps to %q, which has no display name", label, p)
		}
	}
	for _, p := range SupportedProviders() {
		if ProviderDisplayName(p) == p {
			t.Errorf("%s: display name is the raw id", p)
		}
	}
}
