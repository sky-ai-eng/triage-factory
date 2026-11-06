package delegate

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The reasons wakeForUnreadInput can leave a message unread for that are not
// rungs of the refusal ladder: the gate let the wake through and the flip then
// refused it, or the write failed.
const (
	unreadWakeRefused = "wake_refused"
	unreadWakeFailed  = "wake_failed"
)

// unreadAtConclusionReasons is every reason label the counter can carry: each
// ladder rung a concluded conversation can meet, and the two wake failures
// above. step_handed_off is not among them, because the wake does not count
// it: a reaction is still to come, and the wake runs again after it.
func unreadAtConclusionReasons() []string {
	reasons := []string{
		ResumeBlockedSessionMissing,
		ResumeBlockedWorktreeMissing,
		ResumeBlockedModelMissing,
		ResumeBlockedWorkspaceExpired,
		ResumeBlockedBlueprintConcluded,
		ResumeBlockedBlueprintCancelled,
		ResumeBlockedModelNotEnabled,
		unreadWakeRefused,
		unreadWakeFailed,
	}
	for _, r := range domain.AllEndedReasons() {
		reasons = append(reasons, endedBlock(r))
	}
	return reasons
}

// unreadAtConclusion owns the counter of people's messages left unread on a
// concluded conversation. Package-level and swapped atomically for the reason
// workspaceCheckpoints is.
var unreadAtConclusion atomic.Pointer[unreadAtConclusionStats]

func init() {
	unreadAtConclusion.Store(newUnreadAtConclusionStats(otel.GetMeterProvider()))
}

type unreadAtConclusionStats struct {
	stranded metric.Int64Counter
}

// newUnreadAtConclusionStats builds the instrument against mp. A creation
// error can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newUnreadAtConclusionStats(mp metric.MeterProvider) *unreadAtConclusionStats {
	counter, err := mp.Meter("internal/delegate").Int64Counter("messages.unread_at_conclusion",
		metric.WithDescription("People's messages left unread on a concluded conversation that could not be woken for them, by the reason the wake was refused."))
	if err != nil {
		delegateLog.Warn("unread-at-conclusion counter setup failed", "error", err)
	}
	return &unreadAtConclusionStats{stranded: counter}
}

// OpenUnreadAtConclusionSeries creates every reason's series at zero, so the
// first message left unread after a restart is a change increase() can see.
// Called at dispatcher startup, after telemetry.Init, for the reason
// OpenWorkspaceCheckpointSeries is.
func OpenUnreadAtConclusionSeries() {
	stats := unreadAtConclusion.Load()
	if stats == nil || stats.stranded == nil {
		return
	}
	for _, reason := range unreadAtConclusionReasons() {
		stats.stranded.Add(context.Background(), 0, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

func recordUnreadAtConclusion(reason string) {
	stats := unreadAtConclusion.Load()
	if stats == nil || stats.stranded == nil {
		return
	}
	stats.stranded.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}
