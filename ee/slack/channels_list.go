package slack

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// A team's channel list is built in hand: mergedChannels unions three sources
// (the team's tracked rows, the org's sighting registry and the live Slack
// enumeration), so no store can filter, order or count it. Every narrowing
// the list offers therefore runs here, over the whole merged set, before the
// page window is cut — a filter applied to a page already cut would answer a
// different question from the total beside it.

// The list's sort_key vocabulary.
const (
	// channelSortName orders by channel name. A channel TF knows only by its
	// Slack id has no name, and sorts after every named one in both
	// directions, by channel id.
	channelSortName = "name"
	// channelSortLastMention orders by the last mention the app saw in the
	// channel. A channel with none sorts after every one with a mention in
	// both directions, by name.
	channelSortLastMention = "last_mention_at"
	// channelSortPrimary orders by whose primary the channel is, in the rank
	// of channelPrimaryValues: this team, no team while some team watches it,
	// another team, no team watches it. Desc reverses the rank. Ties order by
	// name.
	channelSortPrimary = "primary"
)

// channelListSortKeys is the vocabulary httpx.Sort validates sort_key against.
var channelListSortKeys = []httpx.SortKey{
	{Name: channelSortName, Default: db.SortAsc},
	{Name: channelSortLastMention, Default: db.SortDesc},
	{Name: channelSortPrimary, Default: db.SortAsc},
}

// The values of the facets node's primary cut, which is also the rank
// channelSortPrimary orders by: whose mentions a channel's are.
const (
	// channelPrimaryThisTeam: this team is the channel's primary.
	channelPrimaryThisTeam = "this_team"
	// channelPrimaryNone: at least one team watches the channel and none is
	// its primary, so its mentions go nowhere.
	channelPrimaryNone = "no_primary"
	// channelPrimaryOtherTeam: another team is the channel's primary.
	channelPrimaryOtherTeam = "other_team"
	// channelPrimaryUnwatched: no team watches the channel.
	channelPrimaryUnwatched = "unwatched"
)

// channelPrimaryValues is the primary cut's vocabulary in rank order.
var channelPrimaryValues = []string{
	channelPrimaryThisTeam, channelPrimaryNone, channelPrimaryOtherTeam, channelPrimaryUnwatched,
}

// channelsListRequest is POST .../channels/list's body.
type channelsListRequest struct {
	// Search is a case-insensitive literal substring the channel's name or
	// its Slack channel id must carry. Trimmed; empty after the trim is
	// absent.
	Search string `json:"search"`
	// Tracked narrows to the channels this team tracks (true) or to the ones
	// it does not (false). Absent lists both.
	Tracked *bool `json:"tracked"`
	// IDs narrows to the channels with these registry ids (a row's id field,
	// what a slack:message handler's channel_in stores). A live Slack channel
	// TF holds no row for has no id and never matches. Absent or empty lists
	// every channel.
	IDs []string `json:"ids"`
	// SortKey reorders the list (channelListSortKeys). Absent is the default
	// order: name, then channel id.
	SortKey string `json:"sort_key"`
	// SortDir is SortKey's direction; absent takes the key's own default, and
	// alone it is refused.
	SortDir string `json:"sort_dir"`

	httpx.PageRequest
}

// channelsListFilterKey is the list's page fingerprint: every field that
// changes which rows a position addresses.
type channelsListFilterKey struct {
	Search  string   `json:"search"`
	Tracked *bool    `json:"tracked"`
	IDs     []string `json:"ids"`
	Sort    db.Sort  `json:"sort"`
}

// channelsListResponse is the list envelope plus two fields about the read
// rather than about any row: the caller's role on the team, which decides
// whether the page offers the tracking verbs, and the process-level warnings
// the live Slack read raised (slack_unreachable, slack_channels_truncated),
// which say the set the page and its total describe is short.
type channelsListResponse struct {
	Items         []channelView     `json:"items"`
	NextPageToken string            `json:"next_page_token,omitempty"`
	TotalCount    int               `json:"total_count"`
	Role          string            `json:"role"`
	Warnings      []channelsWarning `json:"warnings"`
}

// channelFilter is a resolved set of the list's narrowing fields.
type channelFilter struct {
	search  string
	tracked *bool
	ids     map[string]bool
}

// resolveChannelFilter validates the narrowing fields the list and the facets
// node share, appending every fault to v. It returns the filter and the ids in
// their canonical form for the fingerprint.
func resolveChannelFilter(v *httpx.Validation, search string, tracked *bool, ids []string) (channelFilter, []string) {
	f := channelFilter{search: httpx.Search(v, search), tracked: tracked}
	canonical := make([]string, 0, len(ids))
	for i, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			v.Invalid(fmt.Sprintf("ids[%d]", i), fmt.Sprintf("%q is not a channel registry id", id))
			continue
		}
		// uuid.Parse accepts upper case and braces; the registry's ids are
		// the canonical lower-case form, so that is what the match compares.
		canonical = append(canonical, parsed.String())
	}
	canonical = httpx.CanonicalStrings(canonical)
	if len(canonical) > 0 {
		f.ids = make(map[string]bool, len(canonical))
		for _, id := range canonical {
			f.ids[id] = true
		}
	}
	return f, canonical
}

// matches reports whether c passes every field of f. ignoreTracked skips the
// tracked field, for the facet cut that counts by it.
func (f channelFilter) matches(c channelView, ignoreTracked bool) bool {
	if !ignoreTracked && f.tracked != nil && c.Tracked != *f.tracked {
		return false
	}
	if f.ids != nil && !f.ids[c.ID] {
		return false
	}
	return httpx.SearchMatches(f.search, c.Name, c.ChannelID)
}

// filterChannels returns the channels f matches, in their given order.
func filterChannels(channels []channelView, f channelFilter) []channelView {
	out := make([]channelView, 0, len(channels))
	for _, c := range channels {
		if f.matches(c, false) {
			out = append(out, c)
		}
	}
	return out
}

// channelPrimary is whose primary c is, one of channelPrimaryValues.
func channelPrimary(c channelView) string {
	switch {
	case c.IsPrimary:
		return channelPrimaryThisTeam
	case slices.ContainsFunc(c.TrackedBy, func(t trackerView) bool { return t.IsPrimary }):
		return channelPrimaryOtherTeam
	case len(c.TrackedBy) > 0:
		return channelPrimaryNone
	default:
		return channelPrimaryUnwatched
	}
}

// sortChannels orders channels in place under order. Every order ends in the
// channel id, ascending whatever the direction, so the merged set has one
// total order and an offset addresses the same row on every read of an
// unchanged set.
func sortChannels(channels []channelView, order db.Sort) {
	slices.SortFunc(channels, func(a, b channelView) int { return compareChannels(a, b, order) })
}

func compareChannels(a, b channelView, order db.Sort) int {
	switch order.Key {
	case channelSortName:
		return compareChannelNames(a, b, order.Desc())
	case channelSortLastMention:
		if c := compareLastMention(a, b, order.Desc()); c != 0 {
			return c
		}
	case channelSortPrimary:
		c := cmp.Compare(slices.Index(channelPrimaryValues, channelPrimary(a)), slices.Index(channelPrimaryValues, channelPrimary(b)))
		if order.Desc() {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return compareChannelNames(a, b, false)
}

// compareChannelNames orders named channels before unnamed ones in both
// directions — a missing name is the absence of a value, not one that sorts
// below "a" — then by name, then by channel id ascending.
func compareChannelNames(a, b channelView, desc bool) int {
	if (a.Name == "") != (b.Name == "") {
		if a.Name == "" {
			return 1
		}
		return -1
	}
	c := strings.Compare(a.Name, b.Name)
	if desc {
		c = -c
	}
	if c != 0 {
		return c
	}
	return strings.Compare(a.ChannelID, b.ChannelID)
}

// compareLastMention orders channels with a mention before channels without
// one in both directions, then by when the mention was. Channels it cannot
// tell apart compare equal, for the caller's tiebreaker.
func compareLastMention(a, b channelView, desc bool) int {
	switch {
	case a.LastMentionAt == nil && b.LastMentionAt == nil:
		return 0
	case a.LastMentionAt == nil:
		return 1
	case b.LastMentionAt == nil:
		return -1
	}
	c := a.LastMentionAt.Compare(*b.LastMentionAt)
	if desc {
		c = -c
	}
	return c
}

// channelPage cuts page's window out of the filtered, ordered set.
func channelPage(channels []channelView, page httpx.Page) []channelView {
	if page.CountOnly || page.Offset >= len(channels) {
		return []channelView{}
	}
	return channels[page.Offset:min(page.Offset+page.Limit, len(channels))]
}

// handleList answers one page of the team's channels: the merged set of its
// tracked rows, the org's sighting registry and the live Slack channels of
// every connected workspace, narrowed by search, tracked and ids, in the order
// sort_key names (default: name, then channel id).
//
// The live half is enumerated from Slack on every read, page and search
// alike: conversations.list and users.conversations per connected workspace,
// each collecting at most slackConversationsListCap channels. Past that cap
// the workspace's live channels are a prefix of its universe, so a search
// cannot find a channel beyond it and total_count counts only what the cap
// let in; the response says so with a slack_channels_truncated warning rather
// than passing the prefix off as the whole set. A workspace whose read fails
// contributes no live channels and raises slack_unreachable. Tracked and
// sighted channels are listed either way.
//
// Authorization is the team roster's: any member of the team, a 404 for
// anyone else in the org. It registers through apiMutating because it is a
// POST; CSRF follows the method rather than the intent.
//
// POST /api/slack/teams/{team_id}/channels/list
func (h *channelsHandler) handleList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, role, ok := h.memberTeam(w, r)
	if !ok {
		return
	}

	var req channelsListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	filter, ids := resolveChannelFilter(&v, req.Search, req.Tracked, req.IDs)
	order := httpx.Sort(&v, req.SortKey, req.SortDir, channelListSortKeys)
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(channelsListFilterKey{
		Search:  httpx.SearchKey(filter.search),
		Tracked: filter.tracked,
		IDs:     ids,
		Sort:    order,
	}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	channels, warnings, err := h.mergedChannels(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	matched := filterChannels(channels, filter)
	sortChannels(matched, order)
	items := channelPage(matched, page)
	httpx.WriteJSON(w, http.StatusOK, channelsListResponse{
		Items:         items,
		NextPageToken: httpx.NextPageToken(page, len(items), len(matched)),
		TotalCount:    len(matched),
		Role:          role,
		Warnings:      warnings,
	})
}

// channelsFacetsRequest is POST .../channels/facets's body: the list's
// narrowing fields, with the same meaning, and neither its paging nor its
// order, which a count does not have.
type channelsFacetsRequest struct {
	Search  string   `json:"search"`
	Tracked *bool    `json:"tracked"`
	IDs     []string `json:"ids"`
}

// channelsFacetsResponse is the facets node's fixed schema of cuts. A value
// no channel carries is absent from its cut rather than listed at zero; each
// cut lists its values in its vocabulary's order.
type channelsFacetsResponse struct {
	// Tracked counts the channels this team tracks ("true") and the ones it
	// does not ("false"), under every filter but tracked.
	Tracked []httpx.FacetJSON `json:"tracked"`
	// Primary counts channels by whose primary they are
	// (channelPrimaryValues), under every filter.
	Primary []httpx.FacetJSON `json:"primary"`
}

// channelFacets counts channels into the node's two cuts under f.
func channelFacets(channels []channelView, f channelFilter) channelsFacetsResponse {
	tracked := map[string]int{}
	primary := map[string]int{}
	for _, c := range channels {
		if !f.matches(c, true) {
			continue
		}
		tracked[fmt.Sprint(c.Tracked)]++
		if f.tracked == nil || c.Tracked == *f.tracked {
			primary[channelPrimary(c)]++
		}
	}
	cut := func(counts map[string]int, values []string) []httpx.FacetJSON {
		out := []httpx.FacetJSON{}
		for _, value := range values {
			if n := counts[value]; n > 0 {
				out = append(out, httpx.FacetJSON{Value: value, Count: n})
			}
		}
		return out
	}
	return channelsFacetsResponse{
		Tracked: cut(tracked, []string{"true", "false"}),
		Primary: cut(primary, channelPrimaryValues),
	}
}

// handleFacets is the channel list's synthetic sibling: how many of the
// team's channels it tracks, and whose primary each is — the figures a page
// draws beside the list. It is a node rather than a client's count because a
// client holds one page of the list, and a count over a page is a count of
// that page.
//
// It reads the same merged set the list does, built the same way and with the
// same gaps: when a workspace's live read fails or stops at its cap, the
// counts cover what the list would then show, and the list's warnings are
// where that is said. Authorization is the list's.
//
// POST /api/slack/teams/{team_id}/channels/facets
func (h *channelsHandler) handleFacets(w http.ResponseWriter, r *http.Request) {
	orgID, userID, teamID, _, ok := h.memberTeam(w, r)
	if !ok {
		return
	}

	var req channelsFacetsRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	filter, _ := resolveChannelFilter(&v, req.Search, req.Tracked, req.IDs)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	channels, _, err := h.mergedChannels(r.Context(), orgID, userID, teamID)
	if err != nil {
		httpx.InternalError(w, "slack/channels", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, channelFacets(channels, filter))
}
