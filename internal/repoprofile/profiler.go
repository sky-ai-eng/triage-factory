package repoprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/repoevent"
	"github.com/sky-ai-eng/triage-factory/internal/reporename"
	"github.com/sky-ai-eng/triage-factory/internal/syslimit"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

const (
	profileBatchSize = 5
	maxDocChars      = 10000
	reprofileTTL     = 3 * 24 * time.Hour // skip repos profiled within the last 3 days
)

// llmResolveFunc is the RunOptions.LLMResolver shape the profiler's Haiku
// calls carry — the brain-side llmcred adapter minting short-lived STS creds
// for a role-mode Bedrock org (nil in local/tests → Run's built-in
// resolution).
type llmResolveFunc func(ctx context.Context, orgID string) (map[string]string, error)

// Profiler builds and persists AI-generated profiles for GitHub repositories.
type Profiler struct {
	resolver   github.Resolver         // per-(org, owner) GitHub client source — App-installation token in multi, keychain PAT in local.
	secrets    agentproc.SecretsReader // per-org LLM-credential reader for the profiling Haiku calls (nil in local → ambient subscription; system-door reader in multi).
	llmResolve llmResolveFunc          // brain-side role-aware LLM resolver (nil in local/tests).
	repos      db.RepositoryStore      // profile reads + upserts go through the store
	orgs       db.OrgsStore            // iterate active orgs at the top of each profile run
	recorder   *systemllm.Recorder     // captures per-batch LLM cost + tokens into system_llm_runs (TFAC-451)
	ws         *websocket.Hub
	// notify publishes repository_updated over ws. Built org-scoped (the
	// local shape) in NewProfiler; SetRecipients rebuilds it with the
	// multi-mode per-user fan-out.
	notify *repoevent.Notifier
	// recipients is the audience resolver SetRecipients captured (nil in
	// local), kept alongside notify because the batch-failure toast needs
	// it directly: its body names repo slugs, which are visibility-scoped
	// data just like the event payloads.
	recipients repoevent.RecipientsFunc

	// batchFn runs one Haiku profiling batch. Defaulted (in NewProfiler) to a
	// closure over profileBatch that captures the recorder + system-job
	// limiter, so the field signature stays free of both and test overrides
	// need no change. Overridable so tests can exercise the with-docs upsert /
	// fallback paths without spawning a real agent subprocess.
	batchFn func(ctx context.Context, orgID string, batch []repoWithDocs, secrets agentproc.SecretsReader) ([]repoProfileResult, error)
}

// NewProfiler creates a Profiler with the given GitHub resolver, per-org
// secrets reader, repo store, orgs store, recorder, shared system-job limiter,
// and WS hub. The resolver is consulted per (org, owner) inside the profiling
// loop so the same code path serves both local (keychain PAT) and multi (App
// token). The limiter (nil → unlimited) bounds concurrent profiling sandboxes
// against the other background jobs; it is captured by batchFn alongside the
// recorder.
func NewProfiler(resolver github.Resolver, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, repos db.RepositoryStore, orgs db.OrgsStore, recorder *systemllm.Recorder, limiter *syslimit.Limiter, ws *websocket.Hub) *Profiler {
	p := &Profiler{resolver: resolver, secrets: secrets, llmResolve: llmResolve, repos: repos, orgs: orgs, recorder: recorder, ws: ws}
	p.notify = repoevent.NewNotifier(ws, nil)
	p.batchFn = func(ctx context.Context, orgID string, batch []repoWithDocs, secrets agentproc.SecretsReader) ([]repoProfileResult, error) {
		return profileBatch(ctx, orgID, batch, secrets, llmResolve, recorder, limiter)
	}
	return p
}

// SetRecipients switches the profiler's repository_updated broadcasts
// AND its repo-naming toasts from the org-scoped local shape to the
// multi-mode per-user fan-out: repositories is org-wide but its REST
// read is visibility-scoped, and the websocket hub has no team axis, so
// the audience must be resolved per emission (see internal/repoevent).
// Multi-mode wiring only, called once at boot before any Trigger —
// local mode never calls it, keeping the single org-wide broadcast N=1
// has always had.
func (p *Profiler) SetRecipients(recipients repoevent.RecipientsFunc) {
	p.recipients = recipients
	p.notify = repoevent.NewNotifier(p.ws, recipients)
}

// repoWithDocs groups a repository row with the documentation text to send to the LLM.
type repoWithDocs struct {
	profile domain.Repository
	docs    string
}

// fireBatchFailureToast surfaces a genuinely failed profiling batch to
// the users who can see its repos. Local (no recipients resolver): one
// org-wide toast naming every repo in the batch — N=1, nothing to scope.
// Multi: the slugs in the body are visibility-scoped data, so resolve
// each repo's audience and send each affected user a toast naming only
// the repos they can see; a user outside every repo's audience gets
// nothing. A per-repo resolution failure skips that repo's slug (fail
// closed, same posture as repoevent.Notifier) — the row itself was still
// saved by the fallback upsert either way.
func (p *Profiler) fireBatchFailureToast(ctx context.Context, orgID string, batch []repoWithDocs) {
	const bodyFmt = "Profiling failed for %s — rows saved without AI summary"
	if p.recipients == nil {
		names := make([]string, len(batch))
		for i, d := range batch {
			names[i] = d.profile.Slug()
		}
		toast.Warning(p.ws, orgID, fmt.Sprintf(bodyFmt, strings.Join(names, ", ")))
		return
	}
	perUser := map[string][]string{}
	for _, d := range batch {
		uids, err := p.recipients(ctx, orgID, d.profile.Owner, d.profile.Repo)
		if err != nil {
			repoprofileLog.Error("resolve toast audience failed; omitting repo from failure toast", "repo", d.profile.Slug(), "error", err)
			continue
		}
		for _, uid := range uids {
			perUser[uid] = append(perUser[uid], d.profile.Slug())
		}
	}
	for uid, names := range perUser {
		toast.FireUser(p.ws, orgID, uid, toast.LevelWarning, "", fmt.Sprintf(bodyFmt, strings.Join(names, ", ")))
	}
}

// Run iterates active orgs and profiles each one's configured repos.
// Per-org outer loop: resolves the per-org repo list inside the loop
// so a new org added between boot and the next forced re-profile
// picks up correctly. Local mode
// collapses to N=1 — the single runmode.LocalDefaultOrgID sentinel.
//
// If force is true, the TTL check is skipped (used for manual
// re-profile triggered by a GitHub config change).
//
// Per-org errors are logged but do not abort the whole run. The
// returned error is only non-nil if context is cancelled mid-flight;
// individual-org failures degrade gracefully (row writes still happen
// where possible, falls back to docs-only).
//
// Run is the all-orgs sweep retained for tests and ad-hoc backfill; the
// running server profiles per-org through the Manager's Runner (driven by
// the system:poll: "profiler" subscriber), which calls RunOrg directly.
func (p *Profiler) Run(ctx context.Context, force bool) error {
	orgIDs, err := p.orgs.ListActiveSystem(ctx)
	if err != nil {
		return fmt.Errorf("list active orgs: %w", err)
	}
	for _, orgID := range orgIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := p.RunOrg(ctx, orgID, force); err != nil {
			if ctx.Err() != nil {
				return err
			}
			repoprofileLog.Error("profile org failed", "org", orgID, "error", err)
		}
	}
	return nil
}

// RunOrg profiles a single org's configured repos. It is the per-org unit
// the Manager's per-org Runner drives off system:poll: sentinels (each
// carries evt.OrgID) and the explicit re-profile button. It resolves the
// org's configured repo list, then reuses runOrg — including its TTL skip,
// per-repo client resolution + skip-on-error, and the "fetch error leaves
// the row untouched for retry" contract (TFAC-331). force bypasses the
// 3-day TTL. The returned bool reports whether any profile row was written
// this cycle (see runOrg) — the Runner uses it to skip the bare-clone
// bootstrap on a no-op TTL-skip cycle.
func (p *Profiler) RunOrg(ctx context.Context, orgID string, force bool) (bool, error) {
	repos, err := p.repos.ListTrackedNamesSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("load configured repos: %w", err)
	}
	return p.runOrg(ctx, orgID, repos, force)
}

// runOrg profiles one org's repos. Per-repo failures are logged and
// the loop continues — partial progress is better than aborting the
// whole batch on a transient GitHub API failure or a malformed repo
// name. Cross-org repo dedup at the GitHub fetch layer (two orgs
// both polling owner/repo doing two fetches) is a deferred concern
// — local mode is N=1 so there's no immediate dedup pressure.
// The returned bool reports whether this cycle *successfully persisted* any
// profile row — at least one repo passed the TTL + fetch gate and had its
// clone metadata written by an UpsertSystem that returned nil. A
// steady-state cycle where every repo is TTL-skipped (or where every write
// failed) returns false, letting the caller skip the post-profile bare-clone
// bootstrap (and its per-repo WS broadcasts) when nothing landed on disk.
func (p *Profiler) runOrg(ctx context.Context, orgID string, repos []string, force bool) (bool, error) {
	if len(repos) == 0 {
		return false, nil
	}

	// Debug, not Info: this fires every poll cycle regardless of outcome
	// (the steady state is every repo TTL-skipped). The outcome logs below
	// promote to Info only when the cycle actually wrote something.
	repoprofileLog.Debug("profiling configured repos", "org", orgID, "repos", len(repos))

	// touched flips true the first time a repo's row is *successfully*
	// upserted this cycle (any of the three write sites below). A failed
	// write leaves it false so the caller doesn't bootstrap bare clones for
	// a clone_url that never landed.
	touched := false

	// Resolve the clone protocol once for the whole run rather than
	// re-reading per-org settings inside the per-repo loop. The setting
	// can't change mid-run — handleSettingsPost serializes the org-
	// settings write behind the same `onGitHubChanged` callback that
	// owns this goroutine — so capturing it here matches actual
	// semantics and avoids N redundant DB reads.
	preferSSH := false
	if orgSet, oErr := p.orgs.GetSettingsSystem(ctx, orgID); oErr != nil {
		repoprofileLog.Warn("load org settings to pick clone protocol failed, defaulting to https", "org", orgID, "error", oErr)
	} else {
		preferSSH = orgSet.GitHubCloneProtocol == "ssh"
	}

	var withDocs []repoWithDocs
	var withoutDocs []domain.Repository

	for _, name := range repos {
		if err := ctx.Err(); err != nil {
			return touched, err
		}

		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 {
			repoprofileLog.Warn("skipping malformed repo name", "repo", name)
			continue
		}
		owner, repo := parts[0], parts[1]

		// One read of the stored row, serving two purposes: the TTL gate
		// (skipped when forced) and the registry id the websocket events
		// below are keyed on. Ref-keyed, because name came out of the tracked
		// set and is the same name the GitHub fetches are built from; the
		// rename applied further down moves owner/repo and leaves the id
		// alone, so an id read here stays the right one either way.
		existing, existingErr := p.repos.GetByRefSystem(ctx, orgID, domain.RepoRef{Owner: owner, Repo: repo})
		switch {
		case existingErr != nil && !force:
			// Without the row the TTL gate cannot be evaluated, and profiling
			// every repo every cycle is exactly what it exists to prevent.
			repoprofileLog.Warn("check existing profile failed, skipping", "repo", name, "error", existingErr)
			continue
		case existingErr != nil:
			// A forced pass has no gate to fail, so it proceeds — minus the
			// live updates, which have no id to be keyed on.
			repoprofileLog.Warn("read existing repository row failed; forced pass continues without live updates", "repo", name, "error", existingErr)
		case !force && existing != nil && existing.ProfiledAt != nil:
			if age := time.Since(*existing.ProfiledAt); age < reprofileTTL {
				repoprofileLog.Debug("recently profiled, skipping", "repo", name, "age", age.Round(time.Hour), "ttl", reprofileTTL)
				continue
			}
		}
		// Empty when the read failed, or when no row exists yet — the upsert
		// below is what creates one, and a repo first learned about on this
		// pass has nothing for a client to merge into anyway. Events keyed on
		// nothing merge into nothing, so they are dropped rather than sent.
		repositoryID := ""
		if existing != nil {
			repositoryID = existing.ID
		}

		// Resolve the per-(org, owner) GitHub client. Done after the TTL
		// skip so we don't mint a token for a repo we're about to skip.
		client, cerr := p.resolver.ClientFor(ctx, orgID, owner)
		if cerr != nil {
			repoprofileLog.Warn("resolve github client failed, skipping", "repo", name, "error", cerr)
			continue
		}

		// GetFileContent returns ("", nil) only for a genuine 404 — a real
		// fetch failure (401/403, network, 5xx) comes back as a non-nil error
		// (internal/github/events.go). GetRepoMeta likewise errors on any
		// non-2xx. Collect all four outcomes so we can tell "the repo
		// genuinely lacks this file" from "we couldn't reach GitHub."
		readme, readmeErr := client.GetFileContent(ctx, owner, repo, "README.md")
		claudeMd, claudeErr := client.GetFileContent(ctx, owner, repo, "CLAUDE.md")
		agentsMd, agentsErr := client.GetFileContent(ctx, owner, repo, "AGENTS.md")
		meta, metaErr := client.GetRepoMeta(ctx, owner, repo)

		// TFAC-331 root cause 2: error ≠ absence. If ANY fetch failed, skip
		// the repo entirely — no upsert, no broadcast, no docs
		// classification — leaving the existing row untouched so the next
		// profiler run retries. Persisting false has_* flags here would turn a
		// transient or auth failure into a durable "this repo has no docs"
		// that suppresses AI profiling forever. Genuine 404s (nil error, empty
		// content) fall through; their false flags are then correct.
		if ferr := errors.Join(readmeErr, claudeErr, agentsErr, metaErr); ferr != nil {
			repoprofileLog.Warn("doc fetch failed, leaving row untouched for retry", "repo", name, "error", ferr)
			continue
		}

		// All fetches succeeded (metaErr nil ⇒ meta non-nil), which makes this
		// the point where the profiler knows what GitHub currently calls the
		// repository: a request for a renamed one redirects, so the response's
		// full_name answers a question the request never asked. Applying it
		// here rather than after the upsert is what keeps the upsert landing
		// on the moved row instead of minting a second one under the old name.
		//
		// This is the rename path a PAT org has. The poller's App-grant
		// listing sees the same fact within a cycle, but only for App orgs;
		// here it costs one field on a response already in hand, bounded by
		// the profile TTL.
		if moved, ok := p.applyRenameFromMeta(ctx, orgID, name, meta); ok {
			name = moved
			owner, repo, _ = strings.Cut(name, "/")
		}

		// Both HTTPS and SSH clone forms come back from the same
		// /repos/:owner/:repo response, so picking is a one-line branch. Empty
		// SSHURL (legacy GHE deployments without ssh_url surfaced) falls back
		// to HTTPS so we always have *some* URL on the row.
		defaultBranch := meta.DefaultBranch
		cloneURL := meta.CloneURL
		if preferSSH && meta.SSHURL != "" {
			cloneURL = meta.SSHURL
		}

		// ID is carried, not written: UpsertSystem keys on the identity
		// columns below and ignores it. It rides along because prof travels
		// into the batch path, and both emit sites key their event on it.
		prof := domain.Repository{
			ID:     repositoryID,
			Owner:  owner,
			Repo:   repo,
			Source: domain.RepoSourceGitHub,
			// The repository id GitHub just sent on the same response the
			// clone URL and default branch came from. This is the only place
			// TF learns it — it is not worth a request of its own, and a row
			// without one behaves exactly as it always has — so a repo whose
			// meta fetch failed (skipped above) or that is inside its profile
			// TTL simply keeps whatever id it already had.
			ExternalID:    meta.ExternalID(),
			HasReadme:     readme != "",
			HasClaudeMd:   claudeMd != "",
			HasAgentsMd:   agentsMd != "",
			CloneURL:      cloneURL,
			DefaultBranch: defaultBranch,
		}

		// A repo with no docs is fully inspected by this fetch — there's
		// nothing to send to the LLM, so stamp profiled_at now to gate it
		// behind the TTL. Without this, a docs-less repo has profiled_at=nil
		// forever and gets all four files re-fetched every poll cycle (the
		// profiler now runs per-cycle, not just on config change). Repos WITH
		// docs are stamped in the batch path below — and only on a successful
		// run, so a transient batch failure still retries next cycle
		// (error ≠ absence, the TFAC-331 invariant).
		docs := buildDocText(readme, claudeMd, agentsMd)
		if docs == "" {
			now := time.Now()
			prof.ProfiledAt = &now
		}

		// Persist docs flags immediately so the UI can show them before profiling completes
		if err := p.repos.UpsertSystem(ctx, orgID, prof); err != nil {
			repoprofileLog.Error("upsert docs flags failed", "repo", name, "error", err)
		} else {
			touched = true
		}
		if prof.ID != "" {
			p.notify.Publish(ctx, orgID, repoevent.Update{
				ID:          prof.ID,
				Slug:        prof.Slug(),
				HasReadme:   repoevent.Ptr(prof.HasReadme),
				HasClaudeMd: repoevent.Ptr(prof.HasClaudeMd),
				HasAgentsMd: repoevent.Ptr(prof.HasAgentsMd),
			})
		}

		if docs == "" {
			withoutDocs = append(withoutDocs, prof)
		} else {
			withDocs = append(withDocs, repoWithDocs{profile: prof, docs: docs})
		}
	}

	// with_docs==without_docs==0 means every repo was TTL-skipped or
	// errored — the routine no-op case, so Debug; a nonzero count means at
	// least one repo was freshly fetched this cycle.
	if len(withDocs) > 0 || len(withoutDocs) > 0 {
		repoprofileLog.Info("doc scan complete", "with_docs", len(withDocs), "without_docs", len(withoutDocs))
	} else {
		repoprofileLog.Debug("doc scan complete", "with_docs", len(withDocs), "without_docs", len(withoutDocs))
	}

	// Batch-profile repos that have docs through Haiku.
	profiled := 0
	for i := 0; i < len(withDocs); i += profileBatchSize {
		if err := ctx.Err(); err != nil {
			return touched, err
		}

		end := i + profileBatchSize
		if end > len(withDocs) {
			end = len(withDocs)
		}
		batch := withDocs[i:end]

		results, err := p.batchFn(ctx, orgID, batch, p.secrets)
		if err != nil {
			// A provider-backoff skip (systemllm's circuit breaker) is an
			// anticipated, self-healing deferral, not a genuine failure —
			// log it quietly and skip the user-facing toast so a boot-time
			// overload doesn't read as a wall of errors.
			backoff := systemllm.IsProviderBackoff(err)
			if backoff {
				repoprofileLog.Info("profile batch deferred; provider backing off, retrying next cycle", "batch", i/profileBatchSize+1)
			} else {
				repoprofileLog.Error("profile batch failed", "batch", i/profileBatchSize+1, "error", err)
			}
			if !backoff {
				p.fireBatchFailureToast(ctx, orgID, batch)
			}
			// Fallback: upsert without profile_text so the row at least exists.
			for _, d := range batch {
				if uErr := p.repos.UpsertSystem(ctx, orgID, d.profile); uErr != nil {
					repoprofileLog.Error("upsert fallback failed", "repo", d.profile.Slug(), "error", uErr)
				} else {
					touched = true
				}
			}
			continue
		}

		byRepo := make(map[string]string, len(results))
		for _, r := range results {
			byRepo[r.Repo] = r.Profile
		}

		now := time.Now()
		for _, d := range batch {
			prof := d.profile
			if text := byRepo[prof.Slug()]; text != "" {
				prof.ProfileText = text
			}
			// Stamp on every successful batch result, even when the model
			// returned empty text: the batch completing IS a finished
			// inspection, so gate it behind the TTL. (A FAILED batch took the
			// fallback path above, which leaves profiled_at unset so the repo
			// retries next cycle — error ≠ absence.)
			prof.ProfiledAt = &now
			if err := p.repos.UpsertSystem(ctx, orgID, prof); err != nil {
				repoprofileLog.Error("upsert profile failed", "repo", prof.Slug(), "error", err)
				continue
			}
			touched = true
			if prof.ProfileText != "" {
				profiled++
				if prof.ID == "" {
					continue
				}
				p.notify.Publish(ctx, orgID, repoevent.Update{
					ID:          prof.ID,
					Slug:        prof.Slug(),
					ProfileText: repoevent.Ptr(prof.ProfileText),
				})
			}
		}
	}

	// touched mirrors the caller-visible "did this cycle write anything"
	// signal (see the function doc) — the same steady-state-vs-real-work
	// split as doc scan complete above.
	if touched {
		repoprofileLog.Info("profile run done", "profiled_with_ai", profiled, "without_docs", len(withoutDocs))
	} else {
		repoprofileLog.Debug("profile run done", "profiled_with_ai", profiled, "without_docs", len(withoutDocs))
	}
	return touched, nil
}

// applyRenameFromMeta applies a rename this metadata response revealed and
// reports the repository's current slug.
//
// tracked is the slug the request was made under; meta.FullName is what GitHub
// answered with. Differing means the repository was renamed (or transferred —
// same condition, since the id survives both). ok=false is the ordinary case:
// the names agree, GitHub sent no id or name to compare, or the rename could
// not be applied — and the caller then profiles under the name it already had,
// which still resolves through GitHub's redirect.
func (p *Profiler) applyRenameFromMeta(ctx context.Context, orgID, tracked string, meta *github.RepoMeta) (string, bool) {
	id := meta.ExternalID()
	if meta.FullName == "" || id == "" || domain.SameRepoSlug(meta.FullName, tracked) {
		return "", false
	}
	owner, repo, ok := strings.Cut(meta.FullName, "/")
	if !ok || owner == "" || repo == "" {
		return "", false
	}
	applied := reporename.Apply(ctx, p.repos, p.resolver, repoprofileLog, orgID, []domain.RepoRef{{
		Source: domain.RepoSourceGitHub, Owner: owner, Repo: repo, ExternalID: id,
	}})
	if applied == 0 {
		return "", false
	}
	return meta.FullName, true
}

// repoProfileInput is the per-repo JSON sent to the LLM.
type repoProfileInput struct {
	Repo string `json:"repo"`
	Docs string `json:"docs"`
}

// repoProfileResult is one entry in the LLM's JSON array response.
type repoProfileResult struct {
	Repo    string `json:"repo"`
	Profile string `json:"profile"`
}

func profileBatch(ctx context.Context, orgID string, batch []repoWithDocs, secrets agentproc.SecretsReader, llmResolve llmResolveFunc, recorder *systemllm.Recorder, limiter *syslimit.Limiter) ([]repoProfileResult, error) {
	inputs := make([]repoProfileInput, len(batch))
	for i, d := range batch {
		inputs[i] = repoProfileInput{
			Repo: d.profile.Slug(),
			Docs: d.docs,
		}
	}

	inputJSON, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("marshal batch: %w", err)
	}

	// Bound concurrent background sandboxes across all orgs + jobs. A
	// cancelled ctx returns the error into the existing batch-failure path
	// (fallback upsert without AI summary, row left for retry). A nil limiter
	// is an unlimited no-op (used by tests).
	if err := limiter.Acquire(ctx); err != nil {
		return nil, err
	}
	defer limiter.Release()

	// Complete owns the mode branch: local mode runs the shared agent
	// runtime exactly as before (Message, the combined instructions+data
	// prompt); multi mode calls the org's configured Anthropic/Bedrock
	// provider directly, with the instructions as the system prompt and just
	// the repo batch as the user turn. We only parse the returned text's
	// JSON array below; cost + tokens land in system_llm_runs via Complete.
	// LLMResolver routes a role-mode Bedrock org through internal/llmcred so
	// the direct call is signed with a minted short-lived STS session
	// credential (TFAC-616); nil for local/ambient.
	result, err := recorder.Complete(ctx, systemllm.CompleteOptions{
		OrgID:        orgID,
		Job:          systemllm.JobRepoProfiler,
		Message:      fmt.Sprintf(repoProfilePrompt, string(inputJSON)),
		SystemPrompt: repoProfileSystemPrompt,
		UserMessage:  fmt.Sprintf(repoProfileUserPrompt, string(inputJSON)),
		Model:        ai.SystemJobModel,
		DirectModel:  ai.SystemJobModelDirect,
		MaxTokens:    16384,
		Temperature:  0.1,
		TraceID:      "repoprofile-batch",
		Secrets:      secrets,
		LLMResolver:  llmResolve,
		Metadata:     map[string]any{"repo_count": len(batch)},
	})
	if err != nil {
		return nil, fmt.Errorf("repoprofile agent failed: %w", err)
	}

	raw := ai.StripCodeFences([]byte(result.Text))

	var results []repoProfileResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("parse response: %w, raw: %s", err, string(raw))
	}

	return results, nil
}

// buildDocText concatenates available documentation for a repo into a single
// block to send to the LLM. Returns empty string if no docs were found.
func buildDocText(readme, claudeMd, agentsMd string) string {
	var parts []string
	if readme != "" {
		parts = append(parts, "README.md:\n"+truncateStr(readme, maxDocChars))
	}
	if claudeMd != "" {
		parts = append(parts, "CLAUDE.md:\n"+truncateStr(claudeMd, maxDocChars))
	}
	if agentsMd != "" {
		parts = append(parts, "AGENTS.md:\n"+truncateStr(agentsMd, maxDocChars))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
