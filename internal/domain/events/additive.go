package events

// AdditiveFor returns eventType's declared Additive flag, or false when the
// type has no registered schema — the safe default (defer, don't inject)
// for an unclassified event type. internal/routing's tryAutoDelegate is the
// sole consumer: an additive firing against an entity with an active auto
// run injects into it via the staged-injection seam instead of deferring to
// pending_firings.
func AdditiveFor(eventType string) bool {
	s, ok := Get(eventType)
	if !ok {
		return false
	}
	return s.Additive
}
