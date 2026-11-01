package domain

import "fmt"

// AdditiveEventInjection is the agent-facing copy for an additive event that
// arrived while the task already had a live conversation: a follow-up on the
// conversation in progress (e.g. a second Slack mention on a live thread), not
// a request for a second one. metadataJSON is the
// triggering event's raw metadata JSON; "" (a best-effort lookup failure, or
// an event type with no metadata) still renders a body naming the event type
// alone.
//
// Bare (no <system-note> wrapper): the live path wraps one injection, the
// staged path bundles and wraps the block (StagedInjectionBlock).
func AdditiveEventInjection(eventType, metadataJSON string) string {
	if metadataJSON == "" {
		return fmt.Sprintf(
			"A new %s event occurred on this entity while this run is active. "+
				"Fold it into your current work if relevant; it will not spawn a separate run.",
			eventType,
		)
	}
	return fmt.Sprintf(
		"A new %s event occurred on this entity while this run is active. Event metadata:\n%s\n"+
			"Fold it into your current work if relevant; it will not spawn a separate run.",
		eventType, metadataJSON,
	)
}

// TaskClosingGuidance is what an agent working on a task should do once an
// event has resolved the situation the task is about.
const TaskClosingGuidance = "If your own work caused this, finish what you were doing. Otherwise start nothing new on this task and wrap up."

// TaskClosingInjection is the agent-facing copy telling a conversation on a
// task whose close is held that an event resolved the situation the task is
// about, so the task closes once its conversations finish: the conversations
// working when the close was held, and each one engaged while it still is. metadataJSON is the
// closing event's metadata JSON; "" renders a body naming the event type
// alone. Bare, like AdditiveEventInjection.
func TaskClosingInjection(eventType, metadataJSON string) string {
	if metadataJSON == "" {
		return fmt.Sprintf(
			"A %s event resolved the situation this task is about. %s The task closes once its conversations finish.",
			eventType, TaskClosingGuidance,
		)
	}
	return fmt.Sprintf(
		"A %s event resolved the situation this task is about. Event metadata:\n%s\n%s The task closes once its conversations finish.",
		eventType, metadataJSON, TaskClosingGuidance,
	)
}
