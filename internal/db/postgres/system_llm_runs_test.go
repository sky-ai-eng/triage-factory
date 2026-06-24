package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSystemLLMRunStore_Postgres_RoundTrip pins that Record (admin pool)
// inserts a row whose every field reads back intact under the admin pool,
// the server-side gen_random_uuid() id default fires, and an empty
// metadata_json lands as NULL. Skips cleanly without Docker via the
// pgtest harness. TFAC-451.
func TestSystemLLMRunStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID := seedPgSystemLLMOrg(t, h)

	started := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
		OrgID:               orgID,
		Job:                 "repo_profiler",
		Model:               "haiku",
		TotalCostUSD:        0.05,
		InputTokens:         11,
		OutputTokens:        22,
		CacheReadTokens:     333,
		CacheCreationTokens: 44,
		DurationMs:          2000,
		NumTurns:            2,
		IsError:             true,
		MetadataJSON:        `{"repo_count":3}`,
		StartedAt:           started,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		id, job, model              string
		cost                        float64
		in, out, cr, cc, dur, turns int
		isErr                       bool
		meta                        *string
	)
	// Read back from the admin pool (bypasses RLS — no claims context here).
	err := h.AdminDB.QueryRow(`
		SELECT id::text, job, model, total_cost_usd, input_tokens, output_tokens,
		       cache_read_tokens, cache_creation_tokens, duration_ms, num_turns,
		       is_error, metadata_json
		FROM system_llm_runs WHERE org_id = $1
	`, orgID).Scan(
		&id, &job, &model, &cost, &in, &out, &cr, &cc, &dur, &turns, &isErr, &meta,
	)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if id == "" {
		t.Error("expected gen_random_uuid() id, got empty")
	}
	if job != "repo_profiler" || model != "haiku" {
		t.Errorf("job/model = %q/%q, want repo_profiler/haiku", job, model)
	}
	if in != 11 || out != 22 || cr != 333 || cc != 44 {
		t.Errorf("tokens = (%d,%d,%d,%d), want (11,22,333,44)", in, out, cr, cc)
	}
	if dur != 2000 || turns != 2 || !isErr {
		t.Errorf("dur/turns/err = %d/%d/%v, want 2000/2/true", dur, turns, isErr)
	}
	if meta == nil || *meta != `{"repo_count":3}` {
		t.Errorf("metadata_json = %v, want {\"repo_count\":3}", meta)
	}
}

// TestSystemLLMRunStore_Postgres_EmptyMetadataIsNull confirms empty
// MetadataJSON serializes to SQL NULL via the NULLIF guard.
func TestSystemLLMRunStore_Postgres_EmptyMetadataIsNull(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID := seedPgSystemLLMOrg(t, h)
	if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
		OrgID:     orgID,
		Job:       "scorer",
		Model:     "haiku",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var meta *string
	if err := h.AdminDB.QueryRow(
		`SELECT metadata_json FROM system_llm_runs WHERE org_id = $1`, orgID,
	).Scan(&meta); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if meta != nil {
		t.Errorf("metadata_json = %q, want NULL", *meta)
	}
}

func seedPgSystemLLMOrg(t *testing.T, h *pgtest.Harness) string {
	t.Helper()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	email := fmt.Sprintf("sysllm-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "System LLM Test User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "SysLLM Org "+orgID[:8], "sysllm-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	return orgID
}
