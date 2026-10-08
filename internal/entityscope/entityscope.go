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

// Resolve returns the entity for ref, creating one when no row answers for
// it. ref.Scope must be set (Of). The order is the identity rule's:
//
//   - with ref.ExternalID, the row carrying it in the scope, whatever key it is
//     stored under. When follow is true, ref.SourceID is the key the provider
//     answers with now — it came off a response — and a row stored under
//     another key is renamed onto it (RenameSystem); when the rename is refused
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
// url is the link a new or renamed row is stored under where TF does not build
// it from the key itself (domain.EntityURL). title is left empty: the poll
// cycle, or the source's own ingest, fills it.
func Resolve(ctx context.Context, entities db.EntityStore, orgID string, ref domain.EntityRef, kind, url string, follow bool) (*domain.Entity, error) {
	if link := domain.EntityURL(ref.Source, ref.Scope, ref.SourceID); link != "" {
		url = link
	}
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
				e.SourceID = ref.SourceID
				if url != "" {
					e.URL = url
				}
			}
			return e, nil
		}
	}
	e, _, err := entities.FindOrCreateSystem(ctx, orgID, ref.Source, ref.Scope, ref.SourceID, ref.ExternalID, kind, "", url)
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
