package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBranchesExist_ThreeWayResult pins the confident/omit contract: an existing
// ref → true, a resolved repo with a null ref → false (gone), and a null
// repository alias (inaccessible/renamed) → OMITTED so a non-answer never marks
// a branch deleted.
func TestBranchesExist_ThreeWayResult(t *testing.T) {
	body := `{"data":{
		"r0": {"ref": {"id": "REF_alive"}},
		"r1": {"ref": null},
		"r2": null
	}}`
	c := clientAgainst(graphqlServing(t, body))

	refs := []BranchRef{
		{Owner: "octo", Repo: "repo", Branch: "alive"},
		{Owner: "octo", Repo: "repo", Branch: "gone"},
		{Owner: "octo", Repo: "ghost", Branch: "whatever"},
	}
	got, err := c.BranchesExist(refs)
	if err != nil {
		t.Fatalf("BranchesExist: %v", err)
	}
	if v, ok := got[refs[0]]; !ok || !v {
		t.Errorf("alive: got (exists=%v, present=%v), want (true, true)", v, ok)
	}
	if v, ok := got[refs[1]]; !ok || v {
		t.Errorf("gone: got (exists=%v, present=%v), want (false, true)", v, ok)
	}
	if _, ok := got[refs[2]]; ok {
		t.Errorf("ghost (repository null): should be omitted as unknown, but it was present in the result")
	}
}

// TestBranchesExist_QualifiesRefAndAliases pins the request shape: each input
// rides a GraphQL variable qualified as refs/heads/<branch> (so a branch name
// with specials needs no escaping), and the fields are aliased r0/r1 so the
// response maps back positionally.
func TestBranchesExist_QualifiesRefAndAliases(t *testing.T) {
	var captured struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"r0":{"ref":{"id":"X"}},"r1":{"ref":null}}}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	if _, err := c.BranchesExist([]BranchRef{
		{Owner: "octo", Repo: "repo", Branch: "feature/x"},
		{Owner: "octo", Repo: "repo", Branch: `weird"name`},
	}); err != nil {
		t.Fatalf("BranchesExist: %v", err)
	}

	if !strings.Contains(captured.Query, "r0: repository(") || !strings.Contains(captured.Query, "r1: repository(") {
		t.Errorf("query missing per-input aliases: %q", captured.Query)
	}
	if got := captured.Variables["q0"]; got != "refs/heads/feature/x" {
		t.Errorf("q0 = %v, want refs/heads/feature/x", got)
	}
	// The branch name with a quote rides a variable verbatim — no escaping, no
	// broken query.
	if got := captured.Variables["q1"]; got != `refs/heads/weird"name` {
		t.Errorf("q1 = %v, want refs/heads/weird\"name", got)
	}
}

// TestBranchesExist_Empty pins that no input is a no-op (nil map, nil error) —
// the reconciler calls it unconditionally per owner.
func TestBranchesExist_Empty(t *testing.T) {
	c := clientAgainst("http://unused.invalid")
	got, err := c.BranchesExist(nil)
	if err != nil || got != nil {
		t.Errorf("BranchesExist(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}
