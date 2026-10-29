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
//   - available: every repo the run's team tracks that the agent COULD
//     `workspace add`. Sourced from the repositories table through the
//     team's tracked set, the same gate `workspace add` applies. Empty list
//     means the team tracks no repos (delegated agents shouldn't ever see
//     this since the spawner gates on profile readiness, but the shape
//     stays consistent). A repo the run already has a checkout of stays
//     here: `add` is keyed per checkout, so the base branch beside a PR
//     checkout is still an `add` away.
//   - materialized: the checkouts this run has, with the absolute
//     worktree path and the ref each was materialized for ("default",
//     "pr-<N>", or "ref-<branch>").
//
// Two-section structure (rather than a flat list with a `materialized`
// boolean) keeps the field shape per entry uniform — available entries
// don't carry a path, materialized entries carry path + ref — and keeps a
// repo listed once in "available" however many checkouts of it exist.
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
	// profiling hasn't run yet (skeleton rows in repositories).
	//
	// We deliberately omit profile_text — it's multi-KB of LLM-
	// generated prose, would burn meaningful context on every list
	// call, and can be stale (regenerated only on GitHub config
	// change). Description is the cheap, authoritative signal.
	Description string `json:"description,omitempty"`
}

type listMaterialized struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	// Ref is the materialization selector the worktree was created for:
	// "default", "pr-<N>", or "ref-<branch>". It is the checkout
	// intent, not the agent's live working branch (the agent creates its own
	// branch off the checkout; the push gate authorizes that live branch).
	Ref string `json:"ref"`
}

// listWorkspaces is the orchestration body of `workspace list`,
// extracted from runList so it returns errors instead of os.Exit-ing.
// Mirrors the runAdd / materializeWorkspace split for testability.
//
// Conversation-agnostic (TFAC-498), mirroring materializeWorkspace: it serves any
// conversation — Jira, GitHub, or taskless — since `workspace add` does too. It only
// needs the conversation identity (for scoping the materialized list) plus the
// repos its team tracks, not the task.
//
// All reads route through the agenthost client — in local mode the
// LocalClient hits the SQLite store directly, in sandbox mode the
// IPCClient round-trips to the host daemon. The TRIAGE_FACTORY_CONVERSATION_ID
// validation happens inside host.LookupConversation.
func listWorkspaces(host agenthost.Client) (listOutput, error) {
	ctx := context.Background()
	info, err := host.LookupConversation(ctx)
	if err != nil {
		return listOutput{}, translateLookupErr("workspace list", "", err)
	}

	conv, err := host.GetConversation(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load conversation: %w", err)
	}
	if conv == nil {
		return listOutput{}, fmt.Errorf("%w: %s", errConversationNotFound, info.ConversationID)
	}

	tracked, err := host.ListRepos(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load tracked repos: %w", err)
	}
	rows, err := host.ListConversationWorktrees(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: load materialized worktrees: %w", err)
	}

	// conversation_worktrees paths are recorded in HOST view (the push gate and the
	// workspace snapshot read them host-side); the agent needs cd-able paths in
	// ITS namespace, so translate through the run-root pair (identity in local
	// mode, /work-prefixed in the sandbox). TFAC-546.
	hostRoot, agentRoot, err := host.WorkspaceRoots(ctx)
	if err != nil {
		return listOutput{}, fmt.Errorf("workspace list: resolve run root: %w", err)
	}

	materialized := make([]listMaterialized, 0, len(rows))
	for _, r := range rows {
		materialized = append(materialized, listMaterialized{
			Repo: r.RepoID,
			Path: agenthost.AgentViewPath(hostRoot, agentRoot, r.Path),
			Ref:  r.Ref,
		})
	}

	// Available = tracked-and-profilable, whether or not the run already has
	// a checkout of it: hiding a materialized repo reads as "nothing more to
	// add here" to an agent that needs a second checkout of it.
	// Skeleton rows in repositories (tracked but not yet profiled —
	// clone_url is empty) are filtered out:
	// `workspace add` would reject them later with "no clone URL on
	// its profile" anyway, so surfacing them here as discoverable
	// options would just send the agent toward unusable choices.
	available := make([]listAvailable, 0, len(tracked))
	for _, p := range tracked {
		if p.CloneURL == "" {
			continue
		}
		available = append(available, listAvailable{
			Repo:        p.Slug(),
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
