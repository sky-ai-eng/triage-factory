package server

import (
	"context"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/poller"
)

// readyzTimeout bounds the DB ping the "db" hard check performs — long
// enough to absorb a slow-but-alive connection, short enough that a truly
// wedged pool fails the probe instead of hanging the caller's health
// check. Deliberately tighter than db.go's bare Ping() at open time: this
// runs on every probe hit, not once at boot.
const readyzTimeout = 2 * time.Second

// readyzDegradedFactor is the default multiple of an org's configured
// poll interval past which a stale last_successful_poll flips the
// top-level status from "ok" to "degraded" (never 503 — see
// handleReadyz). It's a reasonable general-purpose default for that one
// dashboard-facing field, not a claim about what's actually wrong — the
// response also carries the raw age_seconds/interval_seconds per org so
// an operator wanting a different threshold (or per-org tuning) can alert
// directly off those instead of this field.
const readyzDegradedFactor = 3

// readyzResponse is GET /readyz's body (TFAC-573; the `lease` field added
// by TFAC-583). Every field is either a boolean-ish check result or an
// opaque org ID + counters — no repo names, usernames, or other tenant
// data, matching /readyz's no-verbose-mode design (there is nothing more
// to disclose at any verbosity).
type readyzResponse struct {
	Status     string            `json:"status"` // "ok" | "degraded" | "not_ready"
	CheckedAt  int64             `json:"checked_at"`
	Version    string            `json:"version"`
	Checks     map[string]string `json:"checks"` // "ok" | "failed" | "standby" (poller_* only) per hard check
	Sources    readyzSources     `json:"sources"`
	RateLimit  readyzRateLimit   `json:"rate_limit"`
	ActiveRuns int               `json:"active_runs"`
	// Lease is the background-brain lease state (TFAC-583) — present on
	// every control pod's response (holder or standby), omitted entirely
	// at TF_ROLE=all / local, where s.leaseStatus is never wired (a
	// single process always self-holds, so the field would be moot and
	// its absence keeps that contract byte-identical to before this
	// ticket).
	Lease *readyzLease `json:"lease,omitempty"`
}

// readyzLease is GET /readyz's `lease` field (TFAC-583, spec §8.3).
type readyzLease struct {
	Name     string `json:"name"`
	HolderID string `json:"holder_id"`
	IsHolder bool   `json:"is_holder"`
	Term     int64  `json:"term"`
}

// LeaseStatusFunc reports this pod's current view of the named lease —
// the shape Server.SetLeaseStatus wires from internal/app's
// leaseStatusForReadyz. A plain function tuple (not a struct) so
// readyz_handler.go depends on nothing from internal/lease.
type LeaseStatusFunc func() (name, holderID string, term int64, isHolder bool)

type readyzSources struct {
	GitHub map[string]readyzOrgSource `json:"github"`
	Jira   map[string]readyzOrgSource `json:"jira"`
}

// readyzRateLimit is the soft rate-limit signal (TFAC-573 follow-up):
// each org's last-observed primary GitHub rate-limit budget, as reported
// by the poller's resolver (see poller.HealthSnapshot.GitHubRateLimit).
// GitHub-only — Jira has no equivalent concept in this codebase. An org
// absent from GitHub means no observation yet, not zero remaining budget;
// never flips the top-level status or the HTTP code.
type readyzRateLimit struct {
	GitHub map[string]readyzOrgRateLimit `json:"github"`
}

type readyzOrgRateLimit struct {
	Remaining int   `json:"remaining"`
	ResetUnix int64 `json:"reset_unix"`
	Used      int   `json:"used"`
}

// readyzOrgSource is one org's soft poll-staleness signal for one source.
// LastSuccessUnix/AgeSeconds are omitted (zero value, tag'd omitempty)
// when the org has never completed a poll of this source — "never
// polled" and "polled 0 seconds ago" are different facts, and the omitted
// keys make that distinction visible in the JSON rather than reading as
// the latter.
type readyzOrgSource struct {
	LastSuccessUnix int64 `json:"last_success_unix,omitempty"`
	AgeSeconds      int64 `json:"age_seconds,omitempty"`
	IntervalSeconds int   `json:"interval_seconds"`
}

// handleReadyz is the readiness probe (TFAC-573; lease-conditionality
// added by TFAC-583, spec §8.3): DB reachability and migrations-applied
// are ALWAYS hard checks. The poller-heartbeat-alive hard check is
// LEASE-CONDITIONAL: on the background-brain lease holder (or any pod
// with no lease wired at all — TF_ROLE=all / local, byte-identical to the
// original TFAC-573 contract) it behaves exactly as before; on a standby
// control pod it is NOT a hard check at all — a standby runs no pollers
// by design, and hard-failing it would pull every non-leader control pod
// out of LB rotation and collapse the HA shape to leader-only serving.
// poller_github/poller_jira report the literal string "standby" there
// instead of "ok"/"failed". Poll staleness per org, per-org GitHub
// rate-limit budget, and active_runs are SOFT signals on the holder,
// reported for the operator to alert on but never flip the HTTP status —
// polling can be legitimately idle (no orgs configured yet) or
// intentionally slow, and a low rate-limit budget is informational (the
// client already backs off on its own), so a staleness threshold beyond
// the endpoint's own default (readyzDegradedFactor) is the operator's
// call, not this handler's. Pre-auth, mirroring /api/health — see the
// doc-comment block above routes() and the Design decisions in TFAC-573
// for why there is no verbose/admin mode.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	resp := readyzResponse{
		CheckedAt: time.Now().Unix(),
		Version:   s.version,
		Checks:    map[string]string{},
	}
	hardOK := true

	if err := s.db.PingContext(ctx); err != nil {
		resp.Checks["db"] = "failed"
		hardOK = false
	} else {
		resp.Checks["db"] = "ok"
	}

	// Always "ok" while the process is answering requests at all — see
	// SetMigrationsOK's doc comment for why the check exists anyway.
	if s.migrationsOK {
		resp.Checks["migrations"] = "ok"
	} else {
		resp.Checks["migrations"] = "failed"
		hardOK = false
	}

	// isHolder defaults true — the byte-identical-to-TFAC-573 path — for
	// every pod that never wires SetLeaseStatus: TF_ROLE=all, local mode,
	// and bare test/dev Server constructions all fall here, and none of
	// them ever populate resp.Lease.
	isHolder := true
	if s.leaseStatus != nil {
		name, holderID, term, holds := s.leaseStatus()
		resp.Lease = &readyzLease{Name: name, HolderID: holderID, Term: term, IsHolder: holds}
		isHolder = holds
	}

	resp.RateLimit.GitHub = map[string]readyzOrgRateLimit{}

	degraded := false
	switch {
	case !isHolder:
		// Standby: no pollers running here by design — informational only,
		// never a hard check. LB/readinessProbe must keep this pod in
		// rotation regardless.
		resp.Checks["poller_github"] = "standby"
		resp.Checks["poller_jira"] = "standby"
		resp.Sources.GitHub = map[string]readyzOrgSource{}
		resp.Sources.Jira = map[string]readyzOrgSource{}
	case s.pollerHealth != nil:
		snap := s.pollerHealth(ctx)
		ghAlive, ghSources, ghDegraded := readyzSourceCheck(snap.GitHub)
		jiraAlive, jiraSources, jiraDegraded := readyzSourceCheck(snap.Jira)
		resp.Checks["poller_github"] = checkLabel(ghAlive)
		resp.Checks["poller_jira"] = checkLabel(jiraAlive)
		resp.Sources.GitHub = ghSources
		resp.Sources.Jira = jiraSources
		hardOK = hardOK && ghAlive && jiraAlive
		degraded = ghDegraded || jiraDegraded

		for orgID, st := range snap.GitHubRateLimit {
			resp.RateLimit.GitHub[orgID] = readyzOrgRateLimit{
				Remaining: st.Remaining,
				ResetUnix: st.Reset.Unix(),
				Used:      st.Used,
			}
		}
	default:
		// Not wired (a bare test/dev Server construction that skipped
		// SetPollerManager) — report the gap loudly rather than nil-
		// dereffing or silently claiming "ok" for a check that never ran.
		resp.Checks["poller_github"] = "failed"
		resp.Checks["poller_jira"] = "failed"
		resp.Sources.GitHub = map[string]readyzOrgSource{}
		resp.Sources.Jira = map[string]readyzOrgSource{}
		hardOK = false
	}

	if n, err := s.countActiveRuns(ctx); err != nil {
		serverLog.Warn("readyz: count active runs failed", "error", err)
	} else {
		resp.ActiveRuns = n
	}

	status := "ok"
	switch {
	case !hardOK:
		status = "not_ready"
	case degraded:
		status = "degraded"
	}
	resp.Status = status

	code := http.StatusOK
	if !hardOK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

// readyzSourceCheck flattens one poller.SourceHealth into the response
// shape: the hard alive bool, the per-org soft-signal map, and whether
// any org's last-successful-poll is stale enough to mark the top-level
// status "degraded" (readyzDegradedFactor x its own configured interval).
// An org with a zero LastSuccess (never polled) is never counted as
// degraded — that's "no signal yet" (a fresh org, or one not configured
// for this source), not staleness.
func readyzSourceCheck(sh poller.SourceHealth) (alive bool, orgs map[string]readyzOrgSource, degraded bool) {
	now := time.Now()
	orgs = make(map[string]readyzOrgSource, len(sh.Orgs))
	for orgID, oh := range sh.Orgs {
		entry := readyzOrgSource{IntervalSeconds: oh.IntervalSeconds}
		if !oh.LastSuccess.IsZero() {
			age := now.Sub(oh.LastSuccess)
			entry.LastSuccessUnix = oh.LastSuccess.Unix()
			entry.AgeSeconds = int64(age.Seconds())
			if oh.IntervalSeconds > 0 && age > time.Duration(readyzDegradedFactor)*time.Duration(oh.IntervalSeconds)*time.Second {
				degraded = true
			}
		}
		orgs[orgID] = entry
	}
	return sh.Alive, orgs, degraded
}

func checkLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

// countActiveRuns is the indexed COUNT backing the active_runs soft
// signal: runs.status has idx_runs_status (plus a partial index on
// status='queued'), so this is an index scan, not a table scan. Reads
// s.db directly (the admin/system pool in multi mode, RLS-exempt)
// rather than a team-scoped store — /readyz is pre-auth with no session
// to scope a team from, and a bare count discloses no tenant data, so
// this is the "explicitly annotated org-wide system job" category in
// CLAUDE.md's multi-mode read-scoping rule, not a cross-team leak.
func (s *Server) countActiveRuns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE status IN ('queued', 'running')`).Scan(&n)
	return n, err
}
