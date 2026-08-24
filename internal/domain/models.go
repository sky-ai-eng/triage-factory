package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

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
//
// Which of the two vocabularies an enable-set is written in follows from the
// same place: a set is stored catalog keys, and the keys a deployment may store
// are its universe's (modelcatalog.UniverseFor). The two never mix in one set,
// because no deployment can dispatch both.
const (
	ModelAliasHaiku  = "haiku"
	ModelAliasSonnet = "sonnet"
	ModelAliasOpus   = "opus"
	ModelAliasFable  = "fable"
)

// ModelSet is a resolved enable-set: the models one org, or one team, may pick
// from. It is the selection-time control — its whole effect is a smaller
// picker, and it says which models may be chosen without claiming any is better
// than another, which is the claim a ranked ceiling has to make and the catalog
// declines to.
//
// The zero value admits everything. That is for a caller who has no enablement
// to apply at all — a test fixture with no stores behind it — and never for
// stored configuration: OrgModelSet resolves an absent stored set to the
// deployment's whole universe rather than to this, so a set built from a
// settings row always names its members.
//
// A RESOLVED set naming nothing admits nothing, and that is a different answer
// from the zero value's. It is what a team disjoint from its org resolves to,
// and every model then refuses — which is the right direction for a set nobody
// can read as a choice.
type ModelSet struct {
	// keys is nil for the unrestricted zero value and non-nil (possibly empty)
	// for a resolved one, which is what keeps "admits everything" and "admits
	// nothing in common" distinguishable.
	keys  map[string]bool
	order []string
}

// OrgModelSet resolves an org's stored enable-set. stored is org_settings
// .enabled_models as it was written; universeDefault is the set an org that has
// never expressed a preference gets — every model this DEPLOYMENT offers, which
// callers read from modelcatalog.UniverseFor(mode).DefaultEnabled().
//
// It is a parameter because the two answers differ and this package cannot ask:
// a multi deployment offers native wire ids and a local one the harness aliases
// its subprocess resolves, and domain sits below the registry that knows. A set
// is therefore always in one vocabulary — whichever this caller's deployment
// dispatches — and a stored key from the other one is simply a key nothing
// enables.
//
// An absent stored set tracks the default, so a model added by a later release
// is enabled for that org the day it ships. A stored one is frozen at what it
// names, which is the same distinction, read from the other side: an admin who
// picked a set picked THAT set, and a new model is a decision they have not
// made yet.
//
// Absent is nil and nothing else. A stored set naming nothing is a set naming
// nothing — it enables no model rather than widening back to the default, which
// would silently ignore what the row says. The write path refuses to create one
// (the PATCH rejects an empty array, and clearing writes NULL), so this is the
// reading of a row nobody should have written, in the direction that refuses
// rather than the one that grants.
func OrgModelSet(stored, universeDefault []string) ModelSet {
	if stored == nil {
		stored = universeDefault
	}
	return newModelSet(stored)
}

// TeamModelSet resolves a team's effective enable-set: what the team stored,
// narrowed to what its org still enables. An absent stored set — nil, and only
// nil, on the same rule as OrgModelSet — inherits the org's answer whole.
//
// The intersection is not redundant with the ⊆ check the team write enforces.
// That check holds at the moment of the save; the org may shrink its own set
// afterwards, and nothing rewrites a team row when it does — so the narrowing
// has to happen here, at the read, where both halves are in hand.
func TeamModelSet(stored []string, org ModelSet) ModelSet {
	if stored == nil {
		return org
	}
	keep := make([]string, 0, len(stored))
	for _, k := range stored {
		if org.Has(k) {
			keep = append(keep, k)
		}
	}
	return newModelSet(keep)
}

// newModelSet builds a set from a list, preserving first-seen order so a
// refusal names its members the way the caller wrote them.
func newModelSet(keys []string) ModelSet {
	set := ModelSet{keys: make(map[string]bool, len(keys)), order: make([]string, 0, len(keys))}
	for _, k := range keys {
		if set.keys[k] {
			continue
		}
		set.keys[k] = true
		set.order = append(set.order, k)
	}
	return set
}

// Has reports whether key is enabled. The unrestricted zero value answers true
// for everything, including a key no deployment offers — a set that never
// narrowed anything is not the surface that decides what exists.
func (s ModelSet) Has(key string) bool {
	if s.keys == nil {
		return true
	}
	return s.keys[key]
}

// Keys returns the set's members in the order they were resolved. Empty for the
// unrestricted zero value, which has no members to name.
func (s ModelSet) Keys() []string { return slices.Clone(s.order) }

// String renders the set for a refusal message, which is the only reason a
// caller needs to see it as prose: a run that will not start has to say which
// models this team may pick instead. Bare, with no punctuation of its own —
// every caller wraps it in its own sentence.
func (s ModelSet) String() string {
	if len(s.order) == 0 {
		return "none"
	}
	return strings.Join(s.order, ", ")
}

// ErrModelNotEnabled is a model that is real, priced and reachable, and that
// this team may nonetheless not run: its org or its team took it out of the
// enable-set. Distinct from an unconnected provider because the remedies
// differ — one is a credential nobody bound, the other a decision somebody
// made — and a caller told the wrong one goes to the wrong settings page.
//
// It is a refusal and never a substitution. Every wrapper names the model and
// the set that excludes it, because those two facts are the whole fix.
var ErrModelNotEnabled = errors.New("model is not enabled for this team")

// TeamModels is one team's resolved model configuration at a moment in time:
// the model an unset step inherits, and the set every model that team runs has
// to belong to.
//
// The two travel together because they are one answer to one question — "what
// may this team run right now" — read from two rows that can move independently.
// Resolving them apart is how a default validated against one org's set gets
// dispatched under another's.
//
// The default is unexported and reachable only through RequireDefault, which is
// the enforcement: a caller cannot read it without being handed the refusal it
// may carry. The set is not, because a set is never wrong — it is whatever it
// resolved to, empty included.
type TeamModels struct {
	def     string
	enabled ModelSet
}

// NewTeamModels pairs a team's default with the set it is held to.
func NewTeamModels(defaultModel string, enabled ModelSet) TeamModels {
	return TeamModels{def: defaultModel, enabled: enabled}
}

// Enabled is the set every model this team dispatches has to belong to — its
// own stored set narrowed to its org's, or its org's whole set when it stored
// none.
func (m TeamModels) Enabled() ModelSet { return m.enabled }

// RequireDefault resolves the model an unset step inherits, refusing when the
// team's enable-set no longer includes it — the org may have disabled it since
// it was picked, and nothing rewrites a team's row when that happens.
//
// Ask it wherever the default is about to be USED, and nowhere else. A pinned
// step is held to the set by its own pin and has no business failing over a
// default it does not read; that distinction is what keeps a mid-flight
// blueprint of pinned steps running while its team re-picks.
//
// A refusal is never a substitution: TF ships no fallback model, so a run
// started on something else would make the transcript a lie about what produced
// it and the ledger a lie about what was bought.
func (m TeamModels) RequireDefault() (string, error) {
	model := strings.TrimSpace(m.def)
	if model == "" {
		return "", errors.New("this team has no default model — pick one in Settings")
	}
	if !m.enabled.Has(model) {
		return "", fmt.Errorf(
			"%w: the team default is %s, which its enabled models no longer include (%s) — pick a model from that set in Settings",
			ErrModelNotEnabled, model, m.enabled)
	}
	return model, nil
}
