package agenthost

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	linearclient "github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linearoauth"
)

// The two refusal keys that are not manifest reasons: what a verb learns only
// when it tries the source, never at the seal.
const (
	// reasonCredentialRefused — the source refused the sealed credential.
	reasonCredentialRefused = "credential_refused"
	// reasonAfterStart — the manifest now names the source usable, but the
	// engagement started without a proxy for it, so nothing can reach it.
	reasonAfterStart = "after_start"
)

// sourceGuidance is one row of the refusal table: what an agent is told about
// a source in a given state, as a short note (a run's <run_context> and the
// exec help index) and as the refusal a verb returns. kind, state and reason
// empty match any value. {source} renders as the source's label.
type sourceGuidance struct {
	kind    string
	state   domain.SourceState
	reason  string
	note    string
	refusal string
}

// carryOn ends every refusal a retry will not fix: what the agent does next.
const carryOn = "Carry on without {source} and say in your summary which {source} actions you could not take"

// sourceGuidanceTable is the one table every explanation of a source's state is
// read from — the run's <run_context>, the help index, and every refusal a
// Jira, Linear or extension verb returns, in both modes. Lookup prefers the
// most specific row (see guidanceFor), so a source-specific row overrides a
// general one and a reason-specific row overrides its state's.
var sourceGuidanceTable = []sourceGuidance{
	{
		state: domain.SourceUnresolved,
		note:  "unavailable this run — Triage Factory could not fetch the organization's {source} credential; it keeps trying while the run continues",
		refusal: "{source} is unavailable for now: Triage Factory could not obtain the organization's {source} credential. " +
			"Retry this command in a few minutes. If it still fails, carry on without {source} and say in your summary which {source} actions you could not take",
	},
	{
		state:  domain.SourceUnresolved,
		reason: domain.SourceReasonTokenEndpointUnavailable,
		note:   "unavailable this run — {source} did not renew the organization's access token; Triage Factory keeps trying while the run continues",
		refusal: "{source} is unavailable for now: {source}'s token endpoint did not renew the organization's access token. " +
			"Retry this command in a few minutes. If it still fails, carry on without {source} and say in your summary which {source} actions you could not take",
	},
	{
		state:  domain.SourceUnresolved,
		reason: domain.SourceReasonTokenExpired,
		note:   "unavailable this run — the organization's {source} access token expired before it could be renewed; Triage Factory keeps trying while the run continues",
		refusal: "{source} is unavailable for now: the organization's {source} access token expired and has not been renewed yet. " +
			"Retry this command in a few minutes. If it still fails, carry on without {source} and say in your summary which {source} actions you could not take",
	},
	{
		state:   domain.SourceUnconfigured,
		note:    "not set up for this organization — its commands refuse",
		refusal: "{source} is not set up for this organization: no {source} credential is configured, so its commands refuse until an org admin connects {source} in Settings. " + carryOn,
	},
	{
		state:   domain.SourceDisabled,
		note:    "turned off for this organization by an org admin — its commands refuse",
		refusal: "{source} is turned off for this organization: an org admin paused it, which stops polling, new tasks and agent access alike until it is turned back on in Settings → Event sources",
	},
	{
		state:   domain.SourceUnlicensed,
		note:    "not enabled for this organization — its commands refuse",
		refusal: "{source} is not enabled for this organization, so its commands refuse. " + carryOn,
	},
	{
		state:   domain.SourceWithheld,
		note:    "withheld from this run — its commands refuse",
		refusal: "{source} was withheld from this run, so its commands refuse. " + carryOn,
	},
	{
		state:  domain.SourceSealed,
		reason: reasonCredentialRefused,
		refusal: "{source} refused the organization's credential. " +
			"If it keeps failing, the credential was revoked and an org admin has to reconnect {source} in Settings; carry on without {source} and say in your summary which {source} actions you could not take",
	},
	{
		kind:   eventsource.KindLinear,
		state:  domain.SourceSealed,
		reason: reasonCredentialRefused,
		refusal: "Linear refused the organization's credential. If the organization connected Linear as an app, its access token may have just expired and is renewed automatically, so retry this command in a few minutes. " +
			"If it keeps failing, the credential was revoked and an org admin has to reconnect Linear in Settings; carry on without Linear and say in your summary which Linear actions you could not take",
	},
	{
		reason:  reasonAfterStart,
		refusal: "{source} became available to this organization after this run started, so this run has no access to it. " + carryOn,
	},
}

// guidanceFor returns the most specific row matching (kind, state, reason): a
// row naming the kind beats one that does not, then a row naming the reason
// beats one that does not, then a row naming the state. ok is false when no
// row matches, which only a sealed state with no refusal reason produces.
func guidanceFor(kind string, state domain.SourceState, reason string) (sourceGuidance, bool) {
	var (
		best      sourceGuidance
		bestScore = -1
	)
	for _, g := range sourceGuidanceTable {
		if (g.kind != "" && g.kind != kind) || (g.state != "" && g.state != state) || (g.reason != "" && g.reason != reason) {
			continue
		}
		score := 0
		if g.kind != "" {
			score += 4
		}
		if g.reason != "" {
			score += 2
		}
		if g.state != "" {
			score++
		}
		if score > bestScore {
			best, bestScore = g, score
		}
	}
	return best, bestScore >= 0
}

func renderGuidance(text, kind string) string {
	return strings.ReplaceAll(text, "{source}", eventsource.Label(kind))
}

// SourceNote is the one-line note for kind's manifest entry, "Linear:
// unavailable this run — …", and "" for an entry that needs none (a sealed
// source). It is per-run data: it belongs in a run's <run_context>, never in
// an agentprompt block.
func SourceNote(kind string, e domain.SourceEntry) string {
	g, ok := guidanceFor(kind, e.State, e.Reason)
	if !ok || g.note == "" {
		return ""
	}
	return eventsource.Label(kind) + ": " + renderGuidance(g.note, kind)
}

// SourceNotes is SourceNote for every source m names, in its kind order,
// leaving out those that need none.
func SourceNotes(m domain.SourceManifest) []string {
	var out []string
	for _, kind := range m.Kinds() {
		if note := SourceNote(kind, m[kind]); note != "" {
			out = append(out, note)
		}
	}
	return out
}

// SourceRefusal is a verb's refusal for want of a usable credential for a
// source, keyed by the source, the state it is in and the reason within that
// state. Its text is the refusal table's: an agent reads the same words for
// the same situation whichever mode it runs in and whichever gate refused.
type SourceRefusal struct {
	Kind   string
	State  domain.SourceState
	Reason string
	cause  error
}

func (e *SourceRefusal) Error() string {
	if g, ok := guidanceFor(e.Kind, e.State, e.Reason); ok && g.refusal != "" {
		return renderGuidance(g.refusal, e.Kind)
	}
	return eventsource.Label(e.Kind) + " is unavailable to this run"
}

// Unwrap exposes what the refusal stands for: eventsource.ErrDisabled for a
// source an admin turned off, so a caller can tell that apart from an unbound
// credential without matching prose, and the error it explains, if any.
func (e *SourceRefusal) Unwrap() []error {
	var out []error
	if e.State == domain.SourceDisabled {
		out = append(out, eventsource.ErrDisabled)
	}
	if e.cause != nil {
		out = append(out, e.cause)
	}
	return out
}

// UnresolvedReason is the manifest reason an unresolved source records for the
// error its credential resolve failed with: a token endpoint that did not
// renew an access token, or a failure of any other kind.
func UnresolvedReason(err error) string {
	if linearoauth.TokenEndpointUnavailable(err) {
		return domain.SourceReasonTokenEndpointUnavailable
	}
	return domain.SourceReasonResolveFailed
}

// LiveSourceManifest builds the manifest a run with no sealed bundle (local
// mode) reads, from the org's live availability answer. Every credential
// there is resolved live at the moment a verb uses it, so an available source
// is recorded as sealed — its credential is as reachable as a sealed one —
// and an unbuilt source is left out, as the brain leaves it out.
func LiveSourceManifest(avail eventsource.Availability) domain.SourceManifest {
	m := make(domain.SourceManifest, len(avail))
	for _, src := range avail {
		var state domain.SourceState
		switch src.State {
		case eventsource.StateAvailable:
			state = domain.SourceSealed
		case eventsource.StateUnconfigured:
			state = domain.SourceUnconfigured
		case eventsource.StateDisabled:
			state = domain.SourceDisabled
		case eventsource.StateUnlicensed:
			state = domain.SourceUnlicensed
		default:
			continue
		}
		m[src.Kind] = domain.SourceEntry{State: state}
	}
	return m
}

// proxyMissState reads a source credential proxy's miss answer (credmiss) off
// a Jira or Linear client error: the state it names, when it is a miss for
// kind. The proxy answers with a JSON body, so the client reports it as an
// HTTP status error carrying that body.
func proxyMissState(err error, kind string) (domain.SourceState, bool) {
	var body string
	var (
		le *linearclient.StatusError
		je *jiraclient.StatusError
	)
	switch {
	case errors.As(err, &le):
		body = le.Body
	case errors.As(err, &je):
		body = je.Body
	default:
		return "", false
	}
	var miss struct {
		Reason string `json:"reason"`
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(body), &miss) != nil || miss.Source != kind {
		return "", false
	}
	return credbundle.StateForMissReason(miss.Reason)
}
