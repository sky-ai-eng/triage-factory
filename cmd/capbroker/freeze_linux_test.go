//go:build linux

package capbroker

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

func TestFreezeRun(t *testing.T) {
	s := NewServer(&fakeOps{})
	var got []string
	op := func(_ context.Context, id string) error { got = append(got, id); return nil }

	// A run the broker holds is frozen.
	live := &runEntry{done: make(chan struct{})}
	s.runs["tf-abc-1"] = live
	if _, err := s.freezeRun(context.Background(), freezeRunArgs{ContainerID: "tf-abc-1"}, op); err != nil || len(got) != 1 {
		t.Fatalf("live run: err=%v calls=%v", err, got)
	}

	// An id it holds nothing for, or a run that has exited, has nothing to freeze.
	if _, err := s.freezeRun(context.Background(), freezeRunArgs{ContainerID: "tf-gone-2"}, op); err != nil || len(got) != 1 {
		t.Errorf("unknown run: err=%v calls=%v", err, got)
	}
	exited := &runEntry{done: make(chan struct{})}
	close(exited.done)
	s.runs["tf-done-3"] = exited
	if _, err := s.freezeRun(context.Background(), freezeRunArgs{ContainerID: "tf-done-3"}, op); err != nil || len(got) != 1 {
		t.Errorf("exited run: err=%v calls=%v", err, got)
	}

	// The credential sidecar shares the registry and is never frozen.
	s.runs["tf-abc-1"+sandbox.SidecarContainerIDSuffix] = live
	_, err := s.freezeRun(context.Background(), freezeRunArgs{ContainerID: "tf-abc-1" + sandbox.SidecarContainerIDSuffix}, op)
	if err == nil || !strings.Contains(err.Error(), "sidecar") || len(got) != 1 {
		t.Errorf("sidecar: err=%v calls=%v", err, got)
	}
}
