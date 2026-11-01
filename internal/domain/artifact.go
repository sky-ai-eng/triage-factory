package domain

import (
	"slices"
	"strings"
	"time"
)

// ArtifactPerformers is who performs the rows a run's artifacts are read
// from: its agent, and a person. Every person row that names a conversation
// is a person acting on that conversation's own deliverables — approving,
// editing, dismissing or rejecting a draft pull request, posting a staged
// review, closing the task — so the review a person's approval posts is the
// run's artifact, and so is the close of a draft the run opened. TF's own
// rows (the board mirrors, an event-driven teardown) are not the run's work.
func ArtifactPerformers() []string {
	return []string{PerformedByAgent, PerformedByPerson}
}

// Artifact is one object a run changed, read from the ledger: the
// external_actions rows of a conversation performed by one of
// ArtifactPerformers that name an object (ObjectID), grouped by that object.
// It describes what the run did to the object, not the object's current
// state. It is not stored.
//
// Target and URL are display values, read so that they follow a rename:
//   - Target is the current key of the entity the group's rows name, for a
//     kind that is an entity (a pull request, an issue) or lives on one (a
//     comment, a review, a message), taken from the latest row that names one:
//     a comment's edit and reaction rows name only its repository. For every
//     other kind, and when no row names an entity, it is the latest row's
//     target.
//   - URL is the entity's url for a pull request or an issue. For every other
//     kind, and while the entity has none, it is the latest url any row in the
//     group carries (current_url when a rename has moved the object), since a
//     reaction's row carries none.
//
// EntityID is that same entity, the one Target names, so a comment whose
// latest row is a reaction still carries the pull request it is on.
//
// "Latest row" is the row with the greatest occurred_at. SQLite, whose
// occurred_at is whole seconds, breaks a tie in write order.
type Artifact struct {
	ObjectID   ObjectRef
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

// ObjectActivity is one object the runs of a team or an org changed: the
// artifact rows of the scope that name it, grouped by the object alone rather
// than per conversation. The Artifact fields and their display rule are read
// over that whole group. ConversationCount is the number of distinct
// conversations among the rows (a row whose conversation was purged counts
// toward none), and TeamIDs the distinct teams the rows name, sorted.
type ObjectActivity struct {
	Artifact

	ConversationCount int
	TeamIDs           []string
}

// ArtifactDeliverable returns the deliverable among deliverables that is the
// same object as a, and false when none is. A pull request deliverable matches
// a pull_request artifact on the same entity. A review deliverable matches a
// review artifact in the same scope whose key names the review id the
// deliverable carries as its external_id, which it has once it is submitted:
// a staged review has no artifact to match. When more than one deliverable
// matches, the first in the slice answers.
func ArtifactDeliverable(a Artifact, deliverables []Deliverable) (Deliverable, bool) {
	if !ArtifactMayHaveDeliverable(a) {
		return Deliverable{}, false
	}
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

// ArtifactMayHaveDeliverable reports whether a is of a kind
// ArtifactDeliverable can match: a pull request or a review. A reader with
// none of these needs no deliverables.
func ArtifactMayHaveDeliverable(a Artifact) bool {
	return a.ObjectKind == ObjectKindPullRequest || a.ObjectKind == ObjectKindReview
}

// ArtifactDeliverableKeys returns what a deliverable carries when it is the
// same object as one of artifacts (ArtifactDeliverable): the entity ids of
// the pull requests and the review ids their keys name, each sorted and
// deduplicated, for a reader to fetch only those deliverables. A key that
// names no review id contributes none.
func ArtifactDeliverableKeys(artifacts []Artifact) (entityIDs, reviewIDs []string) {
	reviewPrefix := ObjectKey(ProviderGitHub, ObjectKindReview, "", "")
	for _, a := range artifacts {
		switch a.ObjectKind {
		case ObjectKindPullRequest:
			if a.EntityID != "" {
				entityIDs = append(entityIDs, a.EntityID)
			}
		case ObjectKindReview:
			if id, ok := strings.CutPrefix(a.ObjectKey, reviewPrefix); ok && GitHubReviewObjectKey(id) == a.ObjectKey {
				reviewIDs = append(reviewIDs, id)
			}
		}
	}
	slices.Sort(entityIDs)
	slices.Sort(reviewIDs)
	return slices.Compact(entityIDs), slices.Compact(reviewIDs)
}
