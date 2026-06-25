package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BranchRef identifies one branch on a repository for an existence check.
// Owner and Repo are the GitHub coordinates; Branch is the short name (no
// "refs/heads/" prefix).
type BranchRef struct {
	Owner  string
	Repo   string
	Branch string
}

// branchRefBatchSize caps how many aliased repository.ref(...) lookups go into
// a single GraphQL call. Each alias is ~3 nodes (repository + ref + id), so the
// static node budget is trivial; the cap keeps the query text and the variable
// count bounded. 50 aliases ⇒ 150 variables, comfortably within limits.
const branchRefBatchSize = 50

// BranchesExist batch-checks whether each branch ref still exists on GitHub,
// aliasing one repository(owner,name){ ref(qualifiedName:"refs/heads/<b>") }
// field per input. It is the reconciler's branch-freshness probe (TFAC-464):
// the tracker has no branch discovery and throttles PR refreshes, so the
// reconciler queries refs itself rather than leaning on tracker snapshots.
//
// The returned map answers only the refs it is CONFIDENT about:
//
//   - true  → the ref exists.
//   - false → the repository resolved but the ref did not — the branch is gone.
//
// A ref whose repository did NOT resolve (deleted/renamed repo, or a per-field
// access error that GitHub returns as a null field + errors[]) is OMITTED, not
// reported false: a permission or transient failure must never be read as
// "branch deleted" and flip a branch artifact terminal. Callers treat a missing
// key as "unknown — leave the artifact as-is".
//
// Batches internally to stay under GitHub's per-query limits; transparent to
// callers. A genuine whole-query failure (auth, cost ceiling, malformed)
// propagates; per-field nulls degrade to the partial result PostGraphQL passes
// through.
func (c *Client) BranchesExist(refs []BranchRef) (map[BranchRef]bool, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make(map[BranchRef]bool, len(refs))
	for i := 0; i < len(refs); i += branchRefBatchSize {
		end := i + branchRefBatchSize
		if end > len(refs) {
			end = len(refs)
		}
		if err := c.branchesExistBatch(refs[i:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// branchesExistBatch is the single-call implementation for one batch. It writes
// confident answers into out; unknown (repository-null) aliases are left absent.
func (c *Client) branchesExistBatch(refs []BranchRef, out map[BranchRef]bool) error {
	var decls, fields strings.Builder
	vars := make(map[string]any, len(refs)*3)
	for i, r := range refs {
		ov, nv, qv := fmt.Sprintf("o%d", i), fmt.Sprintf("n%d", i), fmt.Sprintf("q%d", i)
		if i > 0 {
			decls.WriteString(", ")
		}
		fmt.Fprintf(&decls, "$%s: String!, $%s: String!, $%s: String!", ov, nv, qv)
		// Alias r<i> keys the field back to refs[i]; values ride GraphQL
		// variables so a branch name with quotes/specials needs no escaping.
		fmt.Fprintf(&fields, "  r%d: repository(owner: $%s, name: $%s) { ref(qualifiedName: $%s) { id } }\n", i, ov, nv, qv)
		vars[ov] = r.Owner
		vars[nv] = r.Repo
		vars[qv] = "refs/heads/" + r.Branch
	}
	query := fmt.Sprintf("query(%s) {\n%s}", decls.String(), fields.String())

	data, err := c.PostGraphQL(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("branch existence check: %w", err)
	}

	// Decode the aliased fields generically: r<i> → { ref: {id} | null } | null.
	// A JSON-null repository alias decodes to a nil pointer (key present, value
	// nil) — the "unknown" case we omit from out.
	var resp struct {
		Data map[string]*struct {
			Ref *struct {
				ID string `json:"id"`
			} `json:"ref"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("parse branch existence response: %w", err)
	}
	for i, r := range refs {
		repo, ok := resp.Data[fmt.Sprintf("r%d", i)]
		if !ok || repo == nil {
			// Repository didn't resolve — gone/renamed/inaccessible, or a
			// per-field error. Leave it unknown so the caller never marks the
			// branch deleted on a non-answer.
			continue
		}
		out[r] = repo.Ref != nil
	}
	return nil
}
