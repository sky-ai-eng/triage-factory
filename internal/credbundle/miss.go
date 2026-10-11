package credbundle

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The reasons a credential proxy can fail to produce a credential for one
// request. A sidecar token source wraps the one that applies, so the proxy
// answering the request can name the reason without reading the error's text,
// which may carry credential-setup detail that neither the jailed caller nor
// the log may see.
var (
	ErrNoBundle      = errors.New("credbundle: no bundle")
	ErrNoRepoToken   = errors.New("credbundle: no token for this repository")
	ErrNoCLIToken    = errors.New("credbundle: no gh CLI token")
	ErrTokenExpiring = errors.New("credbundle: token expired or about to expire")
)

// SourceMissError is a request to a source's credential proxy that the held
// bundle has no usable credential for, named by the source and the state its
// manifest entry is in. The state is the whole explanation the proxy gives;
// the entry's reason stays with the bundle, where the agenthost reads it.
type SourceMissError struct {
	Kind  string
	State domain.SourceState
}

func (e *SourceMissError) Error() string {
	return fmt.Sprintf("credbundle: no %s credential (%s)", e.Kind, e.State)
}

// SourceMiss builds the miss for kind from its manifest entry: the entry's
// state, or unconfigured when the manifest names no entry for kind — a bundle
// that does not mention a source carries no credential for it.
func SourceMiss(m domain.SourceManifest, kind string) error {
	state := domain.SourceUnconfigured
	if e, ok := m.Entry(kind); ok && e.State != "" {
		state = e.State
	}
	return &SourceMissError{Kind: kind, State: state}
}

// The closed vocabulary MissReason answers in. Every value is safe to show the
// jailed agent and to log: none carries a credential, a repository, or any
// other tenant data.
const (
	MissNoBundle      = "no_bundle"
	MissNoRepoToken   = "no_repo_token"
	MissNoCLIToken    = "no_cli_token"
	MissTokenExpiring = "token_expiring"
	MissOther         = "other"

	// One reason per source state, for a SourceMissError. The source itself
	// travels beside the reason (MissSource), never inside it.
	MissSourceUnresolved   = "source_unresolved"
	MissSourceUnconfigured = "source_unconfigured"
	MissSourceDisabled     = "source_disabled"
	MissSourceUnlicensed   = "source_unlicensed"
	MissSourceWithheld     = "source_withheld"
)

// missReasonForState maps a source state to its miss reason. A sealed state
// never misses, so it, like any value outside the vocabulary, is MissOther.
var missReasonForState = map[domain.SourceState]string{
	domain.SourceUnresolved:   MissSourceUnresolved,
	domain.SourceUnconfigured: MissSourceUnconfigured,
	domain.SourceDisabled:     MissSourceDisabled,
	domain.SourceUnlicensed:   MissSourceUnlicensed,
	domain.SourceWithheld:     MissSourceWithheld,
}

// MissReason maps a credential-lookup error to the closed vocabulary above. An
// error wrapping none of the sentinels is MissOther.
func MissReason(err error) string {
	var sm *SourceMissError
	switch {
	case errors.As(err, &sm):
		if r, ok := missReasonForState[sm.State]; ok {
			return r
		}
		return MissOther
	case errors.Is(err, ErrNoBundle):
		return MissNoBundle
	case errors.Is(err, ErrNoRepoToken):
		return MissNoRepoToken
	case errors.Is(err, ErrNoCLIToken):
		return MissNoCLIToken
	case errors.Is(err, ErrTokenExpiring):
		return MissTokenExpiring
	default:
		return MissOther
	}
}

// MissSource is the source a credential-lookup error names, "" for one that
// names none.
func MissSource(err error) string {
	var sm *SourceMissError
	if errors.As(err, &sm) {
		return sm.Kind
	}
	return ""
}

// StateForMissReason is MissReason's inverse for the source reasons: the state
// a miss reason names, and false for a reason that names no source state.
func StateForMissReason(reason string) (domain.SourceState, bool) {
	for state, r := range missReasonForState {
		if r == reason {
			return state, true
		}
	}
	return "", false
}
