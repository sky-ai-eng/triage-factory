package server

import (
	"context"
	"encoding/json"
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

// trackedIDFaults is the body of a check for predicate fields that name rows
// the handler's team tracks by id, such as linear_team_id_in and
// jira_project_id_in and their old_ siblings: every entry predicate adds over
// prior to one of fields must be in the set tracked returns. tracked is read
// only when an entry was added. Each fault is the field, the quoted entry,
// and hint.
func trackedIDFaults(predicate, prior *string, fields []string, hint string, tracked func() (map[string]bool, error)) ([]string, error) {
	added := make([][]string, len(fields))
	anyAdded := false
	for i, field := range fields {
		priorIn, err := predicateIDList(prior, field)
		if err != nil {
			return nil, err
		}
		nextIn, err := predicateIDList(predicate, field)
		if err != nil {
			return nil, err
		}
		added[i] = addedEntries(priorIn, nextIn)
		anyAdded = anyAdded || len(added[i]) > 0
	}
	if !anyAdded {
		return nil, nil
	}
	set, err := tracked()
	if err != nil {
		return nil, err
	}
	var faults []string
	for i, field := range fields {
		for _, id := range added[i] {
			if !set[id] {
				faults = append(faults, fmt.Sprintf("%s: %q %s", field, id, hint))
			}
		}
	}
	return faults, nil
}

// predicateIDList reads one list field from a predicate; nil for match-all or
// a predicate without the field.
func predicateIDList(predicate *string, field string) ([]string, error) {
	if predicate == nil {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*predicate), &fields); err != nil {
		return nil, fmt.Errorf("decode predicate: %w", err)
	}
	raw, ok := fields[field]
	if !ok {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("decode predicate %s: %w", field, err)
	}
	return ids, nil
}

// addedEntries returns the entries of next that prior does not hold, each
// once, in next's order. The stored entries are not re-checked, so a reference
// that went stale after it was stored never blocks an edit that keeps it.
func addedEntries(prior, next []string) []string {
	seen := make(map[string]bool, len(prior))
	for _, id := range prior {
		seen[id] = true
	}
	var added []string
	for _, id := range next {
		if !seen[id] {
			seen[id] = true
			added = append(added, id)
		}
	}
	return added
}
