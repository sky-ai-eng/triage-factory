package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestListComments_FollowsAShortPage: Jira caps a comment page below what was
// asked for, and the read continues from where the short page ended until it
// holds as many as asked.
func TestListComments_FollowsAShortPage(t *testing.T) {
	const capPerPage = 2
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rest/api/3/issue/10001/comment") {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("startAt")+"/"+r.URL.Query().Get("maxResults"))
		mu.Unlock()
		var page []string
		for i := start; i < min(start+capPerPage, 10); i++ {
			page = append(page, fmt.Sprintf(`{"id":"%d","author":{"displayName":"Ada","accountId":"acc-ada"},"created":"2026-10-01T09:0%d:00.000+0000"}`, 20000+i, i))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"startAt":%d,"maxResults":%d,"total":10,"comments":[%s]}`, start, capPerPage, strings.Join(page, ","))
	}))
	t.Cleanup(srv.Close)

	got, err := cloudClient(srv.URL).ListComments(context.Background(), "10001", 5, 3)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if want := []string{"20005", "20006", "20007"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("comments = %v, want %v", ids, want)
	}
	if got[0].Author == nil || got[0].Author.AccountID != "acc-ada" || got[0].Created != "2026-10-01T09:05:00.000+0000" {
		t.Errorf("first comment = %+v, want its author and time decoded", got[0])
	}
	if want := []string{"5/3", "7/1"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("pages asked for = %v, want %v", asked, want)
	}
}

// TestListComments_StopsAtTheEndOfTheList: asking past the last comment ends
// the read with what there is.
func TestListComments_StopsAtTheEndOfTheList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("startAt") == "0" {
			fmt.Fprint(w, `{"startAt":0,"maxResults":5,"total":1,"comments":[{"id":"20000","created":"2026-10-01T09:00:00.000+0000"}]}`)
			return
		}
		fmt.Fprint(w, `{"startAt":1,"maxResults":4,"total":1,"comments":[]}`)
	}))
	t.Cleanup(srv.Close)

	got, err := cloudClient(srv.URL).ListComments(context.Background(), "10001", 0, 5)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(got) != 1 || got[0].ID != "20000" || got[0].Author != nil {
		t.Errorf("comments = %+v, want the one comment, with no author", got)
	}
}
