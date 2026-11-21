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

// fleetAccess is the sentence every console route's description ends with.
const fleetAccess = " It answers only the deployment's operators, needs an Enterprise licence that includes fleet administration, and refuses API tokens."

// hoursQuery declares the ?hours= parameter parseWindow reads.
var hoursQuery = httpx.QueryParam{
	Name: "hours", Type: httpx.ParamInteger,
	Doc: "The window's length in hours, ending now: 24 when absent or not a whole number, and moved into the range 1 to 168 when outside it.",
}

// parseWindow reads ?hours= (default 24, clamped 1..168) into a start time.
func parseWindow(raw string, now time.Time) (since time.Time, hours int) {
	hours = 24
	if raw != "" {
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
	GeneratedAt time.Time     `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	StaleAfterS float64       `json:"stale_after_seconds" doc:"How long after its last heartbeat, in seconds, an instance counts as stale."`
	Totals      fleetTotals   `json:"totals" doc:"Counts across every registered instance."`
	Queue       queueSummary  `json:"queue" doc:"The conversations waiting for an executor, and how long conversations started within the window waited."`
	Runs        runsSummary   `json:"runs" doc:"The conversations started within the window."`
	Versions    []versionSkew `json:"versions" doc:"How many instances run each build version, most instances first."`
	Spend       *spendSummary `json:"spend,omitempty" doc:"LLM spend across the deployment within the window, absent when it could not be read."`
}

type fleetTotals struct {
	Instances   int `json:"instances" doc:"The number of registered instances."`
	Executors   int `json:"executors" doc:"The number of registered instances that are not control instances."`
	Control     int `json:"control" doc:"The number of registered control instances."`
	Draining    int `json:"draining" doc:"The number of instances that are draining."`
	Gated       int `json:"gated" doc:"The number of executors that last reported they admit no new work."`
	Stale       int `json:"stale" doc:"The number of instances whose last heartbeat is older than stale_after_seconds."`
	CapacityMax int `json:"capacity_max" doc:"The sum of the concurrent run limits of the instances that are not stale."`
	ActiveRuns  int `json:"active_runs" doc:"The sum of the active runs the instances that are not stale last reported."`
}

type queueSummary struct {
	Depth       int     `json:"depth" doc:"The number of conversations waiting for an executor."`
	OldestWaitS float64 `json:"oldest_wait_seconds" doc:"How long the conversation waiting longest has waited, in seconds, or 0 when none is waiting."`
	WaitP50MS   *int    `json:"wait_p50_ms,omitempty" doc:"The median time from enqueue to an executor's claim, in milliseconds, across the conversations started within the window that have been claimed, absent when none has."`
	WaitP95MS   *int    `json:"wait_p95_ms,omitempty" doc:"The 95th percentile of the same wait, in milliseconds, absent when no conversation started within the window has been claimed."`
}

type runsSummary struct {
	WindowHours   int               `json:"window_hours" doc:"The window's length in hours."`
	Total         int               `json:"total" doc:"The number of conversations started within the window."`
	Active        int               `json:"active" doc:"How many of them are still active."`
	Completed     int               `json:"completed" doc:"How many of them completed."`
	Failed        int               `json:"failed" doc:"How many of them failed."`
	DurationP50MS *int              `json:"duration_p50_ms,omitempty" doc:"The median duration, in milliseconds, of those that have finished, absent when none has."`
	DurationP95MS *int              `json:"duration_p95_ms,omitempty" doc:"The 95th percentile duration, in milliseconds, of those that have finished, absent when none has."`
	FailureKinds  []failureKindRate `json:"failure_kinds" doc:"The failed conversations counted by failure kind, most failures first; failures with no kind are left out."`
}

type failureKindRate struct {
	Kind  string `json:"kind" doc:"The failure kind."`
	Count int    `json:"count" doc:"How many failed conversations have this kind."`
}

type versionSkew struct {
	Version string `json:"version" doc:"A build version."`
	Count   int    `json:"count" doc:"How many registered instances run it."`
}

type spendSummary struct {
	WindowHours int     `json:"window_hours" doc:"The window's length in hours."`
	TotalUSD    float64 `json:"total_usd" doc:"The deployment's total LLM spend within the window, in USD."`
}

var opOverview = httpx.NewJSONOp[httpx.NoBody, overviewDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/overview",
	ID:          "get_fleet_overview",
	Summary:     "Get the fleet overview",
	Description: "Returns counts across every registered instance, the queue of conversations waiting for an executor, the conversations started within the window, the build versions instances run, and the deployment's LLM spend within the window." + fleetAccess,
	Query:       []httpx.QueryParam{hoursQuery},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	q, ok := opOverview.Query(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	since, hours := parseWindow(q.Get("hours"), now)

	instances, err := h.stores.Instances.List(ctx)
	if err != nil {
		httpx.InternalError(w, "fleet-overview-instances", err)
		return
	}
	timings, err := h.stores.ConversationQueue.RecentConversationTimingsSystem(ctx, since, 0)
	if err != nil {
		httpx.InternalError(w, "fleet-overview-timings", err)
		return
	}
	queued, err := h.stores.ConversationQueue.QueuedConversationAgesSystem(ctx)
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

	opOverview.Write(w, dto)
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
		// QueuedConversationAgesSystem returns oldest-first, so [0] is the oldest wait.
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
		switch {
		case t.Completed:
			rs.Completed++
		case t.Status == domain.StatusFailed:
			// Every failed run counts (failure_kind is a classification that is
			// legitimately empty on an unclassified/legacy failure); only the
			// classified ones contribute to the by-kind breakdown.
			rs.Failed++
			if t.FailureKind != "" {
				failureCounts[t.FailureKind]++
			}
		}
		if domain.IsActiveConversationStatus(t.Status) {
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
	ID            string    `json:"id" doc:"The instance's id."`
	Role          string    `json:"role" doc:"The instance's role: control or executor."`
	Version       string    `json:"version" doc:"The build version the instance runs."`
	BootEpoch     int64     `json:"boot_epoch" doc:"A number that goes up by one each time the instance restarts."`
	StartedAt     time.Time `json:"started_at" doc:"When the instance's current process started, as an RFC 3339 timestamp."`
	LastHeartbeat time.Time `json:"last_heartbeat_at" doc:"When the instance last sent a heartbeat, as an RFC 3339 timestamp."`
	HeartbeatAgeS float64   `json:"heartbeat_age_seconds" doc:"How long ago the instance last sent a heartbeat, in seconds."`
	Stale         bool      `json:"stale" doc:"Whether the instance's last heartbeat is older than stale_after_seconds."`
	Draining      bool      `json:"draining" doc:"Whether the instance is draining: it starts no new work, and its running work finishes."`
	DispatchGated *bool     `json:"dispatch_gated,omitempty" doc:"Whether the executor last reported that it admits no new work, for lack of memory or disk or because it is stopping; absent on a control instance."`
	MaxRuns       *int      `json:"max_runs,omitempty" doc:"How many runs the executor can hold at once, absent on a control instance."`
	ActiveRuns    *int      `json:"active_runs,omitempty" doc:"How many runs the executor last reported holding, absent on a control instance."`
	MemTotalMB    *int      `json:"mem_total_mb,omitempty" doc:"The executor's total memory in MiB, absent on a control instance and when it could not be read."`
	MemAvailMB    *int      `json:"mem_available_mb,omitempty" doc:"The executor's available memory in MiB as of its last heartbeat, absent on a control instance and when it could not be read."`
	// Latest telemetry sample extras (nil until the first sample lands).
	CPUPct     *float64 `json:"cpu_pct,omitempty" doc:"The instance's CPU use, as a percentage, in its latest telemetry sample from the last five minutes, absent when there is none or it was not measured."`
	Load1      *float64 `json:"load1,omitempty" doc:"The host's one-minute load average in the instance's latest telemetry sample from the last five minutes, absent when there is none or it was not measured."`
	ClaimsMin  *int     `json:"claims_last_sample,omitempty" doc:"How many conversations the executor claimed between its previous telemetry sample and its latest one from the last five minutes, absent when there is none and on a control instance."`
	SpawnP50MS *int     `json:"spawn_p50_ms,omitempty" doc:"The median time, in milliseconds, from claim to agent start across the runs the executor started between its previous telemetry sample and its latest one from the last five minutes, absent when there is none or it started no run."`
}

type instancesDTO struct {
	GeneratedAt time.Time     `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	StaleAfterS float64       `json:"stale_after_seconds" doc:"How long after its last heartbeat, in seconds, an instance counts as stale."`
	Instances   []instanceDTO `json:"instances" doc:"Every registered instance."`
}

var opInstances = httpx.NewJSONOp[httpx.NoBody, instancesDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/instances",
	ID:          "list_fleet_instances",
	Summary:     "List the fleet's instances",
	Description: "Returns every registered instance with its role, version, heartbeat, drain state, and capacity, and its latest telemetry sample from the last five minutes." + fleetAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	opInstances.Write(w, instancesDTO{
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
	Draining bool `json:"draining" doc:"Whether the instance is to drain: true stops it starting new work until it is undrained, and false ends any drain, including one that would last until its next restart."`
}

// drainResponse echoes the drain state the request wrote.
type drainResponse struct {
	ID       string `json:"id" doc:"The instance's id."`
	Draining bool   `json:"draining" doc:"The drain state the request set."`
}

var opDrain = httpx.NewJSONOp[drainRequest, drainResponse](httpx.OpSpec{
	Pattern:     "POST /api/fleet/instances/{id}/drain",
	ID:          "drain_fleet_instance",
	Summary:     "Drain or undrain an instance",
	Description: "Sets whether an instance is draining. A draining instance starts no new work and lets its running work finish, and the drain lasts across restarts until it is undrained." + fleetAccess,
	PathParams:  map[string]string{"id": "The instance's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *handler) handleDrain(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		httpx.BadRequest(w, "instance id is required")
		return
	}
	req, ok := opDrain.Decode(w, r)
	if !ok {
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
	opDrain.Write(w, drainResponse{ID: id, Draining: req.Draining})
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
	ID             string     `json:"id" doc:"The claim's id: one executor's engagement with one conversation."`
	ConversationID string     `json:"conversation_id" doc:"The id of the conversation the engagement drove."`
	OrgID          string     `json:"org_id" doc:"The id of the organization the conversation belongs to."`
	ExecutorID     string     `json:"executor_id" doc:"The id of the instance that held the engagement."`
	ClaimedAt      time.Time  `json:"claimed_at" doc:"When the executor claimed the conversation, as an RFC 3339 timestamp."`
	ReleasedAt     *time.Time `json:"released_at,omitempty" doc:"When the executor released the conversation, as an RFC 3339 timestamp, absent while the engagement is live."`
	// Live marks an unreleased claim — still holding a slot, and whatever
	// series it has is still growing.
	Live bool `json:"live" doc:"Whether the engagement is still live, holding one of the executor's run slots."`
	// DurationS is the engagement's wall clock: claimed → released, or
	// claimed → now while live. Deliberately NOT the claim's runtime-reported
	// duration_ms, which times the agent invocation inside a wider engagement
	// (fetch, clone, sidecar bring-up) and would therefore be a denominator
	// smaller than the interval the CPU counter actually accumulated over —
	// inflating any derived average past what the box saw.
	DurationS float64 `json:"duration_seconds" doc:"The engagement's wall-clock length in seconds, from claim to release, or to now while it is live."`
	// PeakMemMB / CPUUsec are the teardown snapshot — the truth half of the
	// honest-display contract. Absent means NOT MEASURED (an unsandboxed
	// local claim, a pre-5.19 kernel with no memory.peak, a crashed
	// teardown), never measured-zero; render a dash, not a 0.
	PeakMemMB *int   `json:"peak_mem_mb,omitempty" doc:"The sandbox's peak memory in MiB, read when the engagement ended; absent when it was not measured, which is not the same as zero."`
	CPUUsec   *int64 `json:"cpu_usec,omitempty" doc:"The CPU time the sandbox used, in microseconds, read when the engagement ended; absent when it was not measured, which is not the same as zero."`
	// Status / FailureKind / Outcome are what the engagement was spent on.
	Status      string `json:"status,omitempty" doc:"The status of the conversation the engagement drove."`
	FailureKind string `json:"failure_kind,omitempty" doc:"The conversation's failure kind, absent when it has not failed or its failure has no kind."`
	Outcome     string `json:"outcome,omitempty" doc:"How the engagement ended, such as completed, failed, cancelled, parked, or one of the requeued outcomes; absent while it is live."`
	// LastActivityAt / CurrentOp are what the holder's last renewal stamped:
	// when the engagement last did anything its stall watchdog counts, and
	// the operation in flight then. Absent before the first renewal; on a
	// released claim they are the last reading it took.
	LastActivityAt *time.Time `json:"last_activity_at,omitempty" doc:"When the engagement last did work, as an RFC 3339 timestamp, as of the executor's last renewal of its claim; absent before the first renewal."`
	CurrentOp      string     `json:"current_op,omitempty" doc:"The operation the engagement had in flight at the executor's last renewal of its claim, absent when it had none."`
	// LastCheckpointAt is when the engagement's workspace was last covered by
	// a stored checkpoint, as the holder's last renewal stamped it. Absent for
	// an engagement that does not checkpoint, and before the first renewal.
	LastCheckpointAt *time.Time `json:"last_checkpoint_at,omitempty" doc:"When the engagement's workspace was last covered by a stored checkpoint, as an RFC 3339 timestamp, as of the executor's last renewal of its claim; absent for an engagement that does not checkpoint and before the first renewal."`
	// Sources is the engagement's source manifest as of its latest seal: for
	// each event source, whether its credential was in the sandbox's sealed
	// bundle and, when not, why not (state, a closed-vocabulary reason, and a
	// sealed credential's expiry). It outlives the engagement, so a finished
	// run still shows what it held. Absent for an engagement that was never
	// sealed.
	Sources domain.SourceManifest `json:"sources,omitempty" doc:"For each event source, keyed by its kind, whether the sandbox held its credential as of the engagement's latest seal and why not when it did not; absent for an engagement that was never sealed."`
}

type sandboxesDTO struct {
	GeneratedAt time.Time         `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	InstanceID  string            `json:"instance_id" doc:"The id of the executor the engagements ran on."`
	Limit       int               `json:"limit" doc:"The most engagements the response could hold, after the limit parameter was read."`
	Sandboxes   []sandboxClaimDTO `json:"sandboxes" doc:"The executor's most recent engagements, newest claim first."`
}

var opInstanceSandboxes = httpx.NewJSONOp[httpx.NoBody, sandboxesDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/instances/{id}/sandboxes",
	ID:          "list_fleet_instance_sandboxes",
	Summary:     "List an executor's recent sandboxes",
	Description: "Returns the executor's most recent engagements across every organization, each with what its sandbox used and which source credentials it held." + fleetAccess,
	PathParams:  map[string]string{"id": "The executor's instance id."},
	Query: []httpx.QueryParam{{
		Name: "limit", Type: httpx.ParamInteger,
		Doc: "The most engagements to return: 25 when absent, and 100 when above 100. A value that is not a whole number of at least 1 is refused.",
	}},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// parseSandboxLimit reads ?limit= — default 25, capped at 100, and REJECTED
// (not silently defaulted) when it is unparseable or below 1. Deliberately
// stricter than parseWindow's clamp above: an out-of-range window is a
// reasonable "show me as much as you can", whereas limit=abc is a caller bug
// that a silent default would hide behind plausible-looking data.
func parseSandboxLimit(raw string) (int, bool) {
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
	q, ok := opInstanceSandboxes.Query(w, r)
	if !ok {
		return
	}
	limit, ok := parseSandboxLimit(q.Get("limit"))
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
	opInstanceSandboxes.Write(w, sandboxesDTO{
		GeneratedAt: now, InstanceID: id, Limit: limit, Sandboxes: out,
	})
}

func sandboxClaim(c domain.ExecutorClaim, now time.Time) sandboxClaimDTO {
	d := sandboxClaimDTO{
		ID: c.ID, ConversationID: c.ConversationID, OrgID: c.OrgID, ExecutorID: c.ExecutorID,
		ClaimedAt: c.ClaimedAt, ReleasedAt: c.ReleasedAt, Live: c.ReleasedAt == nil,
		PeakMemMB: c.PeakMemMB, CPUUsec: c.CPUUsec,
		Status: c.Status, FailureKind: c.FailureKind, Outcome: c.Outcome,
		LastActivityAt: c.LastActivityAt, CurrentOp: c.CurrentOp,
		LastCheckpointAt: c.LastCheckpointAt,
		Sources:          c.Sources,
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

// --- GET /api/fleet/claims/{id} ---
//
// One engagement, by claim id: the same row the sandbox breakdown lists, for an
// operator who holds a claim id and not the executor it ran on. Its source
// manifest is the reason to ask — which credentials that sandbox held, and why
// each other one was absent. Cross-org and operator-gated like its siblings;
// capabilities belong to an engagement, not a conversation, which can have
// several, so this is addressed by the claim.

type claimDetailDTO struct {
	GeneratedAt time.Time `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	sandboxClaimDTO
}

var opClaim = httpx.NewJSONOp[httpx.NoBody, claimDetailDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/claims/{id}",
	ID:          "get_fleet_claim",
	Summary:     "Get an executor engagement",
	Description: "Returns one executor engagement by its claim id, in any organization: what its sandbox used and which source credentials it held." + fleetAccess,
	PathParams:  map[string]string{"id": "The claim's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		httpx.BadRequest(w, "claim id is required")
		return
	}
	now := time.Now().UTC()
	claim, err := h.stores.ConversationQueue.ClaimByIDSystem(r.Context(), id)
	if err != nil {
		httpx.InternalError(w, "fleet-claim", err)
		return
	}
	if claim == nil {
		httpx.NotFound(w, "claim")
		return
	}
	opClaim.Write(w, claimDetailDTO{GeneratedAt: now, sandboxClaimDTO: sandboxClaim(*claim, now)})
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
	At           time.Time `json:"at" doc:"When the sample was taken, as an RFC 3339 timestamp."`
	MemCurrentMB *int      `json:"mem_current_mb,omitempty" doc:"The sandbox's memory in use when the sample was taken, in MiB, absent when it was not measured."`
	CPUUsecCum   *int64    `json:"cpu_usec_cum,omitempty" doc:"The CPU time the sandbox had used since it started, in microseconds, absent when it was not measured; the difference between two samples is the CPU used between them."`
}

type sandboxSeriesDTO struct {
	GeneratedAt time.Time          `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	ClaimID     string             `json:"claim_id" doc:"The claim's id."`
	Samples     []sandboxSampleDTO `json:"samples" doc:"Every sample taken of the engagement's sandbox, oldest first, empty when none was taken."`
}

var opClaimSeries = httpx.NewJSONOp[httpx.NoBody, sandboxSeriesDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/claims/{id}/series",
	ID:          "get_fleet_claim_series",
	Summary:     "Get an engagement's sandbox resource samples",
	Description: "Returns the memory and cumulative CPU samples taken of one engagement's sandbox while it ran. The samples miss short spikes, so the engagement's peak memory can be above the highest sample." + fleetAccess,
	PathParams:  map[string]string{"id": "The claim's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	opClaimSeries.Write(w, sandboxSeriesDTO{
		GeneratedAt: now, ClaimID: id, Samples: out,
	})
}

// --- GET /api/fleet/timeseries ---

type sampleDTO struct {
	InstanceID     string    `json:"instance_id" doc:"The id of the instance that took the sample."`
	At             time.Time `json:"at" doc:"When the sample was taken, as an RFC 3339 timestamp."`
	ActiveRuns     *int      `json:"active_runs,omitempty" doc:"How many runs the executor held, absent on a control instance."`
	QueuedVisible  *int      `json:"queued_visible,omitempty" doc:"How many conversations across the deployment were waiting for an executor, absent on a control instance and when it could not be read."`
	MemAvailableMB *int      `json:"mem_available_mb,omitempty" doc:"The instance's available memory in MiB, absent when it could not be read."`
	CPUPct         *float64  `json:"cpu_pct,omitempty" doc:"The instance's CPU use as a percentage, absent when it could not be read."`
	Load1          *float64  `json:"load1,omitempty" doc:"The host's one-minute load average, absent when it could not be read."`
	Claims         *int      `json:"claims,omitempty" doc:"How many conversations the executor claimed since its previous sample, absent on a control instance."`
	SpawnP50MS     *int      `json:"spawn_p50_ms,omitempty" doc:"The median time, in milliseconds, from claim to agent start across the runs the executor started since its previous sample, absent when it started none and on a control instance."`
	OOMKills       *int      `json:"oom_kills,omitempty" doc:"How many processes the kernel killed for lack of memory on the instance since its previous sample, absent when it could not be read."`
}

type timeseriesDTO struct {
	GeneratedAt time.Time   `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	WindowHours int         `json:"window_hours" doc:"The window's length in hours."`
	Samples     []sampleDTO `json:"samples" doc:"Every instance's telemetry samples taken within the window, oldest first."`
}

var opTimeseries = httpx.NewJSONOp[httpx.NoBody, timeseriesDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/timeseries",
	ID:          "get_fleet_timeseries",
	Summary:     "Get the fleet's telemetry samples",
	Description: "Returns every instance's telemetry samples taken within the window, about one a minute each: runs held, queue depth, memory, CPU, load, claims, spawn latency, and out-of-memory kills." + fleetAccess,
	Query:       []httpx.QueryParam{hoursQuery},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *handler) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	q, ok := opTimeseries.Query(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	since, hours := parseWindow(q.Get("hours"), now)

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
	opTimeseries.Write(w, timeseriesDTO{GeneratedAt: now, WindowHours: hours, Samples: out})
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
	OrgID       string  `json:"org_id" doc:"The organization's id."`
	Count       int     `json:"count" doc:"How many of the organization's conversations are waiting for an executor."`
	OldestWaitS float64 `json:"oldest_wait_seconds" doc:"How long the organization's conversation waiting longest has waited, in seconds."`
}

type backlogDTO struct {
	GeneratedAt time.Time         `json:"generated_at" doc:"When the server built the response, as an RFC 3339 timestamp."`
	Depth       int               `json:"depth" doc:"How many conversations across the deployment are waiting for an executor."`
	OldestWaitS float64           `json:"oldest_wait_seconds" doc:"How long the conversation waiting longest has waited, in seconds, or 0 when none is waiting."`
	ByOrg       []backlogOrgShare `json:"by_org" doc:"Each organization with a conversation waiting, most waiting first, then longest wait first; null when none is waiting."`
}

var opBacklog = httpx.NewJSONOp[httpx.NoBody, backlogDTO](httpx.OpSpec{
	Pattern:     "GET /api/fleet/backlog",
	ID:          "get_fleet_backlog",
	Summary:     "Get the fleet's backlog",
	Description: "Returns how many conversations across the deployment are waiting for an executor and how long the oldest has waited, with each organization's count and oldest wait." + fleetAccess,
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (h *handler) handleBacklog(w http.ResponseWriter, r *http.Request) {
	if h.gate(w, r) == nil {
		return
	}
	now := time.Now().UTC()
	queued, err := h.stores.ConversationQueue.QueuedConversationAgesSystem(r.Context())
	if err != nil {
		httpx.InternalError(w, "fleet-backlog", err)
		return
	}
	dto := backlogDTO{GeneratedAt: now, Depth: len(queued), ByOrg: backlogByOrg(queued, now)}
	if len(queued) > 0 {
		// QueuedConversationAgesSystem is oldest-first, so [0] is the oldest wait.
		dto.OldestWaitS = now.Sub(queued[0].EnqueuedAt).Seconds()
	}
	opBacklog.Write(w, dto)
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
