package tracker

import (
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// linearNoParent is parent_changed's dedup_key when the parent was cleared.
const linearNoParent = "none"

// DiffLinearSnapshots compares two Linear issue snapshots and returns
// per-action events, the sibling of DiffJiraSnapshots. done is the issue's
// team's done states; entering one of them emits linear:issue:completed. A
// zero prev (no ID, no identifier) is first discovery.
//
// A new identifier — the issue moved to another team, or its team's key was
// renamed — is linear:issue:identifier_changed, emitted ahead of every other
// event the diff finds, since the others describe the issue under the name it
// has now. The caller has already renamed the entity.
//
// State comparisons go through LinearStateRef.SameState, so a state renamed in
// Linear is not a transition: the id is unchanged and the snapshot just stores
// the new name. The assignee is compared on the Linear user id, so a person
// renaming themselves is not a reassignment either. The creator is carried on
// every event and never diffed: Linear does not let it change.
//
// comments is every comment made since prev, oldest first, with TF's marked
// (linearNewComments): the snapshot holds only the newest.
//
// Every event's source time is the issue's updatedAt, which Linear bumps for
// each change diffed here, except commented's, which is its comment's.
func DiffLinearSnapshots(prev, curr domain.LinearSnapshot, entityID string, done []domain.LinearStateRef, comments []NewComment) []domain.Event {
	terminal := func(snap domain.LinearSnapshot) bool {
		return domain.ContainsState(done, snap.StateRef())
	}
	now := time.Now()
	eid := &entityID
	var evts []domain.Event
	emit := func(eventType, dedupKey string, metadata any) {
		emitWithFallback(eventType, dedupKey, curr.UpdatedAt, metadata, eid, now, &evts)
	}
	id := linearIdentity(curr)

	if prev.ID == "" && prev.Identifier == "" {
		switch {
		case terminal(curr):
			emit(domain.EventLinearIssueCompleted, "", events.LinearIssueCompletedMetadata{
				LinearIssueIdentity: id, FinalStatus: curr.State.Name,
			})
		case curr.OpenChildCount > 0:
			// A parent of open sub-issues is a container, not a unit of work:
			// no assigned/available. became_atomic below is the event that
			// discovers it once the sub-issues close.
		case curr.AssigneeUserID != "":
			emit(domain.EventLinearIssueAssigned, "", events.LinearIssueAssignedMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name,
			})
		default:
			emit(domain.EventLinearIssueAvailable, "", events.LinearIssueAvailableMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name,
			})
		}
		return evts
	}

	evts = append(evts, linearIdentifierChangedEvents(prev, curr, entityID)...)

	if prev.BodyHash != "" && curr.BodyHash != "" && prev.BodyHash != curr.BodyHash {
		emit(domain.EventLinearIssueBodyUpdated, "", events.LinearIssueBodyUpdatedMetadata{
			LinearIssueIdentity: id, Labels: curr.Labels,
			PreviousBodyHash: prev.BodyHash, BodyHash: curr.BodyHash,
		})
	}

	if !prev.StateRef().SameState(curr.StateRef()) && curr.State.Name != "" {
		emit(domain.EventLinearIssueStatusChanged, curr.State.Name, events.LinearIssueStatusChangedMetadata{
			LinearIssueIdentity: id, OldStatus: prev.State.Name, NewStatus: curr.State.Name,
			Priority: curr.PriorityLabel,
		})
		if terminal(curr) {
			emit(domain.EventLinearIssueCompleted, "", events.LinearIssueCompletedMetadata{
				LinearIssueIdentity: id, FinalStatus: curr.State.Name,
			})
		}
	}

	// The open-children gate applies to reassignment as it does to first
	// discovery, so a container cannot gain a task by being reassigned.
	if prev.AssigneeUserID != curr.AssigneeUserID && curr.OpenChildCount == 0 {
		if curr.AssigneeUserID != "" {
			emit(domain.EventLinearIssueAssigned, "", events.LinearIssueAssignedMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name,
			})
		} else {
			emit(domain.EventLinearIssueAvailable, "", events.LinearIssueAvailableMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name,
			})
		}
	}

	if prev.Priority != curr.Priority && curr.PriorityLabel != "" {
		emit(domain.EventLinearIssuePriorityChanged, curr.PriorityLabel, events.LinearIssuePriorityChangedMetadata{
			LinearIssueIdentity: id, OldPriority: prev.PriorityLabel, NewPriority: curr.PriorityLabel,
		})
	}

	// One event for the comments TF did not post, however many, attributed to
	// the newest of them and timed at its creation.
	if newest, authors, ok := commentedBy(comments); ok {
		emitWithFallback(domain.EventLinearIssueCommented, "", commentedAt(newest, curr.UpdatedAt), events.LinearIssueCommentedMetadata{
			LinearIssueIdentity: id, CommentID: newest.ID,
			Commenter: newest.Author, CommenterUserID: newest.AuthorID, CommenterUserIDs: authors,
		}, eid, now, &evts)
	}

	// Compared, and keyed, on the parent's UUID: its identifier changes when
	// the parent moves team, which is not a change of parent, and a dedup key
	// that changed with it would open a second task for the same one. The
	// metadata names the parents by identifier, which is what a reader knows
	// them by.
	if prev.ParentID != curr.ParentID {
		key := curr.ParentID
		if curr.ParentID == "" {
			key = linearNoParent
		}
		emit(domain.EventLinearIssueParentChanged, key, events.LinearIssueParentChangedMetadata{
			LinearIssueIdentity: id, OldParent: prev.ParentIdentifier, NewParent: curr.ParentIdentifier,
		})
	}

	if prev.OpenChildCount > 0 && curr.OpenChildCount == 0 && !terminal(curr) {
		emit(domain.EventLinearIssueBecameAtomic, "", events.LinearIssueBecameAtomicMetadata{
			LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name,
		})
	}

	return evts
}

// linearIdentifierChangedEvents is identifier_changed for an issue whose
// identifier went from prev's to curr's, or nothing when it did not change. Its
// source time is curr's updatedAt, as for every other Linear transition.
func linearIdentifierChangedEvents(prev, curr domain.LinearSnapshot, entityID string) []domain.Event {
	if prev.Identifier == "" || curr.Identifier == "" || prev.Identifier == curr.Identifier {
		return nil
	}
	var evts []domain.Event
	emitWithFallback(domain.EventLinearIssueIdentifierChanged, "", curr.UpdatedAt, events.LinearIssueIdentifierChangedMetadata{
		LinearIssueIdentity: linearIdentity(curr),
		OldIdentifier:       prev.Identifier,
		OldLinearTeamID:     prev.TeamID,
		OldLinearTeamKey:    prev.TeamKey,
	}, &entityID, time.Now(), &evts)
	return evts
}

// linearIdentity is the identity block every Linear event's metadata carries.
func linearIdentity(snap domain.LinearSnapshot) events.LinearIssueIdentity {
	return events.LinearIssueIdentity{
		IssueIdentifier: snap.Identifier,
		IssueID:         snap.ID,
		LinearTeamID:    snap.TeamID,
		LinearTeamKey:   snap.TeamKey,
		Assignee:        snap.Assignee,
		AssigneeUserID:  snap.AssigneeUserID,
		Creator:         snap.Creator,
		CreatorUserID:   snap.CreatorUserID,
		Title:           snap.Title,
	}
}

// linearCommentOlder reports whether comment time a is before b. A time that
// does not parse, or is absent, is not older, so a comment whose time is
// unknown still counts as new.
func linearCommentOlder(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	return errA == nil && errB == nil && ta.Before(tb)
}
