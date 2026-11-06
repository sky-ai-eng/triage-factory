package server

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// The three lists the prompt and blueprint pickers read each take `search`
// over a name: a prompt's and a blueprint's own, and a step's, which is the
// name of the prompt it runs. Each route narrows on the server, refuses a
// needle past the cap by naming the field, and refuses a page token minted
// under a different needle.

// pickerListRoute is one of the three routes and how a test reads the name a
// row matched on.
type pickerListRoute struct {
	path string
	// key names a row in the answer: a prompt's or blueprint's name, a step's
	// prompt id (its name is its prompt's, which the step row does not carry).
	key func(row map[string]any) string
}

func pickerListRoutes(stepPrompt map[string]string) []pickerListRoute {
	return []pickerListRoute{
		{path: "/api/prompts/list", key: func(r map[string]any) string { return r["name"].(string) }},
		{path: "/api/blueprints/list", key: func(r map[string]any) string { return r["name"].(string) }},
		{path: "/api/blueprint-steps/list", key: func(r map[string]any) string {
			return stepPrompt[r["step_prompt_id"].(string)]
		}},
	}
}

// seedPickerRows creates one wrapped blueprint per name — a blueprint, its one
// step, and the prompt that step runs, all carrying that name — and returns
// each step prompt's id mapped to its name.
func seedPickerRows(t *testing.T, s *Server, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range names {
		_, promptID := createWrappedBlueprint(t, s, name)
		out[promptID] = name
	}
	return out
}

func TestPickerLists_SearchNarrowsOnTheServer(t *testing.T) {
	s := newTestServer(t)
	stepPrompt := seedPickerRows(t, s, "Zqx 100% coverage", "Zqx 1000 widgets", "Zqy review")

	for _, route := range pickerListRoutes(stepPrompt) {
		t.Run(route.path, func(t *testing.T) {
			for _, tc := range []struct {
				search string
				want   []string
			}{
				// Case-insensitive, and trimmed.
				{"  zQx ", []string{"Zqx 100% coverage", "Zqx 1000 widgets"}},
				// A % is a literal percent sign, not a wildcard that would
				// also match "1000".
				{"100%", []string{"Zqx 100% coverage"}},
				{"zqz", nil},
			} {
				page := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, route.path,
					map[string]any{"search": tc.search}))
				got := []string{}
				for _, row := range page.Items {
					got = append(got, route.key(row))
				}
				sort.Strings(got)
				if strings.Join(got, "|") != strings.Join(tc.want, "|") || page.Total() != len(tc.want) {
					t.Errorf("search %q = %v (total %d), want %v", tc.search, got, page.Total(), tc.want)
				}
			}
		})
	}
}

func TestPickerLists_SearchIsValidated(t *testing.T) {
	s := newTestServer(t)
	stepPrompt := seedPickerRows(t, s, "Zqx one", "Zqx two")

	for _, route := range pickerListRoutes(stepPrompt) {
		t.Run(route.path, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, route.path, map[string]any{"search": strings.Repeat("x", 201)})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("201-rune search = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, "INVALID_FIELD", "search")
			if rec := doJSON(t, s, http.MethodPost, route.path, map[string]any{"search": strings.Repeat("é", 200)}); rec.Code != http.StatusOK {
				t.Errorf("200-rune search = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			// A field of the wrong type is refused by name, not dropped.
			rec = doJSON(t, s, http.MethodPost, route.path, map[string]any{"search": 7})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("numeric search = %d, want 400: %s", rec.Code, rec.Body.String())
			}

			// A page token is minted for the needle it was asked under.
			first := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, route.path,
				map[string]any{"search": "zqx", "page_size": 1}))
			if first.NextPageToken == "" {
				t.Fatalf("page 1 of two matches minted no token")
			}
			rec = doJSON(t, s, http.MethodPost, route.path, map[string]any{
				"search": "zqx one", "page_size": 1, "page_token": first.NextPageToken,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("token under another search = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, "INVALID_PARAM", "page_token")
			// Spellings the predicate folds together are one query, so they
			// share a token.
			second := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, route.path,
				map[string]any{"search": " ZQX ", "page_size": 1, "page_token": first.NextPageToken}))
			if len(second.Items) != 1 || route.key(second.Items[0]) == route.key(first.Items[0]) {
				t.Errorf("page 2 = %+v after page 1 = %+v, want the other match", second.Items, first.Items)
			}
		})
	}
}
