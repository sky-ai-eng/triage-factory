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
// the new name. status_changed's dedup_key is the new state's id, so a state
// renamed while a task waits on it does not open a second task the next time
// the issue enters it. The assignee is compared on the Linear user id, so a
// person renaming themselves is not a reassignment either. The creator is
// carried on every event and never diffed: Linear does not let it change.
//
// comments is every comment made since prev, oldest first, with TF's marked
// (linearNewComments): the snapshot holds only the newest.
//
// Labels are compared by id (linearLabelChanges), so a label renamed in Linear
// is not a change. labelGroups maps a label's id to the label group it sits
// in, by the group's id and name; a label it does not name is reported in no
// group. The issue's own labels carry no group, so the caller reads the groups
// from the team's label list (linearLabelGroups).
//
// Every event's source time is the issue's updatedAt, which Linear bumps for
// each change diffed here, except commented's, which is its comment's.
func DiffLinearSnapshots(prev, curr domain.LinearSnapshot, entityID string, done []domain.LinearStateRef, comments []NewComment, labelGroups map[string]domain.LinearLabelRef) []domain.Event {
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
	labelIDs := linearLabelIDs(curr)

	if prev.ID == "" && prev.Identifier == "" {
		switch {
		case terminal(curr):
			emit(domain.EventLinearIssueCompleted, "", events.LinearIssueCompletedMetadata{
				LinearIssueIdentity: id, FinalStatus: curr.State.Name, FinalStatusID: curr.State.ID,
			})
		case curr.OpenChildCount > 0:
			// A parent of open sub-issues is a container, not a unit of work:
			// no assigned/available. became_atomic below is the event that
			// discovers it once the sub-issues close.
		case curr.AssigneeUserID != "":
			emit(domain.EventLinearIssueAssigned, "", events.LinearIssueAssignedMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name, StatusID: curr.State.ID,
				Labels: curr.Labels, LabelIDs: labelIDs,
			})
		default:
			emit(domain.EventLinearIssueAvailable, "", events.LinearIssueAvailableMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name, StatusID: curr.State.ID,
				Labels: curr.Labels, LabelIDs: labelIDs,
			})
		}
		return evts
	}

	evts = append(evts, linearIdentifierChangedEvents(prev, curr, entityID)...)

	if prev.BodyHash != "" && curr.BodyHash != "" && prev.BodyHash != curr.BodyHash {
		emit(domain.EventLinearIssueBodyUpdated, "", events.LinearIssueBodyUpdatedMetadata{
			LinearIssueIdentity: id, Labels: curr.Labels, LabelIDs: labelIDs,
			PreviousBodyHash: prev.BodyHash, BodyHash: curr.BodyHash,
		})
	}

	if !prev.StateRef().SameState(curr.StateRef()) && curr.State.Name != "" {
		emit(domain.EventLinearIssueStatusChanged, linearStateDedupKey(curr.State), events.LinearIssueStatusChangedMetadata{
			LinearIssueIdentity: id,
			OldStatus:           prev.State.Name, OldStatusID: prev.State.ID,
			NewStatus: curr.State.Name, NewStatusID: curr.State.ID,
			Priority: curr.PriorityLabel,
		})
		if terminal(curr) {
			emit(domain.EventLinearIssueCompleted, "", events.LinearIssueCompletedMetadata{
				LinearIssueIdentity: id, FinalStatus: curr.State.Name, FinalStatusID: curr.State.ID,
			})
		}
	}

	// The open-children gate applies to reassignment as it does to first
	// discovery, so a container cannot gain a task by being reassigned.
	if prev.AssigneeUserID != curr.AssigneeUserID && curr.OpenChildCount == 0 {
		if curr.AssigneeUserID != "" {
			emit(domain.EventLinearIssueAssigned, "", events.LinearIssueAssignedMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name, StatusID: curr.State.ID,
				Labels: curr.Labels, LabelIDs: labelIDs,
			})
		} else {
			emit(domain.EventLinearIssueAvailable, "", events.LinearIssueAvailableMetadata{
				LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name, StatusID: curr.State.ID,
				Labels: curr.Labels, LabelIDs: labelIDs,
			})
		}
	}

	if prev.Priority != curr.Priority && curr.PriorityLabel != "" {
		emit(domain.EventLinearIssuePriorityChanged, curr.PriorityLabel, events.LinearIssuePriorityChangedMetadata{
			LinearIssueIdentity: id, OldPriority: prev.PriorityLabel, NewPriority: curr.PriorityLabel,
		})
	}

	// Keyed on the label's id, as parent_changed is on the parent's: a label
	// renamed after it was added, then added again, is the same label and must
	// not open a second task. Not gated on open sub-issues: someone added the
	// label on purpose.
	added, removed := linearLabelChanges(prev, curr)
	for _, l := range added {
		emit(domain.EventLinearIssueLabelAdded, linearLabelDedupKey(l), linearLabelMetadata(id, curr, l, labelGroups[l.ID], labelIDs))
	}
	for _, l := range removed {
		emit(domain.EventLinearIssueLabelRemoved, linearLabelDedupKey(l), linearLabelMetadata(id, curr, l, labelGroups[l.ID], labelIDs))
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
	// metadata names the parents by UUID, which is what a predicate matches,
	// and by identifier, which is what a reader knows them by.
	if prev.ParentID != curr.ParentID {
		key := curr.ParentID
		if curr.ParentID == "" {
			key = linearNoParent
		}
		emit(domain.EventLinearIssueParentChanged, key, events.LinearIssueParentChangedMetadata{
			LinearIssueIdentity: id,
			OldParentID:         prev.ParentID, NewParentID: curr.ParentID,
			OldParent: prev.ParentIdentifier, NewParent: curr.ParentIdentifier,
		})
	}

	if prev.OpenChildCount > 0 && curr.OpenChildCount == 0 && !terminal(curr) {
		emit(domain.EventLinearIssueBecameAtomic, "", events.LinearIssueBecameAtomicMetadata{
			LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name, StatusID: curr.State.ID,
			Labels: curr.Labels, LabelIDs: labelIDs,
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

// linearLabelChanges is the labels curr has that prev does not, and the ones
// prev has that curr does not, compared by id so a renamed label is neither.
// A prev stored before label ids were recorded has names alone, and that one
// diff compares by name: a label it lost is returned with its name and no id.
func linearLabelChanges(prev, curr domain.LinearSnapshot) (added, removed []domain.LinearLabelRef) {
	if len(prev.LabelRefs) != len(prev.Labels) {
		addedNames, removedNames := labelChanges(prev.Labels, curr.Labels)
		addedSet := toSet(addedNames)
		for _, l := range curr.LabelRefs {
			if addedSet[l.Name] {
				added = append(added, l)
			}
		}
		for _, name := range removedNames {
			removed = append(removed, domain.LinearLabelRef{Name: name})
		}
		return added, removed
	}
	prevIDs := make(map[string]bool, len(prev.LabelRefs))
	for _, l := range prev.LabelRefs {
		prevIDs[l.ID] = true
	}
	currIDs := make(map[string]bool, len(curr.LabelRefs))
	for _, l := range curr.LabelRefs {
		currIDs[l.ID] = true
		if !prevIDs[l.ID] {
			added = append(added, l)
		}
	}
	for _, l := range prev.LabelRefs {
		if !currIDs[l.ID] {
			removed = append(removed, l)
		}
	}
	return added, removed
}

// linearLabelMetadata is the metadata of a label event on curr for label l,
// which sits in group (zero for a label in none). labelIDs is curr's label ids.
func linearLabelMetadata(id events.LinearIssueIdentity, curr domain.LinearSnapshot, l, group domain.LinearLabelRef, labelIDs []string) events.LinearIssueLabelMetadata {
	return events.LinearIssueLabelMetadata{
		LinearIssueIdentity: id, Priority: curr.PriorityLabel, Status: curr.State.Name, StatusID: curr.State.ID,
		LabelID: l.ID, LabelName: l.Name, LabelGroupID: group.ID, LabelGroup: group.Name,
		Labels: curr.Labels, LabelIDs: labelIDs,
	}
}

// linearLabelIDs is the ids of snap's labels, in the order of its label names.
func linearLabelIDs(snap domain.LinearSnapshot) []string {
	ids := make([]string, 0, len(snap.LabelRefs))
	for _, l := range snap.LabelRefs {
		ids = append(ids, l.ID)
	}
	return ids
}

// linearStateDedupKey is status_changed's dedup_key: the new state's id, or
// its name for a state read without one.
func linearStateDedupKey(state domain.LinearStateRef) string {
	if state.ID != "" {
		return state.ID
	}
	return state.Name
}

// linearLabelDedupKey is a label event's dedup_key: the label's id, or its name
// for a label only a snapshot from before label ids were recorded knew of.
func linearLabelDedupKey(l domain.LinearLabelRef) string {
	if l.ID != "" {
		return l.ID
	}
	return l.Name
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
