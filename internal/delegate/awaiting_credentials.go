package delegate

import "time"

// awaitingCredentialsTimeout is how long the executor waits for the brain to
// seal a run's credential bundle before giving up. Brain down at claim time is
// the case this bounds: the credential-sidecar bring-up fails, the run's setup
// requeues, and it re-requests from scratch — no work is lost, just delayed a
// start the brain couldn't have serviced anyway. Read via awaitingCredentialsKnobs
// (overridable in tests via SetAwaitingCredentialsTimeout).
const awaitingCredentialsTimeout = 2 * time.Minute

// awaitingCredentialsPollInterval is how often the wait re-checks
// claim_credentials. A doorbell (the cred_request tf_ctl notification fired by
// MarkAwaitingCredentials) makes the common case near-instant; this poll is the
// backstop for a dropped notification, bounded by awaitingCredentialsTimeout
// either way.
const awaitingCredentialsPollInterval = 500 * time.Millisecond
