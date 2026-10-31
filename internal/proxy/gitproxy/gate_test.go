package gitproxy

import (
	"net/http"
	"testing"
)

// TestParseGitPath is the path-shape allowlist: exactly the three git
// smart-HTTP shapes are recognized (and yield owner/repo + op); everything
// else — a GHES API path, Git-LFS, a web route, a traversal — is rejected so
// the Handler 403s it before any credential is injected.
func TestParseGitPath(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		wantOwner string
		wantRepo  string
		wantOp    string
		wantOK    bool
	}{
		// Allowed shapes.
		{"advertise", http.MethodGet, "/octo/repo/info/refs", "octo", "repo", gitOpAdvertise, true},
		{"advertise .git suffix", http.MethodGet, "/octo/repo.git/info/refs", "octo", "repo", gitOpAdvertise, true},
		{"fetch", http.MethodPost, "/octo/repo/git-upload-pack", "octo", "repo", gitOpFetch, true},
		{"push", http.MethodPost, "/octo/repo.git/git-receive-pack", "octo", "repo", gitOpPush, true},

		// GHES same-host API path — THE hole the allowlist closes. owner="api",
		// repo="v3", rest=["repos",...] doesn't match info/refs or the packs.
		{"ghes api repos", http.MethodGet, "/api/v3/repos/octo/repo", "", "", "", false},
		{"ghes api admin", http.MethodPost, "/api/v3/admin/users", "", "", "", false},

		// Git-LFS (same host, would otherwise inherit the credential).
		{"lfs batch", http.MethodPost, "/octo/repo.git/info/lfs/objects/batch", "", "", "", false},

		// Web routes / other.
		{"web settings", http.MethodGet, "/octo/repo/settings", "", "", "", false},
		{"web tree", http.MethodGet, "/octo/repo/tree/main", "", "", "", false},
		{"root", http.MethodGet, "/", "", "", "", false},
		{"repo only no owner", http.MethodGet, "/repo.git/info/refs", "", "", "", false},

		// Method mismatches.
		{"advertise via POST", http.MethodPost, "/octo/repo/info/refs", "", "", "", false},
		{"upload-pack via GET", http.MethodGet, "/octo/repo/git-upload-pack", "", "", "", false},

		// Path traversal in owner/repo is rejected.
		{"traversal owner", http.MethodPost, "/../octo/git-receive-pack", "", "", "", false},
		{"traversal repo", http.MethodPost, "/octo/..%2f/git-receive-pack", "", "", "", false},
		{"dotdot segment", http.MethodGet, "/octo/re..po/info/refs", "", "", "", false},

		// Off-charset segments are rejected (defense-in-depth): a surviving
		// percent-escape, whitespace, or any byte outside [A-Za-z0-9._-].
		{"percent-escape in repo", http.MethodGet, "/octo/re%2epo/info/refs", "", "", "", false},
		{"percent-slash in owner", http.MethodGet, "/oc%2fto/repo/info/refs", "", "", "", false},
		{"whitespace in owner", http.MethodGet, "/oc to/repo/info/refs", "", "", "", false},
		{"at-sign in repo", http.MethodPost, "/octo/re@po/git-receive-pack", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, repo, op, ok := parseGitPath(c.method, c.path)
			if ok != c.wantOK || owner != c.wantOwner || repo != c.wantRepo || op != c.wantOp {
				t.Errorf("parseGitPath(%s %s) = (%q, %q, %q, %v); want (%q, %q, %q, %v)",
					c.method, c.path, owner, repo, op, ok, c.wantOwner, c.wantRepo, c.wantOp, c.wantOK)
			}
		})
	}
}

// TestIsGitSmartHTTPPath_SupersetOfParseGitPath is the drift guard between the
// routing predicate and the admission one. The front door that serves this
// proxy alongside the gh credential injector on one listener routes with
// IsGitSmartHTTPPath; if that predicate ever missed a shape parseGitPath
// admits, a legitimate git request would be routed to the API injector and
// forwarded to GitHub under a real token. So every path parseGitPath accepts
// must route here — and the reverse must NOT hold, since routing a malformed
// git path to the git gate (which 403s it) is the whole point.
func TestIsGitSmartHTTPPath_SupersetOfParseGitPath(t *testing.T) {
	admitted := []struct{ method, path string }{
		{http.MethodGet, "/octo/repo/info/refs"},
		{http.MethodGet, "/octo/repo.git/info/refs"},
		{http.MethodPost, "/octo/repo/git-upload-pack"},
		{http.MethodPost, "/octo/repo.git/git-receive-pack"},
	}
	for _, c := range admitted {
		if _, _, _, ok := parseGitPath(c.method, c.path); !ok {
			t.Fatalf("fixture drift: parseGitPath(%s %s) no longer admits this shape", c.method, c.path)
		}
		if !IsGitSmartHTTPPath(c.method, c.path) {
			t.Errorf("IsGitSmartHTTPPath(%s %s) = false; a path the gate admits must route to it", c.method, c.path)
		}
	}

	// Malformed git paths route to the gate (which refuses them) rather than
	// falling through to whatever else shares the listener.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/oc%2fto/repo/info/refs"},
		{http.MethodPost, "/octo/re@po/git-receive-pack"},
		{http.MethodGet, "/info/refs"},
	} {
		if _, _, _, ok := parseGitPath(c.method, c.path); ok {
			t.Fatalf("fixture drift: parseGitPath(%s %s) now admits this shape", c.method, c.path)
		}
		if !IsGitSmartHTTPPath(c.method, c.path) {
			t.Errorf("IsGitSmartHTTPPath(%s %s) = false; a git-shaped path must reach the gate's refusal, not the API path", c.method, c.path)
		}
	}

	// Non-git shapes must NOT route here — most importantly the GHES API
	// prefix, which is the other half of the shared listener.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v3/repos/octo/repo"},
		{http.MethodPost, "/api/graphql"},
		{http.MethodPost, "/octo/repo.git/info/lfs/objects/batch"},
		{http.MethodGet, "/octo/repo/settings"},
		{http.MethodPost, "/octo/repo/info/refs"},
		{http.MethodGet, "/octo/repo/git-upload-pack"},
		{http.MethodGet, "/"},
	} {
		if IsGitSmartHTTPPath(c.method, c.path) {
			t.Errorf("IsGitSmartHTTPPath(%s %s) = true; want false", c.method, c.path)
		}
	}
}

// TestParseGateCommands covers the enforcement parser that — unlike the
// observe-only parseReceivePackCommands — keeps delete commands so the gate
// can reject them.
func TestParseGateCommands(t *testing.T) {
	body := pktTest(zeroOIDTest+" aaaa1111 refs/heads/main\x00report-status\n") +
		pktTest("bbbb2222 "+zeroOIDTest+" refs/heads/del\n") + // delete
		pktTest("cccc3333 dddd4444 refs/tags/v1\n") +
		"0000PACK..."
	cmds := parseGateCommands([]byte(body))
	if len(cmds) != 3 {
		t.Fatalf("parsed %d commands, want 3 (deletes included)", len(cmds))
	}
	want := []gateCommand{
		{ref: "refs/heads/main", isDelete: false},
		{ref: "refs/heads/del", isDelete: true},
		{ref: "refs/tags/v1", isDelete: false},
	}
	for i, w := range want {
		if cmds[i] != w {
			t.Errorf("cmd[%d] = %+v, want %+v", i, cmds[i], w)
		}
	}
}

// pktTest frames s as a git pkt-line. (Local copy so the white-box package
// test doesn't depend on the external test package's pkt helper.)
func pktTest(s string) string {
	const hex = "0123456789abcdef"
	n := len(s) + 4
	return string([]byte{hex[(n>>12)&0xf], hex[(n>>8)&0xf], hex[(n>>4)&0xf], hex[n&0xf]}) + s
}

const zeroOIDTest = "0000000000000000000000000000000000000000"
