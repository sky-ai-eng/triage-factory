package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// buildRunCredentials wires the per-org run-credential seam shared by
// every AI feature. Both modes resolve a run's LLM key + default model
// through these, so the event → router → task → delegation chain stops
// branching on mode.
//
//   - runSecrets reads a run's per-org LLM credential. Multi mode uses the
//     system/admin door (these runs are claims-free background work, and
//     the orgID always originates from the run/entity/task, never user
//     input). Local mode keeps it nil → the agent runs unsandboxed and
//     inherits the host's ambient Claude subscription.
//
//     TODO(TFAC-888): that nil is why a local org's own bound Anthropic key
//     is never read by a run — agentproc.anthropicEnv is reachable only with
//     a reader, so the key is validated, stored, and then ignored while the
//     subprocess authenticates from the operator's environment. Wiring a
//     reader here is the fix, and it changes who pays for a local user
//     holding both a key and a subscription.
//
//   - modelFor resolves the run's team default model (per-(org, team),
//     capped by the org max tier). A prompt's own Model still overrides it.
//
//   - ghResolver picks the right GitHub credential (App-installation token
//     → org PAT) per (org, target). Shared by the poller, spawner, and repo
//     profiler.
func (a *App) buildRunCredentials() error {
	if !a.local() {
		a.runSecrets = agentproc.NewSystemSecretsReader(a.stores.Secrets)
		// Shared LLM-credential resolver (internal/llmcred, TFAC-616): the one
		// seam every brain-side Bedrock/Anthropic resolution flows through.
		// Multi mode only — local keeps the ambient path (runSecrets nil), so
		// role mode there simply degrades to the host subscription. Env knobs
		// (mint TTL + executor egress binding) are validated here so a typo
		// fails boot rather than shipping an unbound or always-failing mint.
		ttl, err := llmcred.TTLFromEnv()
		if err != nil {
			return err
		}
		egress, err := llmcred.NetworkBindingFromEnv()
		if err != nil {
			return err
		}
		a.llmResolver = llmcred.NewResolver(a.runSecrets, llmcred.NewAWSMinter(), ttl, egress)
	}
	// One ledger recorder per process, for every call TF makes on its own
	// behalf: the three background jobs and the availability probe. Built here
	// rather than at either use site because it also carries the shared
	// provider circuit breaker the background jobs coordinate through, and two
	// of those would be two independent opinions about whether the upstream is
	// down.
	a.llmRecorder = systemllm.NewRecorder(a.stores.SystemLLMRuns)
	a.modelFor = func(ctx context.Context, orgID, teamID string) string {
		return resolveAIModelForTeam(ctx, a.stores, orgID, teamID)
	}
	a.ghResolver = ghclient.NewResolver(a.stores.Secrets, a.stores.GitHubApps, a.stores.Orgs, a.stores.Agents, nil)
	return nil
}

// resolveAIModelForTeam looks up the model a specific team uses for
// delegation, clamped by the org's max-tier cap (domain.EffectiveModel).
//
// teamID is the run's owning team. A multi-team org can have teams with
// different DefaultModel settings, so resolving from the run's own team
// (not the org default) honors each team's choice. An empty teamID falls
// back to the org's default team.
//
// Falls back to the shipped default model on any error so a transient DB
// hiccup doesn't silently clear the spawner's credentials.
func resolveAIModelForTeam(ctx context.Context, stores db.Stores, orgID, teamID string) string {
	multi := runmode.Current() == runmode.ModeMulti
	fallback := domain.DefaultModelFor(multi)
	if teamID == "" {
		var err error
		teamID, err = stores.Teams.GetDefaultForOrgSystem(ctx, orgID)
		if err != nil || teamID == "" {
			if err != nil {
				appLog.Warn("resolve default team failed; using default model", "org", orgID, "error", err, "model", fallback)
			}
			return fallback
		}
	}
	teamSet, err := stores.Teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		appLog.Warn("read team settings failed; using default model", "team", teamID, "error", err, "model", fallback)
		return fallback
	}

	var maxTier string
	if orgSet, err := stores.Orgs.GetSettingsSystem(ctx, orgID); err != nil {
		appLog.Warn("read org settings failed; applying no model cap", "org", orgID, "error", err)
	} else {
		maxTier = orgSet.MaxLLMModelTier
	}

	model, _ := domain.EffectiveModel(teamSet.DefaultModel, maxTier, multi)
	return model
}
