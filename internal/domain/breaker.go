package domain

// BreakerStreak is the run of failures counting toward one trigger's breaker
// on one entity: the failed or aborted runs the trigger fired there since the
// newest run that resets it, which is the trigger's own completed run or a
// person's completed run on the entity.
type BreakerStreak struct {
	Failures int
	// LastConversationID is the newest conversation of the newest failed run
	// in the streak; empty when Failures is 0.
	LastConversationID string
}

// TrippedBreaker is one enabled trigger whose breaker holds on one active
// entity: its streak there has reached its threshold.
type TrippedBreaker struct {
	TriggerID string
	EntityID  string
}
