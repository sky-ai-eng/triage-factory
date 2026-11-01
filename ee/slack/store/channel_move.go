package slackstore

import (
	"net/url"
	"strings"
)

// ChannelMove is what ChannelRegistryStore.MoveSystem rewrote. Every count is
// zero on a redelivery of a change already applied.
type ChannelMove struct {
	// To is the id the channel moved to: newID, or the id a recorded change
	// names when one has overtaken this one (see MoveSystem).
	To string
	// Absorbed is the registry row the new id already had, merged into the
	// channel's row and deleted; "" when there was none.
	Absorbed string
	// Entities is how many thread entities' keys and permalinks moved.
	Entities int
	// Superseded are the ids of the entities closed because an older entity
	// for the same thread stayed active; see MoveSystem.
	Superseded []string
	// Actions counts the external action pointers rewritten.
	Actions int
	// Trackers is how many team tracking rows, and Handlers how many
	// slack:message handler filters, named the absorbed row.
	Trackers int
	Handlers int
	// SkippedHandlers are the slack:message handlers naming the absorbed row
	// whose stored filter could not be decoded. They are left as stored, still
	// naming it.
	SkippedHandlers []string
}

// MovePermalink moves a Slack permalink into oldID's archive
// ("https://<team>.slack.com/archives/<channel>/p<ts>", with
// "?thread_ts=…&cid=<channel>" on a reply) to newID: the archive path segment,
// and the cid parameter when it names oldID too. Every other part of the link
// is kept as it was. Reports false, with u unchanged, for anything that is
// not a link into oldID's archive.
func MovePermalink(u, oldID, newID string) (string, bool) {
	if u == "" || oldID == "" || newID == "" || oldID == newID {
		return u, false
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u, false
	}
	segments := strings.Split(parsed.Path, "/")
	moved := false
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "archives" && segments[i+1] == oldID {
			segments[i+1] = newID
			moved = true
			break
		}
	}
	if !moved {
		return u, false
	}
	parsed.Path = strings.Join(segments, "/")
	parsed.RawPath = ""
	if parsed.RawQuery != "" {
		params := strings.Split(parsed.RawQuery, "&")
		for i, p := range params {
			if p == "cid="+oldID {
				params[i] = "cid=" + newID
			}
		}
		parsed.RawQuery = strings.Join(params, "&")
	}
	return parsed.String(), true
}

// MoveChannelFilter replaces the registry row oldID with newID in a
// slack:message handler's channel_in list, keeping the list's order and
// dropping the copy of newID a list naming both would otherwise hold twice.
// Reports false, with channels unchanged, when the list does not name oldID.
func MoveChannelFilter(channels []string, oldID, newID string) ([]string, bool) {
	found := false
	for _, c := range channels {
		if c == oldID {
			found = true
			break
		}
	}
	if !found || oldID == newID {
		return channels, false
	}
	out := make([]string, 0, len(channels))
	haveNew := false
	for _, c := range channels {
		if c == oldID {
			c = newID
		}
		if c == newID {
			if haveNew {
				continue
			}
			haveNew = true
		}
		out = append(out, c)
	}
	return out, true
}
