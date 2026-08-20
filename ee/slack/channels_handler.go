// Slack channel tracking API (TFAC-543): a team lists channels it's
// tracking (union sightings + live Slack candidates), tracks/untracks a
// full set, and reassigns a channel's primary tracking team. The primary
// lifecycle (first-tracker default, oldest-tracker succession) is handled
// server-side by TeamChannelStore.ReconcilePrimariesSystem; a team's first
// tracked channel seeds its default slack:message blueprint so mentions in a
// newly tracked channel have somewhere to route once TFAC-510's routing flip
// lands. Deliberately under /api/slack/* rather than
// /api/settings/team/... — ee-owned routes stay in the slack namespace.
package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// slackEntitlementGate is the shared local-mode-404 + entitlement-404 gate
// every Slack route runs before its own authz: local mode 404s
// unconditionally (the Slack store is Postgres-only — see
// workspacesHandler's doc), then the `slack` entitlement is checked for the
// resolved org. workspacesHandler.memberGate delegates here so the two
// handler families can't drift.
func slackEntitlementGate(w http.ResponseWriter, r *http.Request) (orgID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		httpx.NotFound(w, "route")
		return "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", false
	}
	if !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		httpx.NotFound(w, "route")
		return "", false
	}
	return orgID, true
}

// channelsHandler serves /api/slack/teams/{team_id}/channels and
// /api/slack/channels/{channel_id}/primary — the channel tracking surface a
// team's settings page builds against. Needs both the transactional store
// runner (RLS-gated team/registry reads + the tracking write) and the
// non-tx admin-pool aggregate (cross-team trackers, secrets, post-commit
// reconcile/ensure/auto-join).
type channelsHandler struct {
	tx     db.TxRunner
	stores db.Stores
	az     *authz.Checker
	client *http.Client
}

// channelsWarning is one entry in a channelsResponse's Warnings — a tagged
// union serialized as a single JSON shape rather than two response fields,
// since PUT's response is "the GET shape + warnings" (one envelope): GET
// only ever populates Code (a process-level degradation, e.g.
// "slack_unreachable"); PUT's auto-join step only ever populates ChannelID
// + Reason (a per-channel outcome, "invite_required" | "join_failed").
type channelsWarning struct {
	Code      string `json:"code,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// trackerView is one team's tracking claim on a channel, as disclosed on
// tracked_by — name + role-in-channel only, never activity/config (the
// ratified cross-team disclosure line — see TeamChannelStore.
// ListTrackersForOrgSystem's doc).
type trackerView struct {
	TeamID    string `json:"team_id"`
	TeamName  string `json:"team_name"`
	IsPrimary bool   `json:"is_primary"`
}

// channelView is one merged row of the GET/PUT response's channels array —
// the union of a team's tracked rows, the org's sighting registry, and live
// Slack candidates, deduped by channel_id.
type channelView struct {
	ChannelID     string        `json:"channel_id"`
	Name          string        `json:"name"`
	WorkspaceID   string        `json:"workspace_id"`
	IsPrivate     bool          `json:"is_private"`
	BotIsMember   bool          `json:"bot_is_member"`
	Tracked       bool          `json:"tracked"`
	IsPrimary     bool          `json:"is_primary"`
	TrackedBy     []trackerView `json:"tracked_by"`
	LastMentionAt *time.Time    `json:"last_mention_at,omitempty"`
	Source        string        `json:"source"` // "tracked" | "sighting" | "slack"
}

type channelsResponse struct {
	Role     string            `json:"role"`
	Warnings []channelsWarning `json:"warnings"`
	Channels []channelView     `json:"channels"`
}

// GET /api/slack/teams/{team_id}/channels — any member of the team.
func (h *channelsHandler) handleList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := slackEntitlementGate(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	teamID, err := h.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "slack/channels", err)
		return
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	_, role, err := h.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	// A non-member of an in-org team gets a 404, not an empty list — the
	// channel roster is member-visible, not org-visible (same non-disclosure
	// posture as VerifyTeamInOrg above).
	if role == "" {
		httpx.NotFound(w, "team")
		return
	}

	resp, err := h.buildChannelsResponse(r.Context(), orgID, userID, teamID, role)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// putChannelsRequest is PUT's full-set replace body.
type putChannelsRequest struct {
	ChannelIDs []string `json:"channel_ids"`
}

// PUT /api/slack/teams/{team_id}/channels — team admin, full-set replace.
func (h *channelsHandler) handlePut(w http.ResponseWriter, r *http.Request) {
	orgID, ok := slackEntitlementGate(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	teamID, err := h.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "slack/channels", err)
		return
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}
	if !h.az.RequireTeamAdmin(w, r, orgID, userID, teamID) {
		return
	}

	var req putChannelsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	desired := normalizeChannelIDs(req.ChannelIDs)

	var prior []slackstore.TeamChannel
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		var e error
		if prior, e = bundle.TeamChannels.ListForTeam(r.Context(), orgID, teamID); e != nil {
			return e
		}
		if e := bundle.TeamChannels.ReplaceForTeam(r.Context(), orgID, teamID, desired); e != nil {
			return e
		}
		// Default blueprint, first-track only: the prior set was empty, the
		// new set isn't, and the team hasn't already configured its own
		// slack:message trigger.
		if len(prior) == 0 && len(desired) > 0 {
			hasTrigger, e := teamHasSlackMessageTrigger(r.Context(), tx, orgID, teamID)
			if e != nil {
				return e
			}
			if !hasTrigger {
				if e := seedDefaultSlackMessageBlueprint(r.Context(), tx, orgID, teamID); e != nil {
					return e
				}
			}
		}
		return nil
	}); err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}

	added, removed := diffChannelIDs(prior, desired)

	admin := slackstore.FromStores(h.stores)
	touched := append(append([]string{}, added...), removed...)
	if len(touched) > 0 {
		if err := admin.TeamChannels.ReconcilePrimariesSystem(r.Context(), orgID, touched); err != nil {
			httpx.InternalError(w, "slack/channels", err)
			return
		}
	}

	var joinWarnings []channelsWarning
	if len(added) > 0 {
		joinWarnings = h.ensureAndAutoJoin(r.Context(), orgID, userID, added)
	}

	resp, err := h.buildChannelsResponse(r.Context(), orgID, userID, teamID, "admin")
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	resp.Warnings = append(resp.Warnings, joinWarnings...)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// primaryRequest is POST .../primary's body.
type primaryRequest struct {
	TeamID string `json:"team_id"`
}

// POST /api/slack/channels/{channel_id}/primary — org admin, primary
// reassignment. Plain intra-org errors: unlike the workspace-connect
// surface, this route may name teams freely (the ratified disclosure line
// for intra-org errors here).
func (h *channelsHandler) handlePrimary(w http.ResponseWriter, r *http.Request) {
	orgID, ok := slackEntitlementGate(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}

	channelID := r.PathValue("channel_id")
	if channelID == "" {
		httpx.NotFound(w, "channel")
		return
	}

	var req primaryRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.TeamID == "" {
		httpx.BadRequest(w, "team_id is required")
		return
	}
	teamID, err := h.az.ResolveTeamID(r.Context(), orgID, userID, req.TeamID)
	if err != nil {
		authz.WriteResolveError(w, "slack/channels", err)
		return
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	// An archived team is read-only-and-vanished (TFAC-448) — reassignment
	// must not make one the live routing owner of a channel, mirroring
	// handlePut's destination-team guard (VerifyTeamNotArchived above).
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}

	admin := slackstore.FromStores(h.stores)
	if err := admin.TeamChannels.SetPrimarySystem(r.Context(), orgID, channelID, teamID); err != nil {
		if errors.Is(err, slackstore.ErrTeamNotTrackingChannel) {
			httpx.BadRequest(w, "team does not track this channel")
			return
		}
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "primary set"})
}

// buildChannelsResponse reads the three sources (tracked, sighting, live
// Slack) and merges them by channel_id — precedence tracked > sighting >
// slack, applied by only filling a field the earlier source left unset.
func (h *channelsHandler) buildChannelsResponse(ctx context.Context, orgID, userID, teamID, role string) (*channelsResponse, error) {
	var (
		tracked    []slackstore.TeamChannel
		registry   []slackstore.Channel
		workspaces []slackstore.Workspace
	)
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		var e error
		if tracked, e = bundle.TeamChannels.ListForTeam(ctx, orgID, teamID); e != nil {
			return e
		}
		if registry, e = bundle.Channels.ListForOrg(ctx, orgID); e != nil {
			return e
		}
		workspaces, e = bundle.Workspaces.ListForOrg(ctx, orgID)
		return e
	}); err != nil {
		return nil, err
	}

	admin := slackstore.FromStores(h.stores)
	trackersByChannel, err := admin.TeamChannels.ListTrackersForOrgSystem(ctx, orgID)
	if err != nil {
		return nil, err
	}
	teamNames, err := h.teamNames(ctx, orgID, trackerTeamIDs(trackersByChannel))
	if err != nil {
		return nil, err
	}

	merged := map[string]*channelView{}
	var order []string
	get := func(id string) *channelView {
		if v, ok := merged[id]; ok {
			return v
		}
		v := &channelView{ChannelID: id, TrackedBy: []trackerView{}}
		merged[id] = v
		order = append(order, id)
		return v
	}

	for _, tc := range tracked {
		v := get(tc.ChannelID)
		v.Tracked = true
		v.IsPrimary = tc.IsPrimary
		v.Source = "tracked"
	}
	for _, c := range registry {
		v := get(c.ChannelID)
		if v.Source == "" {
			v.Source = "sighting"
		}
		if v.Name == "" {
			v.Name = c.Name
		}
		if v.WorkspaceID == "" {
			v.WorkspaceID = c.WorkspaceID
		}
		v.LastMentionAt = c.LastMentionAt
	}

	warnings := []channelsWarning{}
	candidates, anyFailed, anyTruncated := h.liveSlackCandidates(ctx, orgID, workspaces)
	if anyFailed {
		warnings = append(warnings, channelsWarning{Code: "slack_unreachable"})
	}
	if anyTruncated {
		warnings = append(warnings, channelsWarning{Code: "slack_channels_truncated"})
	}
	for _, sc := range candidates {
		v := get(sc.ChannelID)
		if v.Source == "" {
			v.Source = "slack"
		}
		if v.Name == "" {
			v.Name = sc.Name
		}
		if v.WorkspaceID == "" {
			v.WorkspaceID = sc.WorkspaceID
		}
		v.IsPrivate = sc.IsPrivate
		v.BotIsMember = sc.BotIsMember
	}

	for channelID, trackers := range trackersByChannel {
		v, ok := merged[channelID]
		if !ok {
			// Every tracked channel gets a registry row (Channels.EnsureSystem,
			// see ensureAndAutoJoin), so this shouldn't happen in practice —
			// skip defensively rather than fabricate a bare entry.
			continue
		}
		tb := make([]trackerView, 0, len(trackers))
		for _, tc := range trackers {
			tb = append(tb, trackerView{TeamID: tc.TeamID, TeamName: teamNames[tc.TeamID], IsPrimary: tc.IsPrimary})
		}
		v.TrackedBy = tb
	}

	sort.Strings(order)
	out := make([]channelView, 0, len(order))
	for _, id := range order {
		out = append(out, *merged[id])
	}
	return &channelsResponse{Role: role, Warnings: warnings, Channels: out}, nil
}

// teamNames resolves id->name for exactly the teams referenced in
// tracked_by (teamIDs) — name + role-in-channel only, never activity/
// config, and does NOT widen GET /api/teams (which deliberately returns
// only the caller's teams). Admin pool, cross-team by design (the ratified
// disclosure line for tracked_by). Deliberately narrower than an org-wide
// team enumeration: an org can have many teams that track no Slack channel
// at all, and this response only ever needs names for the ones that do.
func (h *channelsHandler) teamNames(ctx context.Context, orgID string, teamIDs []string) (map[string]string, error) {
	return h.stores.Teams.NamesForIDsSystem(ctx, orgID, teamIDs)
}

// trackerTeamIDs collects the distinct team IDs referenced across every
// channel's tracker list — the exact (and typically much smaller than
// org-wide) set teamNames needs to resolve.
func trackerTeamIDs(trackersByChannel map[string][]slackstore.TeamChannel) []string {
	seen := map[string]bool{}
	var ids []string
	for _, trackers := range trackersByChannel {
		for _, tc := range trackers {
			if seen[tc.TeamID] {
				continue
			}
			seen[tc.TeamID] = true
			ids = append(ids, tc.TeamID)
		}
	}
	return ids
}

// slackCandidate is one live Slack channel candidate resolved via
// conversations.list (the public universe) merged with users.conversations
// (the bot's own memberships, public + private).
type slackCandidate struct {
	ChannelID, WorkspaceID, Name string
	IsPrivate, BotIsMember       bool
}

// liveSlackCandidates fetches source (3) for every connected workspace.
// anyFailed=true means at least one workspace's live fetch failed (bad/
// missing bot token, or a Slack API error) — the caller surfaces a single
// "slack_unreachable" warning rather than 500ing the whole list; candidates
// from workspaces that DID succeed are still returned (partial degradation).
// anyTruncated=true means at least one workspace's enumeration hit
// slackConversationsListCap before Slack ran out of pages — the returned
// candidates for that workspace are a prefix, not the whole channel
// universe, so the caller must not treat "channel absent from candidates"
// as "channel doesn't exist" (it surfaces a warning instead).
func (h *channelsHandler) liveSlackCandidates(ctx context.Context, orgID string, workspaces []slackstore.Workspace) (candidates []slackCandidate, anyFailed, anyTruncated bool) {
	for _, ws := range workspaces {
		botToken, err := h.stores.Secrets.GetSystem(ctx, orgID, ws.BotTokenRef)
		if err != nil || botToken == "" {
			slackLog.Warn("channels: bot token unavailable for live candidates", "workspace", ws.WorkspaceID, "error", err)
			anyFailed = true
			continue
		}
		pub, pubTruncated, err := slackConversationsList(ctx, h.client, botToken)
		if err != nil {
			slackLog.Warn("channels: conversations.list failed", "workspace", ws.WorkspaceID, "error", err)
			anyFailed = true
			continue
		}
		mine, mineTruncated, err := slackUsersConversations(ctx, h.client, botToken)
		if err != nil {
			slackLog.Warn("channels: users.conversations failed", "workspace", ws.WorkspaceID, "error", err)
			anyFailed = true
			continue
		}
		if pubTruncated || mineTruncated {
			slackLog.Warn("channels: live candidate enumeration truncated", "workspace", ws.WorkspaceID, "cap", slackConversationsListCap)
			anyTruncated = true
		}
		member := make(map[string]bool, len(mine))
		for _, m := range mine {
			member[m.ID] = true
		}
		for _, c := range pub {
			candidates = append(candidates, slackCandidate{
				ChannelID: c.ID, WorkspaceID: ws.WorkspaceID, Name: c.Name,
				IsPrivate: false, BotIsMember: member[c.ID],
			})
		}
		for _, m := range mine {
			if !m.IsPrivate {
				continue // public memberships are already covered via conversations.list above
			}
			candidates = append(candidates, slackCandidate{
				ChannelID: m.ID, WorkspaceID: ws.WorkspaceID, Name: m.Name,
				IsPrivate: true, BotIsMember: true,
			})
		}
	}
	return candidates, anyFailed, anyTruncated
}

// ensureAndAutoJoin is PUT's step 4 (post-commit, admin pool + live Slack
// calls, never rolls back the tracking write): Channels.EnsureSystem for
// every added channel the registry has never seen, then best-effort
// conversations.join for added PUBLIC channels the bot isn't already a
// member of. Returns the join-related warnings — a tracked-but-botless
// channel is never silently reported as healthy.
func (h *channelsHandler) ensureAndAutoJoin(ctx context.Context, orgID, userID string, added []string) []channelsWarning {
	var workspaces []slackstore.Workspace
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		workspaces, e = slackstore.FromTx(tx).Workspaces.ListForOrg(ctx, orgID)
		return e
	}); err != nil {
		slackLog.Warn("channels: list workspaces for auto-join failed", "org_id", orgID, "error", err)
	}

	candidates, _, _ := h.liveSlackCandidates(ctx, orgID, workspaces)
	byID := make(map[string]slackCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ChannelID] = c
	}
	// Best-effort workspace_id fallback for EnsureSystem when a just-added
	// channel has no live-candidate match (e.g. it fell outside the
	// enumeration cap): an org with exactly one connected workspace has no
	// ambiguity, so use it; a multi-workspace org with no live match leaves
	// workspace_id blank (EnsureSystem's "refresh only non-empty" contract).
	soleWorkspaceID := ""
	if len(workspaces) == 1 {
		soleWorkspaceID = workspaces[0].WorkspaceID
	}

	admin := slackstore.FromStores(h.stores)
	var warnings []channelsWarning
	for _, channelID := range added {
		cand, known := byID[channelID]
		workspaceID, name := soleWorkspaceID, ""
		if known {
			workspaceID, name = cand.WorkspaceID, cand.Name
		}
		if err := admin.Channels.EnsureSystem(ctx, orgID, workspaceID, channelID, name); err != nil {
			slackLog.Warn("channels: ensure registry row failed", "org_id", orgID, "channel", channelID, "error", err)
		}

		if known && cand.BotIsMember {
			continue
		}
		// Either unknown to the live view (workspace unreachable, or it fell
		// outside the enumeration cap) or known-and-not-a-member: attempt the
		// join either way, and let Slack's own response tell us whether it's
		// a private channel (conversations.join only works on public
		// channels — see isSlackJoinPrivateChannelError) rather than
		// pre-deciding from a flag the live-candidate view can never
		// actually report as "private AND not a member" (users.conversations
		// only ever lists the bot's OWN current memberships).
		botToken, ok := h.botTokenForWorkspace(ctx, orgID, workspaces, workspaceID)
		if !ok {
			warnings = append(warnings, channelsWarning{ChannelID: channelID, Reason: "join_failed"})
			continue
		}
		err := slackConversationsJoin(ctx, h.client, botToken, channelID)
		if err == nil {
			continue
		}
		if isSlackJoinPrivateChannelError(err) {
			warnings = append(warnings, channelsWarning{ChannelID: channelID, Reason: "invite_required"})
			continue
		}
		slackLog.Warn("channels: auto-join failed", "org_id", orgID, "channel", channelID, "error", err)
		warnings = append(warnings, channelsWarning{ChannelID: channelID, Reason: "join_failed"})
	}
	return warnings
}

func (h *channelsHandler) botTokenForWorkspace(ctx context.Context, orgID string, workspaces []slackstore.Workspace, workspaceID string) (string, bool) {
	for _, ws := range workspaces {
		if ws.WorkspaceID != workspaceID {
			continue
		}
		token, err := h.stores.Secrets.GetSystem(ctx, orgID, ws.BotTokenRef)
		if err != nil || token == "" {
			return "", false
		}
		return token, true
	}
	return "", false
}

// normalizeChannelIDs drops blanks and de-duplicates, preserving first-seen
// order — mirrors the pg TeamChannelStore's own ReplaceForTeam normalization
// so the "first-track" empty→non-empty check (handlePut) and the added/
// removed diff (diffChannelIDs) agree with what actually got persisted.
func normalizeChannelIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// diffChannelIDs computes added/removed against the prior tracked set —
// used both for ReconcilePrimariesSystem's touched-channel set and for the
// auto-join pass (added only).
func diffChannelIDs(prior []slackstore.TeamChannel, desired []string) (added, removed []string) {
	priorSet := make(map[string]bool, len(prior))
	for _, p := range prior {
		priorSet[p.ChannelID] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, id := range desired {
		desiredSet[id] = true
		if !priorSet[id] {
			added = append(added, id)
		}
	}
	for _, p := range prior {
		if !desiredSet[p.ChannelID] {
			removed = append(removed, p.ChannelID)
		}
	}
	return added, removed
}

// teamHasSlackMessageTrigger reports whether teamID already has a
// slack:message trigger (any source — system or user). This replaces slug
// idempotency for the default-blueprint seed: a team that configured its
// own trigger is respected (no default forced beside it), and a team that
// deleted the default doesn't get it resurrected on the next PUT.
func teamHasSlackMessageTrigger(ctx context.Context, tx db.TxStores, orgID, teamID string) (bool, error) {
	// Unwindowed (ListOpts zero Limit): this derives a per-channel set from
	// the team's whole trigger list rather than browsing it, so a page would
	// silently narrow what the channel is reported as tracking.
	handlers, _, err := tx.EventHandlers.List(ctx, orgID,
		db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger, TeamID: teamID}, db.Unwindowed)
	if err != nil {
		return false, err
	}
	for _, eh := range handlers {
		if eh.EventType == domain.EventSlackMessage {
			return true, nil
		}
	}
	return false, nil
}

// slackMessagePromptName/Body are the shipped-content for the default
// slack:message blueprint seeded on a team's first tracked channel. The
// message's channel/thread_ts/sender/text reach the conversation through the task
// context's raw event metadata — a task itself carries only its title — so the
// body points there rather than interpolating anything. Deliberately doesn't
// hinge on whether the triggering message was an explicit @-mention or an
// engaged-thread follow-up — the agent's job is the same either way, and
// mention-ness is metadata, not a different situation (see
// domain.EventSlackMessage's doc). Greenfield: pre-release, existing
// seeded rows are dogfood-only and are not migrated.
const (
	slackMessagePromptName = "Slack assistant"
	slackMessagePromptBody = "A teammate reached out to the agent in a Slack thread. The task's entity is the " +
		"thread. Parse the channel, thread_ts, sender, and message text out of the raw event metadata in " +
		"the task context above.\n\n" +
		"Read the thread (`triagefactory exec slack read thread --channel <channel> --ts <thread_ts>`, or `--ts <ts>` when " +
		"thread_ts is empty — the triggering message itself started the thread) to see the full request in " +
		"context, gather what you need from the linked entity and any referenced repos/issues, do the work using " +
		"the triagefactory exec subcommands available to you, and reply in that same thread with " +
		"`triagefactory exec slack send` — your stdout is not visible to the user, so the send is the only way they see your " +
		"answer."
)

// slackMessageTriggerBreakerThreshold / MinAutonomySuitability match the
// shipped system triggers' defaults (see db.ShippedEventHandlers).
const (
	slackMessageTriggerBreakerThreshold = 3
	slackMessageTriggerMinAutonomy      = 0.0
)

// seedDefaultSlackMessageBlueprint creates, as user-source rows attributed
// to the tracking admin (creator_user_id = them), a 1-step blueprint
// wrapping a fresh prompt and a slack:message trigger scoped to any channel
// (predicate {"channel_in":[]}). Called inside the same tx as the tracking
// write (handlePut) — see that call site's doc for why this is the
// app-pool Create path rather than the admin-pool seeders.
func seedDefaultSlackMessageBlueprint(ctx context.Context, tx db.TxStores, orgID, teamID string) error {
	promptID := uuid.New().String()
	if _, err := tx.Prompts.Create(ctx, orgID, teamID, domain.Prompt{
		ID:     promptID,
		Name:   slackMessagePromptName,
		Body:   slackMessagePromptBody,
		Source: "user",
	}); err != nil {
		return fmt.Errorf("seed slack message prompt: %w", err)
	}

	blueprintID := uuid.New().String()
	if _, err := tx.Blueprints.Create(ctx, orgID, teamID, domain.Blueprint{
		ID:     blueprintID,
		Name:   slackMessagePromptName,
		Source: "user",
	}); err != nil {
		return fmt.Errorf("seed slack message blueprint: %w", err)
	}
	if _, err := tx.Blueprints.ReplaceSteps(ctx, orgID, blueprintID, []string{promptID}, []string{""}); err != nil {
		return fmt.Errorf("seed slack message blueprint steps: %w", err)
	}

	predicate := `{"channel_in":[]}`
	threshold := slackMessageTriggerBreakerThreshold
	minAutonomy := slackMessageTriggerMinAutonomy
	if _, err := tx.EventHandlers.Create(ctx, orgID, teamID, domain.EventHandler{
		ID:                     uuid.New().String(),
		Kind:                   domain.EventHandlerKindTrigger,
		EventType:              domain.EventSlackMessage,
		ScopePredicateJSON:     &predicate,
		Enabled:                true,
		Source:                 domain.EventHandlerSourceUser,
		AppliesToUnowned:       false,
		BlueprintID:            blueprintID,
		TriggerType:            domain.TriggerTypeEvent,
		BreakerThreshold:       &threshold,
		MinAutonomySuitability: &minAutonomy,
	}); err != nil {
		return fmt.Errorf("seed slack message trigger: %w", err)
	}
	return nil
}
