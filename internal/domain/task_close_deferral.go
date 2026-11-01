package domain

// TaskCloseDeferral is a close an event owed a task, held while one of the
// task's conversations was still working: the event resolved the situation
// the task is about, and closing the task then would stop the work under it.
// Each working conversation is told instead, and the router's sweep applies
// the close once none of the task's conversations is working, if it still
// holds then. A deliverable on the task that awaits a person's verdict, on
// a pull request still open, holds it too, until the person resolves it.
// The task's own event arriving again cancels it. Work an event started that
// is still going once the close has held for the sweep's limit is stopped;
// a person's work is not.
//
// It is the tasks columns close_deferred_event_id and close_deferred_at
// (when the hold began), read only while the task is active.
type TaskCloseDeferral struct {
	OrgID  string `json:"org_id"`
	TaskID string `json:"task_id"`
	// EventID is the closing event whose close is held.
	EventID string `json:"event_id"`
}
