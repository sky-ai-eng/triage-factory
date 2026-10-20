package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// PredicateReferenceCheck reports the entries of an event handler's scope
// predicate that name something the handler's team cannot match on, such as a
// row the team does not track or an id of the wrong kind. The predicate
// validator cannot catch these: the value is well-formed, and whether it
// resolves depends on stored rows. Each returned string is one fault's
// message, and it names the predicate field it is about.
//
// predicate is the canonical predicate the write would store, and prior is the
// one the row stores now. Either is nil for match-all, and prior is nil on
// create. A check refuses only entries predicate adds over prior, so a
// reference that went stale after it was stored (its row untracked since)
// never blocks an edit that leaves it in place.
//
// It runs in the write's own transaction, under the caller's claims, so the
// rows it reads are the ones the write commits against.
type PredicateReferenceCheck func(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]string, error)

var (
	predicateReferenceMu     sync.RWMutex
	predicateReferenceChecks = map[string]PredicateReferenceCheck{}
)

// RegisterPredicateReferenceCheck declares eventType's check. A source outside
// core registers its own from init(), beside its event schema, so core holds
// none of its symbols. Panics on an empty event type, a nil check, or a second
// registration for the type: each is a wiring bug.
func RegisterPredicateReferenceCheck(eventType string, check PredicateReferenceCheck) {
	predicateReferenceMu.Lock()
	defer predicateReferenceMu.Unlock()
	if eventType == "" || check == nil {
		panic("server.RegisterPredicateReferenceCheck: event type and check are required")
	}
	if _, dup := predicateReferenceChecks[eventType]; dup {
		panic(fmt.Sprintf("server.RegisterPredicateReferenceCheck: duplicate registration for %q", eventType))
	}
	predicateReferenceChecks[eventType] = check
}

// predicateReferenceError carries a check's faults out of the write
// transaction they were found in, rolling the write back.
type predicateReferenceError struct{ faults []httpx.ErrorItem }

func (e *predicateReferenceError) Error() string {
	return "scope_predicate names something the team cannot match on"
}

// checkPredicateReferences runs eventType's registered check, if it has one,
// for a transaction body. Faults come back as a *predicateReferenceError,
// which writePredicateReferenceError answers.
func checkPredicateReferences(ctx context.Context, tx db.TxStores, orgID, teamID, eventType string, predicate, prior *string) error {
	predicateReferenceMu.RLock()
	check, ok := predicateReferenceChecks[eventType]
	predicateReferenceMu.RUnlock()
	if !ok {
		return nil
	}
	messages, err := check(ctx, tx, orgID, teamID, predicate, prior)
	if err != nil {
		return fmt.Errorf("check %s predicate references: %w", eventType, err)
	}
	if len(messages) == 0 {
		return nil
	}
	faults := make([]httpx.ErrorItem, 0, len(messages))
	for _, msg := range messages {
		faults = append(faults, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: msg, Field: fieldScopePredicate})
	}
	return &predicateReferenceError{faults: faults}
}

// writePredicateReferenceError answers a *predicateReferenceError with 422
// INVALID_FIELD: the predicate is well-formed and names something the caller
// can fix, the same fault class as an unavailable event source.
func writePredicateReferenceError(w http.ResponseWriter, err error) bool {
	var pe *predicateReferenceError
	if !errors.As(err, &pe) {
		return false
	}
	httpx.WriteErrors(w, http.StatusUnprocessableEntity, pe.faults...)
	return true
}
