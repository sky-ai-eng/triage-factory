// Package reachability provides a URL-only, no-auth host-reachability probe.
//
// It answers exactly one question — "can this deployment reach this host at
// all?" — which is deliberately distinct from credential/auth validation
// (internal/auth). A host that answers with ANY HTTP status (200, 401, 403,
// 404, 5xx, …) is reachable; only a transport-level failure (DNS, dial, TLS,
// timeout) is unreachable. The response body is never inspected.
//
// This is the URL sub-step of the setup wizard's two-stage connect: confirm a
// host is routable from here before asking for credentials. It earns its keep
// most on a private GitHub Enterprise Server (RFC-1918, VPN-only): the probe
// fails fast and tells the operator TF must run inside their network — the
// same precondition polling needs anyway. Note this means private/internal
// hosts are a legitimate *success*, so the probe intentionally does not filter
// reserved IP ranges.
package reachability

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// probeTimeout caps a single reachability probe. Short on purpose: a reachable
// host answers in well under a second, and an unreachable one should fail the
// wizard step quickly rather than hang the operator on a long default.
const probeTimeout = 10 * time.Second

// userAgent is sent on every probe. GitHub's API returns 403 for requests
// without a User-Agent — still "reachable", but a named UA yields a clean 200
// and is good external-citizen behavior.
const userAgent = "triage-factory-reachability"

// client is the shared probe client. No credential is ever attached, and
// redirects are NOT followed: a 3xx already proves the host answered, so the
// initial response is the verdict. Chasing the Location would only re-point
// the probe at a possibly-unrelated (internal) host — needless SSRF surface,
// and it could flip the verdict based on where the redirect leads rather than
// whether the host the operator entered is reachable.
var client = &http.Client{
	Timeout: probeTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Result is the verdict for one probe. Reachable is the load-bearing bit;
// Status carries the HTTP status the host answered with (0 when unreachable);
// Reason is a short, operator-readable cause, populated only when unreachable.
type Result struct {
	Reachable bool   `json:"reachable"`
	ProbedURL string `json:"probed_url"`
	Status    int    `json:"status,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Probe issues a single no-auth GET to probeURL and classifies the outcome.
// Any HTTP response ⇒ Reachable; a transport-level error ⇒ unreachable with a
// classified Reason. It never returns an error — the verdict is the Result —
// so an HTTP handler can always answer 200 with the body as the source of
// truth, separating "host unreachable" from "the server itself errored".
func Probe(ctx context.Context, probeURL string) Result {
	res := Result{ProbedURL: probeURL}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		res.Reason = "invalid URL"
		return res
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		res.Reason = classify(err)
		return res
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused; the body is irrelevant.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	res.Reachable = true
	res.Status = resp.StatusCode
	return res
}

// classify turns a transport-level error into a short reason. The buckets that
// matter for the wizard: name resolution (typo'd host), timeout (firewalled /
// wrong network — the GHES-behind-a-VPC signal), then the bare underlying
// cause for everything else (e.g. "connect: connection refused", "tls: …").
func classify(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connection timed out — host did not respond"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "could not resolve host"
	}
	// Unwrap the *url.Error envelope so the reason is the bare cause rather
	// than the noisy `Get "https://…": …` wrapper.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}
