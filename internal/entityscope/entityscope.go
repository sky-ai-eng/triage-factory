// Package entityscope resolves entities for callers that hold an org and an
// object's key but not the org's settings: the exec recording funnel, exec
// memory load, and the produced-entity attach a conversation's memory owes. Of
// says which scope the object is keyed under; Resolve finds or creates its
// entity there under the identity rule.
package entityscope

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Of returns the scope an entity of source is keyed under in orgID, read from
// the org's settings through domain.EntityScope. "" with a nil error means the
// source has no scope in the org — it is not configured there, or it is a
// source TF keys no entities under — and a caller records, creates and finds
// nothing for it.
func Of(ctx context.Context, stores db.Stores, orgID, source string) (string, error) {
	if stores.Orgs == nil {
		return "", errors.New("entityscope: no org store to read settings from")
	}
	settings, err := stores.Orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("entityscope: load org settings: %w", err)
	}
	return domain.EntityScope(source, settings), nil
}

// A Resolver finds or creates the entity a source keys under sourceID, in
// place of the entity store, for a source whose entities hold a reference
// only the source can fill in: a Slack thread holds its channel's registry
// row, which ee/slack resolves. stores are the resolving process's; kind and
// url are what Resolve would create the row with.
type Resolver func(ctx context.Context, stores db.Stores, orgID, sourceID, kind, url string) (*domain.Entity, error)

var (
	resolverMu sync.RWMutex
	resolvers  = map[string]Resolver{}
)

// RegisterResolver installs source's Resolver. A resolver is handed the
// stores of the process resolving — an executor installs no routes, and it
// records the entities its runs touch — so a source registers it from init().
// Panics on an empty source, a nil resolver, or a second registration for the
// source — wiring bugs, the posture of events.RegisterCurrentView.
func RegisterResolver(source string, r Resolver) {
	resolverMu.Lock()
	defer resolverMu.Unlock()
	if source == "" || r == nil {
		panic("entityscope.RegisterResolver: source and resolver are required")
	}
	if _, dup := resolvers[source]; dup {
		panic(fmt.Sprintf("entityscope.RegisterResolver: duplicate registration for %q", source))
	}
	resolvers[source] = r
}

// ResetResolver removes source's Resolver. Test-only.
func ResetResolver(source string) {
	resolverMu.Lock()
	defer resolverMu.Unlock()
	delete(resolvers, source)
}

// Resolve returns the entity for ref, creating one when no row answers for
// it. ref.Scope must be set (Of). A source that registered a Resolver
// resolves its own entities. For every other source the order is the
// identity rule's:
//
//   - with ref.ExternalID, the row carrying it in the scope, whatever key it is
//     stored under. When follow is true, ref.SourceID is the key the provider
//     answers with now — it came off a response — and a row stored under
//     another key is renamed onto it (RenameSystem, whose poll_seq bump makes
//     a poll cycle that read the row under its old key drop that read rather
//     than commit it over the rename); when the rename is refused
//     because an active row still holds the key, the row is returned under its
//     stored key, which a later read renames. When follow is false the key may
//     be older than the row's (an artifact's target), and the row is returned
//     as stored.
//   - otherwise the row FindOrCreateSystem returns for the key, which skips a
//     row carrying another id; one with no id learns ref.ExternalID
//     (StampExternalIDSystem) — the path a row created before its provider id
//     was recorded learns it by.
//   - otherwise a new row, carrying ref.ExternalID.
//
// container is what the same response as ref says the object sits in, which
// a new row is created with: a Linear row only with its team
// (db.EntityStore.FindOrCreateLinearSystem) and a Jira row only with its
// project (db.EntityStore.FindOrCreateJiraSystem). With it empty, a Linear or
// Jira ref that matches no row is refused (db.ErrEntityLinearTeamRequired,
// db.ErrEntityJiraProjectRequired). Every other source ignores it.
//
// url is the link a new or renamed row is stored under where TF does not build
// it from the key itself (domain.EntityURL). title is left empty: the poll
// cycle, or the source's own ingest, fills it.
func Resolve(ctx context.Context, stores db.Stores, orgID string, ref domain.EntityRef, container domain.EntityContainer, kind, url string, follow bool) (*domain.Entity, error) {
	if link := domain.EntityURL(ref.Source, ref.Scope, ref.SourceID); link != "" {
		url = link
	}
	resolverMu.RLock()
	r := resolvers[ref.Source]
	resolverMu.RUnlock()
	if r != nil {
		return r(ctx, stores, orgID, ref.SourceID, kind, url)
	}
	entities := stores.Entities
	if ref.ExternalID != "" {
		e, err := entities.GetByExternalIDSystem(ctx, orgID, ref.Source, ref.Scope, ref.ExternalID)
		if err != nil {
			return nil, err
		}
		if e != nil {
			if !follow || e.SourceID == ref.SourceID {
				return e, nil
			}
			out, err := entities.RenameSystem(ctx, orgID, ref.Source, ref.Scope, ref.ExternalID, ref.SourceID, url)
			if errors.Is(err, db.ErrEntityKeyOccupied) {
				return e, nil
			}
			if err != nil {
				return nil, err
			}
			if out.Renamed {
				e.SourceID, e.PollSeq = ref.SourceID, out.PollSeq
				if url != "" {
					e.URL = url
				}
			}
			return e, nil
		}
	}
	var e *domain.Entity
	var err error
	switch {
	case ref.Source == domain.ArtifactProviderLinear && container.LinearTeamID != "":
		e, _, err = entities.FindOrCreateLinearSystem(ctx, orgID, ref.Scope, ref.SourceID, ref.ExternalID, container.LinearTeamID, "", url)
	case ref.Source == domain.ArtifactProviderJira && container.JiraProject.ID != "" && container.JiraProject.Key != "":
		e, _, err = entities.FindOrCreateJiraSystem(ctx, orgID, ref.Scope, ref.SourceID, ref.ExternalID, container.JiraProject, "", url)
	default:
		e, _, err = entities.FindOrCreateSystem(ctx, orgID, ref.Source, ref.Scope, ref.SourceID, ref.ExternalID, kind, "", url)
	}
	if err != nil {
		return nil, err
	}
	if ref.ExternalID != "" && e.ExternalID == "" {
		stamped, err := entities.StampExternalIDSystem(ctx, orgID, e.ID, ref.ExternalID)
		if err != nil {
			return nil, err
		}
		if stamped != nil {
			e = stamped
		}
	}
	return e, nil
}
