package domain

import "fmt"

// AdditiveEventInjection is the agent-facing copy for an additive event that
// arrived while the entity already had an active auto run (TFAC-594): a
// follow-up on the conversation in progress (e.g. a second Slack mention on a
// live thread), not a request for a second one. metadataJSON is the
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
