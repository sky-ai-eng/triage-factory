package inference

import (
	"errors"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestBifrostError_CarriesTheCause pins the property the flattened message
// alone cannot supply: every transport failure in every bifrost provider
// reports the same fixed sentence, and the dial/TLS/reset error underneath it
// is the only thing that says which failure happened.
func TestBifrostError_CarriesTheCause(t *testing.T) {
	status := 502
	kind := schemas.ProviderConnectionFailed
	err := bifrostError(&schemas.BifrostError{
		StatusCode: &status,
		Error: &schemas.ErrorField{
			Message: schemas.ErrProviderDoRequest,
			Type:    &kind,
			Error:   errors.New("dial tcp 10.42.7.1:41231: connect: connection refused"),
		},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		schemas.ErrProviderDoRequest,
		"connection refused",
		"HTTP 502",
		kind,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

// TestBifrostError_DoesNotRepeatTheCause guards the common shape where the
// provider stores the same text in both fields.
func TestBifrostError_DoesNotRepeatTheCause(t *testing.T) {
	err := bifrostError(&schemas.BifrostError{
		Error: &schemas.ErrorField{Message: "boom", Error: errors.New("boom")},
	})
	if got := strings.Count(err.Error(), "boom"); got != 1 {
		t.Fatalf("cause repeated %d times: %q", got, err)
	}
}

// TestBifrostError_NoDetail falls back to the status-code vocabulary rather
// than rendering an empty reason.
func TestBifrostError_NoDetail(t *testing.T) {
	status := 401
	err := bifrostError(&schemas.BifrostError{StatusCode: &status})
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected the status-code reason, got %q", err)
	}
	if bifrostError(nil) != nil {
		t.Fatal("a nil bifrost error must convert to a nil error")
	}
}

// TestAnnotateEndpoint names the hop. Which host answered is the first thing
// worth knowing about a transport failure: a run pointed at its own sidecar
// proxy and one that reached the public provider fail identically otherwise.
func TestAnnotateEndpoint(t *testing.T) {
	c := &Client{endpoints: map[schemas.ModelProvider]string{
		ProviderAnthropic: "http://10.42.7.1:41231",
		ProviderBedrock:   "",
	}}

	got := c.annotateEndpoint(ProviderAnthropic, errors.New("boom")).Error()
	if !strings.Contains(got, "10.42.7.1:41231") {
		t.Errorf("configured base URL must appear: %q", got)
	}

	// An unset base URL is not "no information" — it says the call went to the
	// provider's built-in endpoint rather than through a proxy.
	got = c.annotateEndpoint(ProviderBedrock, errors.New("boom")).Error()
	if !strings.Contains(got, "built-in") {
		t.Errorf("an unset base URL must still name the hop: %q", got)
	}

	if c.annotateEndpoint(ProviderAnthropic, nil) != nil {
		t.Error("annotating a nil error must stay nil")
	}
}

// TestAnnotateEndpoint_Redacts keeps operator-supplied gateway URLs from
// carrying credential material into an error string that lands in logs and in
// the run transcript.
func TestAnnotateEndpoint_Redacts(t *testing.T) {
	c := &Client{endpoints: map[schemas.ModelProvider]string{
		ProviderAnthropic: "https://user:hunter2@gw.example.com/v1?key=sk-secret",
	}}
	got := c.annotateEndpoint(ProviderAnthropic, errors.New("boom")).Error()
	for _, leak := range []string{"hunter2", "sk-secret"} {
		if strings.Contains(got, leak) {
			t.Errorf("error leaks %q: %q", leak, got)
		}
	}
	if !strings.Contains(got, "gw.example.com") {
		t.Errorf("the host must survive redaction: %q", got)
	}
}

// TestEndpointsOf reads the base URLs off a real account, which is what makes
// the annotation above match what bifrost actually dialed.
func TestEndpointsOf(t *testing.T) {
	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "placeholder",
		BaseURL:  "http://10.42.7.1:41231",
		Models:   []string{"claude-sonnet-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := endpointsOf(acct)[ProviderAnthropic]; got != "http://10.42.7.1:41231" {
		t.Fatalf("endpoint = %q", got)
	}
}
