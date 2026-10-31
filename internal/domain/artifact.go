package domain

import "time"

// Artifact is one object a run's agent changed, read from the ledger: the
// external_actions rows of a conversation with performed_by = 'agent' that
// name an object (ObjectID), grouped by that object. It describes what the
// run did to the object, not the object's current state. It is not stored.
//
// Target and URL are display values. For a kind identified by EntityID (a
// pull request, a GitHub issue) they are the entity's current key and url,
// which follow a rename. For every other kind they are the latest row's
// target and its url, current_url when a rename has moved the object.
// "Latest row" is the greatest (occurred_at, id) in the group.
type Artifact struct {
	ObjectID   string
	ObjectKind string
	Provider   string
	Scope      string

	EntityID     string
	RepositoryID string
	ObjectKey    string

	Target string
	URL    string

	// LastAction is the action of the latest row, ActionCount the number of
	// rows in the group, and FirstAt / LastAt the occurred_at of its earliest
	// and latest rows.
	LastAction  string
	ActionCount int
	FirstAt     time.Time
	LastAt      time.Time
}

// ArtifactDeliverable returns the deliverable among deliverables that is the
// same object as a, and false when none is. A pull request deliverable matches
// a pull_request artifact on the same entity. A review deliverable matches a
// review artifact in the same scope whose key names the review id the
// deliverable carries as its external_id, which it has once it is submitted:
// a staged review has no artifact to match. When more than one deliverable
// matches, the first in the slice answers.
func ArtifactDeliverable(a Artifact, deliverables []Deliverable) (Deliverable, bool) {
	for _, d := range deliverables {
		switch {
		case a.ObjectKind == ObjectKindPullRequest && d.Kind == DeliverableKindPullRequest:
			if d.EntityID != "" && d.EntityID == a.EntityID {
				return d, true
			}
		case a.ObjectKind == ObjectKindReview && d.Kind == DeliverableKindReview:
			if key := GitHubReviewObjectKey(d.ExternalID); key != "" && key == a.ObjectKey && d.Scope == a.Scope {
				return d, true
			}
		}
	}
	return Deliverable{}, false
}
