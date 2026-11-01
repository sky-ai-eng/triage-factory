package jira

import (
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
// with a 302 fails with the redirect's status instead of reporting the page
// it would have landed on: following it would send a GET to a page that never
// saw the comment, and AddComment would report it posted.
func TestClient_RedirectThatTurnsAWriteIntoARead(t *testing.T) {
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

	_, err := NewClient(DataCenterPAT(srv.URL, "pat")).AddComment(context.Background(), "SKY-1", "done")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusFound || se.Class != upstream.Rejected {
		t.Fatalf("AddComment err = %v, want a rejected StatusError carrying the 302", err)
	}
	if n := loginHits.Load(); n != 0 {
		t.Errorf("the redirect was followed %d time(s), want none", n)
	}
}

// TestClient_RedirectsThatKeepTheMethodAreFollowed pins what the write
// refusal leaves alone: a 307 resends the write, body and all, and a read
// follows a 302 as it always has.
func TestClient_RedirectsThatKeepTheMethodAreFollowed(t *testing.T) {
	t.Run("307 resends the write", func(t *testing.T) {
		var moved atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				raw, _ := io.ReadAll(r.Body)
				moved.Store(r.Method + " " + string(raw))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"101"}`)
				return
			}
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(srv.Close)

		id, err := NewClient(DataCenterPAT(srv.URL, "pat")).AddComment(context.Background(), "SKY-1", "done")
		if err != nil || id != "101" {
			t.Fatalf("AddComment = %q, %v; want the comment the new location created", id, err)
		}
		if got, want := moved.Load(), `POST {"body":"done"}`; got != want {
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

		c := NewClient(DataCenterPAT(srv.URL, "pat"))
		body, err := c.get(context.Background(), c.apiURL("serverInfo"))
		if err != nil || string(body) != `{"ok":true}` {
			t.Fatalf("get = %s, %v; want the redirected page", body, err)
		}
	})
}
