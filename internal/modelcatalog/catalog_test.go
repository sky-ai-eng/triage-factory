package modelcatalog

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// The embedded file and the embedded datasheet ship in one binary, so this
// failing means the build is broken, not that a deployment is misconfigured.
func TestCatalog_EmbeddedFileJoins(t *testing.T) {
	if err := JoinError(); err != nil {
		t.Fatalf("supported_models.json does not join against the pricing datasheet: %v", err)
	}
	if len(Entries()) == 0 {
		t.Fatal("catalog is empty; TF would offer no models at all")
	}
	for _, e := range Entries() {
		if e.DisplayName == "" {
			t.Errorf("%s: empty display name", e.Key)
		}
		if e.Provider == "" {
			t.Errorf("%s: empty provider", e.Key)
		}
		if e.ContextWindow <= 0 {
			t.Errorf("%s: context window %d, want a positive window", e.Key, e.ContextWindow)
		}
		if e.Prices.Output <= 0 {
			t.Errorf("%s: output price %v, want a positive rate", e.Key, e.Prices.Output)
		}
	}
}

// Prices are pinned exactly, not asserted to be "reasonable": the daily
// datasheet refresh rewrites the upstream snapshot wholesale, and a rate that
// silently moves is the failure this catalog exists to make visible. A refresh
// that legitimately changes a price changes these numbers in the same commit.
func TestCatalog_PinnedAgainstCommittedSnapshot(t *testing.T) {
	want := []Entry{
		{
			Key: "claude-haiku-4-5-20251001", DisplayName: "Claude Haiku 4.5", Provider: "anthropic",
			Prices:        PricesPerMTok{Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
			ContextWindow: 200_000, SupportsPromptCaching: true, DisplayOrder: 0,
		},
		{
			Key: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", Provider: "anthropic",
			Prices:        PricesPerMTok{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
			ContextWindow: 1_000_000, SupportsPromptCaching: true, DisplayOrder: 1,
		},
		{
			Key: "claude-opus-5", DisplayName: "Claude Opus 5", Provider: "anthropic",
			Prices:        PricesPerMTok{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
			ContextWindow: 1_000_000, SupportsPromptCaching: true, DisplayOrder: 2,
		},
		{
			Key: "claude-fable-5", DisplayName: "Claude Fable 5", Provider: "anthropic",
			Prices:        PricesPerMTok{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
			ContextWindow: 1_000_000, SupportsPromptCaching: true, DisplayOrder: 3,
		},
	}

	got := Entries()
	if len(got) != len(want) {
		t.Fatalf("catalog has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("entry %d:\n got %+v\nwant %+v", i, got[i], w)
		}
	}
}

// The failure mode the join exists to catch, exercised through the same code
// path the embedded file takes. The message must name the key: an operator
// reading a boot failure has to know which entry to remove.
func TestLoad_NamesAKeyTheDatasheetCannotResolve(t *testing.T) {
	const missing = "vendor-model-that-does-not-exist"
	file := fmt.Sprintf(`[
		{"key": %q, "display_name": "Real Model"},
		{"key": %q, "display_name": "Retired Model"}
	]`, realKey(t), missing)

	got, err := load([]byte(file), inference.ModelInfo)
	if err == nil {
		t.Fatal("load returned no error for a key with no datasheet row")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the unresolvable key %q", err, missing)
	}
	// The resolvable entry still ships: one retired model costs its own row,
	// not the catalog.
	if len(got) != 1 || got[0].Key != realKey(t) {
		t.Errorf("entries = %+v, want only the resolvable one", got)
	}
}

// A malformed row is caught for the same reason a missing datasheet row is: it
// would otherwise reach a picker as a nameless or double-named option.
func TestLoad_RejectsMalformedRows(t *testing.T) {
	real := realKey(t)
	for _, tc := range []struct{ name, file, wantIn string }{
		{"empty key", `[{"key": "", "display_name": "Nameless"}]`, "empty key"},
		{"empty display name", fmt.Sprintf(`[{"key": %q, "display_name": ""}]`, real), "empty display_name"},
		{
			"duplicate key",
			fmt.Sprintf(`[{"key": %q, "display_name": "One"}, {"key": %q, "display_name": "Two"}]`, real, real),
			"duplicate key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := load([]byte(tc.file), inference.ModelInfo)
			if err == nil {
				t.Fatalf("load returned no error; entries = %+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestLoad_RejectsUnparseableFile(t *testing.T) {
	if _, err := load([]byte(`{"key": "not-an-array"}`), inference.ModelInfo); err == nil {
		t.Fatal("load accepted a file that is not an array of entries")
	}
}

// Display order must be contiguous from zero even when an entry drops, or a
// client sorting by it inherits a hole that reads as a missing model.
func TestLoad_DisplayOrderIsContiguousAfterADrop(t *testing.T) {
	real := realKey(t)
	file := fmt.Sprintf(`[
		{"key": "gone-from-upstream", "display_name": "Retired"},
		{"key": %q, "display_name": "Real Model"}
	]`, real)

	got, _ := load([]byte(file), inference.ModelInfo)
	if len(got) != 1 {
		t.Fatalf("entries = %+v, want 1", got)
	}
	if got[0].DisplayOrder != 0 {
		t.Errorf("display order = %d, want 0 (the dropped entry must not leave a hole)", got[0].DisplayOrder)
	}
}

// An org that has expressed no preference has everything enabled — the absent
// value resolves to the whole catalog, never to nothing.
func TestEnabled_AbsentSetMeansEveryModel(t *testing.T) {
	set := Enabled(nil)
	for _, e := range Entries() {
		if !set.Has(e.Key) {
			t.Errorf("%s: not enabled under the default set", e.Key)
		}
	}
	if got, want := len(DefaultEnabled()), len(Entries()); got != want {
		t.Errorf("DefaultEnabled has %d keys, want %d (one per catalog entry)", got, want)
	}
	if set.Has("some-model-nobody-enabled") {
		t.Error("default set claims a key that is not in the catalog")
	}
}

func TestEnabled_StoredSetIsHonoured(t *testing.T) {
	set := Enabled([]string{"claude-opus-5"})
	if !set.Has("claude-opus-5") {
		t.Error("stored key is not enabled")
	}
	if set.Has("claude-sonnet-5") {
		t.Error("a key outside the stored set is enabled")
	}
}

// Entries hands out a copy: a caller that reorders or truncates its slice must
// not reorder the catalog for the next request.
func TestEntries_IsACopy(t *testing.T) {
	first := Entries()
	if len(first) == 0 {
		t.Fatal("catalog is empty")
	}
	first[0] = Entry{Key: "clobbered"}
	if Entries()[0].Key == "clobbered" {
		t.Error("mutating a returned slice changed the catalog")
	}
}

// realKey is a catalog key that resolves against the committed datasheet, for
// tests that need one alongside a synthetic failure. Reading it from the
// catalog rather than hardcoding one keeps these tests working when the
// supported set changes.
func realKey(t *testing.T) string {
	t.Helper()
	if err := JoinError(); err != nil {
		t.Fatalf("catalog join is broken, so no key is known good: %v", err)
	}
	entries := Entries()
	if len(entries) == 0 {
		t.Fatal("catalog is empty")
	}
	return entries[0].Key
}

// The join error carries one cause per unusable entry, so a stale file reports
// its whole list in a single build rather than one key at a time.
func TestLoad_ReportsEveryUnusableEntry(t *testing.T) {
	file := `[
		{"key": "missing-one", "display_name": "One"},
		{"key": "missing-two", "display_name": "Two"}
	]`
	_, err := load([]byte(file), inference.ModelInfo)
	if err == nil {
		t.Fatal("load returned no error")
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Fatalf("error %v does not carry the individual causes", err)
	}
	if got := len(joined.Unwrap()); got != 2 {
		t.Errorf("error carries %d causes, want 2", got)
	}
}
