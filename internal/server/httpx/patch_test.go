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
