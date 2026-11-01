package tracker

import (
	"net/http"
	"strings"
)

// isBranchRefRead reports whether r is the branch watch's read of a base
// branch's tip. A test server whose subject is not the watch answers it 404:
// the watch logs the failed read and probes nothing.
func isBranchRefRead(r *http.Request) bool {
	return strings.Contains(r.URL.Path, "/git/ref/heads/")
}
