package agentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// metaToolsFingerprint is the metadata key under which an assistant row
// records the fingerprint of the tools its request carried. Stored rows carry
// this key, so it cannot be renamed.
const metaToolsFingerprint = "tools_fingerprint"

// canonicalTools renders a tool list the way the Anthropic API binds a
// thinking block to it: as a set keyed by name, so the order the tools are
// listed in is free, and with each definition whole, so a changed name,
// description, or input schema is a different set.
func canonicalTools(tools []schemas.ChatTool) ([]byte, error) {
	name := func(t schemas.ChatTool) string {
		if t.Function != nil {
			return t.Function.Name
		}
		return ""
	}
	sorted := append([]schemas.ChatTool(nil), tools...)
	sort.SliceStable(sorted, func(i, j int) bool { return name(sorted[i]) < name(sorted[j]) })
	return json.Marshal(sorted)
}

// toolsFingerprint identifies the set of tools a request carries. Two
// requests with the same fingerprint offer the model the same tools as far as
// a replayed thinking block is concerned.
//
// It returns "" for a list it cannot render. No row is recorded under "", and
// no recorded fingerprint equals it.
func toolsFingerprint(tools []schemas.ChatTool) string {
	b, err := canonicalTools(tools)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// recordToolsFingerprint stamps an assistant row with the fingerprint of the
// tools its request carried, which is what a later request compares its own
// tools against before it replays the row's reasoning.
func recordToolsFingerprint(row *domain.Message, fingerprint string) {
	if fingerprint == "" {
		return
	}
	if row.Metadata == nil {
		row.Metadata = map[string]any{}
	}
	row.Metadata[metaToolsFingerprint] = fingerprint
}

func recordedToolsFingerprint(row domain.Message) string {
	fingerprint, _ := row.Metadata[metaToolsFingerprint].(string)
	return fingerprint
}

// reasoningBoundTo returns rows with the reasoning removed from every row a
// request carrying the tools behind fingerprint must not replay it from.
//
// The Anthropic API refuses a thinking block sent back under tools other than
// the ones its request carried, with a 400 that a retry does not change. The
// tools come from the running binary, so a release that changes one, or a
// claim that offers a different set, changes them under every block a parked
// conversation holds.
//
// The cut is the last row that carries reasoning and records other tools:
// that row and every row before it go out without reasoning, and every row
// after it keeps its own. Cutting everything before it, and not only the rows
// that differ, keeps what is replayed one unbroken run of the blocks the
// model produced, which is the only removal the API accepts. A conversation
// that returns to tools it held before therefore does not get the blocks from
// its first time under them back: the rows between were produced without
// them.
//
// A row that records no fingerprint keeps its reasoning unless a later row
// puts it behind the cut. Such a row comes from a binary that records none,
// and that binary replays every block a conversation holds. A block this one
// left out and that one sent back would sit in front of the blocks produced
// in between, which are bound to the history without it, and every call that
// binary made on the conversation would be refused.
//
// The cut is derived from the rows on every request and stored nowhere, so
// every request of an engagement, and of every later one under the same
// tools, removes the same blocks.
func reasoningBoundTo(rows []domain.Message, fingerprint string) []domain.Message {
	cut, found := 0.0, false
	for _, r := range rows {
		if len(r.Reasoning) == 0 {
			continue
		}
		if recorded := recordedToolsFingerprint(r); recorded == "" || recorded == fingerprint {
			continue
		}
		if key := assemblyKey(r); !found || key > cut {
			cut, found = key, true
		}
	}
	if !found {
		return rows
	}
	out := make([]domain.Message, len(rows))
	copy(out, rows)
	for i := range out {
		if assemblyKey(out[i]) <= cut {
			out[i].Reasoning = nil
		}
	}
	return out
}
