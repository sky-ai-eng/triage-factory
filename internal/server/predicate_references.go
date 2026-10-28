package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// PredicateReferenceCheck reports the entries of an event handler's scope
// predicate that name something the handler's team cannot match on, such as a
// row the team does not track or an id of the wrong kind. The predicate
// validator cannot catch these: the value is well-formed, and whether it
// resolves depends on stored rows. Each returned fault is one refused entry.
//
// predicate is the canonical predicate the write would store, or, when the
// write is already refused for some of its values, the rest of it
// (events.PredicateError.Remainder), so the answer names every fault at once.
// prior is the one the row stores now. Either is nil for match-all, and prior
// is nil on create. A check refuses only entries predicate adds over prior, so a
// reference that went stale after it was stored (its row untracked since)
// never blocks an edit that leaves it in place.
//
// It runs in the write's own transaction, under the caller's claims, so the
// rows it reads are the ones the write commits against.
type PredicateReferenceCheck func(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]PredicateReferenceFault, error)

// PredicateReferenceFault is one entry a PredicateReferenceCheck refuses: the
// predicate key it is in, which the answer names as the fault's field, and a
// message naming the entry and what the key takes.
type PredicateReferenceFault struct {
	Key     string
	Message string
}

// predicateField is the error field for a fault in one predicate key, or in
// the predicate as a whole when key is empty.
func predicateField(key string) string {
	if key == "" {
		return fieldScopePredicate
	}
	return fieldScopePredicate + "." + key
}

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

// unprocessableError carries a write's 422 faults out of the transaction
// they were completed in, rolling the write back.
type unprocessableError struct{ faults []httpx.ErrorItem }

func (e *unprocessableError) Error() string {
	return "the event handler write has faults the caller can fix"
}

// checkPredicateReferences runs eventType's registered check, if it has one,
// for a transaction body, and refuses the write when the request has any 422
// fault. found is the ones met before the transaction outside the predicate's
// references, such as an out-of-range value or a repeated list entry, and
// catalogFaults are the provider catalog check's (checkPredicateCatalog). The
// write routes carry both here rather than answering them early, so a request
// is told every 422 fault in one answer: found, then the registered check's,
// then catalogFaults. Faults come back as an *unprocessableError, which
// writeUnprocessableError answers.
func checkPredicateReferences(ctx context.Context, tx db.TxStores, orgID, teamID, eventType string, predicate, prior *string, found, catalogFaults []httpx.ErrorItem) error {
	faults := slices.Clone(found)
	predicateReferenceMu.RLock()
	check, ok := predicateReferenceChecks[eventType]
	predicateReferenceMu.RUnlock()
	if ok {
		refused, err := check(ctx, tx, orgID, teamID, predicate, prior)
		if err != nil {
			return fmt.Errorf("check %s predicate references: %w", eventType, err)
		}
		for _, f := range refused {
			faults = append(faults, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: f.Message, Field: predicateField(f.Key)})
		}
	}
	faults = append(faults, catalogFaults...)
	if len(faults) == 0 {
		return nil
	}
	return &unprocessableError{faults: faults}
}

// writeUnprocessableError answers an *unprocessableError with 422: every
// fault in it is a well-formed value the caller can fix, such as a predicate
// entry the team cannot match on or an unavailable event source.
func writeUnprocessableError(w http.ResponseWriter, err error) bool {
	var ue *unprocessableError
	if !errors.As(err, &ue) {
		return false
	}
	httpx.WriteErrors(w, http.StatusUnprocessableEntity, ue.faults...)
	return true
}

// trackedIDFaults is the body of a check for predicate fields that name rows
// the handler's team tracks by id, such as linear_team_id_in and
// jira_project_id_in and their old_ siblings: every entry predicate adds over
// prior to one of fields must be in the set tracked returns. tracked is read
// only when an entry was added. Each fault's message is the field, the quoted
// entry, and hint.
func trackedIDFaults(predicate, prior *string, fields []string, hint string, tracked func() (map[string]bool, error)) ([]PredicateReferenceFault, error) {
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
	var faults []PredicateReferenceFault
	for i, field := range fields {
		for _, id := range added[i] {
			if !set[id] {
				faults = append(faults, PredicateReferenceFault{Key: field, Message: fmt.Sprintf("%s: %q %s", field, id, hint)})
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
