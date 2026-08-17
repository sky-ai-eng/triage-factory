package server

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
	WindowSince   time.Time        `json:"window_since"`
	WindowUntil   time.Time        `json:"window_until"`
	QueueDepth    int              `json:"queue_depth"`
	OldestWaitS   float64          `json:"oldest_wait_seconds"`
	WaitP50MS     *int             `json:"wait_p50_ms,omitempty"`
	WaitP95MS     *int             `json:"wait_p95_ms,omitempty"`
	RunsTotal     int              `json:"runs_total"`
	RunsActive    int              `json:"runs_active"`
	RunsCompleted int              `json:"runs_completed"`
	RunsFailed    int              `json:"runs_failed"`
	DurationP50MS *int             `json:"duration_p50_ms,omitempty"`
	DurationP95MS *int             `json:"duration_p95_ms,omitempty"`
	FailureKinds  []opsFailureKind `json:"failure_kinds"`
}

type opsFailureKind struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// handleUsageOrgOps serves GET /api/usage/org/ops — org-admin gated. Reuses the
// usage window parsing and the ConversationQueue's org-scoped timing reads.
func (h *usageHandler) handleUsageOrgOps(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}
	if h.runQueue == nil {
		// This pod holds no run-queue reader — deployment shape, not an outage.
		writeNotConfigured(w, "the run queue reader is not configured on this deployment")
		return
	}
	since, until, errMsg := parseUsageWindow(r.URL.Query(), time.Now().UTC())
	if errMsg != "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: errMsg,
		})
		return
	}

	timings, err := h.runQueue.RecentRunTimingsForOrgSystem(r.Context(), orgID, since, until, 0)
	if err != nil {
		internalError(w, "usage-ops-timings", err)
		return
	}
	queued, err := h.runQueue.QueuedRunAgesForOrgSystem(r.Context(), orgID)
	if err != nil {
		internalError(w, "usage-ops-queue", err)
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
		switch t.Status {
		case "completed":
			resp.RunsCompleted++
		case "failed":
			// Count every failed run; failure_kind is a classification that is
			// legitimately empty on an unclassified/legacy failure, so only the
			// classified ones feed the by-kind breakdown.
			resp.RunsFailed++
			if t.FailureKind != "" {
				failureCounts[t.FailureKind]++
			}
		}
		if domain.IsActiveRunStatus(t.Status) {
			resp.RunsActive++
		}
	}
	resp.WaitP50MS = usagePercentileMS(waits, 50)
	resp.WaitP95MS = usagePercentileMS(waits, 95)
	resp.DurationP50MS = usagePercentileMS(durations, 50)
	resp.DurationP95MS = usagePercentileMS(durations, 95)
	resp.FailureKinds = sortedOpsFailureKinds(failureCounts)

	writeJSON(w, http.StatusOK, resp)
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
