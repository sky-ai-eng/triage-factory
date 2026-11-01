package domain

import (
	"slices"
	"time"
)

// SourceState is one event source's standing in a single executor engagement:
// whether the brain sealed that source's credential into the engagement's
// bundle and, when it did not, why not. It is eventsource's availability
// vocabulary with `available` split by what happened at the seal, since an
// available source whose credential could not be fetched is a different fact
// from one whose credential is in hand.
type SourceState string

const (
	// SourceSealed — the source's credential is in this engagement's bundle.
	SourceSealed SourceState = "sealed"
	// SourceUnresolved — the org has the source, but the brain could not
	// resolve its credential on this seal. The refresh sweep re-seals an
	// engagement in this state, so it can become sealed mid-run.
	SourceUnresolved SourceState = "unresolved"
	// SourceUnconfigured — the org has no credential for the source.
	SourceUnconfigured SourceState = "unconfigured"
	// SourceDisabled — an org admin turned the source off.
	SourceDisabled SourceState = "disabled"
	// SourceUnlicensed — the org is not entitled to the source.
	SourceUnlicensed SourceState = "unlicensed"
	// SourceWithheld — policy excluded the source from this engagement. Reserved:
	// nothing writes it yet.
	SourceWithheld SourceState = "withheld"
)

// Usable reports whether an engagement in this state is given the means to
// reach the source: a sealed credential, or a credential the next re-seal may
// bring. The sidecar starts a credential proxy for exactly these states, and a
// run's <tools> section documents exactly these sources.
func (s SourceState) Usable() bool {
	return s == SourceSealed || s == SourceUnresolved
}

// The closed vocabulary of SourceEntry.Reason. Every value is safe to show the
// jailed agent, an operator and a log: none carries a credential or tenant
// data.
const (
	// SourceReasonTokenEndpointUnavailable — the source's OAuth token endpoint
	// failed or could not be reached, so an access token could not be renewed.
	SourceReasonTokenEndpointUnavailable = "token_endpoint_unavailable"
	// SourceReasonTokenExpired — the sealed access token has passed its expiry.
	// Derived by a reader holding the bundle, from SourceEntry.ExpiresAt; the
	// brain never seals a token it already knows is expired.
	SourceReasonTokenExpired = "token_expired"
	// SourceReasonResolveFailed — the credential could not be read for any
	// other reason.
	SourceReasonResolveFailed = "resolve_failed"
)

// SourceEntry is one source's line in a SourceManifest.
type SourceEntry struct {
	State SourceState `json:"state"`
	// Reason refines State from the closed vocabulary above, empty when the
	// state says everything.
	Reason string `json:"reason,omitempty"`
	// ExpiresAt is when the sealed credential stops working, for a sealed
	// source whose credential expires: the earliest expiry among what was
	// sealed for it. Nil for a credential with no expiry and for every other
	// state. The refresh sweep re-seals an engagement whose sealed credential
	// is close to it.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// AccountID and AccountLogin are the account the sealed credential acts
	// as, for a sealed source whose seal learned it. On the GitHub entry they
	// are the bot user of the App the engagement's tokens were minted from,
	// the author of every pull request the run opens: GitHub's id for it,
	// empty where the App's bot id is not stored, and its "<slug>[bot]"
	// login. The reconciler checks a pull request's author against them
	// before it treats the pull request as the run's. Both empty for every
	// other source and state, and for a seal that could not learn the account.
	AccountID    string `json:"account_id,omitempty"`
	AccountLogin string `json:"account_login,omitempty"`
}

// SourceManifest is the record of which source credentials one executor
// engagement holds, keyed by event-source kind. The brain writes it on every
// seal, from the same pass that resolves the credentials: onto the engagement's
// claims row, where it outlives the engagement, and as a copy inside the sealed
// bundle for the sidecar, which has no database. A source TF has declared but
// not built is never in it.
//
// It explains and it never gates. The credential funnel an exec verb resolves
// through and the live off-switch check stay authoritative; when they and the
// manifest disagree, they win and the manifest supplies only the explanation.
type SourceManifest map[string]SourceEntry

// Entry returns kind's entry and whether the manifest has one.
func (m SourceManifest) Entry(kind string) (SourceEntry, bool) {
	e, ok := m[kind]
	return e, ok
}

// EntryFor returns kind's entry, or an unconfigured one when the manifest names
// none: a manifest that does not mention a source carries no credential for
// it. Every reader that explains a missing credential from the manifest starts
// here, so an unnamed source is explained the same way everywhere.
func (m SourceManifest) EntryFor(kind string) SourceEntry {
	if e, ok := m[kind]; ok && e.State != "" {
		return e
	}
	return SourceEntry{State: SourceUnconfigured}
}

// Usable reports whether kind's entry is in a usable state (SourceState.Usable).
// A kind the manifest does not name is not usable.
func (m SourceManifest) Usable(kind string) bool {
	e, ok := m[kind]
	return ok && e.State.Usable()
}

// Kinds returns the manifest's kinds in sorted order, so every rendering of
// one manifest lists its sources the same way.
func (m SourceManifest) Kinds() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// UsableKinds returns the kinds in a usable state, sorted.
func (m SourceManifest) UsableKinds() []string {
	var out []string
	for _, k := range m.Kinds() {
		if m[k].State.Usable() {
			out = append(out, k)
		}
	}
	return out
}

// At returns the manifest as it stands at now: a sealed entry whose credential
// has expired reads as unresolved for SourceReasonTokenExpired, since the
// bundle still carries it but nothing it reaches will accept it, and the next
// re-seal is what replaces it. Every other entry is returned unchanged. The
// receiver is not modified.
func (m SourceManifest) At(now time.Time) SourceManifest {
	if m == nil {
		return nil
	}
	out := make(SourceManifest, len(m))
	for k, e := range m {
		if e.State == SourceSealed && e.ExpiresAt != nil && !now.Before(*e.ExpiresAt) {
			e = SourceEntry{State: SourceUnresolved, Reason: SourceReasonTokenExpired}
		}
		out[k] = e
	}
	return out
}
