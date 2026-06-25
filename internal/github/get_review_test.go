package github

import (
	"log/slog"
	"strings"
	"testing"
)

// TestGetReview pins the node(id:) fetch: a submitted/dismissed review's state,
// body, and inline comments (with their GraphQL node ids, the join key the
// proposed-vs-final diff needs) all come back.
func TestGetReview(t *testing.T) {
	body := `{"data":{"node":{
		"state":"CHANGES_REQUESTED",
		"body":"please fix",
		"comments":{"nodes":[
			{"id":"PRRC_1","path":"a.go","line":10,"startLine":null,"body":"nit"},
			{"id":"PRRC_2","path":"b.go","line":20,"startLine":null,"body":"bug"}
		]}
	}}}`
	rv, err := clientAgainst(graphqlServing(t, body)).GetReview("PRR_1")
	if err != nil {
		t.Fatalf("GetReview: %v", err)
	}
	if rv == nil {
		t.Fatal("nil review")
	}
	if rv.State != "CHANGES_REQUESTED" || rv.Body != "please fix" || len(rv.Comments) != 2 {
		t.Fatalf("unexpected review: %+v", rv)
	}
	c0 := rv.Comments[0]
	if c0.ID != "PRRC_1" || c0.Path != "a.go" || c0.Line == nil || *c0.Line != 10 || c0.Body != "nit" {
		t.Errorf("comment 0 = %+v", c0)
	}
}

// TestGetReview_AbsentIsNotAnError pins that a node that doesn't resolve to a
// review (null, or a node of another type that leaves the inline fragment
// empty) returns (nil, nil) — a normal "no review" result the caller degrades
// on, not an error.
func TestGetReview_AbsentIsNotAnError(t *testing.T) {
	for _, body := range []string{`{"data":{"node":null}}`, `{"data":{"node":{}}}`} {
		rv, err := clientAgainst(graphqlServing(t, body)).GetReview("X")
		if err != nil || rv != nil {
			t.Errorf("body %s: got (%+v, %v), want (nil, nil)", body, rv, err)
		}
	}
}

// TestGetReview_EmptyID guards the input.
func TestGetReview_EmptyID(t *testing.T) {
	if _, err := clientAgainst("http://unused.invalid").GetReview(""); err == nil {
		t.Error("empty review id should error")
	}
}

// TestGetReview_TruncationWarns pins the comment-cap watchdog: a review with
// >100 inline comments (hasNextPage) logs a warning so the silently-truncated
// diff stays visible, while still returning the page it got.
func TestGetReview_TruncationWarns(t *testing.T) {
	logs := &captureHandler{}
	prev := githubLog
	githubLog = slog.New(logs)
	t.Cleanup(func() { githubLog = prev })

	body := `{"data":{"node":{
		"state":"COMMENTED","body":"",
		"comments":{"pageInfo":{"hasNextPage":true},"nodes":[{"id":"C1","path":"a.go","line":1,"body":"x"}]}
	}}}`
	rv, err := clientAgainst(graphqlServing(t, body)).GetReview("PRR_big")
	if err != nil || rv == nil {
		t.Fatalf("GetReview: rv=%v err=%v", rv, err)
	}
	if len(rv.Comments) != 1 {
		t.Errorf("returned comments dropped: got %d, want the page intact", len(rv.Comments))
	}
	warned := false
	for _, r := range logs.recorded() {
		if strings.Contains(r.Message, "review comments truncated") {
			warned = true
		}
	}
	if !warned {
		t.Error("expected a truncation warning when hasNextPage is true")
	}
}
