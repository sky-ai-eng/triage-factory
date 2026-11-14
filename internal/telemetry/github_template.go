package telemetry

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// githubNamedSegments are the path words whose following segments are always
// tenant names, so those segments get placeholders that say what they are
// rather than "{}": /repos/{owner}/{repo} reads as a repository where
// /repos/{}/{} would not. The word itself is kept.
var githubNamedSegments = map[string][]string{
	"repos": {"{owner}", "{repo}"},
	"orgs":  {"{org}"},
	"users": {"{user}"},
}

// githubPathWords are GitHub's own words in the paths TF requests: the literal
// segments of the REST, GraphQL, OAuth, release-download, and smart-HTTP
// paths built in internal/github, cmd/exec, internal/proxy, internal/ghcli,
// and the GitHub App handlers. A segment on this list is the same in every
// tenant's request, so it can name the endpoint; every other segment is
// replaced. The list is an allowlist on purpose: a word TF starts requesting
// without adding it here shows as "{}", which costs a reader some precision,
// where a denylist that missed a name would export it.
var githubPathWords = map[string]struct{}{
	"access_token":     {},
	"access_tokens":    {},
	"actions":          {},
	"alerts":           {},
	"api":              {},
	"app":              {},
	"app-manifests":    {},
	"applications":     {},
	"attempts":         {},
	"batch":            {},
	"branches":         {},
	"check-runs":       {},
	"comments":         {},
	"commits":          {},
	"compare":          {},
	"config":           {},
	"contents":         {},
	"conversions":      {},
	"deliveries":       {},
	"dependabot":       {},
	"dismissals":       {},
	"download":         {},
	"emails":           {},
	"files":            {},
	"git":              {},
	"git-receive-pack": {},
	"git-upload-pack":  {},
	"graphql":          {},
	"heads":            {},
	"hook":             {},
	"info":             {},
	"installation":     {},
	"installations":    {},
	"issues":           {},
	"jobs":             {},
	"lfs":              {},
	"locks":            {},
	"login":            {},
	"logs":             {},
	"memberships":      {},
	"oauth":            {},
	"objects":          {},
	"pulls":            {},
	"reactions":        {},
	"ref":              {},
	"refs":             {},
	"releases":         {},
	"replies":          {},
	"repositories":     {},
	"reviews":          {},
	"runs":             {},
	"search":           {},
	"ssh_signing_keys": {},
	"teams":            {},
	"timeline":         {},
	"token":            {},
	"transfers":        {},
	"user":             {},
	"v3":               {},
	"verify":           {},
}

// githubURLTemplate turns a GitHub request path into a template that names
// the endpoint and no tenant: /repos/acme/widgets/pulls/7/reviews?per_page=100
// becomes /repos/{owner}/{repo}/pulls/{}/reviews. The query is dropped, the
// segments after a named word take that word's placeholders whatever they
// say, a segment on the word list is kept, and every other segment becomes
// "{}". An empty segment stays empty, since it carries nothing.
//
// A free-form segment that happens to equal a word on the list, such as a
// branch named "comments", shows as that word. That can mislead a reader
// about the endpoint, but what it exports is still one of GitHub's words.
func githubURLTemplate(path string) string {
	path, _, _ = strings.Cut(path, "?")
	segs := strings.Split(path, "/")
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if seg == "" {
			continue
		}
		if names, ok := githubNamedSegments[seg]; ok {
			for _, name := range names {
				if i+1 == len(segs) {
					break
				}
				i++
				if segs[i] != "" {
					segs[i] = name
				}
			}
			continue
		}
		if _, ok := githubPathWords[seg]; !ok {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// githubTemplateTransport sits between otelhttp and the transport it sends
// through, because otelhttp hands that transport the request under the ctx
// holding its client span, and offers no other way to add an attribute to it.
// Outside a trace the ctx's span is not recording, so nothing is computed.
type githubTemplateTransport struct{ base http.RoundTripper }

func (t githubTemplateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if span := trace.SpanFromContext(req.Context()); span.IsRecording() {
		span.SetAttributes(URLTemplate(githubURLTemplate(req.URL.EscapedPath())))
	}
	return t.base.RoundTrip(req)
}
