package eventsource

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// ErrDisabled marks every refusal that traces back to an org admin having
// turned a source off. Gates wrap it with DisabledError so a caller can tell
// this apart from an unbound credential without matching on prose: the two
// need opposite responses — bind a credential, versus ask an admin to lift a
// pause — and only one of them is the reader's own to fix.
var ErrDisabled = errors.New("event source turned off by an org admin")

// DisabledError is the refusal a paused source produces at every gate: the
// poller's skip, an agent's exec verb, the credential provisioner. One
// constructor because an agent that reads this text in a tool result and a
// developer who reads it in a log are owed the same explanation, and because
// the useful half is the part naming what a pause covers — a reader who only
// learns "Jira is off" reasonably concludes the credential broke and goes
// looking for it.
func DisabledError(kind string) error {
	return fmt.Errorf("%s is turned off for this organization: an org admin paused it, which stops polling, new tasks and agent access alike until it is turned back on in Settings → Event sources: %w", Label(kind), ErrDisabled)
}

// Disabled reports whether an org admin has turned kind off for orgID. A source
// the product is built on has no off switch and always answers false — see
// Disableable, which this consults first.
//
// This is the whole of the policy check, and it deliberately reads the row
// directly rather than resolving a State: the callers are the machinery — the
// poll cycle, an exec verb's credential resolution, the bundle sealer — and
// what they need to know is whether an admin said stop, not whether the
// credential also happens to be missing today. A source that is BOTH paused
// and unconfigured refuses here for the pause, which is the answer that stays
// true after somebody binds a credential.
//
// It reads the ...System variant because every caller is JWT-less by
// construction: a poll cycle, a background sealer, and a daemon acting for a
// run rather than for a signed-in reader.
func Disabled(ctx context.Context, store db.OrgEventSourceStore, orgID, kind string) (bool, error) {
	// A source with no off switch is never off, whatever a row says. Answering
	// from the vocabulary rather than the table is what makes that structural:
	// the row is unconstrained by design (kind is FK-free, since the vocabulary
	// is a runtime registry), so a hand-written one naming GitHub is inert here
	// instead of quietly disabling the product. It also spares every gate a
	// read for the kinds that can never answer yes — which is most events.
	if !Disableable(kind) {
		return false, nil
	}
	kinds, err := store.ListDisabledSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read event source policy for org %s: %w", orgID, err)
	}
	return slices.Contains(kinds, kind), nil
}

// Label renders a kind the way a person writes it. Core's two are spelled the
// way their own products spell them; a registered source gets its kind
// capitalized, which is right for the single-word kinds a source registry
// admits and is only ever cosmetic.
func Label(kind string) string {
	switch kind {
	case KindGitHub:
		return "GitHub"
	case KindJira:
		return "Jira"
	}
	if kind == "" {
		return "This source"
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}
