package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// isTerminalRunStatus mirrors domain/agent.go's run lifecycle: the states a run
// never leaves. Everything else is in-flight (or queued, handled separately).
func isTerminalRunStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "task_unsolvable":
		return true
	}
	return false
}

// parseWindow reads ?hours= (default 24, clamped 1..168) into a start time.
func parseWindow(r *http.Request, now time.Time) (since time.Time, hours int) {
	hours = 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			hours = n
		}
	}
	if hours < 1 {
		hours = 1
	}
	if hours > 168 {
		hours = 168
	}
	return now.Add(-time.Duration(hours) * time.Hour), hours
}

// --- GET /api/fleet/overview ---

type overviewDTO struct {
	GeneratedAt time.Time     `json:"generated_at"`
	StaleAfterS float64       `json:"stale_after_seconds"`
	Totals      fleetTotals   `json:"totals"`
	Queue       queueSummary  `json:"queue"`
	Runs        runsSummary   `json:"runs"`
	Versions    []versionSkew `json:"versions"`
	Spend       *spendSummary `json:"spend,omitempty"`
}

type fleetTotals struct {
	Instances   int `json:"instances"`
	Executors   int `json:"executors"`
	Control     int `json:"control"`
	Draining    int `json:"draining"`
	Gated       int `json:"gated"`
	Stale       int `json:"stale"`
	CapacityMax int `json:"capacity_max"`
	ActiveRuns  int `json:"active_runs"`
}

type queueSummary struct {
	Depth       int     `json:"depth"`
	OldestWaitS float64 `json:"oldest_wait_seconds"`
	WaitP50MS   *int    `json:"wait_p50_ms,omitempty"`
	WaitP95MS   *int    `json:"wait_p95_ms,omitempty"`
}

type runsSummary struct {
	WindowHours   int               `json:"window_hours"`
	Total         int               `json:"total"`
	Active        int               `json:"active"`
	Completed     int               `json:"completed"`
	Failed        int               `json:"failed"`
	DurationP50MS *int              `json:"duration_p50_ms,omitempty"`
	DurationP95MS *int              `json:"duration_p95_ms,omitempty"`
	FailureKinds  []failureKindRate `json:"failure_kinds"`
}

type failureKindRate struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type versionSkew struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
}

type spendSummary struct {
	WindowHours int     `json:"window_hours"`
	TotalUSD    float64 `json:"total_usd"`
}

func (h *handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	since, hours := parseWindow(r, now)

	instances, err := h.stores.Instances.List(ctx)
	if err != nil {
		httpx.InternalError(w, "fleet-overview-instances", err)
		return
	}
	timings, err := h.stores.RunQueue.RecentRunTimingsSystem(ctx, since, 0)
	if err != nil {
		httpx.InternalError(w, "fleet-overview-timings", err)
		return
	}
	queued, err := h.stores.RunQueue.QueuedRunAgesSystem(ctx)
	if err != nil {
		httpx.InternalError(w, "fleet-overview-queue", err)
		return
	}

	dto := overviewDTO{GeneratedAt: now, StaleAfterS: staleAfter.Seconds()}
	dto.Totals = summarizeInstances(instances, now)
	dto.Versions = versionSkews(instances)
	dto.Queue = summarizeQueue(queued, timings, now)
	dto.Runs = summarizeRuns(timings, hours)

	// Spend overlay — best-effort: a spend-read failure must not sink the whole
	// overview (it is a secondary lens), so log-and-omit rather than 500.
	if total, serr := h.stores.Spend.SpendTotalSystem(ctx, since, now); serr == nil {
		dto.Spend = &spendSummary{WindowHours: hours, TotalUSD: total}
	}

	httpx.WriteJSON(w, http.StatusOK, dto)
}

func summarizeInstances(instances []domain.Instance, now time.Time) fleetTotals {
	var t fleetTotals
	t.Instances = len(instances)
	for _, inst := range instances {
		stale := now.Sub(inst.LastHeartbeatAt) > staleAfter
		if stale {
			t.Stale++
		}
		if inst.Draining {
			t.Draining++
		}
		if inst.DispatchGated != nil && *inst.DispatchGated {
			t.Gated++
		}
		if inst.Role == domain.InstanceRoleControl {
			t.Control++
		} else {
			t.Executors++
		}
		// Live capacity/active only counts non-stale executor-capable rows.
		if !stale {
			if inst.MaxRuns != nil {
				t.CapacityMax += *inst.MaxRuns
			}
			if inst.ActiveRuns != nil {
				t.ActiveRuns += *inst.ActiveRuns
			}
		}
	}
	return t
}

func versionSkews(instances []domain.Instance) []versionSkew {
	counts := map[string]int{}
	for _, inst := range instances {
		counts[inst.Version]++
	}
	out := make([]versionSkew, 0, len(counts))
	for v, c := range counts {
		out = append(out, versionSkew{Version: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Version < out[j].Version
	})
	return out
}

func summarizeQueue(queued []domain.QueuedRun, timings []domain.RunTiming, now time.Time) queueSummary {
	q := queueSummary{Depth: len(queued)}
	if len(queued) > 0 {
		// QueuedRunAgesSystem returns oldest-first, so [0] is the oldest wait.
		q.OldestWaitS = now.Sub(queued[0].EnqueuedAt).Seconds()
	}
	// Wait percentiles: claimed_at − started_at across recently-claimed runs.
	var waits []int
	for _, t := range timings {
		if t.ClaimedAt != nil {
			waits = append(waits, int(t.ClaimedAt.Sub(t.StartedAt).Milliseconds()))
		}
	}
	q.WaitP50MS = percentileMS(waits, 50)
	q.WaitP95MS = percentileMS(waits, 95)
	return q
}

func summarizeRuns(timings []domain.RunTiming, hours int) runsSummary {
	rs := runsSummary{WindowHours: hours, Total: len(timings)}
	failureCounts := map[string]int{}
	var durations []int
	for _, t := range timings {
		switch {
		case t.Status == "completed":
			rs.Completed++
		case t.FailureKind != "":
			rs.Failed++
			failureCounts[t.FailureKind]++
		}
		if !isTerminalRunStatus(t.Status) && t.Status != "queued" {
			rs.Active++
		}
		if t.DurationMS != nil {
			durations = append(durations, *t.DurationMS)
		}
	}
	rs.DurationP50MS = percentileMS(durations, 50)
	rs.DurationP95MS = percentileMS(durations, 95)
	rs.FailureKinds = sortedFailureKinds(failureCounts)
	return rs
}

func sortedFailureKinds(counts map[string]int) []failureKindRate {
	out := make([]failureKindRate, 0, len(counts))
	for k, c := range counts {
		out = append(out, failureKindRate{Kind: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// --- GET /api/fleet/instances ---

type instanceDTO struct {
	ID            string    `json:"id"`
	Role          string    `json:"role"`
	Version       string    `json:"version"`
	BootEpoch     int64     `json:"boot_epoch"`
	StartedAt     time.Time `json:"started_at"`
	LastHeartbeat time.Time `json:"last_heartbeat_at"`
	HeartbeatAgeS float64   `json:"heartbeat_age_seconds"`
	Stale         bool      `json:"stale"`
	Draining      bool      `json:"draining"`
	DispatchGated *bool     `json:"dispatch_gated,omitempty"`
	MaxRuns       *int      `json:"max_runs,omitempty"`
	ActiveRuns    *int      `json:"active_runs,omitempty"`
	MemTotalMB    *int      `json:"mem_total_mb,omitempty"`
	MemAvailMB    *int      `json:"mem_available_mb,omitempty"`
	// Latest telemetry sample extras (nil until the first sample lands).
	CPUPct     *float64 `json:"cpu_pct,omitempty"`
	Load1      *float64 `json:"load1,omitempty"`
	ClaimsMin  *int     `json:"claims_last_sample,omitempty"`
	SpawnP50MS *int     `json:"spawn_p50_ms,omitempty"`
}

type instancesDTO struct {
	GeneratedAt time.Time     `json:"generated_at"`
	StaleAfterS float64       `json:"stale_after_seconds"`
	Instances   []instanceDTO `json:"instances"`
}

func (h *handler) handleInstances(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	instances, err := h.stores.Instances.List(ctx)
	if err != nil {
		httpx.InternalError(w, "fleet-instances", err)
		return
	}
	// Latest telemetry sample per instance from the last few minutes.
	latest := h.latestSamples(ctx, now.Add(-5*time.Minute))

	out := make([]instanceDTO, 0, len(instances))
	for _, inst := range instances {
		age := now.Sub(inst.LastHeartbeatAt)
		d := instanceDTO{
			ID:            inst.ID,
			Role:          inst.Role,
			Version:       inst.Version,
			BootEpoch:     inst.BootEpoch,
			StartedAt:     inst.StartedAt,
			LastHeartbeat: inst.LastHeartbeatAt,
			HeartbeatAgeS: age.Seconds(),
			Stale:         age > staleAfter,
			Draining:      inst.Draining,
			DispatchGated: inst.DispatchGated,
			MaxRuns:       inst.MaxRuns,
			ActiveRuns:    inst.ActiveRuns,
			MemTotalMB:    inst.MemTotalMB,
			MemAvailMB:    inst.MemAvailableMB,
		}
		if s, ok := latest[inst.ID]; ok {
			d.CPUPct = s.CPUPct
			d.Load1 = s.Load1
			d.ClaimsMin = s.Claims
			d.SpawnP50MS = s.SpawnP50MS
		}
		out = append(out, d)
	}
	httpx.WriteJSON(w, http.StatusOK, instancesDTO{
		GeneratedAt: now,
		StaleAfterS: staleAfter.Seconds(),
		Instances:   out,
	})
}

// latestSamples returns the most recent sample per instance since `since`. A
// read failure degrades to no extras (the registry row still renders), never an
// error — the samples are a garnish on the authoritative registry fields.
func (h *handler) latestSamples(ctx context.Context, since time.Time) map[string]domain.InstanceStat {
	out := map[string]domain.InstanceStat{}
	if h.stores.InstanceStats == nil {
		return out
	}
	samples, err := h.stores.InstanceStats.ListSince(ctx, since)
	if err != nil {
		return out
	}
	// ListSince is ascending by `at`, so the last write per id wins.
	for _, s := range samples {
		out[s.InstanceID] = s
	}
	return out
}

// --- POST /api/fleet/instances/{id}/drain ---

type drainRequest struct {
	Draining bool `json:"draining"`
}

func (h *handler) handleDrain(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		httpx.BadRequest(w, "instance id is required")
		return
	}
	var req drainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.BadRequest(w, "invalid JSON body: expected {\"draining\": true|false}")
		return
	}
	matched, err := h.stores.Instances.SetDraining(r.Context(), id, req.Draining)
	if err != nil {
		httpx.InternalError(w, "fleet-drain", err)
		return
	}
	if !matched {
		httpx.NotFound(w, "instance")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "draining": req.Draining})
}

// --- GET /api/fleet/timeseries ---

type sampleDTO struct {
	InstanceID     string    `json:"instance_id"`
	At             time.Time `json:"at"`
	ActiveRuns     *int      `json:"active_runs,omitempty"`
	QueuedVisible  *int      `json:"queued_visible,omitempty"`
	MemAvailableMB *int      `json:"mem_available_mb,omitempty"`
	CPUPct         *float64  `json:"cpu_pct,omitempty"`
	Load1          *float64  `json:"load1,omitempty"`
	Claims         *int      `json:"claims,omitempty"`
	SpawnP50MS     *int      `json:"spawn_p50_ms,omitempty"`
	OOMKills       *int      `json:"oom_kills,omitempty"`
}

type timeseriesDTO struct {
	GeneratedAt time.Time   `json:"generated_at"`
	WindowHours int         `json:"window_hours"`
	Samples     []sampleDTO `json:"samples"`
}

func (h *handler) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	now := time.Now().UTC()
	since, hours := parseWindow(r, now)

	samples, err := h.stores.InstanceStats.ListSince(r.Context(), since)
	if err != nil {
		httpx.InternalError(w, "fleet-timeseries", err)
		return
	}
	out := make([]sampleDTO, 0, len(samples))
	for _, s := range samples {
		out = append(out, sampleDTO{
			InstanceID: s.InstanceID, At: s.At,
			ActiveRuns: s.ActiveRuns, QueuedVisible: s.QueuedVisible,
			MemAvailableMB: s.MemAvailableMB, CPUPct: s.CPUPct, Load1: s.Load1,
			Claims: s.Claims, SpawnP50MS: s.SpawnP50MS, OOMKills: s.OOMKills,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, timeseriesDTO{GeneratedAt: now, WindowHours: hours, Samples: out})
}

// --- GET /api/fleet/queue ---

type orgQueueShare struct {
	OrgID       string  `json:"org_id"`
	Count       int     `json:"count"`
	OldestWaitS float64 `json:"oldest_wait_seconds"`
}

type queueDTO struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Depth       int             `json:"depth"`
	OldestWaitS float64         `json:"oldest_wait_seconds"`
	ByOrg       []orgQueueShare `json:"by_org"`
}

func (h *handler) handleQueue(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	now := time.Now().UTC()
	queued, err := h.stores.RunQueue.QueuedRunAgesSystem(r.Context())
	if err != nil {
		httpx.InternalError(w, "fleet-queue", err)
		return
	}
	dto := queueDTO{GeneratedAt: now, Depth: len(queued), ByOrg: byOrgShares(queued, now)}
	if len(queued) > 0 {
		dto.OldestWaitS = now.Sub(queued[0].EnqueuedAt).Seconds()
	}
	httpx.WriteJSON(w, http.StatusOK, dto)
}

func byOrgShares(queued []domain.QueuedRun, now time.Time) []orgQueueShare {
	// queued is oldest-first, so the first row seen per org is its oldest wait.
	idx := map[string]int{}
	var shares []orgQueueShare
	for _, q := range queued {
		if i, ok := idx[q.OrgID]; ok {
			shares[i].Count++
			continue
		}
		idx[q.OrgID] = len(shares)
		shares = append(shares, orgQueueShare{
			OrgID:       q.OrgID,
			Count:       1,
			OldestWaitS: now.Sub(q.EnqueuedAt).Seconds(),
		})
	}
	sort.Slice(shares, func(i, j int) bool {
		if shares[i].Count != shares[j].Count {
			return shares[i].Count > shares[j].Count
		}
		return shares[i].OldestWaitS > shares[j].OldestWaitS
	})
	return shares
}
