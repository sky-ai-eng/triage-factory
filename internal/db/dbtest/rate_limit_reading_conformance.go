package dbtest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RateLimitReadingStoreFactory hands the conformance suite a wired store and
// the orgID to scope every call. The org row must already exist —
// rate_limit_readings carries an FK to orgs in both dialects.
type RateLimitReadingStoreFactory func(t *testing.T) (store db.RateLimitReadingStore, orgID string)

// RunRateLimitReadingStoreConformance covers the contract every backend impl
// must hold: a key keeps its newest observation whatever order flushes land
// in, including two readings of it in one call; a call larger than one
// statement's batch writes every reading; a field the reading did not state reads back nil, never zero; List
// returns only readings that still describe their buckets; and the expiry
// sweep deletes exactly the rows List leaves out.
func RunRateLimitReadingStoreConformance(t *testing.T, mk RateLimitReadingStoreFactory) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	intp := func(v int) *int { return &v }
	boolp := func(v bool) *bool { return &v }
	timep := func(v time.Time) *time.Time { return &v }

	t.Run("newest_observation_wins", func(t *testing.T) {
		// Two pods flush the same bucket; the later observation must survive
		// whichever write lands last.
		store, orgID := mk(t)
		older := domain.RateLimitReading{
			OrgID: orgID, Upstream: "github", Credential: "installation:1", Bucket: "core",
			Remaining: intp(4000), Limit: intp(5000), ResetAt: timep(now.Add(time.Hour)), ObservedAt: now.Add(-time.Minute),
		}
		newer := older
		newer.Remaining, newer.ObservedAt = intp(3990), now

		if err := store.UpsertSystem(ctx, []domain.RateLimitReading{newer}); err != nil {
			t.Fatalf("UpsertSystem newer: %v", err)
		}
		if err := store.UpsertSystem(ctx, []domain.RateLimitReading{older}); err != nil {
			t.Fatalf("UpsertSystem older: %v", err)
		}
		got, err := store.List(ctx, orgID, now)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].Remaining == nil || *got[0].Remaining != 3990 || !got[0].ObservedAt.Equal(now) {
			t.Fatalf("List after an older flush landed last = %+v, want the newer reading (3990)", got)
		}

		newest := newer
		newest.Remaining, newest.ObservedAt = intp(3980), now.Add(time.Second)
		if err := store.UpsertSystem(ctx, []domain.RateLimitReading{newest}); err != nil {
			t.Fatalf("UpsertSystem newest: %v", err)
		}
		got, err = store.List(ctx, orgID, now)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || *got[0].Remaining != 3980 {
			t.Errorf("List after a newer flush = %+v, want 3980", got)
		}
	})

	t.Run("one_call_holding_a_key_twice_keeps_the_newer", func(t *testing.T) {
		// Postgres refuses a statement that updates one row twice, so the
		// store must reduce the call to one reading per key itself.
		store, orgID := mk(t)
		older := domain.RateLimitReading{
			OrgID: orgID, Upstream: "github", Credential: "pat", Bucket: "core",
			Remaining: intp(4000), Limit: intp(5000), ResetAt: timep(now.Add(time.Hour)), ObservedAt: now.Add(-time.Minute),
		}
		newer := older
		newer.Remaining, newer.ObservedAt = intp(3990), now
		if err := store.UpsertSystem(ctx, []domain.RateLimitReading{newer, older}); err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		got, err := store.List(ctx, orgID, now)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || *got[0].Remaining != 3990 {
			t.Errorf("List = %+v, want the newer reading (3990) alone", got)
		}
	})

	t.Run("a_write_larger_than_one_batch_lands_whole", func(t *testing.T) {
		store, orgID := mk(t)
		const n = 250
		readings := make([]domain.RateLimitReading, 0, n)
		for i := range n {
			readings = append(readings, domain.RateLimitReading{
				OrgID: orgID, Upstream: "github", Credential: "pat", Bucket: fmt.Sprintf("bucket-%03d", i),
				Remaining: intp(i), Limit: intp(n), ResetAt: timep(now.Add(time.Hour)), ObservedAt: now,
			})
		}
		if err := store.UpsertSystem(ctx, readings); err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		got, err := store.List(ctx, orgID, now)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != n {
			t.Fatalf("List = %d readings, want %d", len(got), n)
		}
		for i, r := range got {
			if r.Bucket != fmt.Sprintf("bucket-%03d", i) || *r.Remaining != i {
				t.Fatalf("reading %d = %+v, want bucket-%03d with %d remaining", i, r, i, i)
			}
		}
	})

	t.Run("unstated_fields_read_back_nil", func(t *testing.T) {
		store, orgID := mk(t)
		near := domain.RateLimitReading{
			OrgID: orgID, Upstream: "jira", Credential: "cloud_oauth", Bucket: "quota",
			NearLimit: boolp(false), ResetAt: timep(now.Add(time.Hour)), ObservedAt: now,
		}
		empty := domain.RateLimitReading{
			OrgID: orgID, Upstream: "jira", Credential: "dc_pat", Bucket: "requests",
			Remaining: intp(0), Limit: intp(100), ResetAt: timep(now.Add(time.Second)), ObservedAt: now,
		}
		if err := store.UpsertSystem(ctx, []domain.RateLimitReading{near, empty}); err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		got, err := store.List(ctx, orgID, now)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List = %d readings, want 2: %+v", len(got), got)
		}
		// Ordered by upstream, credential, bucket: cloud_oauth before dc_pat.
		q, r := got[0], got[1]
		if q.Credential != "cloud_oauth" || q.NearLimit == nil || *q.NearLimit ||
			q.Remaining != nil || q.Limit != nil || q.Used != nil {
			t.Errorf("near-limit reading = %+v, want near_limit false and nothing else stated", q)
		}
		if r.Credential != "dc_pat" || r.Remaining == nil || *r.Remaining != 0 || r.Limit == nil || *r.Limit != 100 || r.NearLimit != nil {
			t.Errorf("empty bucket reading = %+v, want remaining 0 (not nil) of 100", r)
		}
		if q.ResetAt == nil || !q.ResetAt.Equal(now.Add(time.Hour)) {
			t.Errorf("reset_at = %v, want %v", q.ResetAt, now.Add(time.Hour))
		}
	})

	t.Run("list_and_sweep_agree_on_expiry", func(t *testing.T) {
		store, orgID := mk(t)
		reading := func(bucket string, reset *time.Time, observed time.Time) domain.RateLimitReading {
			return domain.RateLimitReading{
				OrgID: orgID, Upstream: "linear", Credential: "api_key", Bucket: bucket,
				Remaining: intp(1), Limit: intp(10), ResetAt: reset, ObservedAt: observed,
			}
		}
		rows := []domain.RateLimitReading{
			reading("before_reset", timep(now.Add(time.Minute)), now),
			reading("at_reset", timep(now), now.Add(-time.Minute)),
			reading("no_reset_recent", nil, now.Add(-domain.RateLimitReadingMaxAge+time.Minute)),
			reading("no_reset_old", nil, now.Add(-domain.RateLimitReadingMaxAge)),
		}
		if err := store.UpsertSystem(ctx, rows); err != nil {
			t.Fatalf("UpsertSystem: %v", err)
		}
		listed := func() []string {
			got, err := store.List(ctx, orgID, now)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var buckets []string
			for _, r := range got {
				if !r.Current(now) {
					t.Errorf("List returned %s, which is not current at now", r.Bucket)
				}
				buckets = append(buckets, r.Bucket)
			}
			return buckets
		}
		want := []string{"before_reset", "no_reset_recent"}
		if got := listed(); !slices.Equal(got, want) {
			t.Errorf("List = %v, want %v", got, want)
		}
		n, err := store.DeleteExpiredSystem(ctx, now)
		if err != nil {
			t.Fatalf("DeleteExpiredSystem: %v", err)
		}
		if n != 2 {
			t.Errorf("DeleteExpiredSystem deleted %d, want 2 (at_reset, no_reset_old)", n)
		}
		if got := listed(); !slices.Equal(got, want) {
			t.Errorf("List after the sweep = %v, want %v", got, want)
		}
	})
}
