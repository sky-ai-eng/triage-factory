package slack

import "errors"

// Transport identifies which delivery mechanism a connected workspace uses
// to receive events — the ingest leaf's dispatch key. Declared here (not
// event-related, so no domain.EventSchema involvement) because it's purely
// a connect-time credential-inference concept.
const (
	transportSocket    = "socket"
	transportEventsAPI = "events_api"
)

var (
	// errTransportAmbiguous fires when both a signing secret and an app
	// token were supplied without an explicit transport choice — the one
	// case connect can't infer on its own (decided, not reopened: see
	// TFAC-529's "Decisions already made").
	errTransportAmbiguous = errors.New("both a signing secret and an app-level token were supplied; specify transport explicitly (\"socket\" or \"events_api\")")
	// errTransportMissingCredential fires when neither credential was
	// supplied — there is nothing to infer from.
	errTransportMissingCredential = errors.New("provide either a signing secret (events_api) or an app-level token (socket)")
)

// inferTransport resolves the transport for a connect request from which
// credentials were supplied, per the fixed rule table:
//
//	app token only            -> socket
//	signing secret only       -> events_api
//	both, explicit choice     -> that choice
//	both, no explicit choice  -> error (ambiguous)
//	neither                   -> error (nothing to infer from)
//
// explicit is consulted ONLY in the both-supplied case — a single-credential
// request is unambiguous regardless of what (if anything) explicit carries,
// so an explicit value that just confirms the obvious choice is harmless and
// one that contradicts it is silently irrelevant: the credential shape is
// the source of truth for what the workspace can actually be reached on.
func inferTransport(hasSigningSecret, hasAppToken bool, explicit string) (string, error) {
	switch {
	case hasSigningSecret && hasAppToken:
		switch explicit {
		case transportSocket, transportEventsAPI:
			return explicit, nil
		default:
			return "", errTransportAmbiguous
		}
	case hasAppToken:
		return transportSocket, nil
	case hasSigningSecret:
		return transportEventsAPI, nil
	default:
		return "", errTransportMissingCredential
	}
}
