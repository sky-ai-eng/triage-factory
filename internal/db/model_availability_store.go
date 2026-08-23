package db

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ModelAvailabilityStore owns the model_availability table — per (org,
// provider, model) evidence that the org's credentials can actually invoke
// that model, established by spending one minimal real request on it.
//
// The table is write-once-per-outcome and never swept: there is no TTL, no
// background re-probe, and no expiry column, because a green row is a fact
// about a request that really succeeded and a re-check would spend money to
// learn something a dispatch failure already reports. Every transition is
// caused by a user gesture — connecting a credential, or pressing "test
// connection" — so nothing here needs a lease, a scheduler, or a system
// reader.
//
// Split-pool in Postgres: every method is on the app pool under RLS (member
// SELECT so the model list can render the badge, admin write because spending
// the org's money is an admin act). There are no ...System variants because
// there is no JWT-less reader — a background job that probed would be exactly
// the automatic re-probing this design refuses.
type ModelAvailabilityStore interface {
	// List returns every recorded row for the org, sorted by (provider,
	// model_key). Callers join it against the catalog they are already
	// iterating, so a row for a key this build no longer offers is simply
	// never looked up.
	//
	// An org that has probed nothing returns an empty slice — which reads
	// identically to a build without the table, and is what every deployment
	// has until an admin connects a provider.
	List(ctx context.Context, orgID string) ([]domain.ModelAvailability, error)

	// Get returns one row, or (nil, nil) when this model has never produced a
	// conclusive probe. Absence is an answer, not a miss.
	Get(ctx context.Context, orgID, provider, modelKey string) (*domain.ModelAvailability, error)

	// Record upserts one probe's conclusion and returns the stored row.
	//
	// state must be domain.ModelAvailabilityGreen or ModelAvailabilityRed —
	// an inconclusive probe calls nothing at all, which is what keeps one bad
	// provider minute from turning a working model red. checked_at is stamped
	// by the statement, and detail is dropped on green, so neither can carry a
	// claim the caller made up.
	Record(ctx context.Context, orgID, provider, modelKey, state, detail string) (domain.ModelAvailability, error)
}

// ModelAvailabilityColumns is the projection both dialects read a
// model_availability row through — the point read, the list, and the write's
// RETURNING alike, so no two of them can drift into different shapes.
const ModelAvailabilityColumns = `org_id, provider, model_key, state, checked_at, detail`

// ScanModelAvailability decodes one row of that projection.
//
// It lives here rather than beside each dialect's SQL because the scan has no
// dialect content: every column is a plain type both backends read the same
// way, and the one nullable read (detail, whose NOT NULL DEFAULT ” makes NULL
// unreachable through this application) is tolerated identically in both. The
// scan func is a parameter so a *sql.Row and a *sql.Rows both reach it.
func ScanModelAvailability(scan func(...any) error) (domain.ModelAvailability, error) {
	var (
		out    domain.ModelAvailability
		detail sql.NullString
	)
	if err := scan(&out.OrgID, &out.Provider, &out.ModelKey, &out.State, &out.CheckedAt, &detail); err != nil {
		return domain.ModelAvailability{}, err
	}
	out.CheckedAt = out.CheckedAt.UTC()
	out.Detail = detail.String
	return out, nil
}
