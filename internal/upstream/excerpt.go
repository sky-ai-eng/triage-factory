package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// maxExcerpt is the most characters of an upstream's own error message an
// excerpt carries.
const maxExcerpt = 200

// Excerpt is what an error message shows of a response body. Error messages
// are logged every poll cycle, so a body never reaches one whole: a JSON
// error body contributes the upstream's own message, truncated, and anything
// else (a proxy's HTML page, an empty body) only its size.
//
// The message is the first non-empty of `message` (GitHub), `errorMessages[0]`
// (Jira), `error_description` (OAuth), `error` (Slack, OAuth) and `msg`
// (GoTrue), followed by the first entry of `errors` when there is one: Jira's
// field errors, or GitHub's validation errors, whose top-level message alone
// is only "Validation Failed". Whitespace is collapsed and control characters
// are dropped, so a message cannot carry terminal escapes into a log line.
func Excerpt(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return "empty body"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return fmt.Sprintf("non-JSON body, %d bytes", len(body))
	}
	msg := firstNonEmpty(
		jsonString(obj["message"]),
		firstArrayString(obj["errorMessages"]),
		jsonString(obj["error_description"]),
		jsonString(obj["error"]),
		jsonString(obj["msg"]),
	)
	detail := clean(firstErrorsValue(obj["errors"]))
	switch {
	case msg == "":
		msg = detail
	case detail != "" && !strings.Contains(msg, detail):
		msg += ": " + detail
	}
	if msg == "" {
		return fmt.Sprintf("JSON body with no error message, %d bytes", len(body))
	}
	return truncate(msg, maxExcerpt)
}

// firstNonEmpty is the first of msgs that is non-empty once cleaned.
func firstNonEmpty(msgs ...string) string {
	for _, m := range msgs {
		if m = clean(m); m != "" {
			return m
		}
	}
	return ""
}

// clean drops control characters other than whitespace, then collapses runs
// of whitespace to one space.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// jsonString decodes raw as a JSON string, or returns "" for anything else.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// firstArrayString is the first non-empty string in a JSON array of strings.
func firstArrayString(raw json.RawMessage) string {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return ""
	}
	for _, item := range items {
		if s := jsonString(item); s != "" {
			return s
		}
	}
	return ""
}

// firstErrorsValue reads an `errors` member. Jira sends an object of field
// name to message, whose first value in document order is the excerpt; an
// array (GitHub's validation errors) contributes its first string, or its
// first object's `message`, or that object's `code` and `field` when it has
// no message.
func firstErrorsValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '{':
		dec := json.NewDecoder(bytes.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return ""
		}
		for dec.More() {
			if _, err := dec.Token(); err != nil { // the key
				return ""
			}
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return ""
			}
			if s := jsonString(v); s != "" {
				return s
			}
		}
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return ""
		}
		for _, item := range items {
			if s := jsonString(item); s != "" {
				return s
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(item, &obj) != nil {
				continue
			}
			if s := jsonString(obj["message"]); s != "" {
				return s
			}
			code, field := jsonString(obj["code"]), jsonString(obj["field"])
			switch {
			case code != "" && field != "":
				return fmt.Sprintf("%s field '%s'", code, field)
			case code != "":
				return code
			}
		}
	}
	return ""
}

// truncate cuts s to at most n characters, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
