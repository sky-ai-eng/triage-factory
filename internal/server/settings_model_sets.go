package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// fieldFaults carries field-level refusals out of a write transaction to the
// handler, which is the only place allowed to touch the ResponseWriter. The
// enable-set checks need rows the body-only resolve pass has not read yet — the
// org's set, the team's post-apply state — so they cannot report where every
// other validator does, and an error is the only thing a transaction body can
// hand back.
//
// It accumulates rather than short-circuits, for the same reason
// httpx.Validation does: a body with two bad fields reports two.
type fieldFaults struct{ items []httpx.ErrorItem }

// invalid records a present-but-unusable field.
func (f *fieldFaults) invalid(field, msg string) {
	f.items = append(f.items, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: msg, Field: field})
}

// orNil returns the faults as an error, or nil when nothing failed — so a
// caller writes `return faults.orNil()` and never branches on emptiness.
func (f *fieldFaults) orNil() error {
	if len(f.items) == 0 {
		return nil
	}
	return f
}

// Error joins the messages. It is what a log line or an errors.Is chain sees;
// the wire shape is the item list, written by writeFieldFaults.
func (f *fieldFaults) Error() string {
	msgs := make([]string, 0, len(f.items))
	for _, it := range f.items {
		msgs = append(msgs, it.Message)
	}
	return strings.Join(msgs, "; ")
}

// writeFieldFaults renders accumulated field faults as one 400, returning false
// for any other error so the caller falls through to its own handling.
func writeFieldFaults(w http.ResponseWriter, err error) bool {
	var faults *fieldFaults
	if !errors.As(err, &faults) {
		return false
	}
	httpx.WriteErrors(w, http.StatusBadRequest, faults.items...)
	return true
}

// normalizeModelSet validates one enable-set as it arrives on the wire: a
// non-empty list of distinct, non-blank catalog keys, trimmed.
//
// Empty is refused rather than read as "no preference". Clearing a set has its
// own spelling — an explicit null — and an org that means "nobody may pick
// anything" is asking for a deployment where nothing runs, which is a state to
// reject rather than to store.
//
// Every unknown key is named, not the first: an admin fixing a hand-written
// list should see the whole list of what is wrong with it.
func normalizeModelSet(v *httpx.Validation, raw []string, field string) ([]string, bool) {
	if len(raw) == 0 {
		v.Invalid(field, field+" must name at least one model, or be null to clear it")
		return nil, false
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	var unknown, duplicate []string
	blank := false
	for _, entry := range raw {
		key := strings.TrimSpace(entry)
		switch {
		case key == "":
			blank = true
		case !modelcatalog.Offers(key):
			unknown = append(unknown, key)
		case seen[key]:
			duplicate = append(duplicate, key)
		default:
			seen[key] = true
			out = append(out, key)
		}
	}
	if blank {
		v.Invalid(field, field+" must not contain an empty model key")
	}
	if len(unknown) > 0 {
		v.Invalid(field, fmt.Sprintf(
			"%s names %s this deployment does not offer: %s — offered: %s",
			field, plural(len(unknown), "a model", "models"),
			strings.Join(unknown, ", "), strings.Join(modelcatalog.Keys(), ", ")))
	}
	if len(duplicate) > 0 {
		// A duplicate changes nothing, but accepting it would store a list that
		// does not read back as it was sent.
		v.Invalid(field, fmt.Sprintf("%s names %s twice", field, strings.Join(duplicate, ", ")))
	}
	if blank || len(unknown) > 0 || len(duplicate) > 0 {
		return nil, false
	}
	return out, true
}
