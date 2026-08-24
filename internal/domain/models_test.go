package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The catalog default an org that has expressed no preference resolves to. A
// stand-in for modelcatalog.DefaultEnabled(), which this package cannot import.
func catalogDefault() []string {
	return []string{ModelHaiku, ModelSonnet, ModelOpus, "claude-fable-5"}
}

// An absent org set tracks the default; a stored one is frozen at what it names.
// Those are the same rule read from two sides, and the pair is what makes "the
// org chose nothing" different from "the org chose everything".
func TestOrgModelSet_AbsentTracksTheDefault(t *testing.T) {
	absent := OrgModelSet(nil, catalogDefault())
	for _, key := range catalogDefault() {
		if !absent.Has(key) {
			t.Errorf("%s: not enabled under an absent set", key)
		}
	}

	stored := OrgModelSet([]string{ModelHaiku}, catalogDefault())
	if !stored.Has(ModelHaiku) {
		t.Error("a stored set does not admit the model it names")
	}
	if stored.Has(ModelOpus) {
		t.Error("a stored set admits a model it does not name")
	}
	// The frozen half: a model the default gained is NOT admitted by a set that
	// predates it, which is the whole reason the resolved set is never stored.
	if stored.Has("claude-opus-6-future") {
		t.Error("a stored set admits a model added after it was written")
	}
}

// A team's effective set is its own narrowed to its org's, resolved at every
// read. The intersection is not redundant with the ⊆ check the team write
// enforces: the org may narrow afterwards, and nothing rewrites the team row.
func TestTeamModelSet_IntersectsWithTheOrg(t *testing.T) {
	org := OrgModelSet([]string{ModelHaiku, ModelSonnet}, catalogDefault())

	if got := TeamModelSet(nil, org).Keys(); !reflect.DeepEqual(got, []string{ModelHaiku, ModelSonnet}) {
		t.Errorf("an absent team set = %v, want the org's set inherited whole", got)
	}

	narrowed := TeamModelSet([]string{ModelHaiku}, org)
	if !narrowed.Has(ModelHaiku) || narrowed.Has(ModelSonnet) {
		t.Errorf("a narrowed team set = %v, want just Haiku", narrowed.Keys())
	}

	// A team set naming something its org no longer enables loses it here.
	// Nothing rewrote the team's row to say so, and nothing has to.
	stale := TeamModelSet([]string{ModelHaiku, ModelOpus}, org)
	if stale.Has(ModelOpus) {
		t.Error("a team set outlived its org's narrowing")
	}
	if !stale.Has(ModelHaiku) {
		t.Error("the intersection dropped a model both sides enable")
	}

	// Disjoint sets resolve to nothing, and nothing is a real answer: the team
	// runs no model until somebody re-picks. It must not silently widen back to
	// the org's set, which is what "empty means unrestricted" would do.
	if got := TeamModelSet([]string{ModelOpus}, org).Keys(); len(got) != 0 {
		t.Errorf("a team set disjoint from its org's = %v, want nothing", got)
	}
	if TeamModelSet([]string{ModelOpus}, org).Has(ModelHaiku) {
		t.Error("a disjoint team set widened back to the org's")
	}
}

// The zero value admits everything — for a caller with no enablement to apply
// at all. Nothing built from a settings row is ever this: OrgModelSet resolves
// an absent stored set to the catalog default instead.
func TestModelSet_ZeroValueIsUnrestricted(t *testing.T) {
	var unset ModelSet
	if !unset.Has("anything-at-all") {
		t.Error("the zero value refused a model")
	}
	if len(unset.Keys()) != 0 {
		t.Errorf("the zero value names %v, want nothing to name", unset.Keys())
	}
	if OrgModelSet(nil, catalogDefault()).Has("some-model-nobody-offers") {
		t.Error("an absent stored set resolved to the zero value rather than the catalog default")
	}
}

// Keys preserves first-seen order and drops duplicates, so a refusal names the
// set the way its admin wrote it. String is what a refusal message renders.
func TestModelSet_KeysAndString(t *testing.T) {
	set := OrgModelSet([]string{ModelOpus, ModelHaiku, ModelOpus}, catalogDefault())
	if got := set.Keys(); !reflect.DeepEqual(got, []string{ModelOpus, ModelHaiku}) {
		t.Errorf("Keys() = %v, want first-seen order with the duplicate dropped", got)
	}
	if got := set.String(); !strings.Contains(got, ModelOpus) || !strings.Contains(got, ModelHaiku) {
		t.Errorf("String() = %q, want it to name both members", got)
	}
	var empty ModelSet
	if got := empty.String(); got != "(none)" {
		t.Errorf("an empty set renders as %q, want a word rather than a blank", got)
	}

	// Keys hands out a copy: a caller that sorts or truncates it must not
	// reorder the set for the next reader.
	keys := set.Keys()
	keys[0] = "clobbered"
	if set.Keys()[0] == "clobbered" {
		t.Error("mutating a returned slice changed the set")
	}
}

// RequireDefault is where a team's default is judged, and it is deliberately
// not where its set is resolved: a refusal about the default leaves the set
// intact, which is what keeps a pinned step — and a mid-flight blueprint of
// pinned steps — running while the team re-picks.
func TestTeamModels_RequireDefault(t *testing.T) {
	enabled := OrgModelSet([]string{ModelHaiku, ModelSonnet}, catalogDefault())

	ok := NewTeamModels(ModelSonnet, enabled)
	if got, err := ok.RequireDefault(); err != nil || got != ModelSonnet {
		t.Errorf("an enabled default = (%q, %v), want %q", got, err, ModelSonnet)
	}

	excluded := NewTeamModels(ModelOpus, enabled)
	got, err := excluded.RequireDefault()
	if !errors.Is(err, ErrModelNotEnabled) {
		t.Fatalf("err = %v, want ErrModelNotEnabled", err)
	}
	if got != "" {
		t.Errorf("a refusal handed back %q, want no model at all", got)
	}
	if !strings.Contains(err.Error(), ModelOpus) || !strings.Contains(err.Error(), ModelSonnet) {
		t.Errorf("error %q must name both the model and the set that excludes it", err)
	}
	// The set survives the refusal: a step pinned to something the team DOES
	// enable is unaffected by a default it never reads.
	if !excluded.Enabled().Has(ModelHaiku) {
		t.Error("a refused default took the enable-set with it")
	}

	// No default at all is a refusal too, and a different one: there is nothing
	// to enable, so it must not read as ErrModelNotEnabled — the fix is picking
	// a model, not enabling one.
	none := NewTeamModels("  ", enabled)
	if _, err := none.RequireDefault(); err == nil {
		t.Error("a team with no default resolved a model")
	} else if errors.Is(err, ErrModelNotEnabled) {
		t.Errorf("err = %v, want a missing-default refusal rather than a disabled-model one", err)
	}
}
