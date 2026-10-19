package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RateLimitReadingStore owns the rate_limit_readings table — the latest
// reading of every rate-limit bucket an org's credentials are metered on, as
// the processes making requests last saw it.
//
// Each process keeps its readings in memory (internal/upstream) and writes the
// ones that changed here every few seconds, because a request can land on any
// control pod and only this table holds what every pod saw. A key holds one
// row, and the newest observation wins: an upstream's headers state its own
// count of what is left for the credential, so the latest reading from any
// process is accurate and nothing is summed.
//
// Split-pool in Postgres: the member read runs on the app pool under RLS, and
// the writers — the flusher every serving process runs, with no request
// claims — run on the admin pool with org_id bound by argument.
type RateLimitReadingStore interface {
	// List returns the org's readings that still describe their buckets at
	// now (domain.RateLimitReading.Current), ordered by upstream, credential
	// and bucket. A reading past its reset is not returned: by then the bucket
	// has refilled, whatever it said.
	List(ctx context.Context, orgID string, now time.Time) ([]domain.RateLimitReading, error)

	// UpsertSystem writes readings, for any orgs, keeping each key's newest
	// observation: a reading observed before the one stored for its key is
	// ignored, so two processes flushing the same bucket in either order leave
	// the later reading.
	//
	// Exempt from the returned-row rule: a bulk sync write, whose caller wants
	// only to know that it landed.
	UpsertSystem(ctx context.Context, readings []domain.RateLimitReading) error

	// DeleteExpiredSystem deletes every org's readings that no longer describe
	// their buckets at now, and returns how many it deleted. List already
	// leaves them out; this keeps the table from holding every key a process
	// ever recorded.
	DeleteExpiredSystem(ctx context.Context, now time.Time) (int64, error)
}
