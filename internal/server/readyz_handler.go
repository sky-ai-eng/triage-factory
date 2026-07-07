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

// readyzResponse is GET /readyz's body (TFAC-573). Every field is either
// a boolean-ish check result or an opaque org ID + counters — no repo
// names, usernames, or other tenant data, matching /readyz's no-verbose-
// mode design (there is nothing more to disclose at any verbosity).
type readyzResponse struct {
	Status     string            `json:"status"` // "ok" | "degraded" | "not_ready"
	CheckedAt  int64             `json:"checked_at"`
	Version    string            `json:"version"`
	Checks     map[string]string `json:"checks"` // "ok" | "failed" per hard check
	Sources    readyzSources     `json:"sources"`
	ActiveRuns int               `json:"active_runs"`
}

type readyzSources struct {
	GitHub map[string]readyzOrgSource `json:"github"`
	Jira   map[string]readyzOrgSource `json:"jira"`
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

// handleReadyz is the readiness probe (TFAC-573): DB reachability,
// migrations-applied, and poller-heartbeat-alive are HARD checks — any
// failure returns 503 so an LB/k8s readinessProbe pulls the instance out
// of rotation. Poll staleness per org and active_runs are SOFT signals,
// reported for the operator to alert on but never flip the HTTP status —
// polling can be legitimately idle (no orgs configured yet) or
// intentionally slow, so a staleness threshold beyond the endpoint's own
// default (readyzDegradedFactor) is the operator's call, not this
// handler's. Pre-auth, mirroring /api/health — see the doc-comment block
// above routes() and the Design decisions in TFAC-573 for why there is no
// verbose/admin mode.
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

	degraded := false
	if s.pollerHealth != nil {
		snap := s.pollerHealth(ctx)
		ghAlive, ghSources, ghDegraded := readyzSourceCheck(snap.GitHub)
		jiraAlive, jiraSources, jiraDegraded := readyzSourceCheck(snap.Jira)
		resp.Checks["poller_github"] = checkLabel(ghAlive)
		resp.Checks["poller_jira"] = checkLabel(jiraAlive)
		resp.Sources.GitHub = ghSources
		resp.Sources.Jira = jiraSources
		hardOK = hardOK && ghAlive && jiraAlive
		degraded = ghDegraded || jiraDegraded
	} else {
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
