package server

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedPRSnapshot creates a github entity with the given PRSnapshot and
// returns it as the store reads it back.
func seedPRSnapshot(t *testing.T, s *Server, owner, repo string, number int, snap domain.PRSnapshot) *domain.Entity {
	t.Helper()
	sourceID := owner + "/" + repo + "#" + strconv.Itoa(number)
	entities := sqlitestore.New(s.db).Entities
	entity, _, err := entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", sourceID, "", "pr", snap.Title, snap.URL)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if ok, err := entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, entity.ID, string(data), entity.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	stored, err := entities.Get(context.Background(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || stored == nil {
		t.Fatalf("read seeded entity: %v", err)
	}
	return stored
}

// readPRSnapshot fetches and decodes the current snapshot of the entity.
func readPRSnapshot(t *testing.T, s *Server, entityID string) domain.PRSnapshot {
	t.Helper()
	entity, err := sqlitestore.New(s.db).Entities.Get(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if entity == nil {
		t.Fatalf("entity not found: %s", entityID)
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	return snap
}
