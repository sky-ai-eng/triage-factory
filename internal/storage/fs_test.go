package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestFSStorage_Conformance(t *testing.T) {
	runConformance(t, newFSStorage(t.TempDir()))
}

func TestFSStorage_RejectsBadKeys(t *testing.T) {
	store := newFSStorage(t.TempDir())
	ctx := context.Background()
	for _, key := range []string{"", "  ", "/", "../escape", "a/../../escape"} {
		if err := store.Put(ctx, key, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) = nil; want rejection", key)
		}
	}
}

func TestFSStorage_NestedKeyCreatesDirs(t *testing.T) {
	// A deep, clean key materializes its parent directories on Put and
	// round-trips — the common shape of a caller-namespaced blob key.
	store := newFSStorage(t.TempDir())
	ctx := context.Background()
	key := "org/team/run/workspace/obj"
	if err := store.Put(ctx, key, strings.NewReader("ok")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := string(mustGet(t, store, key)); got != "ok" {
		t.Fatalf("Get(%q) = %q; want %q", key, got, "ok")
	}
}

func TestNew_LocalIsFS(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	store, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := store.(*fsStorage); !ok {
		t.Fatalf("New in local mode = %T; want *fsStorage", store)
	}
}
