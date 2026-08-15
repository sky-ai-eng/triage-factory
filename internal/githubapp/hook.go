package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The App's own webhook, read through the same app-level JWT the rest of this
// package authenticates with. GitHub exposes three things about it:
//
//	GET  /app/hook/config                            — where deliveries are sent
//	GET  /app/hook/deliveries                        — what happened to them
//	POST /app/hook/deliveries/{delivery_id}/attempts — send one again
//
// Together those answer, without waiting for a single delivery to arrive,
// whether this deployment is actually receiving the App's webhooks — and,
// when it isn't, replay what was missed. hookhealth.go turns the first two
// into a state; this file is only the transport.
//
// # What is established about these endpoints, and what is not
//
// From GitHub's published OpenAPI descriptions (github/rest-api-description):
//
//   - GET /app/hook/config documents exactly one response, 200, and its
//     webhook-config schema marks every field optional — so an App with no
//     webhook is expected to answer 200 with an absent/empty url rather than
//     404. That is the documented contract, not an observation against a live
//     App: a 404 is handled anyway, see ErrHookAPIUnavailable.
//   - The deliveries endpoints take per_page, a cursor, and a status filter
//     ("success" = 2xx-3xx, "failure" = 4xx-5xx); the redelivery POST answers
//     202. A delivery's guid is shared across its redeliveries (the list
//     example shows two entries, one guid, redelivery true on the second),
//     which is what makes the receiver's delivery dedup absorb a replay of an
//     applied delivery while still applying one that was rejected.
//   - GitHub Enterprise Server: /app/hook/config is present from the oldest
//     published description (3.0); the deliveries + attempts endpoints appear
//     from 3.2. TF accepts an arbitrary GHES base, so a 3.0/3.1 host is
//     reachable and answers 404 for the deliveries half — which is a capability
//     gap, not a fault, and is reported as one.
//   - The deliveries `status` filter is a github.com parameter that the GHES
//     descriptions do not carry, so an older host is expected to ignore it and
//     answer with successes mixed in. Callers that need only failures re-test
//     the status code themselves rather than trusting the filter.
//
// insecure_ssl is deliberately not parsed: it is string-or-number on the wire
// (the schema is a oneOf), nothing here turns on it, and a field nobody reads
// is a decode failure waiting to happen.

// ErrHookAPIUnavailable reports a 404 from one of the App-webhook endpoints.
// It is not "the request failed": the endpoint set is absent on older GHES,
// and a 404 from /app/hook/config may also be how a GitHub answers for an App
// with no webhook at all. The two are told apart by asking a second endpoint —
// see ProbeWebhookHealth — so this stays a sentinel rather than an error
// string, and never reads as an outage.
var ErrHookAPIUnavailable = errors.New("githubapp: app webhook endpoint not available")

// HookConfig is the App's webhook configuration (GET /app/hook/config).
//
// SecretSet is presence, never the value: GitHub returns the secret masked
// ("********"), so its value proves nothing and is worth nothing — it is read
// as a bool here and the string is dropped on the floor. Whether a stored
// secret actually MATCHES is answered by delivery status codes, not by this.
type HookConfig struct {
	// URL is where GitHub sends this App's deliveries. Empty when the App has
	// no webhook configured.
	URL string
	// ContentType is GitHub's "json" / "form".
	ContentType string
	// SecretSet reports that the App has *a* webhook secret configured on
	// GitHub. It says nothing about whether TF holds the same one.
	SecretSet bool
}

// HookDelivery is one entry from GET /app/hook/deliveries.
//
// GUID is the X-GitHub-Delivery header value the receiver dedups on, and is
// shared by a delivery and every redelivery of it. StatusCode is what the
// receiving endpoint answered (0 when GitHub could not connect at all, with
// Status carrying the prose). Action and InstallationID are nullable on the
// wire and read back as "" / 0.
type HookDelivery struct {
	ID             int64
	GUID           string
	DeliveredAt    time.Time
	Redelivery     bool
	Status         string
	StatusCode     int
	Event          string
	Action         string
	InstallationID int64
}

// HookDeliveryQuery narrows a deliveries listing. The zero value asks for one
// page of the endpoint's default size, unfiltered.
type HookDeliveryQuery struct {
	// Status filters GitHub-side by outcome: "success" (status_code 200-399)
	// or "failure" (400-599). Empty returns both.
	Status string
	// PerPage is GitHub's per_page (max 100). Zero leaves it to GitHub.
	PerPage int
	// MaxPages bounds cursor pagination. Zero or one reads a single page —
	// the right answer for a health probe, which only needs the newest
	// deliveries. The replay path asks for more.
	MaxPages int
}

// hookConfigResponse is the subset of GET /app/hook/config we parse. The
// secret is read into a string only so its emptiness can be tested; it is
// never stored, returned, or logged.
type hookConfigResponse struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Secret      string `json:"secret"`
}

// HookConfig fetches the App's webhook configuration. A 404 comes back as
// ErrHookAPIUnavailable (see the sentinel's doc for why that is ambiguous and
// how the ambiguity is resolved); any other non-200 is a plain error.
func (m *Minter) HookConfig(ctx context.Context) (HookConfig, error) {
	body, _, err := m.appGet(ctx, m.apiBase+"/app/hook/config")
	if err != nil {
		return HookConfig{}, err
	}
	var parsed hookConfigResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return HookConfig{}, fmt.Errorf("githubapp: parse hook config response: %w", err)
	}
	return HookConfig{
		URL:         parsed.URL,
		ContentType: parsed.ContentType,
		SecretSet:   parsed.Secret != "",
	}, nil
}

// hookDeliveryItem is the subset of a deliveries array entry we keep.
type hookDeliveryItem struct {
	ID             int64     `json:"id"`
	GUID           string    `json:"guid"`
	DeliveredAt    time.Time `json:"delivered_at"`
	Redelivery     bool      `json:"redelivery"`
	Status         string    `json:"status"`
	StatusCode     int       `json:"status_code"`
	Event          string    `json:"event"`
	Action         string    `json:"action"`
	InstallationID int64     `json:"installation_id"`
}

// ListHookDeliveries returns the App's recent webhook deliveries, newest
// first as GitHub orders them, following the cursor Link header up to
// q.MaxPages. A 404 is ErrHookAPIUnavailable (GHES < 3.2 has no such
// endpoint).
//
// GitHub retains deliveries for 30 days, so an empty result means "nothing in
// the last 30 days", never "this App has never delivered".
func (m *Minter) ListHookDeliveries(ctx context.Context, q HookDeliveryQuery) ([]HookDelivery, error) {
	params := url.Values{}
	if q.PerPage > 0 {
		params.Set("per_page", strconv.Itoa(q.PerPage))
	}
	if q.Status != "" {
		params.Set("status", q.Status)
	}
	next := m.apiBase + "/app/hook/deliveries"
	if len(params) > 0 {
		next += "?" + params.Encode()
	}

	pages := q.MaxPages
	if pages < 1 {
		pages = 1
	}
	var out []HookDelivery
	for i := 0; i < pages && next != ""; i++ {
		body, link, err := m.appGet(ctx, next)
		if err != nil {
			return nil, err
		}
		var page []hookDeliveryItem
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("githubapp: parse hook deliveries response: %w", err)
		}
		for _, it := range page {
			out = append(out, HookDelivery{
				ID:             it.ID,
				GUID:           it.GUID,
				DeliveredAt:    it.DeliveredAt.UTC(),
				Redelivery:     it.Redelivery,
				Status:         it.Status,
				StatusCode:     it.StatusCode,
				Event:          it.Event,
				Action:         it.Action,
				InstallationID: it.InstallationID,
			})
		}
		next = nextPageURL(link)
	}
	return out, nil
}

// RedeliverHookDelivery asks GitHub to send delivery deliveryID again
// (POST /app/hook/deliveries/{delivery_id}/attempts). GitHub answers 202 and
// performs the delivery asynchronously, so a nil return means "accepted for
// redelivery", not "received".
//
// Only deliveries inside GitHub's 30-day retention window can be replayed; an
// id outside it is refused by GitHub and surfaces here as an error with the
// status.
func (m *Minter) RedeliverHookDelivery(ctx context.Context, deliveryID int64) error {
	if deliveryID <= 0 {
		return fmt.Errorf("githubapp: deliveryID must be positive, got %d", deliveryID)
	}
	appJWT, err := m.AppJWT()
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/app/hook/deliveries/%d/attempts", m.apiBase, deliveryID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("githubapp: build request: %w", err)
	}
	setAppJWTHeaders(req, appJWT)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("githubapp: redeliver hook delivery: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("githubapp: redeliver hook delivery %d: status %d, body: %s",
			deliveryID, resp.StatusCode, truncate(string(body), 512))
	}
	return nil
}

// appGet performs an app-JWT-authenticated GET and returns the body plus the
// Link header. A 404 is folded into ErrHookAPIUnavailable so callers can tell
// "this GitHub does not serve that endpoint" apart from a real failure.
func (m *Minter) appGet(ctx context.Context, endpoint string) (body []byte, link string, err error) {
	appJWT, err := m.AppJWT()
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("githubapp: build request: %w", err)
	}
	setAppJWTHeaders(req, appJWT)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("githubapp: get %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrHookAPIUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("githubapp: get %s: status %d, body: %s",
			endpoint, resp.StatusCode, truncate(string(body), 512))
	}
	return body, resp.Header.Get("Link"), nil
}

// setAppJWTHeaders applies the headers every /app/* request needs. User-Agent
// is required by github.com — some endpoints 403 without one.
func setAppJWTHeaders(req *http.Request, appJWT string) {
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "triage-factory-githubapp")
}
