// Independent SigV4 verification for the Phase 2 tests.
//
// This file reimplements the AWS Signature Version 4 algorithm from the
// spec (https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html)
// WITHOUT importing the AWS SDK's signer, so the Tier 1 tests can
// cross-check aws-sdk-go-v2's SignHTTP against a second implementation,
// and the Tier 2 mock upstream can validate a re-signed request the way
// AWS itself would: reconstruct the canonical request from what arrived
// on the wire (using the SignedHeaders list the Authorization header
// declares), derive the signing key from the shared secret, and compare
// signatures. Sharing the SDK's code here would make the tests circular.
//
// Spec behaviors deliberately mirrored (and pinned by the official
// test-vector test in sigv4_test.go):
//
//   - Canonical URI is the DOUBLE-encoded path: each byte of the wire
//     (once-encoded) path outside [A-Za-z0-9-._~/] is percent-encoded
//     again, uppercase hex. This is the documented behavior for every
//     service except S3.
//   - Canonical query is the sorted, %20-space-encoded re-encoding of
//     the parsed query string.
//   - Canonical headers use lowercase names from the SignedHeaders list,
//     values trimmed with runs of spaces collapsed, multi-values joined
//     with ','. host and content-length come from the request line /
//     framing, not the header map.
package llmproxy_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// sigV4AuthParts is the parsed form of a SigV4 Authorization header.
type sigV4AuthParts struct {
	AccessKeyID   string
	Date          string // YYYYMMDD scope date
	Region        string
	Service       string
	Terminator    string // must be "aws4_request"
	SignedHeaders []string
	Signature     string
}

// parseSigV4Authorization splits an Authorization header of the form
//
//	AWS4-HMAC-SHA256 Credential=<akid>/<date>/<region>/<service>/aws4_request, SignedHeaders=a;b;c, Signature=<hex>
//
// failing the test on any shape violation.
func parseSigV4Authorization(t *testing.T, header string) sigV4AuthParts {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "AWS4-HMAC-SHA256 ")
	if !ok {
		t.Fatalf("Authorization %q does not start with %q", header, "AWS4-HMAC-SHA256 ")
	}
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			t.Fatalf("Authorization component %q is not key=value", part)
		}
		fields[k] = v
	}
	for _, k := range []string{"Credential", "SignedHeaders", "Signature"} {
		if fields[k] == "" {
			t.Fatalf("Authorization %q missing %s", header, k)
		}
	}
	scope := strings.Split(fields["Credential"], "/")
	if len(scope) != 5 {
		t.Fatalf("Credential %q does not have 5 slash-separated components", fields["Credential"])
	}
	return sigV4AuthParts{
		AccessKeyID:   scope[0],
		Date:          scope[1],
		Region:        scope[2],
		Service:       scope[3],
		Terminator:    scope[4],
		SignedHeaders: strings.Split(fields["SignedHeaders"], ";"),
		Signature:     fields["Signature"],
	}
}

// sigV4Input is the provider-neutral view of a request the verifier
// recomputes a signature for. Built from a client-side *http.Request
// (Tier 1: verify what the SDK signer produced before it hits the wire)
// or from what a server received (Tier 2: verify what the proxy sent).
type sigV4Input struct {
	method        string
	host          string
	escapedPath   string // wire-form (once-encoded) path
	rawQuery      string
	header        http.Header
	contentLength int64
	payloadHash   string
}

// clientSigV4Input builds the verifier input from a signed client-side
// request (the request object SignHTTP mutated in place).
func clientSigV4Input(r *http.Request, payloadHash string) sigV4Input {
	host := r.URL.Host
	if r.Host != "" {
		host = r.Host
	}
	return sigV4Input{
		method:        r.Method,
		host:          host,
		escapedPath:   r.URL.EscapedPath(),
		rawQuery:      r.URL.RawQuery,
		header:        r.Header,
		contentLength: r.ContentLength,
		payloadHash:   payloadHash,
	}
}

// recomputeSigV4 independently rebuilds the canonical request and
// string-to-sign for in, derives the signing key from secret, and
// returns all three artifacts so tests can assert on intermediates as
// well as the final signature.
func recomputeSigV4(t *testing.T, in sigV4Input, auth sigV4AuthParts, amzDate, secret string) (canonicalRequest, stringToSign, signature string) {
	t.Helper()

	// Canonical URI: double-encode the wire path.
	canonicalURI := awsPathEscape(in.escapedPath)
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	// Canonical query: parse and re-encode sorted, with %20 for space.
	canonicalQuery := ""
	if in.rawQuery != "" {
		q, err := url.ParseQuery(in.rawQuery)
		if err != nil {
			t.Fatalf("parse query %q: %v", in.rawQuery, err)
		}
		canonicalQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	}

	// Canonical headers, exactly the declared signed set.
	var ch strings.Builder
	for _, name := range auth.SignedHeaders {
		var value string
		switch name {
		case "host":
			value = collapseSpaces(in.host)
		case "content-length":
			value = strconv.FormatInt(in.contentLength, 10)
		default:
			values := in.header.Values(name)
			if len(values) == 0 {
				t.Fatalf("SignedHeaders declares %q but the request has no such header", name)
			}
			cleaned := make([]string, len(values))
			for i, v := range values {
				cleaned[i] = collapseSpaces(v)
			}
			value = strings.Join(cleaned, ",")
		}
		ch.WriteString(name)
		ch.WriteByte(':')
		ch.WriteString(value)
		ch.WriteByte('\n')
	}

	canonicalRequest = strings.Join([]string{
		in.method,
		canonicalURI,
		canonicalQuery,
		ch.String(),
		strings.Join(auth.SignedHeaders, ";"),
		in.payloadHash,
	}, "\n")

	crHash := sha256.Sum256([]byte(canonicalRequest))
	scope := strings.Join([]string{auth.Date, auth.Region, auth.Service, auth.Terminator}, "/")
	stringToSign = strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crHash[:]),
	}, "\n")

	key := []byte("AWS4" + secret)
	for _, chunk := range []string{auth.Date, auth.Region, auth.Service, auth.Terminator} {
		key = hmacSHA256(key, chunk)
	}
	signature = hex.EncodeToString(hmacSHA256(key, stringToSign))
	return canonicalRequest, stringToSign, signature
}

// verifyReceivedSigV4 is the mock upstream's validation path: parse the
// received Authorization header, recompute the signature over what
// actually arrived, and compare — the same check AWS performs. body is
// the received payload; secret is the credential the request should
// have been signed with.
func verifyReceivedSigV4(t *testing.T, rec sigV4Input, secret string) sigV4AuthParts {
	t.Helper()
	authz := rec.header.Get("Authorization")
	if authz == "" {
		t.Fatal("received request has no Authorization header")
	}
	auth := parseSigV4Authorization(t, authz)
	amzDate := rec.header.Get("X-Amz-Date")
	if amzDate == "" {
		t.Fatal("received request has no X-Amz-Date header")
	}
	if !strings.HasPrefix(amzDate, auth.Date) {
		t.Errorf("X-Amz-Date %q does not begin with the credential-scope date %q", amzDate, auth.Date)
	}
	cr, sts, want := recomputeSigV4(t, rec, auth, amzDate, secret)
	if auth.Signature != want {
		t.Errorf("signature mismatch:\n got %s\nwant %s\ncanonical request:\n%s\nstring to sign:\n%s",
			auth.Signature, want, cr, sts)
	}
	return auth
}

// awsPathEscape percent-encodes every byte outside the RFC 3986
// unreserved set, preserving '/', with uppercase hex — the encoding
// AWS applies to the (already once-encoded) path when building the
// canonical URI for non-S3 services.
func awsPathEscape(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// collapseSpaces trims a header value and collapses interior runs of
// whitespace to single spaces, per the canonical-headers rule.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
