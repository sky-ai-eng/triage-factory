package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExchangeManifestCode_ErrorCarriesOnlyAnExcerpt: the handler logs the
// exchange's error at ERROR, so a failed exchange's message carries the status
// and GitHub's own message from a JSON body, and only the size of anything
// else, such as a proxy's HTML page.
func TestExchangeManifestCode_ErrorCarriesOnlyAnExcerpt(t *testing.T) {
	page := "<!DOCTYPE html><html><body><h1>502 Bad Gateway</h1>" + strings.Repeat("<p>upstream</p>", 10000) + "</body></html>"
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    string
		notWant []string
	}{
		{
			name:    "proxy page",
			status:  http.StatusBadGateway,
			body:    page,
			want:    "github returned 502: non-JSON body",
			notWant: []string{"<", "upstream"},
		},
		{
			name:    "github's own error",
			status:  http.StatusNotFound,
			body:    `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/apps/apps"}`,
			want:    "github returned 404: Not Found",
			notWant: []string{"documentation_url"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			_, err := exchangeManifestCode(context.Background(), srv.URL+"/app-manifests/code/conversions")
			if err == nil {
				t.Fatal("exchangeManifestCode returned no error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not carry %q", msg, tc.want)
			}
			for _, s := range tc.notWant {
				if strings.Contains(msg, s) {
					t.Errorf("error carries %q from the body: %q", s, msg)
				}
			}
			if len(msg) > 300 {
				t.Errorf("error is %d bytes long", len(msg))
			}
		})
	}
}
