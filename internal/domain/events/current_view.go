package events

import (
	"context"
	"fmt"
	"sync"
)

// CurrentView rewrites a recorded event's metadata so that the ids in it name
// what they name now: an id the source has since replaced is resolved to its
// replacement. The event row is never rewritten — the log records what was
// true when the event happened — so a reader that acts on those ids (matches a
// handler filter against them, hands them to an agent) reads them through the
// view instead. A view returns metadataJSON unchanged when nothing in it was
// replaced.
type CurrentView func(ctx context.Context, orgID, metadataJSON string) (string, error)

var (
	viewMu       sync.RWMutex
	currentViews = map[string]CurrentView{}
)

// RegisterCurrentView installs eventType's view. A view reads the stores of
// the source that owns the type, so the source registers it when it is
// installed, not from init(). Panics on an empty event type, a nil view, or a
// second registration for the type — wiring bugs, the same posture as
// Register.
func RegisterCurrentView(eventType string, v CurrentView) {
	viewMu.Lock()
	defer viewMu.Unlock()
	if eventType == "" || v == nil {
		panic("events.RegisterCurrentView: event type and view are required")
	}
	if _, dup := currentViews[eventType]; dup {
		panic(fmt.Sprintf("events.RegisterCurrentView: duplicate registration for %q", eventType))
	}
	currentViews[eventType] = v
}

// ResetCurrentView removes eventType's view. Test-only, like Reset.
func ResetCurrentView(eventType string) {
	viewMu.Lock()
	defer viewMu.Unlock()
	delete(currentViews, eventType)
}

// Current returns metadataJSON as eventType's view reads it now, or
// metadataJSON itself when the type registers no view or carries no metadata.
func Current(ctx context.Context, orgID, eventType, metadataJSON string) (string, error) {
	if metadataJSON == "" {
		return metadataJSON, nil
	}
	viewMu.RLock()
	v, ok := currentViews[eventType]
	viewMu.RUnlock()
	if !ok {
		return metadataJSON, nil
	}
	return v(ctx, orgID, metadataJSON)
}
