package domain

import "time"

// TaskCloseDeferral is a close an event owed a task, held while a deliverable
// on the task awaits a person's verdict, on a pull request still open: the
// event resolved the situation the task is about, and the task stays open
// until the person resolves the deliverable. The router's sweep then applies
// the close, if it still holds. The task's own event arriving again cancels
// it. Nothing else holds a close: not a conversation working on the task,
// which a close leaves working.
//
// It is the tasks columns close_deferred_event_id and close_deferred_at
// (when the hold began), read only while the task is active.
type TaskCloseDeferral struct {
	OrgID  string `json:"org_id"`
	TaskID string `json:"task_id"`
	// EventID is the closing event whose close is held.
	EventID string `json:"event_id"`
	// HeldSince is when the hold began. A later closing event takes the
	// hold's place and keeps this time, so it identifies the hold: the note
	// telling a conversation of it is sent once per hold (TaskCloseHoldKey).
	HeldSince time.Time `json:"held_since"`
}
