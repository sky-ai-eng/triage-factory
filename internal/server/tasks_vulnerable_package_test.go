package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// lodashSnapshot is a package with two open alerts, the low one numbered
// first, so a read that orders by severity is told apart from one that keeps
// the snapshot's order.
func lodashSnapshot(repo string) *domain.VulnerablePackageSnapshot {
	return &domain.VulnerablePackageSnapshot{Repo: repo, Ecosystem: "npm", Package: "lodash", Alerts: []domain.DependabotAlert{
		{Number: 4, GHSAID: "GHSA-low0-0000-0000", Severity: "low", ManifestPath: "web/package.json", Scope: "development"},
		{Number: 11, GHSAID: "GHSA-high-0000-0000", CVEID: "CVE-2026-7", Summary: "<b>Prototype pollution</b> in zipObjectDeep", Severity: "high", ManifestPath: "package-lock.json", Scope: "runtime", VulnerableVersionRange: "< 4.17.21", FirstPatchedVersion: "4.17.21"},
	}}
}

// TestTask_VulnerablePackageReadShape: a vulnerable-package task carries its
// package, highest severity, alert count and every open alert, highest
// severity first, each linked to its page on GitHub, on the list and the
// point read alike; a pull request's task carries none of it.
func TestTask_VulnerablePackageReadShape(t *testing.T) {
	s := newTestServer(t)
	pkgTask := seedTaskFixture(t, s.db, taskFixture{name: "vp", status: "queued", vulnerable: lodashSnapshot("octo/vp-read")})
	prTask := seedTaskFixture(t, s.db, taskFixture{name: "vp-pr", status: "queued"})

	want := &vulnerablePackageJSON{
		Repo: "octo/vp-read", Ecosystem: "npm", Package: "lodash",
		HighestSeverity: "high", OpenAlertCount: 2,
		Alerts: []dependabotAlertJSON{
			{Number: 11, URL: "https://github.com/octo/vp-read/security/dependabot/11", GHSAID: "GHSA-high-0000-0000", CVEID: "CVE-2026-7", Summary: "<b>Prototype pollution</b> in zipObjectDeep", Severity: "high", ManifestPath: "package-lock.json", Scope: "runtime", VulnerableVersionRange: "< 4.17.21", FirstPatchedVersion: "4.17.21"},
			{Number: 4, URL: "https://github.com/octo/vp-read/security/dependabot/4", GHSAID: "GHSA-low0-0000-0000", Severity: "low", ManifestPath: "web/package.json", Scope: "development"},
		},
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/list", map[string]any{"ids": []string{pkgTask, prTask}})
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			ID                string                 `json:"id"`
			EntityKind        string                 `json:"entity_kind"`
			VulnerablePackage *vulnerablePackageJSON `json:"vulnerable_package"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("list returned %d items, want 2", len(page.Items))
	}
	for _, item := range page.Items {
		switch item.ID {
		case pkgTask:
			if item.EntityKind != domain.EntityKindVulnerablePackage || !reflect.DeepEqual(item.VulnerablePackage, want) {
				t.Errorf("listed package task = kind %q, %+v; want %+v", item.EntityKind, item.VulnerablePackage, want)
			}
		case prTask:
			if item.VulnerablePackage != nil {
				t.Errorf("listed pull request task carries vulnerable_package %+v", item.VulnerablePackage)
			}
		}
	}

	rec = doJSON(t, s, http.MethodGet, "/api/tasks/"+pkgTask, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		VulnerablePackage *vulnerablePackageJSON `json:"vulnerable_package"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if !reflect.DeepEqual(got.VulnerablePackage, want) {
		t.Errorf("GET vulnerable_package = %+v, want %+v", got.VulnerablePackage, want)
	}

	// A package whose last alert closed reads as such: no severity, an empty
	// list rather than null.
	resolved := seedTaskFixture(t, s.db, taskFixture{name: "vp-resolved", status: "queued", vulnerable: &domain.VulnerablePackageSnapshot{Repo: "octo/vp-read", Ecosystem: "pip", Package: "requests", Alerts: []domain.DependabotAlert{}}})
	rec = doJSON(t, s, http.MethodGet, "/api/tasks/"+resolved, nil)
	var raw struct {
		VulnerablePackage map[string]json.RawMessage `json:"vulnerable_package"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode resolved: %v", err)
	}
	if _, ok := raw.VulnerablePackage["highest_severity"]; ok || string(raw.VulnerablePackage["alerts"]) != "[]" || string(raw.VulnerablePackage["open_alert_count"]) != "0" {
		t.Errorf("resolved package = %s, want no highest_severity, alerts [] and a count of 0", rec.Body.String())
	}
}

// TestTasks_EntityKindsNarrowTheLaneAndItsFacets: entity_kinds narrows the
// list and the total it reports; on the facets route it narrows the event
// types cut, while the entity_kinds cut leaves it out so the kinds a reader
// narrowed away are still offered.
func TestTasks_EntityKindsNarrowTheLaneAndItsFacets(t *testing.T) {
	s := newTestServer(t)
	pkgA := seedTaskFixture(t, s.db, taskFixture{name: "kind-a", status: "queued", vulnerable: lodashSnapshot("octo/kind-a")})
	pkgB := seedTaskFixture(t, s.db, taskFixture{name: "kind-b", status: "queued", vulnerable: lodashSnapshot("octo/kind-b")})
	seedTaskFixture(t, s.db, taskFixture{name: "kind-pr", status: "queued"})

	page := postTaskList(t, s, map[string]any{"statuses": []string{"queued"}, "entity_kinds": []string{domain.EntityKindVulnerablePackage}})
	got := listedIDs(page)
	slices.Sort(got)
	if want := slices.Sorted(slices.Values([]string{pkgA, pkgB})); !slices.Equal(got, want) || page.TotalCount != 2 {
		t.Errorf("entity_kinds=[vulnerable_package] = %v (total %d), want %s and %s", got, page.TotalCount, pkgA, pkgB)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/facets", map[string]any{
		"statuses":     []string{"queued"},
		"entity_kinds": []string{domain.EntityKindVulnerablePackage},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("facets status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var facets struct {
		EventTypes []struct {
			Value string
			Count int
		} `json:"event_types"`
		EntityKinds []struct {
			Value string
			Count int
		} `json:"entity_kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &facets); err != nil {
		t.Fatalf("decode facets: %v", err)
	}
	counts := func(in []struct {
		Value string
		Count int
	}) map[string]int {
		out := map[string]int{}
		for _, f := range in {
			out[f.Value] = f.Count
		}
		return out
	}
	if got, want := counts(facets.EntityKinds), map[string]int{domain.EntityKindPullRequest: 1, domain.EntityKindVulnerablePackage: 2}; !maps.Equal(got, want) {
		t.Errorf("entity_kinds cut = %v, want %v (computed without its own filter)", got, want)
	}
	if got, want := counts(facets.EventTypes), map[string]int{domain.EventGitHubVulnerabilityAlertOpened: 2}; !maps.Equal(got, want) {
		t.Errorf("event_types cut = %v, want %v (narrowed by entity_kinds)", got, want)
	}
}
