package app

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
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
//   - modelFor resolves the run's team model configuration (per-(org, team)):
//     the default an unset step inherits, and the enable-set every model the
//     run touches has to belong to. A prompt's own Model still overrides the
//     default, but not the set.
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
	a.modelFor = func(ctx context.Context, orgID, teamID string) (domain.TeamModels, error) {
		return resolveAIModelForTeam(ctx, a.stores, orgID, teamID)
	}
	// The deployment App — the one shared key a managed workspace rides — is
	// operator environment config, read here once for this process's background
	// consumers: the resolver (so the grant pass, the pollers and the profiler
	// mint tier-2 installation tokens for a managed org exactly as the server's
	// request path does) and the managed installation-set refresh, which lists
	// the App's installations under it. The zero App in local mode and on a
	// deployment whose orgs all bring their own key; a malformed configuration
	// is logged and treated as none, the same failing-closed handling the
	// server applies, so both readers hold the same answer.
	deployment, err := githubapp.DeploymentAppFromEnv()
	if err != nil {
		appLog.Error("read deployment github app from the environment failed; managed-class orgs will resolve nothing in background passes",
			"error", err)
		deployment = githubapp.DeploymentApp{}
	}
	a.deploymentApp = deployment
	a.ghResolver = ghclient.NewResolver(a.stores.Secrets, a.stores.GitHubApps, a.stores.Orgs, a.stores.Agents, nil,
		ghclient.WithDeploymentApp(deployment))
	return nil
}

// resolveAIModelForTeam resolves the model configuration a specific team runs
// under: the default an unset step inherits, paired with the enable-set every
// model that team dispatches is held to.
//
// teamID is the run's owning team. A multi-team org can have teams with
// different DefaultModel settings, so resolving from the run's own team (not the
// org default) honors each team's choice. An empty teamID falls back to the
// org's default team.
//
// It resolves; it does not judge the default. Whether the set still includes it
// is TeamModels.RequireDefault's answer, asked wherever the default is actually
// USED — which keeps a pinned step, and a mid-flight blueprint of pinned steps,
// out of a refusal about a value it never reads.
//
// A read that does not answer IS a refusal here: the check's input is missing,
// which is not the check passing, and dispatching through it would spend on a
// model nobody can show is enabled. Nothing substitutes on the way — TF ships no
// fallback model. In-flight work is untouched either way; this decides new
// claims only.
func resolveAIModelForTeam(ctx context.Context, stores db.Stores, orgID, teamID string) (domain.TeamModels, error) {
	if teamID == "" {
		var err error
		teamID, err = stores.Teams.GetDefaultForOrgSystem(ctx, orgID)
		if err != nil {
			return domain.TeamModels{}, fmt.Errorf("resolve the org's default team: %w", err)
		}
		if teamID == "" {
			return domain.TeamModels{}, fmt.Errorf("organization %s has no default team to resolve a model from", orgID)
		}
	}
	teamSet, err := stores.Teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		return domain.TeamModels{}, fmt.Errorf("read team settings: %w", err)
	}
	orgSet, err := stores.Orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return domain.TeamModels{}, fmt.Errorf("read org settings: %w", err)
	}

	universe := modelcatalog.UniverseFor(runmode.Current() == runmode.ModeMulti)
	return domain.NewTeamModels(teamSet.DefaultModel, domain.TeamModelSet(teamSet.EnabledModels,
		domain.OrgModelSet(orgSet.EnabledModels, universe.DefaultEnabled()))), nil
}
