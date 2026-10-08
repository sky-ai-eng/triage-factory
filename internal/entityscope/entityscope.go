// Package entityscope resolves the scope an entity is keyed under for callers
// that hold an org and an object's key but not the settings or connection the
// scope comes from: the exec recording funnel, exec memory load, and the
// produced-entity attach a conversation's memory owes.
//
// A core source's scope is a function of the org's settings and is computed by
// domain.EntityScope. A source declared outside core whose scope is not —
// Slack, where an org can connect several workspaces and the scope is the one
// the object lives in — registers a Resolver here, the way it registers its
// event-source probe, so core never holds that source's symbols.
package entityscope

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Resolver answers the scope of the entity sourceID names in orgID, or "" when
// it cannot name one (the object is not reachable through any connection the
// org has). stores is the caller's whole bundle, extensions included.
type Resolver func(ctx context.Context, stores db.Stores, orgID, sourceID string) (string, error)

var (
	mu        sync.RWMutex
	resolvers = map[string]Resolver{}
)

// coreSources are the sources domain.EntityScope answers for. A registration
// may not shadow one: two answers for one source is how two callers key one
// object under different scopes.
var coreSources = map[string]bool{"github": true, "jira": true, "linear": true}

// Register installs the resolver for source. It panics on an empty source, a
// nil resolver, a core source, or a second registration, all of which are
// wiring mistakes caught at init.
func Register(source string, r Resolver) {
	if source == "" || r == nil {
		panic("entityscope: Register needs a source and a resolver")
	}
	if coreSources[source] {
		panic(fmt.Sprintf("entityscope: %q is a core source; its scope comes from org settings", source))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := resolvers[source]; dup {
		panic(fmt.Sprintf("entityscope: %q registered twice", source))
	}
	resolvers[source] = r
}

// Of returns the scope an entity of source with key sourceID is keyed under in
// orgID. A core source reads the org's settings through domain.EntityScope; any
// other asks its registered resolver. "" with a nil error means the object has
// no scope here — the source is not configured for the org, or no resolver
// knows it — and a caller records, creates and finds nothing for it.
func Of(ctx context.Context, stores db.Stores, orgID, source, sourceID string) (string, error) {
	if coreSources[source] {
		if stores.Orgs == nil {
			return "", errors.New("entityscope: no org store to read settings from")
		}
		settings, err := stores.Orgs.GetSettingsSystem(ctx, orgID)
		if err != nil {
			return "", fmt.Errorf("entityscope: load org settings: %w", err)
		}
		return domain.EntityScope(source, settings), nil
	}
	mu.RLock()
	r := resolvers[source]
	mu.RUnlock()
	if r == nil {
		return "", nil
	}
	return r(ctx, stores, orgID, sourceID)
}
