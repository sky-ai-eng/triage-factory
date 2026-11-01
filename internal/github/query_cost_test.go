package github

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestQueryDocuments_NameAnOperationAndSelectCost: the operation names are the
// cost histogram's op labels, so every query document in the package declares
// one, no two share it, and each selects rateLimit { cost }, the only place
// GitHub states what the query cost. A document is any string literal in the
// package's own source that starts with the query keyword.
func TestQueryDocuments_NameAnOperationAndSelectCost(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			doc, err := strconv.Unquote(lit.Value)
			if err != nil || !isQueryDocument(doc) {
				return true
			}
			pos := fset.Position(lit.Pos()).String()
			op := upstream.OperationName(doc)
			switch {
			case op == "":
				t.Errorf("%s: query document names no operation", pos)
			case seen[op] != "":
				t.Errorf("%s: operation %s is also declared at %s", pos, op, seen[op])
			default:
				seen[op] = pos
			}
			if !strings.Contains(doc, "rateLimit { cost }") {
				t.Errorf("%s: query document %s does not select rateLimit { cost }", pos, op)
			}
			return true
		})
	}
	if len(seen) == 0 {
		t.Fatal("found no query documents")
	}
}

// isQueryDocument reports whether s starts with the query keyword, followed by
// an operation name, its variables or its selection set.
func isQueryDocument(s string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "query")
	return ok && rest != "" && strings.ContainsRune(" \t\n({", rune(rest[0]))
}

// costServer answers every GraphQL query with an empty result and with
// rateLimit set to the JSON rateLimit returns for the query's operation.
func costServer(t *testing.T, rateLimit func(op string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprintf(w, `{"data":{"search":{"nodes":[]},"nodes":[],"rateLimit":%s}}`,
			rateLimit(upstream.OperationName(req.Query)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestQueryCost_ByOperationForEveryClient: rateLimit.cost is recorded under
// the operation the document names, whichever client sent it, including one
// that names no org or credential and so reports no budget. An anonymous
// document records nothing, and neither does a response whose rateLimit is
// null, as a GHES instance with rate limits off may send.
func TestQueryCost_ByOperationForEveryClient(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	upstream.SetMeterProviderForTest(t, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	costs := map[string]int{"DiscoverPRs": 13, "RefreshPRsWithChecks": 6, "RefreshPRsWithoutChecks": 5, "PRMergeability": 1, "": 1}
	srv := costServer(t, func(op string) string { return fmt.Sprintf(`{"cost":%d}`, costs[op]) })
	ghes := costServer(t, func(string) string { return "null" })
	ctx := context.Background()

	for _, c := range []*Client{NewClient(srv.URL, "tok"), patClient(srv.URL), NewClient(ghes.URL, "tok")} {
		if _, err := c.DiscoverPRs(ctx, "is:pr is:open", 50); err != nil {
			t.Fatalf("DiscoverPRs: %v", err)
		}
	}
	for _, withChecks := range []bool{true, false} {
		if _, err := patClient(srv.URL).RefreshPRs(ctx, []string{"PR_1"}, withChecks); err != nil {
			t.Fatalf("RefreshPRs(%v): %v", withChecks, err)
		}
	}
	if _, err := patClient(srv.URL).PRMergeability(ctx, []string{"PR_1"}); err != nil {
		t.Fatalf("PRMergeability: %v", err)
	}
	if _, err := patClient(srv.URL).PostGraphQL(ctx, gqlProbe, nil); err != nil {
		t.Fatalf("PostGraphQL: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	type series struct{ count, sum int64 }
	got := map[string]series{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			hist, ok := m.Data.(metricdata.Histogram[int64])
			if m.Name != "upstream.query.cost" || !ok {
				continue
			}
			for _, dp := range hist.DataPoints {
				op, _ := dp.Attributes.Value(telemetry.Op("").Key)
				want := attribute.NewSet(telemetry.Upstream("github"), telemetry.Op(op.AsString()))
				if !dp.Attributes.Equals(&want) {
					t.Errorf("series %v, want labels upstream and op only", dp.Attributes.Encoded(attribute.DefaultEncoder()))
				}
				got[op.AsString()] = series{int64(dp.Count), dp.Sum}
			}
		}
	}
	want := map[string]series{
		"DiscoverPRs":             {2, 26},
		"RefreshPRsWithChecks":    {1, 6},
		"RefreshPRsWithoutChecks": {1, 5},
		"PRMergeability":          {1, 1},
	}
	if len(got) != len(want) {
		t.Errorf("series = %v, want %v", got, want)
	}
	for op, w := range want {
		if got[op] != w {
			t.Errorf("%s: count, sum = %v, want %v", op, got[op], w)
		}
	}
}
