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
//
// It says the SETTING is unusable, and nothing else does. A resolver can also
// fail because the settings row could not be read, or because nothing was wired
// to read it with; those are faults rather than configuration, they do not wrap
// this, and a caller must not quiet them down to the same skip — an unreadable
// row is a cycle that failed, not an org that has not picked.
var ErrNoModel = errors.New("systemllm: no usable background jobs model")

// ModelForSettings resolves the model this org's system jobs — the scorer,
// the repo profiler — run on, from the org settings row a caller already
// holds.
//
// Four gates, all of which make the setting unusable rather than substitutable:
// the value must be present, the catalog must offer it, the org must be able to
// authenticate at all, and it must have connected the provider that serves this
// model. The third is what stops a cycle billing an org's work to whatever
// credential the operator's shell happens to hold. The R5 delegation gates (tool support,
// a 64k window) deliberately do NOT apply — these jobs are toolless and
// short-context, so every catalog entry is a legitimate choice here.
//
// Enable-sets are not consulted, and there is no team argument to consult a
// team's with: a set narrows what a TEAM may pick for its runs, and these jobs
// are the org's own work rather than any team's. The org admin who would edit
// the set is the same person who picks this knob, so a second gate over one
// person's two choices would only ever refuse them their own decision.
func ModelForSettings(set domain.OrgSettings) (string, error) {
	model := strings.TrimSpace(set.BackgroundJobsModel)
	if model == "" {
		return "", fmt.Errorf("%w: the background jobs model setting is empty — pick one in Settings", ErrNoModel)
	}
	if !modelcatalog.Offers(model) {
		return "", fmt.Errorf("%w: the background jobs model setting names %q, which this deployment does not offer — pick another in Settings", ErrNoModel, model)
	}
	creds := modelaccess.ForOrg(set)
	if err := creds.Ready(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoModel, err)
	}
	if err := creds.Check(model); err != nil {
		return "", fmt.Errorf("%w: the background jobs model setting names %q: %w", ErrNoModel, model, err)
	}
	return model, nil
}

// ModelFunc reports the model an org's background jobs run on. It is the seam
// the two jobs that hold no org-settings store take (the repo profiler reads the
// row itself, for the clone protocol, and calls ModelForSettings on that read).
type ModelFunc func(ctx context.Context, orgID string) (string, error)

// Resolve calls f, erroring for a nil one. Fail-closed on purpose: the
// alternative — treating nil as "any model will do" — is the shipped fallback
// this package exists to not have.
//
// The error deliberately does NOT wrap ErrNoModel. An unwired resolver is a
// caller bug, not an org that has not picked a model, and the two are logged
// differently by every caller.
func (f ModelFunc) Resolve(ctx context.Context, orgID string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("systemllm: no background jobs model resolver is wired for org %s", orgID)
	}
	return f(ctx, orgID)
}

// NewModelFunc builds the store-backed ModelFunc. The read is per cycle rather
// than cached: an admin who changes the model expects the next cycle to use it,
// and one settings read next to a batch of LLM calls costs nothing worth
// keeping a cache coherent for.
//
// Neither failure here wraps ErrNoModel — an unreadable row and an unwired
// reader are faults, and only the value the row carries can make the setting
// unusable.
func NewModelFunc(orgs OrgSettingsReader) ModelFunc {
	return func(ctx context.Context, orgID string) (string, error) {
		if orgs == nil {
			return "", fmt.Errorf("systemllm: no org-settings reader is wired for org %s", orgID)
		}
		set, err := orgs.GetSettingsSystem(ctx, orgID)
		if err != nil {
			return "", fmt.Errorf("read org settings for the background jobs model: %w", err)
		}
		return ModelForSettings(set)
	}
}
