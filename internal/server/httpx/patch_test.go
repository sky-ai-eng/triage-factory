package httpx

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestPatchValue_TypeFaultMessages pins that a PATCH reader words a type
// fault as the strict body decode does, with null offered as well, so a
// refused number names the rule it broke here too.
func TestPatchValue_TypeFaultMessages(t *testing.T) {
	readInt := func(v *Validation, raw json.RawMessage) { PatchInt(v, raw, "n") }
	readFloat := func(v *Validation, raw json.RawMessage) { PatchFloat(v, raw, "n") }
	readBool := func(v *Validation, raw json.RawMessage) { PatchBool(v, raw, "n") }
	readStrings := func(v *Validation, raw json.RawMessage) { PatchStrings(v, raw, "n") }
	cases := []struct {
		name string
		read func(*Validation, json.RawMessage)
		raw  string
		want string
	}{
		{"int out of range", readInt, `99999999999999999999`, "n must be a whole number from -9223372036854775808 to 9223372036854775807, or null"},
		{"int with an exponent", readInt, `1e3`, "n must be a whole number written without a decimal point or exponent, or null"},
		{"int with a decimal point", readInt, `1.0`, "n must be a whole number written without a decimal point or exponent, or null"},
		{"int given a string", readInt, `"x"`, "n must be a whole number, or null"},
		{"float out of range", readFloat, `1e400`, "n must be a number from -1.7976931348623157e+308 to 1.7976931348623157e+308, or null"},
		{"bool given a string", readBool, `"yes"`, "n must be true or false, or null"},
		{"strings given a bad element", readStrings, `["a", 1]`, "n must be an array of strings, or null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v Validation
			tc.read(&v, json.RawMessage(tc.raw))
			want := []ErrorItem{{Reason: ReasonInvalidField, Message: tc.want, Field: "n"}}
			if got := v.Items(); !slices.Equal(got, want) {
				t.Errorf("faults = %+v, want %+v", got, want)
			}
		})
	}
}

type patchJSONEntry struct {
	Name  string            `json:"name"`
	Count int               `json:"count"`
	Tags  map[string]string `json:"tags"`
}

// TestPatchJSON: absent keeps, null clears, a list is the whole value (an
// empty one included), and a bad body is a fault naming the field and the
// entry key, never a partial read.
func TestPatchJSON(t *testing.T) {
	t.Run("absent, null and values", func(t *testing.T) {
		var v Validation
		if _, st := PatchJSON[patchJSONEntry](&v, nil, "f"); st != PatchAbsent {
			t.Errorf("absent = %v, want PatchAbsent", st)
		}
		if got, st := PatchJSON[patchJSONEntry](&v, json.RawMessage(`null`), "f"); st != PatchClear || got != nil {
			t.Errorf("null = (%v, %v), want (nil, PatchClear)", got, st)
		}
		got, st := PatchJSON[patchJSONEntry](&v, json.RawMessage(`[]`), "f")
		if st != PatchSet || got == nil || len(got) != 0 {
			t.Errorf("[] = (%#v, %v), want an empty non-nil list and PatchSet", got, st)
		}
		got, st = PatchJSON[patchJSONEntry](&v, json.RawMessage(`[{"name":"a","count":2,"tags":{"k":"v"}},{"name":"b"}]`), "f")
		if st != PatchSet || len(got) != 2 || got[0].Name != "a" || got[0].Count != 2 || got[0].Tags["k"] != "v" || got[1].Name != "b" {
			t.Errorf("list = (%+v, %v)", got, st)
		}
		if items := v.Items(); len(items) != 0 {
			t.Errorf("faults on valid input: %+v", items)
		}
	})

	for _, tc := range []struct {
		name string
		raw  string
		want ErrorItem
	}{
		{"unknown entry key", `[{"name":"a","label":"b"}]`,
			ErrorItem{Reason: ReasonUnknownField, Message: `f entries have no field "label"`, Field: "f[].label"}},
		{"wrong type inside an entry", `[{"name":"a","count":"x"}]`,
			ErrorItem{Reason: ReasonInvalidField, Message: "f[].count must be a whole number", Field: "f[].count"}},
		{"an object, not a list", `{"name":"a"}`,
			ErrorItem{Reason: ReasonInvalidField, Message: "f must be an array of objects, or null", Field: "f"}},
		{"a list of strings", `["a"]`,
			ErrorItem{Reason: ReasonInvalidField, Message: "f must be an array of objects, or null", Field: "f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v Validation
			got, st := PatchJSON[patchJSONEntry](&v, json.RawMessage(tc.raw), "f")
			if st == PatchSet || got != nil {
				t.Errorf("a bad body read as (%+v, %v)", got, st)
			}
			if items := v.Items(); !slices.Equal(items, []ErrorItem{tc.want}) {
				t.Errorf("faults = %+v, want %+v", items, tc.want)
			}
		})
	}
}
