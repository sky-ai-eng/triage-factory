package domain

// TaskCloseDeferral is a close an event owed a task and held back because the
// task's own blueprint run caused that event: the run did the work the event
// reports (pushed the fix CI then passed on, merged the pull request, moved
// the issue to done), and closing the task under it would stop that run
// before it finishes. The router's deferral sweep applies the close once the
// run ends. When the run completed, its own ending closes the task once a
// person resolves the deliverables it left: a typed close is dropped, and a
// terminating one waits while any deliverable is unresolved, and is dropped
// once a newer blueprint run takes the task.
//
// It is the pair of tasks columns close_deferred_event_id and
// close_deferred_blueprint_run_id, which are both set or both NULL.
type TaskCloseDeferral struct {
	OrgID  string `json:"org_id"`
	TaskID string `json:"task_id"`
	// EventID is the closing event whose close is held.
	EventID string `json:"event_id"`
	// BlueprintRunID is the task's blueprint run whose action caused EventID.
	BlueprintRunID string `json:"blueprint_run_id"`
}
