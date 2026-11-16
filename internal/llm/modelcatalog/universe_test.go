package modelcatalog

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The shipped SDK list, in file order. Pinned here because the whole point of
// the amendment is that this list is TF's, not the native registry's: an entry
// silently added or reordered changes what a local picker offers and what a
// local install may store.
var claudeCodeAliases = []string{
	domain.ModelAliasHaiku,
	domain.ModelAliasSonnet,
	domain.ModelAliasOpus,
	domain.ModelAliasFable,
}

func TestSDKModels_ClaudeCodeCarriesTheFourAliases(t *testing.T) {
	if err := SDKLoadError(); err != nil {
		t.Fatalf("sdk_models.json is unusable: %v", err)
	}
	got := SDKModels(SDKClaudeCode)
	if len(got) != len(claudeCodeAliases) {
		t.Fatalf("claude-code names %d models, want %d: %+v", len(got), len(claudeCodeAliases), got)
	}
	for i, want := range claudeCodeAliases {
		if got[i].Key != want {
			t.Errorf("model %d = %q, want %q", i, got[i].Key, want)
		}
		if got[i].DisplayOrder != i {
			t.Errorf("%s: display order = %d, want %d", got[i].Key, got[i].DisplayOrder, i)
		}
		if got[i].DisplayName == "" {
			t.Errorf("%s: no display name", got[i].Key)
		}
	}
}

// An SDK row carries no provider and no datasheet facts, and that absence is
// the contract the wire shape publishes: cost is settled by the harness, and
// the access path is a property of the credential rather than of the id. A row
// that grew either would put a number nothing backs in front of a user.
func TestSDKModels_CarryNoProviderAndNoFacts(t *testing.T) {
	for _, m := range SDKModels(SDKClaudeCode) {
		if m.Provider != "" {
			t.Errorf("%s: provider = %q, want none — an alias names no access path", m.Key, m.Provider)
		}
		if m.Facts != nil {
			t.Errorf("%s: carries datasheet facts, want none — the harness settles cost", m.Key)
		}
	}
}

func TestSDKModels_UnknownSDKIsEmpty(t *testing.T) {
	if got := SDKModels("codex-cli"); len(got) != 0 {
		t.Errorf("an SDK this build carries no list for returned %+v, want nothing", got)
	}
}

func TestSDKs_NamesTheOneShippedHarness(t *testing.T) {
	got := SDKs()
	if len(got) != 1 || got[0] != SDKClaudeCode {
		t.Errorf("SDKs() = %v, want exactly [%s]", got, SDKClaudeCode)
	}
}

// SDKModels hands out a copy, for the same reason Entries does: it is read on an
// API request path and a shared header is one append away from a caller
// reordering everybody else's list.
func TestSDKModels_IsACopy(t *testing.T) {
	first := SDKModels(SDKClaudeCode)
	original := first[0]
	original.EffortLevels = slices.Clone(original.EffortLevels)
	first[0].EffortLevels[0] = "tampered"
	first[0] = Model{Key: "tampered"}
	if got := SDKModels(SDKClaudeCode)[0]; !reflect.DeepEqual(got, original) {
		t.Errorf("mutating the returned slice changed the registry: %+v, want %+v", got, original)
	}
}

func TestLoadSDKModels_ReportsEveryUnusableRow(t *testing.T) {
	_, err := loadSDKModels([]byte(`{
		"claude-code": [
			{ "key": "", "display_name": "Nameless" },
			{ "key": "sonnet", "display_name": "" },
			{ "key": "opus", "display_name": "Opus" },
			{ "key": "opus", "display_name": "Opus Again" }
		]
	}`))
	if err == nil {
		t.Fatal("three bad rows loaded without an error")
	}
	for _, want := range []string{"empty key", "empty display_name", "duplicate key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not report %q: %v", want, err)
		}
	}
}

// A dropped row must not leave a hole in the ordering the API publishes, the
// same rule the native join holds.
func TestLoadSDKModels_DropDoesNotHoleTheOrdering(t *testing.T) {
	got, _ := loadSDKModels([]byte(`{
		"claude-code": [
			{ "key": "broken", "display_name": "" },
			{ "key": "sonnet", "display_name": "Claude Sonnet" }
		]
	}`))
	models := got[SDKClaudeCode]
	if len(models) != 1 {
		t.Fatalf("models = %+v, want 1", models)
	}
	if models[0].DisplayOrder != 0 {
		t.Errorf("display order = %d, want 0", models[0].DisplayOrder)
	}
}

// An SDK whose every row is unusable is absent rather than present-and-empty:
// an empty universe reads as "this harness offers no models" instead of as a
// broken file.
func TestLoadSDKModels_AllRowsUnusableDropsTheSDK(t *testing.T) {
	got, err := loadSDKModels([]byte(`{"claude-code": [{ "key": "", "display_name": "" }]}`))
	if err == nil {
		t.Fatal("want an error")
	}
	if _, ok := got[SDKClaudeCode]; ok {
		t.Error("an SDK with no usable rows is still present")
	}
}

// The two universes are the two execution vocabularies, and neither may leak
// into the other: a native id in a local picker is a model the SDK would be
// asked to resolve as a version pin, and an alias in a multi picker goes on the
// bifrost wire unresolved and persists unpriced — §0's original bug.
func TestUniverseFor_VocabulariesDoNotOverlap(t *testing.T) {
	native, local := UniverseFor(true), UniverseFor(false)
	for _, key := range local.Keys() {
		if native.Offers(key) {
			t.Errorf("%q is offered by both universes", key)
		}
	}
	for _, key := range native.Keys() {
		if local.Offers(key) {
			t.Errorf("%q is offered by both universes", key)
		}
	}
}

func TestUniverseFor_LocalIsTheClaudeCodeList(t *testing.T) {
	if got, want := UniverseFor(false).Keys(), claudeCodeAliases; !equalStrings(got, want) {
		t.Errorf("local universe = %v, want %v", got, want)
	}
}

func TestUniverseFor_MultiIsTheNativeRegistry(t *testing.T) {
	u := UniverseFor(true)
	entriesNow := Entries()
	if len(u.Models()) != len(entriesNow) {
		t.Fatalf("native universe has %d models, want %d", len(u.Models()), len(entriesNow))
	}
	for i, m := range u.Models() {
		e := entriesNow[i]
		if m.Key != e.Key || m.Provider != e.Provider || m.DisplayName != e.DisplayName {
			t.Errorf("model %d = %+v, want it to project entry %+v", i, m, e)
		}
		if m.Facts == nil {
			t.Fatalf("%s: no datasheet facts on a native row", m.Key)
		}
		if m.Facts.ContextWindow != e.ContextWindow || m.Facts.Prices != e.Prices {
			t.Errorf("%s: facts %+v do not match the entry", m.Key, *m.Facts)
		}
	}
}

// A sweep's candidates follow the universe. On the native one the provider is a
// property of the id, so a family reaches only the ids that name it; on the SDK
// one no id names a family, so one credential reaches every alias.
func TestCandidatesFor(t *testing.T) {
	native := UniverseFor(true).CandidatesFor(ProviderAnthropic)
	if len(native) == 0 {
		t.Fatal("no native candidates for anthropic")
	}
	for _, m := range native {
		if m.Provider != ProviderAnthropic {
			t.Errorf("%s: candidate served by %q, want %q", m.Key, m.Provider, ProviderAnthropic)
		}
	}
	if got := len(UniverseFor(true).CandidatesFor(ProviderBedrock)); got == len(UniverseFor(true).Models()) {
		t.Error("the native universe offered every model as a bedrock candidate")
	}

	local := UniverseFor(false)
	for _, family := range SupportedProviders() {
		if got := len(local.CandidatesFor(family)); got != len(local.Models()) {
			t.Errorf("%s: %d SDK candidates, want all %d — an alias names no family", family, got, len(local.Models()))
		}
	}
}

func TestUniverse_LookupAndOffersAgree(t *testing.T) {
	for _, multi := range []bool{true, false} {
		u := UniverseFor(multi)
		for _, key := range u.Keys() {
			m, ok := u.Lookup(key)
			if !ok || m.Key != key {
				t.Errorf("multi=%v: Lookup(%q) = (%+v, %v)", multi, key, m, ok)
			}
			if !u.Offers(key) {
				t.Errorf("multi=%v: Offers(%q) = false but Lookup found it", multi, key)
			}
		}
		if _, ok := u.Lookup("nothing-names-this"); ok {
			t.Errorf("multi=%v: Lookup resolved a key outside the universe", multi)
		}
	}
}

func TestUniverse_ModelsIsACopy(t *testing.T) {
	u := UniverseFor(true)
	first := u.Models()
	if len(first) == 0 {
		t.Fatal("native universe is empty")
	}
	original := first[0]
	first[0] = Model{Key: "tampered"}
	if u.Models()[0].Key != original.Key {
		t.Error("mutating the returned slice changed the universe")
	}
}

// The alias constants domain names are what the SQLite dialect defaults are
// spelled in. A local install would seed itself with a model its own save
// refuses — and one no enable-set could admit, since an absent set resolves to
// exactly this universe — if one of them stopped being an offered alias.
func TestLocalUniverse_OffersEveryDomainAlias(t *testing.T) {
	u := UniverseFor(false)
	for _, alias := range []string{
		domain.ModelAliasHaiku, domain.ModelAliasSonnet, domain.ModelAliasOpus, domain.ModelAliasFable,
		domain.LocalDefaultModel, domain.LocalBackgroundJobsModel,
	} {
		if !u.Offers(alias) {
			t.Errorf("the local universe does not offer %q", alias)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEffortLevels_FollowTheDatasheetFlags(t *testing.T) {
	all := domain.EffortLevels
	cases := []struct {
		name  string
		entry Entry
		want  []string
	}{
		{"reasoning alone", Entry{SupportsReasoning: true},
			[]string{domain.EffortLow, domain.EffortMedium, domain.EffortHigh}},
		{"reasoning with xhigh and max", Entry{SupportsReasoning: true, SupportsXHighEffort: true, SupportsMaxEffort: true}, all},
		{"an xhigh ceiling drops max",
			Entry{SupportsReasoning: true, SupportsXHighEffort: true, SupportsMaxEffort: true, EffortCeiling: domain.EffortXHigh},
			[]string{domain.EffortLow, domain.EffortMedium, domain.EffortHigh, domain.EffortXHigh}},
		{"a high ceiling drops both upper levels",
			Entry{SupportsReasoning: true, SupportsXHighEffort: true, SupportsMaxEffort: true, EffortCeiling: domain.EffortHigh},
			[]string{domain.EffortLow, domain.EffortMedium, domain.EffortHigh}},
		{"max without xhigh",
			Entry{SupportsReasoning: true, SupportsMaxEffort: true},
			[]string{domain.EffortLow, domain.EffortMedium, domain.EffortHigh, domain.EffortMax}},
		{"no reasoning", Entry{SupportsXHighEffort: true, SupportsMaxEffort: true}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effortLevels(tc.entry)
			if got == nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("effortLevels = %#v, want %#v", got, tc.want)
			}
		})
	}

	t.Run("an SDK model takes all five", func(t *testing.T) {
		for _, m := range UniverseFor(false).Models() {
			if strings.Join(m.EffortLevels, ",") != strings.Join(all, ",") {
				t.Errorf("%s effort levels = %v, want %v", m.Key, m.EffortLevels, all)
			}
		}
	})
	// The shipped rows, as the datasheet describes them: a direct Anthropic 5.x
	// model takes all five, and a Bedrock row with an xhigh ceiling stops there.
	t.Run("the shipped native rows", func(t *testing.T) {
		u := UniverseFor(true)
		for key, want := range map[string]int{
			"claude-sonnet-5-5":              5,
			"claude-opus-5-5":                5,
			"claude-haiku-5-5":               5,
			"us.anthropic.claude-sonnet-5-5": 4,
			"us.anthropic.claude-haiku-5-5":  4,
		} {
			m, ok := u.Lookup(key)
			if !ok {
				t.Fatalf("%s is not offered", key)
			}
			if m.EffortLevels == nil || len(m.EffortLevels) != want {
				t.Errorf("%s effort levels = %#v, want %d of them", key, m.EffortLevels, want)
			}
		}
		m, _ := u.Lookup("us.anthropic.claude-sonnet-5-5")
		if m.TakesEffort(domain.EffortMax) || !m.TakesEffort(domain.EffortXHigh) {
			t.Errorf("Bedrock Sonnet takes max=%v xhigh=%v, want false/true", m.TakesEffort(domain.EffortMax), m.TakesEffort(domain.EffortXHigh))
		}
	})
}
