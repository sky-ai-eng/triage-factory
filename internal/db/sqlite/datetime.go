package sqlite

import (
	"fmt"
	"strings"
	"time"
)

// subselectDatetime scans a DATETIME that lost its declared column type on the
// way out. The driver converts a plain DATETIME column because it can read the
// column's type; inside a scalar subselect there is no such metadata, so the
// stored text arrives verbatim and has to be parsed.
//
// A scanner rather than a parse at the call site: the call sites are the task
// read's scan helpers, which are byte-identical to their Postgres twins and
// guarded that way by the identical-helpers ratchet. Threading an error return
// through them to serve one column would drift four shared functions for a
// quirk that belongs to one value.
//
// Valid is the "there was a row" answer, exactly as sql.NullTime's is on the
// Postgres side.
type subselectDatetime struct {
	Valid bool
	Time  time.Time
}

func (d *subselectDatetime) Scan(src any) error {
	d.Valid, d.Time = false, time.Time{}
	switch v := src.(type) {
	case nil:
		return nil
	case time.Time:
		// A driver that did convert after all — take it rather than insisting
		// on the text path.
		d.Valid, d.Time = true, v.UTC()
		return nil
	case string:
		return d.parse(v)
	case []byte:
		return d.parse(string(v))
	default:
		return fmt.Errorf("scan %T into a datetime", src)
	}
}

func (d *subselectDatetime) parse(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := parseDBDatetime(raw)
	if err != nil {
		return fmt.Errorf("parse datetime %q: %w", raw, err)
	}
	d.Valid, d.Time = true, parsed.UTC()
	return nil
}

// parseDBDatetime handles the ISO-8601 and SQLite-default datetime
// formats we see in the events table. Direct time.Time scans work for
// plain DATETIME columns because the SQLite driver reads the column
// type and parses; once COALESCE enters the picture that type metadata
// is gone and we get back a raw string.
//
// Accepted shapes, in order:
//   - RFC3339 ("2006-01-02T15:04:05Z07:00")
//   - modernc with _time_format=sqlite ("2006-01-02 15:04:05.999999999-07:00")
//   - SQLite default ("2006-01-02 15:04:05" — CURRENT_TIMESTAMP, naive UTC)
//   - RFC3339 with fractional seconds and 'T' separator
//   - Legacy Go time.String() ("2006-01-02 15:04:05.999999999 -0700 MST"),
//     produced by older modernc-driver writes that bound time.Time as its
//     default String() form. The monotonic-clock suffix " m=+..." is
//     stripped before parsing because time.Parse can't consume it.
func parseDBDatetime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", s); err == nil {
		return t, nil
	}
	cleaned := s
	if i := strings.Index(cleaned, " m=+"); i >= 0 {
		cleaned = cleaned[:i]
	} else if i := strings.Index(cleaned, " m=-"); i >= 0 {
		cleaned = cleaned[:i]
	}
	return time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", cleaned)
}
