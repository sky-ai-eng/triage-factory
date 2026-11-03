package slack

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The channel list has no store: it is filtered, ordered, windowed and
// counted in hand over the merged set. These tests pin that code the way the
// dbtest conformance suite pins a store's predicate and ORDER BY.

func at(minutesAgo int) *time.Time {
	t := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute)
	return &t
}

func channelIDs(channels []channelView) []string {
	out := make([]string, len(channels))
	for i, c := range channels {
		out[i] = c.ChannelID
	}
	return out
}

func mustFilter(t *testing.T, search string, tracked *bool, ids []string) channelFilter {
	t.Helper()
	var v httpx.Validation
	f, _ := resolveChannelFilter(&v, search, tracked, ids)
	if items := v.Items(); len(items) > 0 {
		t.Fatalf("resolveChannelFilter faults = %+v", items)
	}
	return f
}

func ptr[T any](v T) *T { return &v }

const (
	rowEng = "11111111-1111-4111-8111-111111111111"
	rowOps = "22222222-2222-4222-8222-222222222222"
)

func TestFilterChannels_SearchesNameAndChannelID(t *testing.T) {
	channels := []channelView{
		{ChannelID: "C0ENG", Name: "eng-platform"},
		{ChannelID: "C0OPS", Name: "ops"},
		{ChannelID: "C0RAW", Name: ""},
		{ChannelID: "G0PRIV", Name: "Incidents"},
	}
	cases := []struct {
		search string
		want   []string
	}{
		{"", []string{"C0ENG", "C0OPS", "C0RAW", "G0PRIV"}},
		{"platform", []string{"C0ENG"}},
		// The channel id is searched too, so a channel TF knows only by its
		// id is findable.
		{"c0raw", []string{"C0RAW"}},
		{"g0", []string{"G0PRIV"}},
		// Case-insensitive on the name.
		{"INCIDENT", []string{"G0PRIV"}},
		// Trimmed.
		{"  ops ", []string{"C0OPS"}},
		{"nothing-like-this", []string{}},
	}
	for _, tc := range cases {
		got := channelIDs(filterChannels(channels, mustFilter(t, tc.search, nil, nil)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("search %q = %v, want %v", tc.search, got, tc.want)
		}
	}
}

// A percent sign is a LIKE wildcard in the store predicates; the in-hand
// match must read it as the character it is.
func TestFilterChannels_PercentIsLiteral(t *testing.T) {
	channels := []channelView{
		{ChannelID: "C1", Name: "100%-done"},
		{ChannelID: "C2", Name: "100x-done"},
		{ChannelID: "C3", Name: "1000-done"},
	}
	got := channelIDs(filterChannels(channels, mustFilter(t, "100%", nil, nil)))
	if !slices.Equal(got, []string{"C1"}) {
		t.Errorf("search %q = %v, want [C1]", "100%", got)
	}
	got = channelIDs(filterChannels(channels, mustFilter(t, "_", nil, nil)))
	if len(got) != 0 {
		t.Errorf("search %q = %v, want none (no name carries an underscore)", "_", got)
	}
}

func TestFilterChannels_TrackedAndIDs(t *testing.T) {
	channels := []channelView{
		{ID: rowEng, ChannelID: "C1", Name: "eng", Tracked: true},
		{ID: rowOps, ChannelID: "C2", Name: "ops"},
		// A live Slack channel TF holds no row for.
		{ChannelID: "C3", Name: "random"},
	}
	cases := []struct {
		name    string
		tracked *bool
		ids     []string
		want    []string
	}{
		{"both", nil, nil, []string{"C1", "C2", "C3"}},
		{"tracked", ptr(true), nil, []string{"C1"}},
		{"untracked", ptr(false), nil, []string{"C2", "C3"}},
		{"ids", nil, []string{rowOps}, []string{"C2"}},
		// Upper case names the same row.
		{"ids upper case", nil, []string{strings.ToUpper(rowEng)}, []string{"C1"}},
		{"ids and tracked", ptr(true), []string{rowOps}, []string{}},
		// Empty is absent, not "match nothing".
		{"empty ids", nil, []string{}, []string{"C1", "C2", "C3"}},
	}
	for _, tc := range cases {
		got := channelIDs(filterChannels(channels, mustFilter(t, "", tc.tracked, tc.ids)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestResolveChannelFilter_Faults(t *testing.T) {
	var v httpx.Validation
	resolveChannelFilter(&v, strings.Repeat("é", httpx.MaxSearchRunes+1), nil, []string{rowEng, "C0123", ""})
	got := map[string]string{}
	for _, item := range v.Items() {
		got[item.Field] = item.Reason
	}
	want := map[string]string{
		"search": httpx.ReasonInvalidField,
		"ids[1]": httpx.ReasonInvalidField,
		"ids[2]": httpx.ReasonInvalidField,
	}
	if len(got) != len(want) {
		t.Fatalf("faults = %+v, want %v", v.Items(), want)
	}
	for field, reason := range want {
		if got[field] != reason {
			t.Errorf("fault on %s = %q, want %q", field, got[field], reason)
		}
	}

	// The cap is inclusive: exactly MaxSearchRunes runes is a search.
	var ok httpx.Validation
	f, _ := resolveChannelFilter(&ok, strings.Repeat("é", httpx.MaxSearchRunes), nil, nil)
	if len(ok.Items()) != 0 || f.search == "" {
		t.Errorf("a %d-rune search: faults %+v, search %q", httpx.MaxSearchRunes, ok.Items(), f.search)
	}
}

func TestResolveChannelFilter_CanonicalIDs(t *testing.T) {
	var v httpx.Validation
	_, ids := resolveChannelFilter(&v, "", nil, []string{rowOps, strings.ToUpper(rowEng), rowOps})
	if !slices.Equal(ids, []string{rowEng, rowOps}) {
		t.Errorf("canonical ids = %v, want [%s %s]", ids, rowEng, rowOps)
	}
	_, empty := resolveChannelFilter(&v, "", nil, []string{})
	if empty != nil {
		t.Errorf("canonical ids of [] = %v, want nil (absent and empty fingerprint the same)", empty)
	}
}

func TestSortChannels(t *testing.T) {
	theirs := []trackerView{{TeamID: "t2", IsPrimary: true}}
	nobody := []trackerView{{TeamID: "t2"}}
	ours := []trackerView{{TeamID: "t1", IsPrimary: true}}
	channels := []channelView{
		{ChannelID: "C5", Name: "", LastMentionAt: at(1)},
		{ChannelID: "C2", Name: "beta", TrackedBy: theirs},
		{ChannelID: "C1", Name: "alpha", LastMentionAt: at(30), TrackedBy: nobody},
		{ChannelID: "C4", Name: "", TrackedBy: nobody},
		{ChannelID: "C3", Name: "gamma", LastMentionAt: at(5), Tracked: true, IsPrimary: true, TrackedBy: ours},
		// Same name as C2: the channel id breaks the tie.
		{ChannelID: "C0", Name: "beta"},
	}
	cases := []struct {
		name  string
		order db.Sort
		want  []string
	}{
		// Default: name, unnamed after named, then channel id.
		{"default", db.Sort{}, []string{"C1", "C0", "C2", "C3", "C4", "C5"}},
		{"name asc", db.Sort{Key: channelSortName, Dir: db.SortAsc}, []string{"C1", "C0", "C2", "C3", "C4", "C5"}},
		// Unnamed channels stay last, and the channel id stays ascending.
		{"name desc", db.Sort{Key: channelSortName, Dir: db.SortDesc}, []string{"C3", "C0", "C2", "C1", "C4", "C5"}},
		// Most recent first, then the channels with no mention by name.
		{"last_mention_at desc", db.Sort{Key: channelSortLastMention, Dir: db.SortDesc}, []string{"C5", "C3", "C1", "C0", "C2", "C4"}},
		// Channels with no mention stay last ascending too.
		{"last_mention_at asc", db.Sort{Key: channelSortLastMention, Dir: db.SortAsc}, []string{"C1", "C3", "C5", "C0", "C2", "C4"}},
		// this_team, no_primary, other_team, unwatched; ties by name.
		{"primary asc", db.Sort{Key: channelSortPrimary, Dir: db.SortAsc}, []string{"C3", "C1", "C4", "C2", "C0", "C5"}},
		{"primary desc", db.Sort{Key: channelSortPrimary, Dir: db.SortDesc}, []string{"C0", "C5", "C2", "C1", "C4", "C3"}},
	}
	for _, tc := range cases {
		rows := slices.Clone(channels)
		sortChannels(rows, tc.order)
		if got := channelIDs(rows); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestChannelPrimary(t *testing.T) {
	cases := []struct {
		c    channelView
		want string
	}{
		{channelView{Tracked: true, IsPrimary: true, TrackedBy: []trackerView{{TeamID: "t1", IsPrimary: true}}}, channelPrimaryThisTeam},
		{channelView{Tracked: true, TrackedBy: []trackerView{{TeamID: "t1"}, {TeamID: "t2", IsPrimary: true}}}, channelPrimaryOtherTeam},
		{channelView{TrackedBy: []trackerView{{TeamID: "t2", IsPrimary: true}}}, channelPrimaryOtherTeam},
		{channelView{Tracked: true, TrackedBy: []trackerView{{TeamID: "t1"}}}, channelPrimaryNone},
		{channelView{TrackedBy: []trackerView{}}, channelPrimaryUnwatched},
	}
	for i, tc := range cases {
		if got := channelPrimary(tc.c); got != tc.want {
			t.Errorf("case %d: channelPrimary = %q, want %q", i, got, tc.want)
		}
	}
}

func TestChannelPage(t *testing.T) {
	rows := []channelView{{ChannelID: "C1"}, {ChannelID: "C2"}, {ChannelID: "C3"}}
	cases := []struct {
		name string
		page httpx.Page
		want []string
	}{
		{"first", httpx.Page{Limit: 2}, []string{"C1", "C2"}},
		{"last", httpx.Page{Limit: 2, Offset: 2}, []string{"C3"}},
		{"past the end", httpx.Page{Limit: 2, Offset: 5}, []string{}},
		{"count only", httpx.Page{CountOnly: true}, []string{}},
	}
	for _, tc := range cases {
		got := channelPage(rows, tc.page)
		if got == nil {
			t.Errorf("%s: page is nil, want a slice", tc.name)
		}
		if ids := channelIDs(got); !slices.Equal(ids, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, ids, tc.want)
		}
	}
}

func TestChannelFacets(t *testing.T) {
	channels := []channelView{
		{ChannelID: "C1", Name: "eng", Tracked: true, IsPrimary: true, TrackedBy: []trackerView{{TeamID: "t1", IsPrimary: true}}},
		{ChannelID: "C2", Name: "eng-oncall", Tracked: true, TrackedBy: []trackerView{{TeamID: "t1"}}},
		{ChannelID: "C3", Name: "eng-general", TrackedBy: []trackerView{}},
		{ChannelID: "C4", Name: "ops", TrackedBy: []trackerView{{TeamID: "t2", IsPrimary: true}}},
	}
	facet := func(value string, count int) httpx.FacetJSON { return httpx.FacetJSON{Value: value, Count: count} }
	cases := []struct {
		name    string
		filter  channelFilter
		tracked []httpx.FacetJSON
		primary []httpx.FacetJSON
	}{
		{
			name:    "everything",
			filter:  mustFilter(t, "", nil, nil),
			tracked: []httpx.FacetJSON{facet("true", 2), facet("false", 2)},
			primary: []httpx.FacetJSON{
				facet(channelPrimaryThisTeam, 1), facet(channelPrimaryNone, 1),
				facet(channelPrimaryOtherTeam, 1), facet(channelPrimaryUnwatched, 1),
			},
		},
		{
			// The tracked cut ignores the tracked filter, so it still names the
			// value the reader filtered out; the primary cut takes it.
			name:    "tracked",
			filter:  mustFilter(t, "", ptr(true), nil),
			tracked: []httpx.FacetJSON{facet("true", 2), facet("false", 2)},
			primary: []httpx.FacetJSON{facet(channelPrimaryThisTeam, 1), facet(channelPrimaryNone, 1)},
		},
		{
			// Search narrows both cuts, and a value nothing carries is absent.
			name:    "search",
			filter:  mustFilter(t, "eng", nil, nil),
			tracked: []httpx.FacetJSON{facet("true", 2), facet("false", 1)},
			primary: []httpx.FacetJSON{
				facet(channelPrimaryThisTeam, 1), facet(channelPrimaryNone, 1), facet(channelPrimaryUnwatched, 1),
			},
		},
		{
			name:    "nothing matches",
			filter:  mustFilter(t, "nothing", nil, nil),
			tracked: []httpx.FacetJSON{},
			primary: []httpx.FacetJSON{},
		},
	}
	for _, tc := range cases {
		got := channelFacets(channels, tc.filter)
		if got.Tracked == nil || got.Primary == nil {
			t.Errorf("%s: a cut is nil, want []", tc.name)
		}
		if !slices.Equal(got.Tracked, tc.tracked) {
			t.Errorf("%s: tracked = %+v, want %+v", tc.name, got.Tracked, tc.tracked)
		}
		if !slices.Equal(got.Primary, tc.primary) {
			t.Errorf("%s: primary = %+v, want %+v", tc.name, got.Primary, tc.primary)
		}
	}
}
