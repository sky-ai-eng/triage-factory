package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// alertJSON is one alert object as GitHub's list endpoint returns it, with the
// fields TF reads and a few it does not.
func alertJSON(number int, state, ecosystem, name, severity string) string {
	return fmt.Sprintf(`{
		"number": %d, "state": %q, "updated_at": "2026-11-01T10:00:00Z",
		"html_url": "https://github.com/octo/api/security/dependabot/%d",
		"dependency": {"package": {"ecosystem": %q, "name": %q}, "manifest_path": "app/package.json", "scope": "runtime", "relationship": "direct"},
		"security_advisory": {"ghsa_id": "GHSA-%04d", "cve_id": "CVE-2026-%04d", "summary": "advisory text", "severity": %q},
		"security_vulnerability": {"package": {"ecosystem": %q, "name": %q}, "severity": %q, "vulnerable_version_range": "< 2.0.0", "first_patched_version": {"identifier": "2.0.0"}}
	}`, number, state, number, ecosystem, name, number, number, severity, ecosystem, name, severity)
}

// TestClient_ListOpenDependabotAlerts_OnePage: a listing that fits one page
// returns its open alerts with the fields a snapshot records, and the page's
// ETag for the next listing to send.
func TestClient_ListOpenDependabotAlerts_OnePage(t *testing.T) {
	var gotURI, gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotIfNoneMatch = r.URL.RequestURI(), r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `W/"page1"`)
		_, _ = fmt.Fprintf(w, `[%s, %s, {"number": 9, "state": "open", "dependency": {"package": {}}}]`,
			alertJSON(3, "open", "npm", "lodash", "high"),
			alertJSON(4, "fixed", "npm", "lodash", "low"))
	}))
	t.Cleanup(srv.Close)

	alerts, etag, notModified, err := NewClient(srv.URL, "tok").ListOpenDependabotAlerts(context.Background(), "octo", "api", `W/"before"`)
	if err != nil || notModified {
		t.Fatalf("ListOpenDependabotAlerts: notModified=%v err=%v", notModified, err)
	}
	if want := "/repos/octo/api/dependabot/alerts?state=open&per_page=100"; !strings.HasSuffix(gotURI, want) {
		t.Errorf("request = %q, want it to end %q", gotURI, want)
	}
	if gotIfNoneMatch != `W/"before"` {
		t.Errorf("If-None-Match = %q, want the stored ETag", gotIfNoneMatch)
	}
	if etag != `W/"page1"` {
		t.Errorf("etag = %q, want the page's", etag)
	}
	want := []OpenDependabotAlert{{
		Ecosystem: "npm", Package: "lodash",
		Alert: domain.DependabotAlert{
			Number: 3, GHSAID: "GHSA-0003", CVEID: "CVE-2026-0003", Severity: "high",
			ManifestPath: "app/package.json", Scope: "runtime", FirstPatchedVersion: "2.0.0",
		},
		UpdatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}}
	if len(alerts) != 1 || alerts[0] != want[0] {
		t.Errorf("alerts = %+v, want only the open alert with a package: %+v", alerts, want)
	}
}

// TestClient_ListOpenDependabotAlerts_NotModified: GitHub's 304 is the answer
// that nothing changed.
func TestClient_ListOpenDependabotAlerts_NotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"same"` {
			t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)

	alerts, etag, notModified, err := NewClient(srv.URL, "tok").ListOpenDependabotAlerts(context.Background(), "octo", "api", `W/"same"`)
	if err != nil || !notModified || alerts != nil || etag != "" {
		t.Errorf("= (%v, %q, %v, %v), want not modified", alerts, etag, notModified, err)
	}
}

// TestClient_ListOpenDependabotAlerts_FollowsTheNextCursor: a listing of
// several pages follows the Link header's after cursor on the client's own
// base URL, whatever host the link names, and returns no ETag, since a 304 on
// the first page would hide a change on a later one.
func TestClient_ListOpenDependabotAlerts_FollowsTheNextCursor(t *testing.T) {
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.URL.RequestURI())
		w.Header().Set("ETag", `W/"`+r.URL.Query().Get("after")+`"`)
		switch r.URL.Query().Get("after") {
		case "":
			w.Header().Set("Link", `<https://elsewhere.example/repositories/1/dependabot/alerts?per_page=100&after=c2>; rel="next", <https://elsewhere.example/x?before=c0>; rel="prev"`)
			_, _ = fmt.Fprintf(w, `[%s]`, alertJSON(2, "open", "pip", "django", "critical"))
		case "c2":
			_, _ = fmt.Fprintf(w, `[%s]`, alertJSON(1, "open", "pip", "django", "low"))
		default:
			t.Errorf("unexpected cursor in %s", r.URL.RequestURI())
		}
	}))
	t.Cleanup(srv.Close)

	alerts, etag, _, err := NewClient(srv.URL, "tok").ListOpenDependabotAlerts(context.Background(), "octo", "api", "")
	if err != nil {
		t.Fatalf("ListOpenDependabotAlerts: %v", err)
	}
	if len(alerts) != 2 || alerts[0].Alert.Number != 2 || alerts[1].Alert.Number != 1 {
		t.Errorf("alerts = %+v, want both pages' alerts", alerts)
	}
	if etag != "" {
		t.Errorf("etag = %q, want none for a listing of more than one page", etag)
	}
	if want := "/repos/octo/api/dependabot/alerts?state=open&per_page=100&after=c2"; len(uris) != 2 || !strings.HasSuffix(uris[1], want) {
		t.Errorf("requests = %v, want the second to end %q", uris, want)
	}
}

// TestClient_ListOpenDependabotAlerts_AFailedPageFailsTheListing: a later page
// that fails fails the listing, so no caller reads a partial one as complete.
func TestClient_ListOpenDependabotAlerts_AFailedPageFailsTheListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			w.Header().Set("Link", `<https://api.github.com/x?after=c2>; rel="next"`)
			_, _ = fmt.Fprintf(w, `[%s]`, alertJSON(2, "open", "npm", "a", "low"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(srv.Close)

	alerts, _, _, err := NewClient(srv.URL, "tok").ListOpenDependabotAlerts(context.Background(), "octo", "api", "")
	if err == nil || alerts != nil {
		t.Errorf("= (%v, %v), want an error and no alerts", alerts, err)
	}
}

// TestClient_ListOpenDependabotAlerts_StopsAtThePageCap: a listing whose
// pages never end fails at the cap rather than returning what it read.
func TestClient_ListOpenDependabotAlerts_StopsAtThePageCap(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/x?after=c%d>; rel="next"`, requests))
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	_, _, _, err := NewClient(srv.URL, "tok").ListOpenDependabotAlerts(context.Background(), "octo", "api", "")
	if err == nil || !strings.Contains(err.Error(), "pages") {
		t.Errorf("err = %v, want the page cap", err)
	}
	if requests != maxDependabotAlertPages {
		t.Errorf("requests = %d, want %d", requests, maxDependabotAlertPages)
	}
}

func TestNextPageCursor(t *testing.T) {
	for link, want := range map[string]string{
		"": "",
		`<https://api.github.com/r?after=abc%3D&per_page=100>; rel="next"`:                                  "abc=",
		`<https://api.github.com/r?before=zz>; rel="prev", <https://api.github.com/r?after=nx>; rel="next"`: "nx",
		`<https://api.github.com/r?before=zz>; rel="prev"`:                                                  "",
		`<https://api.github.com/r?page=2>; rel="next"`:                                                     "",
	} {
		if got := nextPageCursor(link); got != want {
			t.Errorf("nextPageCursor(%q) = %q, want %q", link, got, want)
		}
	}
}
