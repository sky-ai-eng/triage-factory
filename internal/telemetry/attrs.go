package telemetry

import "go.opentelemetry.io/otel/attribute"

// The shared span-attribute vocabulary. Every key TF puts on a span that
// isn't owned by an instrumentation library (otelhttp's http.*, otelsql's
// db.*) is declared here, and every one of them is an opaque identifier or
// a closed enum.
//
// That restriction is the point of centralizing them. Spans leave the
// process — to a collector, then to a backend an operator may not run
// themselves — so an attribute is a data-egress decision, not a
// cardinality preference. Repo names, PR and issue titles, branch names,
// usernames, emails, commit subjects, message text, and file paths are all
// tenant data and none of them belongs on a span. The reverse join is what
// makes that affordable: a UUID here plus the same UUID in TF's own
// database answers "which repo was that" for someone who is already
// entitled to the answer, and answers nothing for anyone who isn't.
//
// Keys are OTel dot form and unprefixed. Adding one is a deliberate act —
// see the test in attrs_test.go, which pins the exported set so a new
// helper has to be argued for rather than appear.
const (
	keyOrgID          = attribute.Key("org.id")
	keyTeamID         = attribute.Key("team.id")
	keyEventID        = attribute.Key("event.id")
	keyEventType      = attribute.Key("event.type")
	keyEntityID       = attribute.Key("entity.id")
	keyTaskID         = attribute.Key("task.id")
	keyConversationID = attribute.Key("conversation.id")
	keyClaimAttempt   = attribute.Key("claim.attempt")
	keySource         = attribute.Key("source")
	keyDisposition    = attribute.Key("disposition")
	keyOutcome        = attribute.Key("outcome")
	keyRuntime        = attribute.Key("runtime")
	keyAttempt        = attribute.Key("attempt")
	keyCount          = attribute.Key("count")
	keyJob            = attribute.Key("job")
	keyProvider       = attribute.Key("provider")
	keyTransport      = attribute.Key("transport")
)

// OrgID tags a span with the tenant it ran for. The single most useful
// attribute in the whole set: nearly every pipeline in TF fans out per-org,
// so this is what turns "the poll cycle was slow" into "the poll cycle was
// slow for one tenant."
func OrgID(id string) attribute.KeyValue { return keyOrgID.String(id) }

// TeamID tags a span with the team scope it ran under.
func TeamID(id string) attribute.KeyValue { return keyTeamID.String(id) }

// EventID and EventType tag a span with the event that drove it. The type
// is a closed set — it is FK-constrained to the events catalog — so it is
// safe as a span dimension in a way no other event field is.
func EventID(id string) attribute.KeyValue     { return keyEventID.String(id) }
func EventType(kind string) attribute.KeyValue { return keyEventType.String(kind) }

// EntityID tags a span with the PR or issue it concerns. The opaque row id,
// never the repo/number pair or the title — those name the thing to anyone
// reading, which is exactly what this vocabulary refuses to do.
func EntityID(id string) attribute.KeyValue { return keyEntityID.String(id) }

// TaskID and ConversationID tag a span with the unit of work it belongs to.
func TaskID(id string) attribute.KeyValue         { return keyTaskID.String(id) }
func ConversationID(id string) attribute.KeyValue { return keyConversationID.String(id) }

// ClaimAttempt tags a span with which engagement attempt produced it, so a
// retried run's spans are distinguishable from its first try's.
func ClaimAttempt(n int) attribute.KeyValue { return keyClaimAttempt.Int(n) }

// Source names the upstream a span talks to — "github", "jira". A closed
// set: it is the poller/tracker's own dimension, not a hostname.
func Source(name string) attribute.KeyValue { return keySource.String(name) }

// Disposition names what a stage decided to do with its input (routed,
// deduped, filtered). Closed enum per call site.
func Disposition(value string) attribute.KeyValue { return keyDisposition.String(value) }

// Outcome names how a span finished. Its whole reason to exist is the
// distinction between "failed" and "correctly declined to do anything":
// provider backoff, a quiet-skip, a dedup hit are all anticipated, so they
// belong here rather than in an error status, which stays reserved for
// things that are actually wrong.
func Outcome(value string) attribute.KeyValue { return keyOutcome.String(value) }

// Runtime names which agent loop drove a run — "sdk" or "native".
func Runtime(value string) attribute.KeyValue { return keyRuntime.String(value) }

// Attempt counts retries within one logical operation, so a span that
// spent four minutes in backoff is distinguishable from one that spent
// four minutes in a single slow request.
func Attempt(n int) attribute.KeyValue { return keyAttempt.Int(n) }

// Count is a span's cardinality of work — PRs refreshed, events emitted,
// rows swept. A bare number, never a list of what was counted.
func Count(n int) attribute.KeyValue { return keyCount.Int(n) }

// Job names which background system job a span belongs to — "scorer",
// "profiler", "classifier". A closed set of three, fixed at their call
// sites.
func Job(name string) attribute.KeyValue { return keyJob.String(name) }

// Provider names an LLM upstream — "anthropic", "bedrock". Closed set,
// derived from which credential shape resolved; never a hostname or a
// model id.
func Provider(name string) attribute.KeyValue { return keyProvider.String(name) }

// Transport names how a call reached its upstream when a subsystem has
// more than one way to get there — the system-LLM path's "subprocess" (the
// SDK child process) versus "direct" (an in-process API call). Closed set,
// and already this codebase's word for the same distinction in metric
// labels.
func Transport(name string) attribute.KeyValue { return keyTransport.String(name) }
