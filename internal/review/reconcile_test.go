package review

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

func ptr(i int) *int { return &i }

// shiftDiff moves a.go line 3 down to line 5 (two inserts at the top).
const shiftDiff = "diff --git a/a.go b/a.go\n" +
	"--- a/a.go\n+++ b/a.go\n" +
	"@@ -1,5 +1,7 @@\n+newtop1\n+newtop2\n line1\n added2\n added3\n line4\n line5\n"

// deleteDiff removes a.go line 3 (added3) — an outdated anchor.
const deleteDiff = "diff --git a/a.go b/a.go\n" +
	"--- a/a.go\n+++ b/a.go\n" +
	"@@ -1,5 +1,4 @@\n line1\n added2\n-added3\n line4\n line5\n"

// renameDiff is a content-identical rename old.go → new.go.
const renameDiff = "diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n"

func cmt(id, path string, line int, sha string) domain.ReviewArtifactComment {
	return domain.ReviewArtifactComment{ID: id, Path: path, Line: ptr(line), Body: "b", CommitSHA: sha}
}

// TestReconcileToHead_Classifies pins the four per-comment verdicts and that a
// survivor is remapped + re-anchored while an outdated/unpositioned comment is
// returned unchanged.
func TestReconcileToHead_Classifies(t *testing.T) {
	comments := []domain.ReviewArtifactComment{
		cmt("shift", "a.go", 3, "anchor"),
		cmt("del", "a.go", 3, "anchor"),
		cmt("ren", "old.go", 3, "anchor"),
		{ID: "noline", Path: "a.go", Body: "b", CommitSHA: "anchor"}, // unpositioned
	}
	maps := map[string]string{"shift": shiftDiff, "del": deleteDiff, "ren": renameDiff}

	got := map[string]Outcome{}
	for _, c := range comments {
		// Per-comment line-map so each verdict is isolated.
		lm := ghclient.ParseLineMap(maps[c.ID])
		outs := ReconcileToHead([]domain.ReviewArtifactComment{c}, "head", "anchor",
			func(string) (ghclient.LineMap, error) { return lm, nil })
		got[c.ID] = outs[0]
	}

	if o := got["shift"]; !o.Survived || o.Status != ghclient.LineMapMoved || *o.Comment.Line != 5 || o.Comment.CommitSHA != "head" {
		t.Errorf("shift: got %+v (line=%v), want moved→5 re-anchored to head", o, o.Comment.Line)
	}
	if o := got["del"]; o.Survived || !o.Outdated() || *o.Comment.Line != 3 || o.Comment.CommitSHA != "anchor" {
		t.Errorf("del: got %+v, want outdated, comment unchanged (line 3, anchor)", o)
	}
	if o := got["ren"]; !o.Survived || o.Status != ghclient.LineMapMoved || o.Comment.Path != "new.go" || *o.Comment.Line != 3 {
		t.Errorf("ren: got %+v (path=%s), want moved to new.go at line 3", o, o.Comment.Path)
	}
	if o := got["noline"]; !o.Unpositioned || o.Classified() {
		t.Errorf("noline: got %+v, want unpositioned/unclassified", o)
	}
}

// TestReconcileToHead_AtHead pins that a comment already anchored at the target
// head is a no-op survivor with its anchor made explicit — and that NO line-map is
// fetched for it.
func TestReconcileToHead_AtHead(t *testing.T) {
	var calls int
	outs := ReconcileToHead([]domain.ReviewArtifactComment{cmt("c", "a.go", 3, "head")}, "head", "fallback",
		func(string) (ghclient.LineMap, error) { calls++; return ghclient.LineMap{}, nil })
	o := outs[0]
	if !o.Survived || o.Status != ghclient.LineMapUnchanged || o.Comment.CommitSHA != "head" {
		t.Errorf("at-head: got %+v, want unchanged survivor anchored at head", o)
	}
	if calls != 0 {
		t.Errorf("at-head must not fetch a line-map; got %d calls", calls)
	}
}

// TestReconcileToHead_CachesPerAnchor pins that comments sharing an anchor fetch
// the line-map exactly once.
func TestReconcileToHead_CachesPerAnchor(t *testing.T) {
	var calls int
	comments := []domain.ReviewArtifactComment{
		cmt("a", "a.go", 3, "anchor"),
		cmt("b", "a.go", 4, "anchor"),
		cmt("c", "a.go", 5, "anchor"),
	}
	ReconcileToHead(comments, "head", "", func(string) (ghclient.LineMap, error) {
		calls++
		return ghclient.ParseLineMap(shiftDiff), nil
	})
	if calls != 1 {
		t.Errorf("shared anchor fetched %d times, want 1 (cached)", calls)
	}
}

// TestReconcileToHead_MapError pins that a resolver error surfaces as MapErr with
// the comment unchanged (not dropped, not classified).
func TestReconcileToHead_MapError(t *testing.T) {
	boom := errors.New("compare unavailable")
	outs := ReconcileToHead([]domain.ReviewArtifactComment{cmt("c", "a.go", 3, "anchor")}, "head", "",
		func(string) (ghclient.LineMap, error) { return ghclient.LineMap{}, boom })
	o := outs[0]
	if !errors.Is(o.MapErr, boom) || o.Classified() || o.Survived {
		t.Errorf("map error: got %+v, want MapErr set, unclassified", o)
	}
	if *o.Comment.Line != 3 {
		t.Errorf("map error must leave the comment unchanged; got line %v", o.Comment.Line)
	}
}

// TestReconcileToHead_FallbackAnchor pins that an empty CommitSHA falls back to the
// supplied anchor, and that with no fallback either it reports unresolved.
func TestReconcileToHead_FallbackAnchor(t *testing.T) {
	// Empty CommitSHA + fallback "old": resolves and shifts.
	outs := ReconcileToHead([]domain.ReviewArtifactComment{cmt("c", "a.go", 3, "")}, "head", "old",
		func(anchor string) (ghclient.LineMap, error) {
			if anchor != "old" {
				t.Errorf("resolver got anchor %q, want the fallback %q", anchor, "old")
			}
			return ghclient.ParseLineMap(shiftDiff), nil
		})
	if o := outs[0]; !o.Survived || *o.Comment.Line != 5 {
		t.Errorf("fallback anchor: got %+v, want shifted survivor", o)
	}

	// Empty CommitSHA + empty fallback: unresolved (no anchor to map from).
	outs = ReconcileToHead([]domain.ReviewArtifactComment{cmt("c", "a.go", 3, "")}, "head", "",
		func(string) (ghclient.LineMap, error) {
			t.Error("resolver must not be called with no anchor")
			return ghclient.LineMap{}, nil
		})
	if o := outs[0]; o.MapErr == nil || o.Classified() {
		t.Errorf("no anchor: got %+v, want unresolved MapErr", o)
	}
}

// TestFreshness pins the status→wire mapping, including the unknown default.
func TestFreshness(t *testing.T) {
	cases := map[ghclient.LineMapStatus]string{
		ghclient.LineMapUnchanged:  FreshnessCurrent,
		ghclient.LineMapMoved:      FreshnessMoved,
		ghclient.LineMapOutdated:   FreshnessOutdated,
		ghclient.LineMapStatus(""): FreshnessUnknown,
	}
	for status, want := range cases {
		if got := Freshness(status); got != want {
			t.Errorf("Freshness(%q) = %q, want %q", status, got, want)
		}
	}
}
