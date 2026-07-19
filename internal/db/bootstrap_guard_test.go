package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestBootstrapUsesAdminPoolStoreCalls is the static guard. It is
// the always-on (no Docker) companion to the two-pool pgtest regression
// guard TestBootstrapNewOrg_Postgres_TwoPool — same family as
// TestBootstrapSchemaForTest_MatchesMigrate and
// TestBootstrapLocalTenancy_ConstantsMatchRows.
//
// # The invariant
//
// The multi-mode bootstrap chain (BootstrapNewOrg / BootstrapNewTeam /
// BootstrapTeamAgent / BootstrapAgentForOrg / BootstrapLocalOrg in
// bootstrap.go) runs OUTSIDE any WithTx with no JWT claims. The Postgres
// app pool is a bare `authenticator` connection until SET ROLE tf_app +
// claims inside a request tx, so it holds no table grants. Every store
// method bootstrap calls must therefore route through the admin pool —
// either a `*System` admin-pool read twin or one of the admin-pool writes.
//
// PR #265 was one instance of this class: BootstrapTeamAgent
// looked up the agent via the app-pool Agents.GetForOrg, which failed
// with "permission denied for table agents" (SQLSTATE 42501) on the
// grant-less authenticator connection, the error was swallowed by the
// founder-signup log-and-continue, and the org silently shipped without
// its team_agents bot row (auto-delegation dead). The inTx guards on the
// writes protect the *opposite* direction (admin-escape into an app tx);
// nothing protected an app-pool *read* called outside any tx. This test
// closes that gap: a new app-pool read (GetForOrg, GetForTeam,
// ListForOrg, ...) added to bootstrap.go trips here at unit-test time,
// before it can reach a Docker-gated pg test or production.
//
// # What it checks
//
// Parses internal/db/bootstrap.go, finds every store-method call — a
// CallExpr whose Fun is a SelectorExpr rooted at the `stores` parameter,
// i.e. stores.<Store>.<Method>(…) — and fails if <Method> is not
// admin-routed: not `*System`-suffixed and not on the explicit
// admin-write allowlist below.
//
// Scope note: the walk is deliberately limited to bootstrap.go. If the
// bootstrap chain later splits across files, add them to bootstrapFiles
// (and say so here).
func TestBootstrapUsesAdminPoolStoreCalls(t *testing.T) {
	bootstrapFiles := []string{"bootstrap.go"}

	// adminWrites is the allowlist of non-`*System` store calls that are
	// known to route through the admin pool (and carry the inTx guard that
	// refuses an admin-escape from inside a caller's tx). Keyed by the
	// fully-qualified `<Store>.<Method>` pair, NOT the bare method name: an
	// admin-pool write is a property of one specific store's
	// implementation, not of a method name. `Create` is admin-routed on
	// agentStore but app-routed on other stores' `Create` peers — pinning
	// the pair stops a future stores.<OtherStore>.Create() (or any reused
	// name) from passing on the strength of a name match alone. Extending
	// this list is a CONSCIOUS act: only add a pair here after confirming
	// that store's impl actually targets s.admin (see the per-store "Pool
	// split" doc comments in internal/db/postgres/*.go). Adding an
	// app-pool read here would defeat the whole guard — see the invariant
	// documented above.
	adminWrites := map[string]bool{
		// agentStore.Create — admin-pool INSERT, inTx-guarded (agents.go).
		"Agents.Create": true,
		// teamAgentStore.AddForTeam — admin-pool INSERT, inTx-guarded
		// (team_agents.go).
		"TeamAgents.AddForTeam": true,
		// shippedDefaultsStore.SeedShippedIntoTeam — admin-pool tx,
		// inTx-guarded (shipped_defaults.go). Bootstrap seeds a team's
		// prompts/blueprints/handlers directly from the shipped Go slices.
		"ShippedDefaults.SeedShippedIntoTeam": true,
		// orgsStore.CreateLocalTenant — local-mode (SQLite N=1) only; the
		// Postgres impl returns an explicit "not supported in multi mode"
		// error and never touches the app pool (orgs.go).
		"Orgs.CreateLocalTenant": true,
	}

	fset := token.NewFileSet()
	var found int
	for _, name := range bootstrapFiles {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			storesParam := storesParamName(fn)
			if storesParam == "" {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				store, method, ok := storeMethodCall(call, storesParam)
				if !ok {
					return true
				}
				found++

				if strings.HasSuffix(method, "System") {
					return true
				}
				if adminWrites[store+"."+method] {
					return true
				}

				pos := fset.Position(call.Pos())
				t.Errorf("%s:%d: bootstrap calls app-pool store method %s.%s — bootstrap "+
					"runs outside any WithTx with no JWT claims, so every store call must "+
					"route through the admin pool. Use the `*System` admin-pool twin, or if "+
					"%s.%s is a genuinely new admin-pool write add it to adminWrites in this "+
					"test (after confirming it targets s.admin). See SKY-387 / SKY-385.",
					name, pos.Line, store, method, store, method)
				return true
			})
		}
	}

	if found == 0 {
		t.Fatalf("found zero stores.<Store>.<Method>() calls in %v — the AST walk likely "+
			"broke (bootstrap refactored, or the `stores` parameter renamed). This guard is "+
			"only meaningful if it actually inspects the bootstrap store calls.", bootstrapFiles)
	}
}

// storesParamName returns the name of fn's parameter of type Stores (the
// guard roots its call-matching at this identifier), or "" if fn has no
// such parameter.
func storesParamName(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil {
		return ""
	}
	for _, field := range fn.Type.Params.List {
		ident, ok := field.Type.(*ast.Ident)
		if !ok || ident.Name != "Stores" {
			continue
		}
		if len(field.Names) > 0 {
			return field.Names[0].Name
		}
	}
	return ""
}

// storeMethodCall reports whether call is a stores.<Store>.<Method>(…)
// invocation rooted at the identifier storesParam, returning the store
// field and method names. It matches the two-level selector chain
// CallExpr.Fun = SelectorExpr{X: SelectorExpr{X: Ident(storesParam),
// Sel: <Store>}, Sel: <Method>}.
func storeMethodCall(call *ast.CallExpr, storesParam string) (store, method string, ok bool) {
	methodSel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	storeSel, ok := methodSel.X.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	root, ok := storeSel.X.(*ast.Ident)
	if !ok || root.Name != storesParam {
		return "", "", false
	}
	return storeSel.Sel.Name, methodSel.Sel.Name, true
}
