package modelcatalog

// Model is one row of a deployment's universe — the shape every surface that
// offers, validates or probes a model reads, whichever execution vocabulary the
// deployment speaks.
//
// Two fields are optional, and they are optional because the vocabularies know
// different things rather than because a row may be half-filled. A native id
// names the provider that serves it and resolves in the pricing datasheet, so
// Provider and Facts are always there. An SDK alias names neither: the harness
// resolves it against whichever access path its environment selects, so the
// provider is a property of the credential and the cost is settled by the
// harness rather than interpolated from a price table. A reader renders what is
// present and asserts nothing about what is not.
type Model struct {
	Key          string
	DisplayName  string
	DisplayOrder int
	// Provider is the access path this key is reached through, empty where the
	// id does not name one.
	Provider string
	// Facts are the datasheet's, nil where no datasheet row backs the id.
	Facts *Facts
}

// Facts are what the pricing datasheet knows about a model, joined in at
// process start. Present only on a native row.
type Facts struct {
	Prices                PricesPerMTok
	ContextWindow         int
	SupportsPromptCaching bool
}

// Universe is the set of models one deployment may offer or store — the single
// answer to that question, so a validator, a picker and a probe cannot each
// have their own.
//
// It is a value rather than package state because the answer depends on the
// deployment, and the deployment is a parameter here (see UniverseFor).
type Universe struct {
	models []Model
	// sdk names the harness whose vocabulary this is, empty on the native
	// universe. Nothing reads it yet: it is what a second SDK turns into the
	// wire's `sdk` field and the stored (sdk, model) identity.
	sdk string
}

// UniverseFor returns the models this deployment may offer or store.
//
// multiMode is a PARAMETER and this package never reads runmode: both universes
// have to be exercisable from one test binary, and a package that reads the
// ambient mode can only ever answer for the process it is running in.
//
// The mode is what SELECTS here, not what the axis is. The axis is the
// execution vocabulary — a native registry of bifrost wire ids joined to the
// datasheet, and a per-SDK list of that harness's own aliases — and today the
// mode settles which one a deployment speaks, because the dialect is the mode
// and the enqueue stamps the runtime ratchet per dialect: Postgres mints
// delegations native, SQLite mints them sdk. When a deployment can configure
// more than one harness, this takes the access path instead and the mode stops
// deciding anything.
func UniverseFor(multiMode bool) Universe {
	if multiMode {
		return Universe{models: nativeModels()}
	}
	return Universe{models: SDKModels(SDKClaudeCode), sdk: SDKClaudeCode}
}

// nativeModels projects the datasheet-joined registry onto the universe's row
// shape.
func nativeModels() []Model {
	src := entries
	out := make([]Model, 0, len(src))
	for _, e := range src {
		facts := Facts{
			Prices:                e.Prices,
			ContextWindow:         e.ContextWindow,
			SupportsPromptCaching: e.SupportsPromptCaching,
		}
		out = append(out, Model{
			Key:          e.Key,
			DisplayName:  e.DisplayName,
			DisplayOrder: e.DisplayOrder,
			Provider:     e.Provider,
			Facts:        &facts,
		})
	}
	return out
}

// Models returns the universe in display order. Freshly copied per call: it is
// read on an API request path, and a shared slice header is one careless append
// away from a caller reordering every other caller's answer.
func (u Universe) Models() []Model {
	out := make([]Model, len(u.models))
	copy(out, u.models)
	return out
}

// Keys returns the offered keys in display order — the accepted set for any
// stored model value, and what an error message names when it refuses one. A
// validator wants this rather than Models: what makes a value acceptable is
// that this deployment names it, and nothing about its price or window enters
// that.
func (u Universe) Keys() []string {
	keys := make([]string, 0, len(u.models))
	for _, m := range u.models {
		keys = append(keys, m.Key)
	}
	return keys
}

// Offers reports whether key is one this deployment may offer or store. The
// membership test every surface that validates a stored model value runs, so
// "offered" has one definition rather than one per handler.
//
// It answers for the DEPLOYMENT, not for a tenant. Whether a tenant may pick
// the model is the enable-set's question (domain.ModelSet), and the two are
// asked at different moments on purpose: a stored model is validated against
// the universe when it is SAVED, because a name this deployment cannot dispatch
// is a spelling mistake nothing will ever fix, and against the enable-set at
// DISPATCH, because a set drifts after a save and a pin refused at save time
// could never be re-enabled.
func (u Universe) Offers(key string) bool {
	_, ok := u.Lookup(key)
	return ok
}

// Lookup resolves one key to its row. Not-ok for a key outside the universe,
// which is the same answer Offers gives — one predicate, so a route that 404s
// an unknown key and a validator that refuses one cannot disagree.
func (u Universe) Lookup(key string) (Model, bool) {
	for _, m := range u.models {
		if m.Key == key {
			return m, true
		}
	}
	return Model{}, false
}

// CandidatesFor returns the models a probe sweep of one credential family
// should test.
//
// The rule differs by vocabulary because what a provider IS differs by
// vocabulary. On the native universe the provider is a property of the id, so
// the candidates are the ids that name this one. On an SDK universe no id names
// a provider — the harness resolves an alias against whichever access path its
// environment selects — so one credential family reaches every alias and the
// whole list is the candidate set.
func (u Universe) CandidatesFor(provider string) []Model {
	out := make([]Model, 0, len(u.models))
	for _, m := range u.models {
		if m.Provider == "" || m.Provider == provider {
			out = append(out, m)
		}
	}
	return out
}

// DefaultEnabled is the enable-set an org that has never expressed a preference
// resolves to: every model in this deployment's universe, so a model a later
// release adds is enabled for such an org the day it ships.
//
// It is the second argument to domain.OrgModelSet, which owns the absent-value
// decision — a universe owns only what the default IS, since it is the thing
// that knows what this deployment can dispatch.
func (u Universe) DefaultEnabled() []string { return u.Keys() }
