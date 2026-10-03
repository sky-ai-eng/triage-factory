package inference

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// rendered is a provider failure that carries a status, through the real
// renderer.
func rendered(status int, msg string) error {
	return bifrostError(&schemas.BifrostError{
		StatusCode: &status,
		Error:      &schemas.ErrorField{Message: msg},
	})
}

// midStream is a failure with no status, through the real renderer: the shape
// of an error chunk the provider sent mid-stream, or of a stream bifrost could
// not read.
func midStream(msg string) error {
	return bifrostError(&schemas.BifrostError{Error: &schemas.ErrorField{Message: msg}})
}

// timeoutErr is a net.Error that timed out, with a message that matches no
// marker, so only the net.Error arm can classify it.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "deadline passed" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

type classifyCase struct {
	name string
	err  error
	want upstream.Class
}

// classifyCases is the corpus both tests below read: every status class,
// every marker, the overflow, a net.Error, and wrapped errors.
func classifyCases() []classifyCase {
	cases := []classifyCase{}
	for _, s := range []struct {
		status int
		want   upstream.Class
	}{
		{400, upstream.Rejected},
		{401, upstream.Auth},
		{403, upstream.Auth},
		{404, upstream.Rejected},
		{408, upstream.Transient},
		{409, upstream.Transient},
		{413, upstream.Rejected},
		{422, upstream.Rejected},
		{429, upstream.RateLimited},
		{500, upstream.Transient},
		{502, upstream.Transient},
		{503, upstream.Transient},
		{504, upstream.Transient},
		{529, upstream.Transient},
	} {
		cases = append(cases, classifyCase{fmt.Sprintf("rendered %d", s.status), rendered(s.status, "provider message"), s.want})
	}
	// Upper-cased, to pin that matching ignores case.
	for _, m := range markerUnion() {
		cases = append(cases, classifyCase{"marker " + m, midStream("upstream: " + strings.ToUpper(m)), upstream.Transient})
	}
	return append(cases, []classifyCase{
		{"rendered 400 quoting a transport phrase", rendered(400, "invalid_request_error: your prompt mentioned a connection reset"), upstream.Rejected},
		{"rendered 400 quoting a number that spells a status", rendered(400, "invalid_request_error: max_tokens: 500 is below the minimum"), upstream.Rejected},
		{"rendered 401 quoting an overload", rendered(401, "authentication_error: invalid x-api-key while overloaded"), upstream.Auth},
		{"rendered 200 beside an error", rendered(200, "connection reset"), upstream.Rejected},

		{"overflow quoting a count that spells 429", rendered(400, "prompt is too long: 429 tokens > 200 maximum"), upstream.Rejected},
		{"overflow with no status", midStream("prompt is too long: 250000 tokens > 200000 maximum"), upstream.Rejected},
		{"wrapped overflow", fmt.Errorf("compact after context overflow: %w", rendered(400, "prompt is too long: 9 tokens > 8 maximum")), upstream.Rejected},

		{"mid-stream overloaded_error", bifrostError(&schemas.BifrostError{Error: &schemas.ErrorField{Message: "Overloaded", Type: new("overloaded_error")}}), upstream.Transient},
		{"mid-stream api_error", bifrostError(&schemas.BifrostError{Error: &schemas.ErrorField{Message: "Internal server error", Type: new("api_error")}}), upstream.Transient},
		{"stream read unexpected EOF", bifrostError(&schemas.BifrostError{Error: &schemas.ErrorField{Message: "Error reading stream: unexpected EOF", Error: io.ErrUnexpectedEOF}}), upstream.Transient},
		{"bare EOF", errors.New("EOF"), upstream.Transient},
		{"eof inside a word", midStream("invalid geofence parameter"), upstream.Rejected},
		{"eof inside a field name", midStream("bad request field 'eof_marker'"), upstream.Rejected},

		{"net.Error timeout", fmt.Errorf("stream: %w", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}), upstream.Transient},
		{"net.Error that did not time out", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("something odd")}, upstream.Rejected},

		{"wrapped rendered 503 with its endpoint", fmt.Errorf("direct llm call failed: %w", fmt.Errorf("%w [endpoint: https://api.anthropic.com]", rendered(503, "Service Unavailable"))), upstream.Transient},
		{"dial failure rendered as a 502", bifrostError(&schemas.BifrostError{
			StatusCode: new(502),
			Error: &schemas.ErrorField{
				Message: schemas.ErrProviderDoRequest,
				Error:   errors.New("dial tcp 10.42.7.1:41231: connect: connection refused"),
			},
		}), upstream.Transient},

		{"unrecognized", errors.New("inference: request has no model"), upstream.Rejected},
		{"bare transport sentence", midStream(schemas.ErrProviderDoRequest), upstream.Rejected},
		{"request id that spells a status", midStream("invalid api key (request id req_a5003b)"), upstream.Rejected},
		{"bare 500 token", midStream("HTTP 500 from upstream"), upstream.Rejected},
		{"bare 429 token", midStream("status code: 429"), upstream.Rejected},
		{"parenthesized 502 token", midStream("provider returned (502)"), upstream.Rejected},
	}...)
}

func TestClassify(t *testing.T) {
	for _, tc := range classifyCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Classify(context.Background(), tc.err)
			if !ok || got != tc.want {
				t.Errorf("Classify(%q) = (%q, %v), want (%q, true)", tc.err, got, ok, tc.want)
			}
		})
	}
}

func TestClassify_NilIsOK(t *testing.T) {
	if got, ok := Classify(context.Background(), nil); !ok || got != upstream.OK {
		t.Fatalf("Classify(nil) = (%q, %v), want (ok, true)", got, ok)
	}
}

// TestClassify_DoneCtxIsNoOutcome: a failure the caller's own cancellation or
// deadline produced says nothing about the provider, however it reads.
func TestClassify_DoneCtxIsNoOutcome(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		rendered(529, "Overloaded"),
		rendered(401, "invalid x-api-key"),
		midStream("context canceled"),
	} {
		if got, ok := Classify(cancelled, err); ok || got != "" {
			t.Errorf("Classify(cancelled, %q) = (%q, %v), want (\"\", false)", err, got, ok)
		}
	}
}

// TestTransientMarkers_IsTheUnion pins the list to exactly the union below,
// so a marker cannot be added or dropped without this test saying so.
func TestTransientMarkers_IsTheUnion(t *testing.T) {
	got := slices.Clone(transientMarkers)
	slices.Sort(got)
	if want := markerUnion(); !slices.Equal(got, want) {
		t.Fatalf("transientMarkers = %q, want the union %q", got, want)
	}
}

func TestClassifyStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   upstream.Class
	}{
		{200, upstream.OK},
		{400, upstream.Rejected},
		{401, upstream.Auth},
		{403, upstream.Auth},
		{404, upstream.Rejected},
		{408, upstream.Transient},
		{409, upstream.Transient},
		{429, upstream.RateLimited},
		{500, upstream.Transient},
		{529, upstream.Transient},
	} {
		if got := ClassifyStatus(tc.status); got != tc.want {
			t.Errorf("ClassifyStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestUpstreamName(t *testing.T) {
	if got, ok := UpstreamName(ProviderAnthropic); !ok || got != upstream.Anthropic {
		t.Errorf("UpstreamName(anthropic) = (%q, %v)", got, ok)
	}
	if got, ok := UpstreamName(ProviderBedrock); !ok || got != upstream.Bedrock {
		t.Errorf("UpstreamName(bedrock) = (%q, %v)", got, ok)
	}
	if got, ok := UpstreamName(schemas.OpenAI); ok {
		t.Errorf("UpstreamName(openai) = (%q, true), want no label for a provider TF does not serve", got)
	}
}

// TestClassify_CoversEveryCallersTransientReading pins Classify against the
// union of the three per-caller classifications it replaced (frozen below):
// a failure any of them read as transient is Transient or RateLimited here,
// except for the cases named in readsLess.
//
// readsMore names the other direction: failures Classify reads as unavailable
// that one of those callers did not, so the native loop and the breaker now
// agree on them.
func TestClassify_CoversEveryCallersTransientReading(t *testing.T) {
	readsLess := map[string]string{
		"rendered 400 quoting a transport phrase":            "a rendered status decides by itself; the loop matched the phrase",
		"rendered 400 quoting a number that spells a status": "a rendered status decides by itself; the loop matched the number",
		"rendered 401 quoting an overload":                   "a rendered status decides by itself; the loop matched the word",
		"rendered 200 beside an error":                       "a failed call is never OK, and Rejected retries less; the loop matched the phrase",
		"bare 500 token":                                     "a status counts only as rendered (HTTP n); the loop matched bare numbers",
		"bare 429 token":                                     "a status counts only as rendered (HTTP n); the loop matched bare numbers",
		"parenthesized 502 token":                            "a status counts only as rendered (HTTP n); the loop matched bare numbers",
	}
	type caller int
	const (
		nativeLoop caller = iota
		breaker
	)
	readsMore := map[string]caller{
		"rendered 408": nativeLoop,
		"rendered 409": nativeLoop,
		"rendered 529": nativeLoop, // by status: "provider message" lacks the word overloaded

		"marker no such host":              nativeLoop,
		"marker network is unreachable":    nativeLoop,
		"marker no route to host":          nativeLoop,
		"marker broken pipe":               nativeLoop,
		"marker context deadline exceeded": nativeLoop,
		"marker rate limit":                breaker,
		"marker rate_limit":                breaker,
		"marker overloaded":                breaker,
		"marker internal server error":     breaker,
		"marker bad gateway":               breaker,
		"marker service unavailable":       breaker,
		"marker gateway timeout":           breaker,
		"marker timeout":                   breaker,
		"marker timed out":                 breaker,
		"mid-stream overloaded_error":      breaker,
		"mid-stream api_error":             breaker,
		"stream read unexpected EOF":       breaker,
		"bare EOF":                         breaker,
	}

	bg := context.Background()
	for _, tc := range classifyCases() {
		loop, brk, probe := nativeLoopRetried(tc.err), breakerOpened(bg, tc.err), probeInconclusiveStatus(tc.err)
		wasTransient := loop || brk || probe
		got, _ := Classify(bg, tc.err)
		unavailable := got == upstream.Transient || got == upstream.RateLimited

		if reason, ok := readsLess[tc.name]; ok {
			if !wasTransient || unavailable {
				t.Errorf("%s: named as read less (%s), but no longer differs", tc.name, reason)
			}
			delete(readsLess, tc.name)
		} else if wasTransient && !unavailable {
			t.Errorf("%s: %q was transient to a caller (loop %v, breaker %v, probe %v), Classify reads %q", tc.name, tc.err, loop, brk, probe, got)
		}

		if who, ok := readsMore[tc.name]; ok {
			was := loop
			if who == breaker {
				was = brk
			}
			if was || !unavailable {
				t.Errorf("%s: named as newly unavailable, but the caller read %v and Classify reads %q", tc.name, was, got)
			}
			delete(readsMore, tc.name)
		}
	}
	for name := range readsLess {
		t.Errorf("readsLess names %q, which is not in the corpus", name)
	}
	for name := range readsMore {
		t.Errorf("readsMore names %q, which is not in the corpus", name)
	}
}

// markerUnion is the sorted, deduplicated union of the two per-caller marker
// lists below.
func markerUnion() []string {
	out := slices.Concat(loopMarkers, breakerMarkers)
	slices.Sort(out)
	return slices.Compact(out)
}

// The three classifications Classify replaced, frozen as the oracle for
// TestClassify_CoversEveryCallersTransientReading.

var loopMarkers = []string{
	"rate limit", "rate_limit", "overloaded", "internal server error",
	"bad gateway", "service unavailable", "gateway timeout", "timeout",
	"timed out", "connection reset", "connection refused",
}

var breakerMarkers = []string{
	"connection refused", "connection reset", "no such host", "i/o timeout",
	"tls handshake timeout", "timeout awaiting response",
	"context deadline exceeded", "network is unreachable", "no route to host",
	"broken pipe",
}

var loopEOF = regexp.MustCompile(`\beof\b`)

// nativeLoopRetried is the native loop's retry rule.
func nativeLoopRetried(err error) bool {
	if err == nil || errors.Is(err, ErrContextOverflow) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if loopEOF.MatchString(msg) {
		return true
	}
	for _, m := range loopMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	for _, code := range []string{"429", "500", "502", "503", "504"} {
		for i := 0; ; {
			j := strings.Index(msg[i:], code)
			if j < 0 {
				break
			}
			j += i
			end := j + len(code)
			if (j == 0 || !isAlnum(msg[j-1])) && (end >= len(msg) || !isAlnum(msg[end])) {
				return true
			}
			i = j + 1
		}
	}
	return false
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// breakerOpened is the system-job breaker's rule.
func breakerOpened(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrContextOverflow) {
		return false
	}
	if status, ok := RenderedStatus(err); ok {
		return status == 408 || status == 409 || status == 429 || status >= 500
	}
	msg := strings.ToLower(err.Error())
	for _, m := range breakerMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// probeInconclusiveStatus is the availability probe's rule for a status that
// says nothing about entitlement.
func probeInconclusiveStatus(err error) bool {
	status, ok := RenderedStatus(err)
	return ok && (status == 408 || status == 409 || status == 429 || status >= 500)
}
