package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
// or the first value of `errors` (Jira), and `error` (Slack).
func Excerpt(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return "empty body"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return fmt.Sprintf("non-JSON body, %d bytes", len(body))
	}
	for _, msg := range []string{
		jsonString(obj["message"]),
		firstArrayString(obj["errorMessages"]),
		firstErrorsValue(obj["errors"]),
		jsonString(obj["error"]),
	} {
		if msg = strings.Join(strings.Fields(msg), " "); msg != "" {
			return truncate(msg, maxExcerpt)
		}
	}
	return fmt.Sprintf("JSON body with no error message, %d bytes", len(body))
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
// array (GitHub's validation errors) contributes its first string, or the
// `message` of its first object.
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
			if json.Unmarshal(item, &obj) == nil {
				if s := jsonString(obj["message"]); s != "" {
					return s
				}
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
