package domain

import "time"

// RateLimitReading is the latest reading of one rate-limit bucket an upstream
// meters for one credential an org uses — the `rate_limit_readings` row.
//
// Every process records readings in memory as responses arrive
// (internal/upstream) and writes the ones that changed to this table, so a
// request served by any control pod sees what every pod last saw. The newest
// observation of a key wins; nothing is summed, because an upstream's headers
// state its own count of what is left for the credential.
//
// The pointer fields are nil when the reading did not state them, which is a
// different answer from zero: a bucket with nothing left is Remaining 0, a
// reading that said only whether its bucket is near its limit has no Remaining
// at all.
type RateLimitReading struct {
	OrgID string `json:"org_id"`
	// Upstream is the provider ("github", "jira", "linear", "slack").
	Upstream string `json:"upstream"`
	// Credential is the opaque id of the credential the bucket is metered
	// against, as the upstream's client names it ("installation:123", "pat",
	// "cloud_api_token", "bot:T01:A02"). Never a token, a login or a user id.
	Credential string `json:"credential"`
	// Bucket is one budget the upstream meters separately for the credential
	// ("core", "graphql", "requests", a Slack method). Buckets are never
	// combined: they are often in different units.
	Bucket string `json:"bucket"`
	// Remaining is what the reading said is left, nil when it did not say.
	Remaining *int `json:"remaining"`
	// Limit is the bucket's size, nil when the reading did not say.
	Limit *int `json:"limit"`
	// Used is how many calls TF counted itself making against the bucket in
	// its window, on a reading TF counted (Slack states no budget), nil on a
	// reading an upstream stated.
	Used *int `json:"used"`
	// NearLimit is what a reading that states no count said: whether the
	// bucket is close to its limit (Jira Cloud's hourly quota). Nil on every
	// other reading, whose nearness follows from Remaining and Limit.
	NearLimit *bool `json:"near_limit"`
	// ResetAt is when the bucket refills, nil when the reading did not say.
	ResetAt *time.Time `json:"reset_at"`
	// ObservedAt is when the response that carried the reading arrived.
	ObservedAt time.Time `json:"observed_at"`
}

// RateLimitReadingMaxAge bounds how long a reading that names no reset is
// reported. A reading with a reset stops describing its bucket at the reset;
// one without says nothing about when it stops being true, so it is reported
// for this long after it was observed and not after, which is also what clears
// the row of a process that stopped making requests.
const RateLimitReadingMaxAge = time.Hour

// Current reports whether r still describes its bucket at now: before its
// reset, or, with no reset, within RateLimitReadingMaxAge of its observation.
func (r RateLimitReading) Current(now time.Time) bool {
	if r.ResetAt != nil {
		return now.Before(*r.ResetAt)
	}
	return now.Before(r.ObservedAt.Add(RateLimitReadingMaxAge))
}
