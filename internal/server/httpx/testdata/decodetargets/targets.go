// Package decodetargets is the fixture TestDecodeTargets_EveryFaultIsReportable
// scans to show it finds each shape it refuses, through each decode door.
package decodetargets

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

type textRef struct{ s string }

func (r *textRef) UnmarshalText(b []byte) error {
	r.s = string(b)
	return nil
}

type item struct {
	When time.Time `json:"when"`
}

// hidden is embedded by pointer, which encoding/json cannot allocate since
// the field is unexported.
type hidden struct {
	Note string `json:"note"`
}

// stamped decodes through the UnmarshalJSON it promotes from time.Time.
type stamped struct {
	time.Time
}

type body struct {
	httpx.PageRequest
	*hidden

	At     time.Time       `json:"at"`
	Ref    textRef         `json:"ref"`
	Count  int             `json:"count,string"`
	Items  []item          `json:"items"`
	Again  []item          `json:"again"`
	ByRef  map[textRef]int `json:"by_ref"`
	Raw    json.RawMessage `json:"raw"`
	Blob   []byte          `json:"blob"`
	N      json.Number     `json:"n"`
	Fine   string          `json:"fine"`
	Hidden time.Time       `json:"-"`
	quiet  time.Time
}

// decodeList decodes a body whose element type is a type parameter, which
// the scan cannot see through.
func decodeList[T any](w http.ResponseWriter, r *http.Request) {
	var v struct {
		Items []T `json:"items"`
	}
	httpx.DecodeJSONStrict(w, r, &v)
}

func handle(w http.ResponseWriter, r *http.Request) {
	decodeList[string](w, r)
	var b body
	if !httpx.DecodeJSONStrict(w, r, &b) {
		return
	}
	var s stamped
	if !httpx.DecodeJSONStrictLimit(w, r, &s, 1<<10) {
		return
	}
	var at time.Time
	httpx.DecodeJSONStrictBytes(w, nil, &at)
	var raw json.RawMessage
	httpx.DecodeJSONStrictBytes(w, nil, &raw)
	_ = b.quiet
}
