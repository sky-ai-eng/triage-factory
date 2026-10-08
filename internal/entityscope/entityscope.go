// Package entityscope resolves the scope an entity is keyed under for callers
// that hold an org and an object's key but not the org's settings: the exec
// recording funnel, exec memory load, and the produced-entity attach a
// conversation's memory owes.
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
