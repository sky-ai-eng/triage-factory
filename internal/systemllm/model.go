package systemllm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

// OrgSettingsReader is the claims-free org-settings read the model resolution
// needs. Declared here as the narrow shape a caller must satisfy rather than
// taken as db.OrgsStore, so a test supplies one method instead of a store —
// the same discipline agentproc.SecretsReader holds. db.OrgsStore satisfies it
// structurally.
type OrgSettingsReader interface {
	GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error)
}

// ErrNoModel reports that an org's background jobs have no model they can run
// on: the setting is unset, names something this build does not offer, or names
// a model whose provider the org has not connected. Every wrapped message names
// the setting, because changing the setting is the whole remedy.
//
// It is the ONLY answer to those cases. TF ships no fallback model, so there is
// nothing to substitute — a job that gets this error skips its cycle and says
// so, and the next cycle asks again.
var ErrNoModel = errors.New("systemllm: no usable background jobs model")

// ModelForSettings resolves the model this org's system jobs — the scorer, the
// project classifier, the repo profiler — run on, from the org settings row a
// caller already holds.
//
// Three gates, all of which make the setting unusable rather than substitutable:
// the value must be present, the catalog must offer it, and the org must have
// connected the provider that serves it. The R5 delegation gates (tool support,
// a 64k window) deliberately do NOT apply — these jobs are toolless and
// short-context, so every catalog entry is a legitimate choice here.
//
// Team provider restrictions are not consulted, and there is no team argument to
// consult one with: a restriction narrows what a team may spend against, and
// these jobs are the org's own work rather than any team's.
func ModelForSettings(set domain.OrgSettings) (string, error) {
	model := strings.TrimSpace(set.BackgroundJobsModel)
	if model == "" {
		return "", fmt.Errorf("%w: the background jobs model setting is empty — pick one in Settings", ErrNoModel)
	}
	if !modelcatalog.Offers(model) {
		return "", fmt.Errorf("%w: the background jobs model setting names %q, which this deployment does not offer — pick another in Settings", ErrNoModel, model)
	}
	if err := modelaccess.Check(model, set, nil); err != nil {
		return "", fmt.Errorf("%w: the background jobs model setting names %q: %w", ErrNoModel, model, err)
	}
	return model, nil
}

// ModelFunc reports the model an org's background jobs run on. It is the seam
// the two jobs that hold no org-settings store take (the repo profiler reads the
// row itself, for the clone protocol, and calls ModelForSettings on that read).
type ModelFunc func(ctx context.Context, orgID string) (string, error)

// Resolve calls f, answering ErrNoModel for a nil one. Fail-closed on purpose:
// an unwired resolver is a caller bug, and the alternative — treating nil as
// "any model will do" — is the shipped fallback this package exists to not have.
func (f ModelFunc) Resolve(ctx context.Context, orgID string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("%w: no background jobs model resolver is wired for org %s", ErrNoModel, orgID)
	}
	return f(ctx, orgID)
}

// NewModelFunc builds the store-backed ModelFunc. The read is per cycle rather
// than cached: an admin who changes the model expects the next cycle to use it,
// and one settings read next to a batch of LLM calls costs nothing worth
// keeping a cache coherent for.
func NewModelFunc(orgs OrgSettingsReader) ModelFunc {
	return func(ctx context.Context, orgID string) (string, error) {
		if orgs == nil {
			return "", fmt.Errorf("%w: no org-settings reader is wired for org %s", ErrNoModel, orgID)
		}
		set, err := orgs.GetSettingsSystem(ctx, orgID)
		if err != nil {
			return "", fmt.Errorf("read org settings for the background jobs model: %w", err)
		}
		return ModelForSettings(set)
	}
}
