package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPRMergeability_AnswersOnlyPullRequests pins the read's shape: each
// pull request answers its mergeability and head by node id, an id with no
// node (null) or whose node is another type (an object with none of the
// fields asked for) is absent from the answer, and a pull request whose
// mergeable is null beside a field error answers UNKNOWN.
func TestPRMergeability_AnswersOnlyPullRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"nodes":[
			{"id":"PR_a","mergeable":"CONFLICTING","headRefOid":"sha-a"},
			null,
			{},
			{"id":"PR_d","mergeable":"UNKNOWN","headRefOid":"sha-d"},
			{"id":"PR_e","mergeable":null,"headRefOid":"sha-e"}
		],"rateLimit":{"cost":1}},
		"errors":[{"type":"FORBIDDEN","path":["nodes",4,"mergeable"],"message":"forbidden"}]}`))
	}))
	t.Cleanup(srv.Close)

	got, err := NewClient(srv.URL, "tok").PRMergeability(context.Background(), []string{"PR_a", "PR_gone", "I_issue", "PR_d", "PR_e"})
	if err != nil {
		t.Fatalf("PRMergeability: %v", err)
	}
	want := map[string]MergeabilityRead{
		"PR_a": {Mergeable: "CONFLICTING", HeadSHA: "sha-a"},
		"PR_d": {Mergeable: "UNKNOWN", HeadSHA: "sha-d"},
		"PR_e": {Mergeable: "UNKNOWN", HeadSHA: "sha-e"},
	}
	if len(got) != len(want) {
		t.Fatalf("answers = %+v, want %+v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %+v, want %+v", id, got[id], w)
		}
	}
	if _, err := NewClient(srv.URL, "tok").PRMergeability(context.Background(), make([]string, MaxMergeabilityBatch+1)); err == nil {
		t.Error("a batch over the nodes(ids:) limit was sent")
	}
}
