package usage

import (
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// usageOrgOpsResponse is the org-scoped operations subset an org admin sees on
// /usage (TFAC-589) — their queue waits, run durations, and active/terminal
// counts for the window. It is the org-facing, SaaS-safe complement to the
// operator-only fleet console: same run-timing lens, scoped to one org, with no
// cross-tenant or machine-level truth (no instance ids, capacity, or versions).
type usageOrgOpsResponse struct {
	WindowSince   time.Time        `json:"window_since" doc:"The start of the window read, as an RFC 3339 timestamp; the zero time 0001-01-01T00:00:00Z when only until was given."`
	WindowUntil   time.Time        `json:"window_until" doc:"The end of the window read, as an RFC 3339 timestamp; the zero time 0001-01-01T00:00:00Z when only since was given."`
	QueueDepth    int              `json:"queue_depth" doc:"How many of the organization's conversations are waiting to be run now, whatever the window."`
	OldestWaitS   float64          `json:"oldest_wait_seconds" doc:"How long the longest-waiting queued conversation has waited, in seconds, 0 when none is waiting."`
	WaitP50MS     *int             `json:"wait_p50_ms,omitempty" doc:"The median wait between a conversation starting and an executor taking it, in milliseconds, over the conversations started in the window, absent when none was taken."`
	WaitP95MS     *int             `json:"wait_p95_ms,omitempty" doc:"The 95th percentile of that wait in milliseconds, absent when no conversation started in the window was taken."`
	RunsTotal     int              `json:"runs_total" doc:"How many conversations started in the window."`
	RunsActive    int              `json:"runs_active" doc:"How many of those conversations are running now."`
	RunsCompleted int              `json:"runs_completed" doc:"How many of those conversations completed."`
	RunsFailed    int              `json:"runs_failed" doc:"How many of those conversations failed."`
	DurationP50MS *int             `json:"duration_p50_ms,omitempty" doc:"The median run duration in milliseconds, over the conversations started in the window that have one, absent when none has."`
	DurationP95MS *int             `json:"duration_p95_ms,omitempty" doc:"The 95th percentile of run duration in milliseconds, absent when no conversation started in the window has one."`
	FailureKinds  []opsFailureKind `json:"failure_kinds" doc:"The failed conversations by why they failed, most frequent first, leaving out failures with no classification."`
}

type opsFailureKind struct {
	Kind  string `json:"kind" doc:"Why the conversations failed: memory_limit, crash, no_result, agent_error, executor_lost, or session_lost."`
	Count int    `json:"count" doc:"How many failed conversations failed this way."`
}

var opUsageOrgOps = httpx.NewJSONOp[httpx.NoBody, usageOrgOpsResponse](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/usage/ops",
	ID:          "get_org_usage_ops",
	Summary:     "Get an organization's run timings",
	Description: "Returns the organization's queue depth and oldest wait now, and for the conversations started in the window, their counts by outcome, their queue-wait and run-duration percentiles, and their failures by kind.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Query:       httpx.WindowQuery(),
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleUsageOrgOps serves GET /api/orgs/{org_id}/usage/ops — org-admin gated
// against the org in the path. Reuses the usage window parsing and the
// ConversationQueue's org-scoped timing reads.
func (h *Handler) handleUsageOrgOps(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	if h.conversationQueue == nil {
		// This pod holds no conversation-queue reader — deployment shape, not
		// an outage.
		httpx.WriteNotConfigured(w, "the conversation queue reader is not configured on this deployment")
		return
	}
	q, ok := opUsageOrgOps.Query(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	since, until := httpx.ParseWindow(&v, q, time.Now().UTC())
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	timings, err := h.conversationQueue.RecentConversationTimingsForOrgSystem(r.Context(), orgID, since, until, 0)
	if err != nil {
		httpx.InternalError(w, "usage-ops-timings", err)
		return
	}
	queued, err := h.conversationQueue.QueuedConversationAgesForOrgSystem(r.Context(), orgID)
	if err != nil {
		httpx.InternalError(w, "usage-ops-queue", err)
		return
	}

	resp := usageOrgOpsResponse{
		WindowSince: since,
		WindowUntil: until,
		QueueDepth:  len(queued),
		RunsTotal:   len(timings),
	}
	if len(queued) > 0 {
		resp.OldestWaitS = time.Now().UTC().Sub(queued[0].EnqueuedAt).Seconds()
	}

	var waits, durations []int
	failureCounts := map[string]int{}
	for _, t := range timings {
		if t.ClaimedAt != nil {
			waits = append(waits, int(t.ClaimedAt.Sub(t.StartedAt).Milliseconds()))
		}
		if t.DurationMS != nil {
			durations = append(durations, *t.DurationMS)
		}
		switch {
		case t.Completed:
			resp.RunsCompleted++
		case t.Status == domain.StatusFailed:
			// Count every failed run; failure_kind is a classification that is
			// legitimately empty on an unclassified/legacy failure, so only the
			// classified ones feed the by-kind breakdown.
			resp.RunsFailed++
			if t.FailureKind != "" {
				failureCounts[t.FailureKind]++
			}
		}
		if domain.IsActiveConversationStatus(t.Status) {
			resp.RunsActive++
		}
	}
	resp.WaitP50MS = usagePercentileMS(waits, 50)
	resp.WaitP95MS = usagePercentileMS(waits, 95)
	resp.DurationP50MS = usagePercentileMS(durations, 50)
	resp.DurationP95MS = usagePercentileMS(durations, 95)
	resp.FailureKinds = sortedOpsFailureKinds(failureCounts)

	opUsageOrgOps.Write(w, resp)
}

// usagePercentileMS is the nearest-rank (ceil) percentile — the same convention
// the fleet console uses, kept local to avoid a core→ee import.
func usagePercentileMS(vals []int, p float64) *int {
	if len(vals) == 0 {
		return nil
	}
	sorted := append([]int(nil), vals...)
	sort.Ints(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	v := sorted[rank-1]
	return &v
}

func sortedOpsFailureKinds(counts map[string]int) []opsFailureKind {
	out := make([]opsFailureKind, 0, len(counts))
	for k, c := range counts {
		out = append(out, opsFailureKind{Kind: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
