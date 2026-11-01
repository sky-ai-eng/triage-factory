package github

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestClient_RedirectThatTurnsAWriteIntoARead pins that a write redirected
// with a 302 fails with the redirect's status instead of reporting the page it
// would have landed on. Following it would send a GET to a login page that
// never saw the write, and every one of these writes discards its response
// body, so each would report a change that did not happen.
func TestClient_RedirectThatTurnsAWriteIntoARead(t *testing.T) {
	writes := map[string]func(context.Context, *Client) error{
		"DeleteBranchRef": func(ctx context.Context, c *Client) error {
			return c.DeleteBranchRef(ctx, "o", "r", "feature/x")
		},
		"ClosePR": func(ctx context.Context, c *Client) error {
			return c.ClosePR(ctx, "o", "r", 7)
		},
		"UpdatePR": func(ctx context.Context, c *Client) error {
			return c.UpdatePR(ctx, "o", "r", 7, "title", "body")
		},
		"DeleteComment": func(ctx context.Context, c *Client) error {
			return c.DeleteComment(ctx, "o", "r", 11)
		},
		"DismissReview": func(ctx context.Context, c *Client) error {
			return c.DismissReview(ctx, "o", "r", 7, 3, "stale")
		},
		"ReactToCommentScoped": func(ctx context.Context, c *Client) error {
			_, err := c.ReactToCommentScoped(ctx, "o", "r", 11, "+1")
			return err
		},
		"PostGraphQLMutation": func(ctx context.Context, c *Client) error {
			_, err := c.PostGraphQLMutation(ctx, map[string]any{"query": "mutation M { x }"})
			return err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			var loginHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/login" {
					loginHits.Add(1)
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, "<html>sign in</html>")
					return
				}
				http.Redirect(w, r, "/login", http.StatusFound)
			}))
			t.Cleanup(srv.Close)

			err := write(context.Background(), NewClient(srv.URL, "pat"))
			var he *HTTPError
			if !errors.As(err, &he) || he.StatusCode != http.StatusFound || he.Class != upstream.Rejected {
				t.Fatalf("err = %v, want a rejected *HTTPError carrying the 302", err)
			}
			if n := loginHits.Load(); n != 0 {
				t.Errorf("the redirect was followed %d time(s), want none", n)
			}
		})
	}
}

// TestClient_RedirectsThatKeepTheMethodAreFollowed pins what the write
// refusal leaves alone: a 307 resends the write, body and all, and a read
// follows a 302 as it always has, artifact downloads included.
func TestClient_RedirectsThatKeepTheMethodAreFollowed(t *testing.T) {
	t.Run("307 resends the write", func(t *testing.T) {
		var moved atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				raw, _ := io.ReadAll(r.Body)
				moved.Store(r.Method + " " + string(raw))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
				return
			}
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(srv.Close)

		if err := NewClient(srv.URL, "pat").ClosePR(context.Background(), "o", "r", 7); err != nil {
			t.Fatalf("ClosePR: %v", err)
		}
		if got, want := moved.Load(), `PATCH {"state":"closed"}`; got != want {
			t.Errorf("new location got %v, want %q", got, want)
		}
	})

	t.Run("302 on a read is followed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}))
		t.Cleanup(srv.Close)

		body, err := NewClient(srv.URL, "pat").Get(context.Background(), "/repos/o/r")
		if err != nil || string(body) != `{"ok":true}` {
			t.Fatalf("Get = %s, %v; want the redirected page", body, err)
		}
	})

	t.Run("302 on an artifact download is followed", func(t *testing.T) {
		payload := []byte("pretend zip")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/signed" {
				_, _ = w.Write(payload)
				return
			}
			http.Redirect(w, r, "/signed", http.StatusFound)
		}))
		t.Cleanup(srv.Close)

		var dst bytes.Buffer
		n, err := NewClient(srv.URL, "pat").DownloadArtifact(context.Background(), "/repos/o/r/actions/runs/1/logs", &dst, 1024)
		if err != nil || n != int64(len(payload)) || !bytes.Equal(dst.Bytes(), payload) {
			t.Fatalf("DownloadArtifact = %d, %q, %v; want the signed URL's body", n, dst.Bytes(), err)
		}
	})
}

// TestNewClientWithHTTPClient_RefusesWriteToRead pins that a caller-supplied
// client gets the write refusal too, without the caller's own client changing.
func TestNewClientWithHTTPClient_RefusesWriteToRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = io.WriteString(w, "<html>sign in</html>")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	hc := &http.Client{}
	err := NewClientWithHTTPClient(srv.URL, "pat", hc).ClosePR(context.Background(), "o", "r", 7)
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusFound {
		t.Fatalf("ClosePR err = %v, want an *HTTPError carrying the 302", err)
	}
	if hc.CheckRedirect != nil {
		t.Error("the caller's client had its CheckRedirect replaced")
	}
}
