// Slack channel tracking API (TFAC-543): a team lists channels it's
// tracking (union sightings + live Slack candidates), reads one, tracks/
// untracks a full set, and takes a channel's primary. The primary lifecycle
// (first-tracker default, oldest-tracker succession) is handled server-side
// by TeamChannelStore.ReconcilePrimariesSystem; a team's first tracked
// channel seeds its default slack:message blueprint so mentions in a newly
// tracked channel have somewhere to route. The routes are the team's
// resource, under /api/teams/{team_id}/slack-channels.
package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/ttlcache"
)

// refuseInLocalMode answers 404 in local mode, where every Slack route is
// absent: the Slack store is Postgres-only (see workspacesHandler's doc). It
// reports whether it answered.
func refuseInLocalMode(w http.ResponseWriter) bool {
	if runmode.Current() != runmode.ModeLocal {
		return false
	}
	httpx.NotFound(w, "route")
	return true
}

// refuseUnentitled answers 404 for an org without the `slack` entitlement, to
// which every Slack route is absent. It reports whether it answered.
func refuseUnentitled(w http.ResponseWriter, orgID string) bool {
	if entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		return false
	}
	httpx.NotFound(w, "route")
	return true
}

// slackEntitlementGate is the gate every team route runs before its team
// checks: local mode 404s, the org is the credential's, as on every core
// /api/teams/{team_id} route, and that org must hold the `slack` entitlement.
func slackEntitlementGate(w http.ResponseWriter, r *http.Request) (orgID string, ok bool) {
	if refuseInLocalMode(w) {
		return "", false
	}
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", false
	}
	if refuseUnentitled(w, orgID) {
		return "", false
	}
	return orgID, true
}

// channelsHandler serves /api/teams/{team_id}/slack-channels (PUT), its /list
// and /facets, and /api/teams/{team_id}/slack-channels/{channel_id} (GET,
// PATCH) — the channel tracking surface a team's settings page builds against.
// Needs both the transactional store runner (RLS-gated team/registry reads +
// the tracking write) and the non-tx admin-pool aggregate (cross-team
// trackers, secrets, post-commit reconcile/ensure/auto-join).
type channelsHandler struct {
	tx     db.TxRunner
	stores db.Stores
	az     *authz.Checker
	client *http.Client
	// candidates holds each org's last live enumeration of its workspaces'
	// channels, keyed by org id, so the channel list's pages, searches and
	// facets read Slack once per slackCandidatesTTL rather than once per
	// request. PUT's gate reads Slack live and drops the org's entry.
	candidates *ttlcache.Cache[slackCandidateSet]
}

// slackCandidatesTTL is how long one enumeration answers the channel list. A
// channel created or renamed in Slack shows up within it; conversations.list
// allows a workspace about twenty calls a minute, which a reader paging and
// searching would otherwise spend in seconds.
const slackCandidatesTTL = time.Minute

// slackCandidateSet is one org's live enumeration, as liveSlackCandidates
// answered it, and the connections it enumerated through.
type slackCandidateSet struct {
	connections  string
	candidates   []slackCandidate
	anyFailed    bool
	anyTruncated bool
}

// slackConnectionsKey names a set of connections, so an enumeration taken
// before one was connected, disconnected or switched to is not served for the
// new set: a different app's bot can see different channels.
func slackConnectionsKey(conns []slackstore.Connection) string {
	ids := make([]string, len(conns))
	for i, c := range conns {
		ids[i] = c.WorkspaceID + ":" + c.APIAppID
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

// cachedSlackCandidates is liveSlackCandidates through the org's cache entry.
// An enumeration in which a workspace failed is answered but not kept, so the
// next read asks Slack again rather than serving the gap for a minute.
func (h *channelsHandler) cachedSlackCandidates(ctx context.Context, orgID string, workspaces []slackstore.Connection) ([]slackCandidate, bool, bool) {
	key := slackConnectionsKey(workspaces)
	load := func(ctx context.Context) (slackCandidateSet, bool, error) {
		candidates, failed, truncated := h.liveSlackCandidates(ctx, orgID, workspaces)
		return slackCandidateSet{connections: key, candidates: candidates, anyFailed: failed, anyTruncated: truncated}, !failed, nil
	}
	// load never errs: a failed workspace is reported in the set.
	set, _ := h.candidates.Get(ctx, orgID, load)
	if set.connections != key {
		h.candidates.Forget(orgID)
		set, _ = h.candidates.Get(ctx, orgID, load)
	}
	return set.candidates, set.anyFailed, set.anyTruncated
}

// channelsWarning is one entry in a channels response's Warnings — a tagged
// union serialized as a single JSON shape rather than two response fields,
// since the list and PUT answer warnings in one array: reading the merged set
// only ever populates Code (a process-level degradation, e.g.
// "slack_unreachable"); PUT's auto-join step only ever populates ChannelID
// + Reason (a per-channel outcome, "invite_required" | "join_failed").
type channelsWarning struct {
	Code      string `json:"code,omitempty" doc:"What is wrong with the whole read: slack_unreachable when a workspace's channels could not be read from Slack, or slack_channels_truncated when a workspace has more channels than one read of Slack collects; absent on a warning about one channel."`
	ChannelID string `json:"channel_id,omitempty" doc:"Slack's id for the channel a warning about one channel concerns, absent on a warning about the whole read."`
	Reason    string `json:"reason,omitempty" doc:"Why the bot did not join the channel: invite_required when the channel is private and someone must invite the bot in Slack, or join_failed; absent on a warning about the whole read."`
}

// trackerView is one team's tracking claim on a channel, as disclosed on
// tracked_by — name + role-in-channel only, never activity/config (the
// ratified cross-team disclosure line — see TeamChannelStore.
// ListTrackersForOrgSystem's doc).
type trackerView struct {
	TeamID    string `json:"team_id" doc:"The team's id."`
	TeamName  string `json:"team_name" doc:"The team's name."`
	IsPrimary bool   `json:"is_primary" doc:"Whether the team is the channel's primary, the team the channel's mentions are routed to."`
}

// channelView is one merged row of the list's items and of PUT's channels
// array — the union of a team's tracked rows, the org's sighting registry,
// and live Slack candidates, deduped by channel_id. ID is the channel's
// registry row, what a slack:message handler's channel_in names; it is empty
// for a live Slack channel TF holds no row for yet.
type channelView struct {
	ID            string        `json:"id,omitempty" doc:"The channel's registry id, which an event handler's channel filter names; absent for a channel Slack lists that Triage Factory holds no row for yet."`
	ChannelID     string        `json:"channel_id" doc:"Slack's id for the channel."`
	Name          string        `json:"name" doc:"The channel's name, empty when it is not known."`
	WorkspaceID   string        `json:"workspace_id" doc:"The id of the Slack workspace the channel is in."`
	IsPrivate     bool          `json:"is_private" doc:"Whether the channel is private, false when the read of Slack did not list it."`
	BotIsMember   bool          `json:"bot_is_member" doc:"Whether the app's bot is in the channel, false when the read of Slack did not list it."`
	Tracked       bool          `json:"tracked" doc:"Whether the team tracks the channel."`
	IsPrimary     bool          `json:"is_primary" doc:"Whether the team is the channel's primary, the team the channel's mentions are routed to."`
	TrackedBy     []trackerView `json:"tracked_by" doc:"Every team that tracks the channel."`
	LastMentionAt *time.Time    `json:"last_mention_at,omitempty" doc:"When the app last saw a mention in the channel, as an RFC 3339 timestamp, absent when it has seen none."`
	Source        string        `json:"source" doc:"Where the row comes from: tracked when the team tracks the channel, sighting when the team does not track it but Triage Factory holds a registry row for it, such as one a mention the app saw or another team tracking it made, or slack when only the read of Slack lists it."` // "tracked" | "sighting" | "slack"
}

// channelsResponse is PUT's answer: the team's whole merged set after the
// save, with the auto-join outcomes appended to the warnings.
type channelsResponse struct {
	Role     string            `json:"role" doc:"The caller's role on the team, which is admin."`
	Warnings []channelsWarning `json:"warnings" doc:"What is wrong with the read of Slack, and each added channel the bot did not join."`
	Channels []channelView     `json:"channels" doc:"Every channel of the team's list after the change, in Slack channel id order."`
}

// memberTeam runs the gates every read of a team's channels runs: the Slack
// gate, the team resolved in the caller's org, and membership. A non-member of
// an in-org team gets a 404, not an empty answer — the channel roster is
// member-visible, not org-visible (the same non-disclosure posture as
// VerifyTeamInOrg). role is the caller's role on the team.
func (h *channelsHandler) memberTeam(w http.ResponseWriter, r *http.Request) (orgID, userID, teamID, role string, ok bool) {
	orgID, ok = slackEntitlementGate(w, r)
	if !ok {
		return "", "", "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject
	teamID, err := h.az.ResolveTeamID(r.Context(), orgID, userID, r.PathValue("team_id"))
	if err != nil {
		authz.WriteResolveError(w, "slack/channels", err)
		return "", "", "", "", false
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return "", "", "", "", false
	}
	_, role, err = h.az.TeamMemberCountAndRole(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return "", "", "", "", false
	}
	if role == "" {
		httpx.NotFound(w, "team")
		return "", "", "", "", false
	}
	return orgID, userID, teamID, role, true
}

// putChannelsRequest is PUT's full-set replace body.
type putChannelsRequest struct {
	ChannelIDs []string `json:"channel_ids" doc:"Slack's ids for every channel the team is to track; the set replaces the one the team tracks, and an empty array stops it tracking any. Required."`
}

var opPut = httpx.NewJSONOp[putChannelsRequest, channelsResponse](httpx.OpSpec{
	Pattern:     "PUT /api/teams/{team_id}/slack-channels",
	ID:          "replace_team_slack_channels",
	Summary:     "Set the Slack channels a team tracks",
	Description: "Replaces the set of Slack channels the team tracks, each of which the organization's Slack connections must be able to see, joins the app's bot to each added public channel it is not in, and returns every channel of the team's list. A team's first tracked channel also gives it a default handler for Slack mentions when it has none." + slackAccess,
	PathParams:  map[string]string{"team_id": "The team's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// PUT /api/teams/{team_id}/slack-channels — team admin, full-set replace.
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

	req, ok := opPut.Decode(w, r)
	if !ok {
		return
	}
	// Validate the shape of the whole set before any store or Slack work,
	// accumulating every bad entry. Two ids naming one channel (one Slack has
	// since replaced, and the one it has now) are both well-formed and track
	// one row.
	var v httpx.Validation
	if req.ChannelIDs == nil {
		v.Missing("channel_ids")
	}
	seen := make(map[string]bool, len(req.ChannelIDs))
	for i, id := range req.ChannelIDs {
		field := fmt.Sprintf("channel_ids[%d]", i)
		switch {
		case id == "":
			v.Missing(field)
		case !slackChannelIDRe.MatchString(id):
			v.Invalid(field, "invalid Slack channel id "+id)
		case seen[id]:
			v.Invalid(field, "duplicate channel id "+id)
		default:
			seen[id] = true
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	gate, ok := h.gateChannels(w, r, orgID, userID, teamID, req.ChannelIDs)
	if !ok {
		return
	}
	// Tracking holds the channel's registry row, so every channel the save
	// adds gets one first, under the workspace the gate found it in.
	registry := slackstore.FromStores(h.stores).Channels
	for _, c := range gate.adding {
		if _, err := registry.EnsureSystem(r.Context(), orgID, c.workspaceID, c.channelID, c.name); err != nil {
			httpx.InternalError(w, "slack/channels", fmt.Errorf("ensure slack channel %s: %w", c.channelID, err))
			return
		}
	}

	// The tracking write takes the Slack ids, not the rows registered above:
	// a move can absorb one of those rows before this transaction, and the
	// write resolves each id under the move lock.
	var prior []slackstore.TeamChannel
	var tracked []string
	// slackIDs maps each tracked registry row to its Slack channel id, as the
	// write left them, so a channel the auto-join cannot read back is still
	// named by the id the caller knows it by.
	slackIDs := map[string]string{}
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		var e error
		if prior, e = bundle.TeamChannels.ListForTeam(r.Context(), orgID, teamID); e != nil {
			return e
		}
		if tracked, e = bundle.TeamChannels.ReplaceForTeam(r.Context(), orgID, teamID, req.ChannelIDs); e != nil {
			return e
		}
		after, e := bundle.TeamChannels.ListForTeam(r.Context(), orgID, teamID)
		if e != nil {
			return e
		}
		for _, tc := range after {
			slackIDs[tc.SlackChannelID] = tc.ChannelID
		}
		// Default blueprint, first-track only: the prior set was empty, the
		// new set isn't, and the team hasn't already configured its own
		// slack:message handler.
		if len(prior) == 0 && len(tracked) > 0 {
			hasHandler, e := teamHasSlackMessageHandler(r.Context(), tx, orgID, teamID)
			if e != nil {
				return e
			}
			if !hasHandler {
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

	added, removed := diffChannelIDs(prior, tracked)

	touched := append(append([]string{}, added...), removed...)
	if len(touched) > 0 {
		if err := slackstore.FromStores(h.stores).TeamChannels.ReconcilePrimariesSystem(r.Context(), orgID, touched); err != nil {
			httpx.InternalError(w, "slack/channels", err)
			return
		}
	}

	// Past the commit, a failure only costs the channel its auto-join, which
	// is reported as a warning like any other join failure.
	var joinWarnings []channelsWarning
	if len(added) > 0 {
		addedChannels := make([]slackstore.Channel, 0, len(added))
		for _, id := range added {
			ch, err := registry.GetSystem(r.Context(), orgID, id)
			if err != nil || ch == nil {
				slackLog.Warn("channels: read added channel for auto-join failed", "org_id", orgID, "channel_row", id, "error", err)
				joinWarnings = append(joinWarnings, channelsWarning{ChannelID: slackIDs[id], Reason: "join_failed"})
				continue
			}
			addedChannels = append(addedChannels, *ch)
		}
		joinWarnings = append(joinWarnings, h.autoJoin(r.Context(), orgID, gate.workspaces, gate.candidates, addedChannels)...)
	}

	// The auto-join changes which channels the bot is in, so the answer, and
	// the reads after it, enumerate Slack again.
	h.candidates.Forget(orgID)
	resp, err := h.buildChannelsResponse(r.Context(), orgID, userID, teamID, "admin")
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	resp.Warnings = append(resp.Warnings, joinWarnings...)
	opPut.Write(w, *resp)
}

// channelIDFromPath reads {channel_id}, Slack's id for a channel, and answers
// 404 for one that is not the id of a channel a team can track.
func channelIDFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	channelID := r.PathValue("channel_id")
	if !slackChannelIDRe.MatchString(channelID) {
		httpx.NotFound(w, "channel")
		return "", false
	}
	return channelID, true
}

var opGetChannel = httpx.NewJSONOp[httpx.NoBody, channelView](httpx.OpSpec{
	Pattern:     "GET /api/teams/{team_id}/slack-channels/{channel_id}",
	ID:          "get_team_slack_channel",
	Summary:     "Get a channel of a team's Slack channel list",
	Description: "Returns one channel of the team's Slack channel list, as the list would return it, reading Slack's channels to build it." + slackAccess,
	PathParams: map[string]string{
		"team_id":    "The team's id.",
		"channel_id": "Slack's id for the channel. An id Slack has since replaced reads the channel under its current id.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleGet reads one channel of the team's list: the row the list would
// answer for it, or 404 when the list holds none, which is a channel the team
// could not watch. An id Slack has since replaced reads the channel under its
// current id. Authorization is the list's.
//
// GET /api/teams/{team_id}/slack-channels/{channel_id}
func (h *channelsHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, _, ok := h.memberTeam(w, r)
	if !ok {
		return
	}
	channelID, ok := channelIDFromPath(w, r)
	if !ok {
		return
	}
	view, err := h.findChannel(r.Context(), orgID, userID, teamID, channelID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	if view == nil {
		httpx.NotFound(w, "channel")
		return
	}
	opGetChannel.Write(w, *view)
}

// patchChannelRequest is PATCH's body. IsPrimary keeps its raw bytes so that
// an absent field and an explicit null read differently.
type patchChannelRequest struct {
	IsPrimary httpx.Patch[bool] `json:"is_primary" doc:"true makes the team the channel's primary, the team its mentions are routed to; false and null are refused, since a watched channel always has a primary and it moves by setting it on another team. Absent writes nothing."`
}

var opPatchChannel = httpx.NewJSONOp[patchChannelRequest, channelView](httpx.OpSpec{
	Pattern:     "PATCH /api/teams/{team_id}/slack-channels/{channel_id}",
	ID:          "update_team_slack_channel",
	Summary:     "Update a team's tracking of a Slack channel",
	Description: "Makes the team the primary of a channel it tracks, so the channel's mentions are routed to it, and returns the channel as the team's list shows it; the team must not be archived. An empty body writes nothing and returns the channel." + slackAccess,
	PathParams: map[string]string{
		"team_id":    "The team's id.",
		"channel_id": "Slack's id for the channel. An id Slack has since replaced reads the channel under its current id.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handlePatch writes the team's tracking row of a channel. Its one field,
// is_primary, takes only true, which makes the team the channel's primary: a
// watched channel always has a primary, so a team stops being it when another
// team takes it, and false or null is refused. An absent field writes nothing.
// The answer is the channel's row of the team's list.
//
// Only an org admin may write it, because which team is primary decides which
// team the channel's events route to. A team that does not watch the channel
// answers 404, and an archived team is refused: it may not become a channel's
// routing owner.
//
// PATCH /api/teams/{team_id}/slack-channels/{channel_id}
func (h *channelsHandler) handlePatch(w http.ResponseWriter, r *http.Request) {
	orgID, ok := slackEntitlementGate(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject
	teamID, ok := h.az.TeamIDFromPath(w, r, "slack/channels", orgID, userID)
	if !ok {
		return
	}
	if !h.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}
	channelID, ok := channelIDFromPath(w, r)
	if !ok {
		return
	}

	req, ok := opPatchChannel.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	isPrimary, state := req.IsPrimary.ReadNonNull(&v, "is_primary")
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	if state == httpx.PatchClear || (state == httpx.PatchSet && !isPrimary) {
		v.Invalid("is_primary", "is_primary can only be set to true: a watched channel always has a primary, so it moves by setting it on another team")
		v.Flush(w, http.StatusUnprocessableEntity)
		return
	}
	if !h.az.VerifyTeamNotArchived(w, r, orgID, userID, teamID) {
		return
	}

	admin := slackstore.FromStores(h.stores)
	channel, err := admin.Channels.ResolveSystem(r.Context(), orgID, channelID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	tracks := false
	if channel != nil {
		if tracks, err = admin.TeamChannels.TracksChannelSystem(r.Context(), orgID, teamID, channel.ID); err != nil {
			httpx.InternalError(w, "slack/channels", err)
			return
		}
	}
	if !tracks {
		httpx.NotFound(w, "channel")
		return
	}
	if state == httpx.PatchSet {
		if err := admin.TeamChannels.SetPrimarySystem(r.Context(), orgID, channel.ID, teamID); err != nil {
			// The team stopped watching the channel since the read above.
			if errors.Is(err, slackstore.ErrTeamNotTrackingChannel) {
				httpx.NotFound(w, "channel")
				return
			}
			httpx.InternalError(w, "slack/channels", err)
			return
		}
	}

	view, err := h.findChannel(r.Context(), orgID, userID, teamID, channel.ChannelID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	if view == nil {
		httpx.NotFound(w, "channel")
		return
	}
	opPatchChannel.Write(w, *view)
}

// findChannel returns channelID's row of the team's list, or nil when the list
// holds none. A channel TF holds a row for is found under its current Slack id,
// whichever id names it.
func (h *channelsHandler) findChannel(ctx context.Context, orgID, userID, teamID, channelID string) (*channelView, error) {
	row, err := slackstore.FromStores(h.stores).Channels.ResolveSystem(ctx, orgID, channelID)
	if err != nil {
		return nil, err
	}
	if row != nil {
		channelID = row.ChannelID
	}
	channels, _, err := h.mergedChannels(ctx, orgID, userID, teamID)
	if err != nil {
		return nil, err
	}
	for i := range channels {
		if channels[i].ChannelID == channelID {
			return &channels[i], nil
		}
	}
	return nil, nil
}

// buildChannelsResponse is PUT's answer: the whole merged set, in channel id
// order.
func (h *channelsHandler) buildChannelsResponse(ctx context.Context, orgID, userID, teamID, role string) (*channelsResponse, error) {
	channels, warnings, err := h.mergedChannels(ctx, orgID, userID, teamID)
	if err != nil {
		return nil, err
	}
	return &channelsResponse{Role: role, Warnings: warnings, Channels: channels}, nil
}

// mergedChannels reads the three sources (tracked, sighting, live Slack) and
// merges them by channel_id — precedence tracked > sighting > slack, applied
// by only filling a field the earlier source left unset. The rows come back in
// channel id order, with the process-level warnings the live read raised.
func (h *channelsHandler) mergedChannels(ctx context.Context, orgID, userID, teamID string) ([]channelView, []channelsWarning, error) {
	var (
		registry   []slackstore.Channel
		workspaces []slackstore.Connection
	)
	if err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		var e error
		if registry, e = bundle.Channels.ListForOrg(ctx, orgID); e != nil {
			return e
		}
		conns, _, e := bundle.Connections.ListForOrg(ctx, orgID, db.Unwindowed)
		if e != nil {
			return e
		}
		workspaces = activeConnections(conns)
		return nil
	}); err != nil {
		return nil, nil, err
	}

	admin := slackstore.FromStores(h.stores)
	trackersByChannel, err := admin.TeamChannels.ListTrackersForOrgSystem(ctx, orgID)
	if err != nil {
		return nil, nil, err
	}
	teamNames, err := h.teamNames(ctx, orgID, trackerTeamIDs(trackersByChannel))
	if err != nil {
		return nil, nil, err
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

	// The team's own rows come from the org-wide tracker read rather than one
	// under the caller's claims: the tracking rows' RLS admits only the team's
	// members, and an org admin who sets a team's primary need not be one.
	// Stored team ids are canonical, and the path's need not be.
	if parsed, err := uuid.Parse(teamID); err == nil {
		teamID = parsed.String()
	}
	for _, trackers := range trackersByChannel {
		for _, tc := range trackers {
			if tc.TeamID != teamID {
				continue
			}
			v := get(tc.ChannelID)
			v.ID = tc.SlackChannelID
			v.Tracked = true
			v.IsPrimary = tc.IsPrimary
			v.Source = "tracked"
		}
	}
	for _, c := range registry {
		v := get(c.ChannelID)
		v.ID = c.ID
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
	candidates, anyFailed, anyTruncated := h.cachedSlackCandidates(ctx, orgID, workspaces)
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

	for _, trackers := range trackersByChannel {
		// Every tracker of one channel carries the channel's Slack id, and the
		// registry read above lists every tracked channel's row, so this
		// misses only on a row added between the two reads — skipped rather
		// than fabricated as a bare entry.
		v, ok := merged[trackers[0].ChannelID]
		if !ok {
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
	return out, warnings, nil
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
func (h *channelsHandler) liveSlackCandidates(ctx context.Context, orgID string, workspaces []slackstore.Connection) (candidates []slackCandidate, anyFailed, anyTruncated bool) {
	for _, ws := range workspaces {
		botToken, err := h.stores.Secrets.GetSystem(ctx, orgID, ws.BotTokenRef)
		if err != nil || botToken == "" {
			slackLog.Warn("channels: bot token unavailable for live candidates", "workspace", ws.WorkspaceID, "error", err)
			anyFailed = true
			continue
		}
		caller := slackBotCaller(orgID, ws.WorkspaceID, ws.APIAppID)
		pub, pubTruncated, err := slackConversationsList(ctx, h.client, caller, botToken)
		if err != nil {
			slackLog.Warn("channels: conversations.list failed", "workspace", ws.WorkspaceID, "error", err)
			anyFailed = true
			continue
		}
		mine, mineTruncated, err := slackUsersConversations(ctx, h.client, caller, botToken)
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

// orgWorkspaces lists the org's active connections, one per workspace that
// has one, for PUT's gate and auto-join: the bot that would act in a channel
// is the one that answers whether it can see it.
func (h *channelsHandler) orgWorkspaces(ctx context.Context, orgID, userID string) ([]slackstore.Connection, error) {
	var conns []slackstore.Connection
	err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		conns, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgID, db.Unwindowed)
		return e
	})
	return activeConnections(conns), err
}

// slackChannelIDRe is the shape of a channel id a team can track: a public
// (C…) or private (G…) conversation. A direct message (D…) is never tracked.
var slackChannelIDRe = regexp.MustCompile(`^[CG][0-9A-Z]{1,31}$`)

// channelGate is what PUT's gate learned about a save: each channel it adds,
// with the workspace and name to register it under, and the org's workspaces
// and live candidates, which the auto-join reads.
type channelGate struct {
	adding     []channelWish
	workspaces []slackstore.Connection
	candidates []slackCandidate
}

// channelWish is one channel a save adds, as the gate attributed it.
type channelWish struct {
	channelID, workspaceID, name string
}

// channelLookupLimit bounds the channels one save may add that the gate has
// to ask Slack about one by one (conversations.info, a rate-limited call), when
// a listing stopped at slackConversationsListCap before reaching them.
const channelLookupLimit = 20

// notVisibleMessage is the gate's refusal of a channel no connected workspace
// answers for.
func notVisibleMessage(channelID string) string {
	return "channel " + channelID + " is not one the Slack app can see in a connected workspace; invite the app to a private channel before tracking it"
}

// gateChannels checks a save against what the settings page offers, which is
// every channel the list route lists. A channel the team already tracks is
// kept without asking Slack, so an outage, or a channel the app has since
// lost, never locks a team out of saving the rest of its set (removals
// included). A channel the save adds must be one a connected workspace
// answers for: a live candidate there, or a channel the registry holds under
// a connected workspace, which Slack delivered a mention from. Anything else
// is the caller's mistake: a 400 naming each entry. A listing that stopped at its
// cap says nothing about the channels past it, so those are asked about one by
// one (lookUpChannel). A workspace whose listing failed establishes nothing,
// so a channel it might hold is a 502 rather than a row registered on a guess.
func (h *channelsHandler) gateChannels(w http.ResponseWriter, r *http.Request, orgID, userID, teamID string, channelIDs []string) (channelGate, bool) {
	ctx := r.Context()
	var prior []slackstore.TeamChannel
	if err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		prior, e = slackstore.FromTx(tx).TeamChannels.ListForTeam(ctx, orgID, teamID)
		return e
	}); err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return channelGate{}, false
	}
	alreadyTracked := make(map[string]bool, len(prior))
	for _, tc := range prior {
		alreadyTracked[tc.SlackChannelID] = true
	}

	type unchecked struct {
		index     int
		channelID string
		row       *slackstore.Channel
	}
	registry := slackstore.FromStores(h.stores).Channels
	var adding []unchecked
	for i, channelID := range channelIDs {
		row, err := registry.ResolveSystem(ctx, orgID, channelID)
		if err != nil {
			httpx.InternalError(w, "slack/channels", err)
			return channelGate{}, false
		}
		if row != nil && alreadyTracked[row.ID] {
			continue
		}
		adding = append(adding, unchecked{index: i, channelID: channelID, row: row})
	}
	var gate channelGate
	if len(adding) == 0 {
		return gate, true
	}

	workspaces, err := h.orgWorkspaces(ctx, orgID, userID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return channelGate{}, false
	}
	candidates, anyFailed, anyTruncated := h.liveSlackCandidates(ctx, orgID, workspaces)
	gate.workspaces, gate.candidates = workspaces, candidates
	connected := make(map[string]bool, len(workspaces))
	for _, ws := range workspaces {
		connected[ws.WorkspaceID] = true
	}
	byID := make(map[string]slackCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ChannelID] = c
	}

	var v httpx.Validation
	var unverified []string
	var unlisted []int // indexes into adding of the channels to ask Slack about
	for i, a := range adding {
		cand, listed := byID[a.channelID]
		if !listed && a.row != nil {
			cand, listed = byID[a.row.ChannelID]
		}
		switch {
		case listed:
			gate.adding = append(gate.adding, channelWish{channelID: a.channelID, workspaceID: cand.WorkspaceID, name: cand.Name})
		case a.row != nil && connected[a.row.WorkspaceID]:
			gate.adding = append(gate.adding, channelWish{channelID: a.channelID, workspaceID: a.row.WorkspaceID})
		case anyFailed:
			unverified = append(unverified, a.channelID)
		case anyTruncated:
			unlisted = append(unlisted, i)
		default:
			v.Invalid(fmt.Sprintf("channel_ids[%d]", a.index), notVisibleMessage(a.channelID))
		}
	}
	if len(unlisted) > channelLookupLimit {
		v.OutOfRange("channel_ids", fmt.Sprintf(
			"%d channels are past the end of a workspace's channel listing; at most %d of those can be added in one save",
			len(unlisted), channelLookupLimit))
	}
	if v.Flush(w, http.StatusBadRequest) {
		return channelGate{}, false
	}
	for _, i := range unlisted {
		a := adding[i]
		wish, found, err := h.lookUpChannel(ctx, orgID, workspaces, a.channelID)
		switch {
		case err != nil:
			slackLog.Warn("channels: conversations.info failed", "org_id", orgID, "channel", a.channelID, "error", err)
			unverified = append(unverified, a.channelID)
		case found:
			gate.adding = append(gate.adding, wish)
		default:
			v.Invalid(fmt.Sprintf("channel_ids[%d]", a.index), notVisibleMessage(a.channelID))
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return channelGate{}, false
	}
	if len(unverified) > 0 {
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
			Reason:  httpx.ReasonUpstreamUnavailable,
			Message: "Slack could not be reached to verify " + strings.Join(unverified, ", ") + "; nothing was saved",
		})
		return channelGate{}, false
	}
	return gate, true
}

// lookUpChannel asks each connected workspace for channelID directly, for a
// channel a truncated listing may have stopped short of. found=false with a
// nil error means every workspace answered that the bot cannot see it.
func (h *channelsHandler) lookUpChannel(ctx context.Context, orgID string, workspaces []slackstore.Connection, channelID string) (channelWish, bool, error) {
	for _, ws := range workspaces {
		caller, botToken, ok := h.botTokenForWorkspace(ctx, orgID, workspaces, ws.WorkspaceID)
		if !ok {
			return channelWish{}, false, fmt.Errorf("bot token unavailable for workspace %s", ws.WorkspaceID)
		}
		info, err := slackConversationsInfo(ctx, h.client, caller, botToken, channelID)
		if errors.Is(err, errSlackChannelNotFound) {
			continue
		}
		if err != nil {
			return channelWish{}, false, err
		}
		return channelWish{channelID: channelID, workspaceID: ws.WorkspaceID, name: info.Name}, true, nil
	}
	return channelWish{}, false, nil
}

// autoJoin is PUT's post-commit step (live Slack calls, never rolls back the
// tracking write): best-effort conversations.join for added PUBLIC channels
// the bot isn't already a member of. Returns the join-related warnings — a
// tracked-but-botless channel is never silently reported as healthy.
func (h *channelsHandler) autoJoin(ctx context.Context, orgID string, workspaces []slackstore.Connection, candidates []slackCandidate, added []slackstore.Channel) []channelsWarning {
	byID := make(map[string]slackCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ChannelID] = c
	}
	var warnings []channelsWarning
	for _, ch := range added {
		cand, known := byID[ch.ChannelID]
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
		caller, botToken, ok := h.botTokenForWorkspace(ctx, orgID, workspaces, ch.WorkspaceID)
		if !ok {
			warnings = append(warnings, channelsWarning{ChannelID: ch.ChannelID, Reason: "join_failed"})
			continue
		}
		err := slackConversationsJoin(ctx, h.client, caller, botToken, ch.ChannelID)
		if err == nil {
			continue
		}
		if isSlackJoinPrivateChannelError(err) {
			warnings = append(warnings, channelsWarning{ChannelID: ch.ChannelID, Reason: "invite_required"})
			continue
		}
		slackLog.Warn("channels: auto-join failed", "org_id", orgID, "channel", ch.ChannelID, "error", err)
		warnings = append(warnings, channelsWarning{ChannelID: ch.ChannelID, Reason: "join_failed"})
	}
	return warnings
}

// botTokenForWorkspace resolves workspaceID's bot token among workspaces, and
// the caller a call made with it is.
func (h *channelsHandler) botTokenForWorkspace(ctx context.Context, orgID string, workspaces []slackstore.Connection, workspaceID string) (slackCaller, string, bool) {
	for _, ws := range workspaces {
		if ws.WorkspaceID != workspaceID {
			continue
		}
		token, err := h.stores.Secrets.GetSystem(ctx, orgID, ws.BotTokenRef)
		if err != nil || token == "" {
			return slackCaller{}, "", false
		}
		return slackBotCaller(orgID, ws.WorkspaceID, ws.APIAppID), token, true
	}
	return slackCaller{}, "", false
}

// diffChannelIDs computes added/removed registry rows against the prior
// tracked set — used both for ReconcilePrimariesSystem's touched-channel set
// and for the auto-join pass (added only).
func diffChannelIDs(prior []slackstore.TeamChannel, desired []string) (added, removed []string) {
	priorSet := make(map[string]bool, len(prior))
	for _, p := range prior {
		priorSet[p.SlackChannelID] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, id := range desired {
		desiredSet[id] = true
		if !priorSet[id] {
			added = append(added, id)
		}
	}
	for _, p := range prior {
		if !desiredSet[p.SlackChannelID] {
			removed = append(removed, p.SlackChannelID)
		}
	}
	return added, removed
}

// teamHasSlackMessageHandler reports whether teamID already has a live
// slack:message event handler, in any mode and from any source. It is what
// keeps the default-blueprint seed idempotent: a team that configured its own
// handler is respected (no default forced beside it).
func teamHasSlackMessageHandler(ctx context.Context, tx db.TxStores, orgID, teamID string) (bool, error) {
	_, total, err := tx.EventHandlers.List(ctx, orgID,
		db.EventHandlerListFilter{EventType: domain.EventSlackMessage, TeamID: teamID}, db.ListOpts{CountOnly: true})
	if err != nil {
		return false, err
	}
	return total > 0, nil
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
		"thread. Parse the channel, ts, thread_ts, sender, and message text out of the raw event metadata in " +
		"the task context above.\n\n" +
		"Read the thread (`triagefactory exec slack read thread --channel <channel> --ts <thread_ts>`, or `--ts <ts>` when " +
		"thread_ts is empty — the triggering message itself started the thread) to see the full request in " +
		"context, gather what you need from the linked entity and any referenced repos/issues, do the work using " +
		"the triagefactory exec subcommands available to you, and reply with " +
		"`triagefactory exec slack send` — your stdout is not visible to the user, so the send is the only way they see your " +
		"answer. Always reply in the thread: pass `--thread-ts <thread_ts>`, or `--thread-ts <ts>` when thread_ts is " +
		"empty, which starts a thread rooted on the triggering message — never answer at the channel root, even when " +
		"the message to you wasn't in a thread."
)

// slackMessageHandlerBreakerThreshold / MinAutonomySuitability match the
// shipped handlers' defaults (see db.ShippedEventHandlers).
const (
	slackMessageHandlerBreakerThreshold = 3
	slackMessageHandlerMinAutonomy      = 0.0
)

// seedDefaultSlackMessageBlueprint creates, as user-source rows attributed
// to the tracking admin (creator_user_id = them), a 1-step blueprint
// wrapping a fresh prompt and a slack:message event handler in automate,
// scoped to any channel
// (predicate {"channel_in":[]}). Called inside the same tx as the tracking
// write (handlePut) — see that call site's doc for why this is the
// app-pool Create path rather than the admin-pool seeders.
//
// The handler names no channels on purpose. An empty channel_in matches every
// channel the team tracks, because the routing hooks deliver a team's
// handlers only messages from those, so the default follows the tracked set
// as channels are added and removed. Seeding it with the channels tracked at
// this moment would freeze that set, and a channel tracked later would never
// fire it.
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
	if _, err := tx.EventHandlers.Create(ctx, orgID, teamID, domain.EventHandler{
		ID:                     uuid.New().String(),
		Mode:                   domain.EventHandlerModeAutomate,
		EventType:              domain.EventSlackMessage,
		ScopePredicateJSON:     &predicate,
		Source:                 domain.EventHandlerSourceUser,
		Name:                   slackMessagePromptName,
		DefaultPriority:        0.5,
		BlueprintID:            blueprintID,
		BreakerThreshold:       slackMessageHandlerBreakerThreshold,
		MinAutonomySuitability: slackMessageHandlerMinAutonomy,
	}); err != nil {
		return fmt.Errorf("seed slack message handler: %w", err)
	}
	return nil
}
