// Package modelprobe answers, for one org and one model, whether the org's
// credentials can actually invoke it — by invoking it.
//
// The question has no uniform control-plane answer. Anthropic's /v1/models
// reflects the key's access, so there it would work. Bedrock's
// ListFoundationModels reports every model the REGION carries, not the ones
// the account was granted, and none of its filters touch entitlement; its
// per-model GetFoundationModelAvailability exists but is one call per model,
// needs its own IAM action on a different service endpoint, and answers a
// weaker question (an agreement record across four axes) than "can this
// credential call this id right now". It also lists base model ids while the
// current Claude models can only be invoked through inference profiles, so the
// ids you can enumerate and the ids you can call are different sets.
//
// A probe costs the same number of calls, needs no permission the run does not
// already need — if the probe fails, the run would have failed too — works
// identically for every vendor instead of needing an integration each, and
// tests the exact string that will go on the wire.
//
// What it costs is real money, which is why nothing here runs on a timer, at
// boot, or on a poll cycle: every probe is caused by a person, and the surface
// that causes one says so first.
package modelprobe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// probeTimeout bounds one probe. A one-token completion that has not answered
// in this long is not going to say anything about entitlement, and a sweep of
// a provider's candidates runs them one after another inside a single HTTP
// request — so the bound is what keeps a wedged upstream from holding the
// admin's browser open for minutes to reach the same inconclusive it would
// reach here.
const probeTimeout = 15 * time.Second

// probeMessage is the whole prompt. The question is whether the request is
// ACCEPTED, so the cheapest content that is still a valid turn is the right
// content; nothing reads the answer.
const probeMessage = "ping"

// probeMaxTokens caps the completion at one token. Anthropic requires
// max_tokens, and the request is billed on what it generates, so this is the
// floor of what the probe can cost.
const probeMaxTokens = 1

// Result is one probe's conclusion.
type Result struct {
	// Verdict is green, red, or inconclusive. Only the first two are stored.
	Verdict Verdict
	// Detail is the provider's own message on a non-green verdict, carried so
	// an admin reads WHY rather than just that. Empty on green.
	Detail string
}

// Prober runs availability probes for an org against its own credentials.
//
// It holds the same two credential seams every other brain-side LLM caller
// does — the raw secret reader and the role-aware resolver — because a
// role-mode Bedrock org stores no key at all and must mint an STS triple for
// this call exactly as a run would. Probing through a different path than runs
// use would test something other than what runs do.
type Prober struct {
	secrets  agentproc.SecretsReader
	resolver func(ctx context.Context, orgID, model string) (map[string]string, error)
	recorder *systemllm.Recorder
}

// New wires a Prober. resolver may be nil, in which case credentials come from
// the secret store directly (the same fallback every agentproc caller has).
// recorder may be nil, which only means the probe's spend goes unrecorded —
// the probe itself still runs, because a ledger outage must not be able to
// block an admin from establishing what their credentials can do.
func New(secrets agentproc.SecretsReader, resolver func(ctx context.Context, orgID, model string) (map[string]string, error), recorder *systemllm.Recorder) *Prober {
	return &Prober{secrets: secrets, resolver: resolver, recorder: recorder}
}

// Probe spends one minimal request establishing whether orgID's credentials
// can invoke entry.
//
// The returned error is for the cases where the question was never asked at
// all: the org has not connected this model's provider, or its stored material
// cannot be assembled into a request. Those are not verdicts — nothing is
// recorded for them and the caller reports a setup gap, not an unavailable
// model. Every outcome of an attempt that DID reach the wire comes back as a
// Result, including the failures.
func (p *Prober) Probe(ctx context.Context, orgID string, entry modelcatalog.Entry) (Result, error) {
	creds, err := p.resolveCredentials(ctx, orgID, entry.Key)
	if err != nil {
		return Result{}, err
	}
	// The whitelist names exactly the model under test. bifrost reads an empty
	// whitelist as "no models", and a key allowed to serve something else
	// would let a probe pass on a model nobody asked about.
	pc, err := inference.ProviderCredentialsFromEnv(creds, []string{entry.Key})
	if err != nil {
		return Result{}, fmt.Errorf("modelprobe: %w", err)
	}
	if got := string(pc.Provider); got != entry.Provider {
		// Resolution is keyed by the model, so this cannot happen from
		// configuration alone — it would mean the resolver and the catalog
		// disagree about which provider serves this key, and probing the wrong
		// one would record a verdict about a credential path nobody asked
		// about.
		return Result{}, fmt.Errorf("modelprobe: %s resolved %s credentials but the catalog serves it through %s",
			entry.Key, got, entry.Provider)
	}

	account, err := inference.NewAccount(pc)
	if err != nil {
		return Result{}, fmt.Errorf("modelprobe: %w", err)
	}
	client, err := inference.New(account)
	if err != nil {
		return Result{}, fmt.Errorf("modelprobe: %w", err)
	}
	// Released after the call, on its own goroutine: shutting bifrost down
	// waits for its in-flight worker, and on a timed-out probe that worker is
	// still parked in a read only the provider or the transport can end.
	defer func() { go client.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	startedAt := time.Now().UTC()
	completion, callErr := client.Stream(callCtx, inference.Request{
		Provider: pc.Provider,
		// The exact catalog key, never a configured override: the whole
		// advantage of a probe over a control-plane lookup is that it tests
		// the string a run will actually send.
		Model: entry.Key,
		// One user row and no system prompt. A system prefix would earn a
		// cache write on tokens no later request can match, and the probe has
		// nothing to instruct.
		Rows:                          []domain.Message{{Role: "user", Content: probeMessage}},
		MaxTokens:                     probeMaxTokens,
		NoConversationCacheBreakpoint: true,
	})
	p.record(ctx, orgID, entry.Key, startedAt, completion, callErr)

	// The classifier is handed the OUTER ctx as well as the error, so a probe
	// the caller abandoned is inconclusive rather than being read as whatever
	// the transport happened to report on the way down.
	verdict, detail := classify(ctx, callErr)
	if verdict == VerdictGreen {
		return Result{Verdict: VerdictGreen}, nil
	}
	return Result{Verdict: verdict, Detail: detail}, nil
}

// resolveCredentials resolves the org's LLM env map for this model: through the
// role-aware seam when one is wired (minting for a role-mode Bedrock org),
// else from the secret store. Both are keyed by the model, so what comes back
// is the material for THIS model's provider and not whichever the org
// configured first.
func (p *Prober) resolveCredentials(ctx context.Context, orgID, model string) (map[string]string, error) {
	if p.resolver != nil {
		return p.resolver(ctx, orgID, model)
	}
	return agentproc.ResolveCredentialsForBundle(ctx, p.secrets, orgID, model)
}

// record lands the probe's spend in system_llm_runs under job=probe, so it
// appears under system_overhead in usage like every other call TF makes for
// itself. A probe is tiny — two tokens — but it is a real charge on the org's
// bill, and an unexplained charge is exactly what the confirm dialog exists to
// prevent.
//
// Priced on the catalog key, which is guaranteed to carry a price: the catalog
// join drops any entry the datasheet cannot price, so a model that reached
// here has one.
func (p *Prober) record(ctx context.Context, orgID, modelKey string, startedAt time.Time, completion *inference.Completion, callErr error) {
	if p.recorder == nil {
		return
	}
	var (
		usage   systemllm.DirectUsage
		traceID string
		cost    float64
	)
	if completion != nil {
		usage = systemllm.DirectUsageFrom(completion.Usage)
		traceID = completion.ID
		cost, _ = inference.CostForUsage(modelKey, completion.Usage)
	}
	p.recorder.RecordDirect(ctx, systemllm.Call{
		OrgID:     orgID,
		Job:       systemllm.JobProbe,
		Model:     modelKey,
		StartedAt: startedAt,
	}, traceID, usage, cost, int(time.Since(startedAt).Milliseconds()), callErr != nil)
}

// IsSetupGap reports whether err is a Probe error meaning the org never
// connected this model's provider, as opposed to a fault in TF. The two need
// different answers from a caller — one names a thing an admin does in
// Settings, the other is a bug — and neither is a verdict about the model.
func IsSetupGap(err error) bool {
	return errors.Is(err, agentproc.ErrProviderNotConfigured) ||
		errors.Is(err, agentproc.ErrNoCredentialsConfigured) ||
		errors.Is(err, inference.ErrNoCredentials)
}
