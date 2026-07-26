package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrompts_MemoryPathCarriesNoIDs pins the write-side contract: an agent
// writes its run memory to one fixed path, so no shipped prompt may describe
// that path by interpolating run ids. A prompt that composes an entity-memory
// path out of placeholders is describing a tree the agent is not asked to write
// into — the orchestrator owns that layout on both ends.
func TestPrompts_MemoryPathCarriesNoIDs(t *testing.T) {
	entries, err := os.ReadDir("prompts")
	if err != nil {
		t.Fatalf("read prompts dir: %v", err)
	}
	forbidden := []string{"entity-memory/{{", "entity-memory/$"}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("prompts", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(body), bad) {
				t.Errorf("%s composes an entity-memory path from placeholders (%q); the memory write path is fixed", e.Name(), bad)
			}
		}
	}
}
