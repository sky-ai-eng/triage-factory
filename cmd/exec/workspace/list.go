package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// listOutput is the JSON shape printed by `workspace list`. Two sections:
//
//   - available: every repo configured in Triage Factory the agent COULD
//     `workspace add`. Sourced from the repo_profiles table. Empty list
//     means no repos are configured (delegated agents shouldn't ever see
//     this since the spawner gates on profile readiness, but the shape
//     stays consistent).
//   - materialized: repos the agent has already `workspace add`'d for
//     this run, with the absolute worktree path and the feature branch
//     `git worktree add` checked out.
//
// Two-section structure (rather than a flat list with a `materialized`
// boolean) keeps the field shape per entry uniform — available entries
// don't carry a path, materialized entries don't need a default branch
// — and makes the "what could I add vs. what have I added" split
// obvious to a reader skimming the JSON.
type listOutput struct {
	Available    []listAvailable    `json:"available"`
	Materialized []listMaterialized `json:"materialized"`
}

type listAvailable struct {
	Repo string `json:"repo"`
	// Description is the upstream-sourced one-liner from the repo's
	// profile (GitHub repo metadata, captured during profiling). Helps
	// the agent disambiguate between configured repos when the ticket
	// text doesn't make the target obvious. Empty for repos whose
	// profiling hasn't run yet (skeleton rows in repo_profiles).
	//
	// We deliberately omit profile_text — it's multi-KB of LLM-
	// generated prose, would burn meaningful context on every list
	// call, and can be stale (regenerated only on GitHub config
	// change). Description is the cheap, authoritative signal.
	Description string `json:"description,omitempty"`
}

type listMaterialized struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// listWorkspaces is the orchestration body of `workspace list`,
// extracted from runList so it returns errors instead of os.Exit-ing.
// Mirrors the runAdd / materializeWorkspace split for testability.
//
// Run-agnostic (TFAC-498), mirroring materializeWorkspace: it serves any run
// — Jira, GitHub, or taskless — since `workspace add` now does too. It only
// needs the run identity (for scoping the materialized list) plus the
// org-configured repos, not the task.
//
// All reads route through the agenthost client — in local mode the
// LocalClient hits the SQLite store directly, in sandbox mode the
// IPCClient round-trips to the host daemon. The TRIAGE_FACTORY_RUN_ID
// validation happens inside host.LookupRun.
func listWorkspaces(host agenthost.Client) (listOutput, error) {
	ctx := context.Background()
	info, err := host.LookupRun(ctx)
	if err != nil {
		return listOutput{}, translateLookupErr("workspace list", "", err)
	}

	run, err := host.GetAgentRun(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load run: %w", err)
	}
	if run == nil {
		return listOutput{}, fmt.Errorf("%w: %s", errRunNotFound, info.RunID)
	}

	configured, err := host.ListRepos(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load configured repos: %w", err)
	}
	rows, err := host.ListRunWorktrees(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load materialized worktrees: %w", err)
	}

	materializedSet := make(map[string]struct{}, len(rows))
	materialized := make([]listMaterialized, 0, len(rows))
	for _, r := range rows {
		materializedSet[r.RepoID] = struct{}{}
		materialized = append(materialized, listMaterialized{
			Repo:   r.RepoID,
			Path:   r.Path,
			Branch: r.FeatureBranch,
		})
	}

	// Available = configured-and-profilable minus already-materialized.
	// Skeleton rows in repo_profiles (added to the configured list
	// but not yet profiled — clone_url is empty) are filtered out:
	// `workspace add` would reject them later with "no clone URL on
	// its profile" anyway, so surfacing them here as discoverable
	// options would just send the agent toward unusable choices.
	// Materialized filter keeps the list framed as "what's still
	// unmaterialized," matching the agent's mental model.
	available := make([]listAvailable, 0, len(configured))
	for _, p := range configured {
		if p.CloneURL == "" {
			continue
		}
		if _, alreadyAdded := materializedSet[p.ID]; alreadyAdded {
			continue
		}
		available = append(available, listAvailable{
			Repo:        p.ID,
			Description: p.Description,
		})
	}

	return listOutput{Available: available, Materialized: materialized}, nil
}

// runList is the CLI entrypoint: env → listWorkspaces → stdout/stderr.
func runList(host agenthost.Client, _ []string) {
	out, err := listWorkspaces(host)
	if err != nil {
		exitErr(err.Error())
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "workspace list: encode: "+err.Error())
		os.Exit(1)
	}
}
