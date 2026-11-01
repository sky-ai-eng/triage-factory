package poller

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/tracker"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// loadLinearRules reads the union of every team's Linear rules in the org's
// current workspace; toTrackerLinearRules merges them per Linear team. Rules
// saved under another workspace name teams this one does not have, so they are
// never asked about. Empty on error, with ok false.
func (m *Manager) loadLinearRules(ctx context.Context, orgID, workspaceID string) (rules []domain.LinearTeamRules, ok bool) {
	if m.linearRules == nil {
		return nil, true
	}
	rules, err := m.linearRules.ListForOrgSystem(ctx, orgID, workspaceID)
	if err != nil {
		pollerLog.Warn("list linear rules (org union) failed", "org", orgID, "error", err)
		return nil, false
	}
	return rules, true
}

// startLinear launches the Linear tracking loop, startJira's sibling: one
// goroutine waking every basePollInterval, polling the active orgs whose own
// interval has elapsed. Each org's credential, rules and interval are read
// inside the loop, so connecting or arming Linear needs no restart.
func (m *Manager) startLinear() {
	stop := make(chan struct{})
	m.mu.Lock()
	m.linearStop = stop
	m.mu.Unlock()

	go func() {
		m.runLinearCycle(stop)

		ticker := time.NewTicker(basePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.runLinearCycle(stop)
			case <-stop:
				return
			}
		}
	}()

	linearLog.Info("tracker started (per-org cadence resolved each wake)", "base_tick", basePollInterval)
}

// runLinearCycle enumerates active orgs and polls the ones that are due. A
// failure in one org is logged and reported, and the others still poll.
func (m *Manager) runLinearCycle(stop <-chan struct{}) {
	m.stampLinearHeartbeat()
	ctx, span := tracer.Start(context.Background(), "poll.linear",
		trace.WithAttributes(telemetry.Source("linear")))
	defer span.End()
	ctx = upstream.WithProgress(ctx, m.stampLinearHeartbeat)

	now := time.Now()
	orgIDs, err := m.orgs.ListActiveSystem(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "list active orgs")
		linearLog.ErrorContext(ctx, "list active orgs failed", "error", err)
		m.reportError("linear", "", err)
		return
	}
	polled := 0
	defer func() { span.SetAttributes(telemetry.Count(polled)) }()
	for _, orgID := range orgIDs {
		select {
		case <-stop:
			span.SetAttributes(telemetry.Outcome("stopped"))
			return
		default:
		}
		if !m.pollDue("linear", orgID, now) {
			continue
		}
		polled++
		m.runLinearCycleForOrg(ctx, orgID, now)
		m.stampLinearHeartbeat()
	}
	m.prunePoll("linear", orgIDs)
}

// runLinearCycleForOrg polls one org: settings → next-slot reservation →
// pause check → credentials → rules → one RefreshLinear over the merged team
// set. The order and the reasons for it are runJiraCycleForOrg's.
//
// A rate limit stops the cycle where it lands (see Tracker.RefreshLinear).
// It is reported and the org is not stamped successful; when Linear said when
// the window resets, the org's next poll is scheduled then instead of at its
// interval. Restarting from the top after the reset is fine: a Linear cycle
// is a few requests per armed team plus one per 50 tracked issues, so nothing
// at the tail of a long list starves the way GitHub's repo fan-out would
// without its cursor.
func (m *Manager) runLinearCycleForOrg(ctx context.Context, orgID string, now time.Time) {
	ctx, span := tracer.Start(ctx, "poll.linear.org",
		trace.WithAttributes(telemetry.Source("linear"), telemetry.OrgID(orgID)))
	defer span.End()

	ctx, conn := newCycleConnection(ctx)
	refreshed := false
	defer func() {
		if state := m.recordConnection(ctx, linearLog, "linear", orgID, conn); refreshed && state != db.ConnectionDown {
			m.stampLinearSuccess(orgID)
		}
	}()

	orgSet, oerr := m.orgs.GetSettingsSystem(ctx, orgID)
	if oerr != nil {
		span.SetStatus(codes.Error, "load settings")
		linearLog.ErrorContext(ctx, "load settings failed", "org", orgID, "error", oerr)
		m.reportError("linear", orgID, oerr)
		return
	}
	m.schedulePoll("linear", orgID, now.Add(clampPollInterval(orgSet.LinearPollInterval)))
	if skip, outcome := m.sourceDisabled(ctx, "linear", orgID); skip {
		span.SetAttributes(telemetry.Outcome(outcome))
		if outcome == "disabled" {
			conn.skip(outcome)
		}
		return
	}
	creds, lerr := integrations.LoadSystem(ctx, m.secrets, orgID)
	if lerr != nil {
		span.SetStatus(codes.Error, "load creds")
		linearLog.ErrorContext(ctx, "load creds failed", "org", orgID, "error", lerr)
		m.reportError("linear", orgID, lerr)
		return
	}
	// The workspace is the scope every Linear entity and rule is keyed under:
	// the tracker retires rows from any other one, and only this one's rules
	// say what to discover.
	workspaceID := domain.EntityScope("linear", orgSet)
	bound := integrations.LinearSystemConfigured(creds) && workspaceID != ""
	rules, rulesRead := m.loadLinearRules(ctx, orgID, workspaceID)
	teams := toTrackerLinearRules(rules)
	// With no armed team the cycle still refreshes the entities TF holds in
	// this workspace (followsEntities), but only from rules it read, for
	// Jira's reason: refreshing a tracked issue as if no team were armed
	// would advance its snapshot past the transitions its rules would have
	// emitted.
	refreshOnly := bound && rulesRead && len(teams) == 0 && m.followsEntities(ctx, linearLog, "linear", orgID, workspaceID)
	if bound && len(teams) == 0 && !refreshOnly {
		// Nothing in this workspace to poll, but issues a previous workspace
		// left tracked still retire: that needs no Linear call, and nothing
		// else would ever close them. A refresh-only cycle's RefreshLinear
		// retires them itself, first.
		m.trackerForOrg(orgID).RetireLinearOutOfScope(ctx, workspaceID)
	}
	if !bound || (rulesRead && len(teams) == 0) {
		// Nothing to ask Linear: with no credential bound, or no team armed,
		// no armed team is one the credential cannot read. This is what
		// resolves the condition once the last armed team is untracked or
		// disarmed, since no RefreshLinear runs to do it.
		kinds.LinearTeamUnreadable.Sync(ctx, orgID, nil)
	}
	if !bound || (len(rules) == 0 && !refreshOnly) {
		span.SetAttributes(telemetry.Outcome("unconfigured"))
		conn.skip("unconfigured")
		return
	}
	if len(teams) == 0 {
		if !refreshOnly {
			span.SetAttributes(telemetry.Outcome("no_armed_teams"))
			conn.skip("no_armed_teams")
			return
		}
		// RefreshLinear with no teams: discovery and the visibility check
		// ask nothing, and the refresh reads the entities by UUID.
		span.SetAttributes(telemetry.Disposition("refresh_only"))
	}
	if m.linearResolver == nil {
		span.SetStatus(codes.Error, "resolve system client")
		linearLog.ErrorContext(ctx, "no linear resolver wired", "org", orgID)
		m.reportError("linear", orgID, errors.New("poller: no linear resolver wired"))
		return
	}
	client, cerr := m.linearResolver.ForSystem(ctx, orgID)
	if errors.Is(cerr, eventsource.ErrDisabled) {
		// Turned off after the cycle's skip read the policy: the resolver's own
		// gate refused, and this is the same deliberate pause.
		span.SetAttributes(telemetry.Outcome("disabled"))
		conn.skip("disabled")
		return
	}
	if cerr != nil {
		span.SetStatus(codes.Error, "resolve system client")
		linearLog.Log(ctx, upstream.LogLevel(cerr, slog.LevelError), "resolve system client failed", "org", orgID, "error", cerr)
		m.reportError("linear", orgID, cerr)
		return
	}
	_, err := m.trackerForOrg(orgID).RefreshLinear(ctx, workspaceID, client, teams)
	m.clearIfTurnedOff(ctx, orgID)
	if err != nil {
		var rl *linear.RateLimitError
		if errors.As(err, &rl) {
			span.SetAttributes(telemetry.Outcome("rate_limited"))
			linearLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "linear poll cycle stopped by rate limit", "org", orgID, "reset", rl.Reset, "error", err)
			m.reportError("linear", orgID, err)
			if !rl.Reset.IsZero() {
				m.holdPoll("linear", orgID, rl.Reset)
			}
			return
		}
		span.SetStatus(codes.Error, "refresh")
		linearLog.Log(ctx, upstream.LogLevel(err, slog.LevelError), "tracker error", "org", orgID, "error", err)
		m.reportError("linear", orgID, err)
		return
	}
	refreshed = true
}

// clearIfTurnedOff clears Linear's conditions when an admin turned Linear off
// while the refresh ran: the refresh may have raised one after the switch's
// own ClearSource, and a turned-off source is not polled again to resolve it.
// The switch commits before it clears, so this read sees it. A failed read
// clears nothing; turning the source back on and its next cycle correct what
// is left.
func (m *Manager) clearIfTurnedOff(ctx context.Context, orgID string) {
	if m.EventSources == nil {
		return
	}
	off, err := eventsource.Disabled(ctx, m.EventSources, orgID, eventsource.KindLinear)
	if err != nil {
		linearLog.WarnContext(ctx, "re-reading the off switch after the refresh failed; a notification raised as linear was turned off may stay open",
			"org", orgID, "error", err)
		return
	}
	if off {
		notify.ClearSource(ctx, orgID, eventsource.KindLinear)
	}
}

// toTrackerLinearRules collapses the org-wide rule union into the tracker's
// per-Linear-team view, toTrackerJiraRules' sibling: unarmed rows dropped, one
// entry per linear_team_id, and each rule's states set-unioned across the TF
// teams that arm it, in first-seen order. Input arrives ordered by
// (linear_team_id, team_id), so the output is deterministic.
//
// The union is the most permissive reading on purpose, as for Jira: no team's
// pickup issue is missed, and any team's notion of done closes the shared
// entity. Which team sees what is the router's gate, not discovery's.
func toTrackerLinearRules(rules []domain.LinearTeamRules) tracker.LinearRules {
	type merged struct {
		key                                  string
		pickup, inProgress, done             []domain.LinearStateRef
		pickupSeen, inProgressSeen, doneSeen map[string]bool
	}
	byID := map[string]*merged{}
	order := make([]string, 0, len(rules))
	addUnique := func(dst []domain.LinearStateRef, seen map[string]bool, src []domain.LinearStateRef) []domain.LinearStateRef {
		for _, ref := range src {
			if key := domain.LinearStateDedupKey(ref); !seen[key] {
				seen[key] = true
				dst = append(dst, ref)
			}
		}
		return dst
	}
	for _, r := range rules {
		if !r.Armed() {
			continue
		}
		m := byID[r.LinearTeamID]
		if m == nil {
			m = &merged{key: r.LinearTeamKey, pickupSeen: map[string]bool{}, inProgressSeen: map[string]bool{}, doneSeen: map[string]bool{}}
			byID[r.LinearTeamID] = m
			order = append(order, r.LinearTeamID)
		}
		m.pickup = addUnique(m.pickup, m.pickupSeen, r.PickupMembers)
		m.inProgress = addUnique(m.inProgress, m.inProgressSeen, r.InProgressMembers)
		m.done = addUnique(m.done, m.doneSeen, r.DoneMembers)
	}
	out := make(tracker.LinearRules, 0, len(order))
	for _, id := range order {
		m := byID[id]
		out = append(out, tracker.LinearTeamRule{
			ID:         id,
			Key:        m.key,
			Pickup:     m.pickup,
			InProgress: m.inProgress,
			Done:       m.done,
		})
	}
	return out
}
