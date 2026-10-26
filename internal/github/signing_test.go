package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestListSSHSigningKeys walks every page of the listing, returns each key as
// GitHub printed it, and reports a 404 as one.
func TestListSSHSigningKeys(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/users/signer/ssh_signing_keys":
			var items []string
			switch r.URL.Query().Get("page") {
			case "1":
				for i := range 100 {
					items = append(items, fmt.Sprintf(`{"id":%d,"key":"ssh-ed25519 K%d","title":"t"}`, i, i))
				}
			case "2":
				items = append(items, `{"id":100,"key":"ssh-ed25519 LAST","title":"Triage Factory commit signing"}`)
			}
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	c := NewClient(ts.URL, "")

	keys, err := c.ListSSHSigningKeys(context.Background(), "signer")
	if err != nil {
		t.Fatalf("ListSSHSigningKeys: %v", err)
	}
	if len(keys) != 101 || keys[0] != "ssh-ed25519 K0" || !slices.Contains(keys, "ssh-ed25519 LAST") {
		t.Errorf("keys = %d entries (first %q), want 101 ending in the second page's key", len(keys), keys[0])
	}
	if _, err := c.ListSSHSigningKeys(context.Background(), "nobody"); !IsHTTP404(err) {
		t.Errorf("ListSSHSigningKeys(nobody) error = %v, want a 404", err)
	}
}

// TestGetCommitVerification reads a commit's verdict and its committer, and
// refuses a response that carries no verification rather than reading it as
// unverified.
func TestGetCommitVerification(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/repos/acme/app/commits/abc":
			_, _ = w.Write([]byte(`{"sha":"abc","commit":{"committer":{"name":"S","email":"s@example.com"},
				"verification":{"verified":false,"reason":"no_user","signature":"x","payload":"y"}}}`))
		case "/api/v3/repos/acme/app/commits/bare":
			_, _ = w.Write([]byte(`{"sha":"bare","commit":{"committer":{"email":"s@example.com"}}}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	c := NewClient(ts.URL, "")
	ctx := context.Background()

	got, err := c.GetCommitVerification(ctx, "acme", "app", "abc")
	if want := (CommitVerification{Reason: "no_user", CommitterEmail: "s@example.com"}); err != nil || got != want {
		t.Errorf("GetCommitVerification = %+v, %v; want %+v", got, err, want)
	}
	if _, err := c.GetCommitVerification(ctx, "acme", "app", "bare"); err == nil {
		t.Error("a commit with no verification read without an error")
	}
	if _, err := c.GetCommitVerification(ctx, "acme", "app", "gone"); !IsHTTP404(err) {
		t.Errorf("GetCommitVerification(gone) error = %v, want a 404", err)
	}
}
