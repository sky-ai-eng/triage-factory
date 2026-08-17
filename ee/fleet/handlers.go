package fleet

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

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
	timings, err := h.stores.ConversationQueue.RecentRunTimingsSystem(ctx, since, 0)
	if err != nil {
		httpx.InternalError(w, "fleet-overview-timings", err)
		return
	}
	queued, err := h.stores.ConversationQueue.QueuedRunAgesSystem(ctx)
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
		// Fleet rows are multi-mode registrations, so the non-control bucket
		// is exactly the executors (the all role is local-only and never
		// registers into a Postgres fleet).
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

func summarizeQueue(queued []domain.QueuedConversation, timings []domain.ConversationTiming, now time.Time) queueSummary {
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

func summarizeRuns(timings []domain.ConversationTiming, hours int) runsSummary {
	rs := runsSummary{WindowHours: hours, Total: len(timings)}
	failureCounts := map[string]int{}
	var durations []int
	for _, t := range timings {
		switch t.Status {
		case "completed":
			rs.Completed++
		case "failed":
			// Every failed run counts (failure_kind is a classification that is
			// legitimately empty on an unclassified/legacy failure); only the
			// classified ones contribute to the by-kind breakdown.
			rs.Failed++
			if t.FailureKind != "" {
				failureCounts[t.FailureKind]++
			}
		}
		if domain.IsActiveRunStatus(t.Status) {
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
	if !httpx.DecodeJSONStrict(w, r, &req) {
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

// --- GET /api/fleet/instances/{id}/sandboxes ---
//
// The per-executor sandbox breakdown: which runs actually occupied this box,
// and what each one cost. instance_stats samples the WHOLE HOST, so it
// structurally cannot answer that — a busy neighbour process is
// indistinguishable from a heavy run there. These numbers are per-cgroup.
//
// Cross-org by construction: an executor runs whatever the fleet placed on it,
// so the claims listed here span orgs. That is the read-scoping standing rule's
// operator-surface arm — the deployment-operator gate (gate.go) is the
// authorization, and no org can be named to scope to. Identifiers render
// display-only for the same reason: an app deep link into another org's run
// view would not authorize for this viewer.

const (
	defaultSandboxLimit = 25
	maxSandboxLimit     = 100
)

type sandboxClaimDTO struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	OrgID          string     `json:"org_id"`
	ClaimedAt      time.Time  `json:"claimed_at"`
	ReleasedAt     *time.Time `json:"released_at,omitempty"`
	// Live marks an unreleased claim — still holding a slot, and whatever
	// series it has is still growing.
	Live bool `json:"live"`
	// DurationS is the engagement's wall clock: claimed → released, or
	// claimed → now while live. Deliberately NOT the claim's runtime-reported
	// duration_ms, which times the agent invocation inside a wider engagement
	// (fetch, clone, sidecar bring-up) and would therefore be a denominator
	// smaller than the interval the CPU counter actually accumulated over —
	// inflating any derived average past what the box saw.
	DurationS float64 `json:"duration_seconds"`
	// PeakMemMB / CPUUsec are the teardown snapshot — the truth half of the
	// honest-display contract. Absent means NOT MEASURED (an unsandboxed
	// local claim, a pre-5.19 kernel with no memory.peak, a crashed
	// teardown), never measured-zero; render a dash, not a 0.
	PeakMemMB *int   `json:"peak_mem_mb,omitempty"`
	CPUUsec   *int64 `json:"cpu_usec,omitempty"`
	// Status / FailureKind / Outcome are what the engagement was spent on.
	Status      string `json:"status,omitempty"`
	FailureKind string `json:"failure_kind,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
}

type sandboxesDTO struct {
	GeneratedAt time.Time         `json:"generated_at"`
	InstanceID  string            `json:"instance_id"`
	Limit       int               `json:"limit"`
	Sandboxes   []sandboxClaimDTO `json:"sandboxes"`
}

// parseSandboxLimit reads ?limit= — default 25, capped at 100, and REJECTED
// (not silently defaulted) when it is unparseable or below 1. Deliberately
// stricter than parseWindow's clamp above: an out-of-range window is a
// reasonable "show me as much as you can", whereas limit=abc is a caller bug
// that a silent default would hide behind plausible-looking data.
func parseSandboxLimit(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultSandboxLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, false
	}
	if n > maxSandboxLimit {
		n = maxSandboxLimit
	}
	return n, true
}

func (h *handler) handleInstanceSandboxes(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		httpx.BadRequest(w, "instance id is required")
		return
	}
	limit, ok := parseSandboxLimit(r)
	if !ok {
		httpx.BadRequest(w, "limit must be a positive integer")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	// Resolve the instance first so an unknown id 404s with the same shape as
	// handleDrain, rather than reading as a live executor that happens to be
	// idle — those are different operator situations.
	inst, err := h.stores.Instances.Get(ctx, id)
	if err != nil {
		httpx.InternalError(w, "fleet-sandboxes-instance", err)
		return
	}
	if inst == nil {
		httpx.NotFound(w, "instance")
		return
	}

	claims, err := h.stores.ConversationQueue.RecentClaimsForExecutorSystem(ctx, id, limit)
	if err != nil {
		httpx.InternalError(w, "fleet-sandboxes", err)
		return
	}
	out := make([]sandboxClaimDTO, 0, len(claims))
	for _, c := range claims {
		out = append(out, sandboxClaim(c, now))
	}
	httpx.WriteJSON(w, http.StatusOK, sandboxesDTO{
		GeneratedAt: now, InstanceID: id, Limit: limit, Sandboxes: out,
	})
}

func sandboxClaim(c domain.ExecutorClaim, now time.Time) sandboxClaimDTO {
	d := sandboxClaimDTO{
		ID: c.ID, ConversationID: c.ConversationID, OrgID: c.OrgID,
		ClaimedAt: c.ClaimedAt, ReleasedAt: c.ReleasedAt, Live: c.ReleasedAt == nil,
		PeakMemMB: c.PeakMemMB, CPUUsec: c.CPUUsec,
		Status: c.Status, FailureKind: c.FailureKind, Outcome: c.Outcome,
	}
	end := now
	if c.ReleasedAt != nil {
		end = *c.ReleasedAt
	}
	// Clamp at zero: a released_at that precedes claimed_at can only come from
	// clock skew between two pods, and a negative duration would poison every
	// rate derived from it.
	if s := end.Sub(c.ClaimedAt).Seconds(); s > 0 {
		d.DurationS = s
	}
	return d
}

// --- GET /api/fleet/claims/{id}/series ---
//
// One sandbox's in-run resource series. CPU is served CUMULATIVE, exactly as
// stored: the consumer differences consecutive samples into a rate, so a
// dropped tick self-heals into a wider-but-correct interval instead of leaving
// a gap that reads as an idle sandbox.
//
// The series and the claim's teardown snapshot disagree slightly BY DESIGN —
// a periodic sampler misses the sub-minute spikes the kernel's high-watermark
// catches, so a peak_mem_mb above the series' highest point is correct, not a
// bug. The series is shape; the snapshot is truth. They are never reconciled
// numerically, here or in the UI.

type sandboxSampleDTO struct {
	At           time.Time `json:"at"`
	MemCurrentMB *int      `json:"mem_current_mb,omitempty"`
	CPUUsecCum   *int64    `json:"cpu_usec_cum,omitempty"`
}

type sandboxSeriesDTO struct {
	GeneratedAt time.Time          `json:"generated_at"`
	ClaimID     string             `json:"claim_id"`
	Samples     []sandboxSampleDTO `json:"samples"`
}

func (h *handler) handleClaimSeries(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		httpx.BadRequest(w, "claim id is required")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	// The claim lookup is what separates "no such claim" (404) from "a real
	// claim nobody ever sampled" (200 with an empty series — an ordinary
	// sub-minute run, or one that predates the sampler). Without it every
	// unsampled run would read as a missing one.
	claim, err := h.stores.ConversationQueue.ClaimByIDSystem(ctx, id)
	if err != nil {
		httpx.InternalError(w, "fleet-series-claim", err)
		return
	}
	if claim == nil {
		httpx.NotFound(w, "claim")
		return
	}

	out := []sandboxSampleDTO{}
	// Tolerated nil for the same reason latestSamples tolerates it: the store
	// bundle a community/limited build wires may omit the telemetry stores,
	// and a chartless row beats a 500 on a garnish.
	if h.stores.SandboxStats != nil {
		samples, serr := h.stores.SandboxStats.ListForClaim(ctx, id)
		if serr != nil {
			httpx.InternalError(w, "fleet-series", serr)
			return
		}
		for _, s := range samples {
			out = append(out, sandboxSampleDTO{
				At: s.At, MemCurrentMB: s.MemCurrentMB, CPUUsecCum: s.CPUUsecCum,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, sandboxSeriesDTO{
		GeneratedAt: now, ClaimID: id, Samples: out,
	})
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

// --- GET /api/fleet/backlog ---
//
// The deployment-wide operator queue view: fleet backlog depth, the single
// oldest wait, and per-org shares by pending count + each org's oldest wait.
// This is the WAIT-LATENCY lens (who is waiting and for how long) — distinct
// from core's org-facing GET /api/fleet/queue, which is the per-org CAP lens
// (active vs queued vs cap). Different surfaces on purpose; this one is
// operator + FeatureFleet gated (the console), that one is org-admin (core).

type backlogOrgShare struct {
	OrgID       string  `json:"org_id"`
	Count       int     `json:"count"`
	OldestWaitS float64 `json:"oldest_wait_seconds"`
}

type backlogDTO struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Depth       int               `json:"depth"`
	OldestWaitS float64           `json:"oldest_wait_seconds"`
	ByOrg       []backlogOrgShare `json:"by_org"`
}

func (h *handler) handleBacklog(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	now := time.Now().UTC()
	queued, err := h.stores.ConversationQueue.QueuedRunAgesSystem(r.Context())
	if err != nil {
		httpx.InternalError(w, "fleet-backlog", err)
		return
	}
	dto := backlogDTO{GeneratedAt: now, Depth: len(queued), ByOrg: backlogByOrg(queued, now)}
	if len(queued) > 0 {
		// QueuedRunAgesSystem is oldest-first, so [0] is the oldest wait.
		dto.OldestWaitS = now.Sub(queued[0].EnqueuedAt).Seconds()
	}
	httpx.WriteJSON(w, http.StatusOK, dto)
}

func backlogByOrg(queued []domain.QueuedConversation, now time.Time) []backlogOrgShare {
	// queued is oldest-first, so the first row seen per org is its oldest wait.
	idx := map[string]int{}
	var shares []backlogOrgShare
	for _, q := range queued {
		if i, ok := idx[q.OrgID]; ok {
			shares[i].Count++
			continue
		}
		idx[q.OrgID] = len(shares)
		shares = append(shares, backlogOrgShare{
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
