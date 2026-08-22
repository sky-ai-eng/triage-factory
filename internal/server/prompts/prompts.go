// Package prompts holds the prompt primitives shared across the handler
// surfaces that read or write prompts — the standalone prompts API and the
// blueprint editor (whose steps are prompts). These are the pieces the
// handler packages need in common: the model-override allow-list, the
// create/update request shapes, and the source-aware soft delete. The
// prompt HTTP handlers themselves will migrate into this package; for now
// it is the shared contract they all import.
package prompts

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

// CreateRequest is the body of a prompt create.
type CreateRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Model string `json:"model"`
	// TeamID is the acting team the write picker supplied. Required in the UI
	// whenever the caller belongs to ≥2 teams; empty for a solo caller (the
	// picker is hidden), where the resolver falls back to the sole team. See
	// teamscope.ResolveActing.
	TeamID string `json:"team_id"`
}

// UpdateRequest is the body of a prompt update. The owning team is fixed at
// create time, so unlike CreateRequest it carries no team_id.
type UpdateRequest struct {
	Name  string `json:"name"`
	Body  string `json:"body"`
	Model string `json:"model"`
}

// ValidModel reports whether m is an acceptable prompts.model value — "" (the
// step is unset and inherits the team default at dispatch) or a model catalog
// key.
//
// The catalog is the accepted set rather than a list kept beside it: the value
// stored here is dispatched verbatim, so anything the catalog does not name is
// a model the ledger has no price for and, quite possibly, one no credential
// can invoke. The picker draws from the same read, which is what makes a
// rejection here something only a headless caller can provoke.
func ValidModel(m string) bool {
	return m == "" || modelcatalog.Offers(m)
}

// InvalidModelError is the 400 message for a value ValidModel rejects. It names
// the offered keys rather than the requirement in the abstract, because the
// caller's next move is picking one of them.
func InvalidModelError() string {
	return `model must be "" (inherit the team default) or one of: ` + strings.Join(modelcatalog.Keys(), ", ")
}

// SoftDeleteBySource soft-deletes a prompt according to its source and
// returns the resulting status. System / imported prompts are hidden (kept
// out of the re-import set); user (and any other) prompts get deleted_at
// stamped. Both are soft — conversations.prompt_id and
// blueprint_steps.step_prompt_id are RESTRICT FKs, and historical timelines
// resolve the prompt via the ...System reads — so the row survives as
// audit. Shared by the prompt-delete delete-pairing (a prompt taking its
// sole-owner blueprint) and the blueprint-delete cascade (a blueprint
// taking its step prompts), the two halves of the same pairing, so the
// source dispatch lives in one place. The caller must have already fetched
// p.
func SoftDeleteBySource(ctx context.Context, tx db.TxStores, orgID string, p *domain.Prompt) (status string, err error) {
	if p.Source == "system" || p.Source == "imported" {
		if _, e := tx.Prompts.Hide(ctx, orgID, p.ID); e != nil {
			return "", e
		}
		return "hidden", nil
	}
	if e := tx.Prompts.Delete(ctx, orgID, p.ID); e != nil {
		return "", e
	}
	return "deleted", nil
}
