package telemetry

import "go.opentelemetry.io/otel/attribute"

// The shared span-attribute vocabulary: every key TF sets itself (as
// opposed to otelhttp's http.* and otelsql's db.*), each an opaque
// identifier or a closed enum.
//
// Centralizing them is what makes that restriction enforceable. Spans
// leave the process for a backend the operator may not run, so an
// attribute is a data-egress decision, not a cardinality preference — repo
// names, titles, usernames, and file paths are tenant data and belong
// nowhere near one. A UUID plus TF's own database answers "which repo was
// that" for someone already entitled to the answer, and nobody else.
//
// attrs_test.go pins the exported set, so a new helper has to be argued
// for rather than appear.
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

// Opaque row identifiers, the backbone of the set. Each is the id and
// nothing else — never the repo/number pair, the title, or the login that
// would name the thing to a reader. org.id in particular is what turns
// "the poll cycle was slow" into "slow for one tenant", since nearly every
// pipeline in TF fans out per-org.
func OrgID(id string) attribute.KeyValue          { return keyOrgID.String(id) }
func TeamID(id string) attribute.KeyValue         { return keyTeamID.String(id) }
func EventID(id string) attribute.KeyValue        { return keyEventID.String(id) }
func EntityID(id string) attribute.KeyValue       { return keyEntityID.String(id) }
func TaskID(id string) attribute.KeyValue         { return keyTaskID.String(id) }
func ConversationID(id string) attribute.KeyValue { return keyConversationID.String(id) }

// EventType is FK-constrained to the events catalog, which is what makes
// it safe as a dimension in a way no other event field is.
func EventType(kind string) attribute.KeyValue { return keyEventType.String(kind) }

// Closed enums, each fixed by its call sites:
//
//   - Source — the upstream a span talks to ("github", "jira"). The
//     poller/tracker's own dimension, not a hostname.
//   - Disposition — what a stage decided to do with its input (routed,
//     deduped, filtered).
//   - Runtime — which agent loop drove a run ("sdk", "native").
//   - Job — which background system job a span belongs to ("scorer",
//     "profiler", "classifier").
//   - Provider — an LLM vendor ("anthropic", "bedrock"), derived from
//     which credential shape resolved; never a hostname or a model id.
//   - Transport — how a call reached its upstream where a subsystem has
//     more than one route (the system-LLM path's "subprocess" vs "direct").
func Source(name string) attribute.KeyValue       { return keySource.String(name) }
func Disposition(value string) attribute.KeyValue { return keyDisposition.String(value) }
func Runtime(value string) attribute.KeyValue     { return keyRuntime.String(value) }
func Job(name string) attribute.KeyValue          { return keyJob.String(name) }
func Provider(name string) attribute.KeyValue     { return keyProvider.String(name) }
func Transport(name string) attribute.KeyValue    { return keyTransport.String(name) }

// Outcome names how a span finished. It exists to separate "failed" from
// "correctly declined to do anything" — provider backoff, a quiet-skip, a
// dedup hit — so the error status stays reserved for things actually
// wrong, and "traces with errors" stays a usable filter.
func Outcome(value string) attribute.KeyValue { return keyOutcome.String(value) }

// Counters. Attempt separates a span that spent four minutes in backoff
// from one slow request; ClaimAttempt says which engagement attempt
// produced a span; Count is a span's cardinality of work (PRs refreshed,
// events emitted) — a bare number, never a list of what was counted.
func Attempt(n int) attribute.KeyValue      { return keyAttempt.Int(n) }
func ClaimAttempt(n int) attribute.KeyValue { return keyClaimAttempt.Int(n) }
func Count(n int) attribute.KeyValue        { return keyCount.Int(n) }
