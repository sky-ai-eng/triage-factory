package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type opTestBody struct {
	Name string `json:"name" doc:"A name."`
}

// writeHints are complete hints for an op that writes.
var writeHints = Hints{ReadOnly: HintNo, Destructive: HintNo, Idempotent: HintYes, OpenWorld: HintNo}

func opTestSpec(pattern string) OpSpec {
	spec := OpSpec{Pattern: pattern, ID: "do_thing", Hints: writeHints}
	if strings.Contains(pattern, "{id}") {
		spec.PathParams = map[string]string{"id": "The thing's id."}
	}
	return spec
}

func TestNewOp_RefusesMalformedSpecs(t *testing.T) {
	with := func(pattern string, edit func(*OpSpec)) OpSpec {
		spec := opTestSpec(pattern)
		edit(&spec)
		return spec
	}
	jsonOp := func(spec OpSpec) func() { return func() { NewJSONOp[opTestBody, opTestBody](spec) } }
	cases := []struct {
		name string
		make func()
		want string
	}{
		{"pattern with no method", jsonOp(with("/api/things", func(*OpSpec) {})), "Pattern must be"},
		{"pattern outside /api", jsonOp(with("POST /things", func(*OpSpec) {})), "Pattern must be"},
		{"pattern with another method", jsonOp(with("HEAD /api/things", func(*OpSpec) {})), "Pattern must be"},
		{"ID not lower snake case", jsonOp(with("POST /api/things", func(s *OpSpec) { s.ID = "doThing" })), "ID must be"},
		{"ID with an empty segment", jsonOp(with("POST /api/things", func(s *OpSpec) { s.ID = "do__thing" })), "ID must be"},
		{"path parameter undocumented", jsonOp(with("POST /api/things/{id}", func(s *OpSpec) { s.PathParams = nil })), "PathParams must document"},
		{"path parameter documented that the path lacks", jsonOp(with("POST /api/things/{id}", func(s *OpSpec) { s.PathParams["other"] = "x" })), "PathParams must document"},
		{"wildcard documented with its dots", jsonOp(with("POST /api/files/{path...}", func(s *OpSpec) { s.PathParams = map[string]string{"path...": "x"} })), "PathParams must document"},
		{"query name repeated", jsonOp(with("POST /api/things", func(s *OpSpec) {
			s.Query = []QueryParam{{Name: "q", Type: ParamString}, {Name: "q", Type: ParamString}}
		})), `"q" is declared twice`},
		{"query with no type", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Query = []QueryParam{{Name: "q"}} })), `"q" has no Type`},
		{"ReadOnly unset", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Hints.ReadOnly = HintUnset })), "must set ReadOnly and OpenWorld"},
		{"OpenWorld unset", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Hints.OpenWorld = HintUnset })), "must set ReadOnly and OpenWorld"},
		{"a write with Destructive unset", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Hints.Destructive = HintUnset })), "must set Destructive and Idempotent"},
		{"a write with Idempotent unset", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Hints.Idempotent = HintUnset })), "must set Destructive and Idempotent"},
		{"a read with Destructive set", jsonOp(with("POST /api/things", func(s *OpSpec) {
			s.Hints = Hints{ReadOnly: HintYes, Destructive: HintNo, OpenWorld: HintNo}
		})), "must leave Destructive and Idempotent unset"},
		{"a read with Idempotent set", jsonOp(with("POST /api/things", func(s *OpSpec) {
			s.Hints = Hints{ReadOnly: HintYes, Idempotent: HintYes, OpenWorld: HintNo}
		})), "must leave Destructive and Idempotent unset"},
		{"a GET with a body", jsonOp(with("GET /api/things", func(*OpSpec) {})), "its Req must be NoBody"},
		{"a GET with a form", func() {
			NewJSONOp[NoBody, opTestBody](with("GET /api/things", func(s *OpSpec) {
				s.Hints = Hints{ReadOnly: HintYes, OpenWorld: HintNo}
				s.Form = []FormPart{{Name: "file", File: true}}
			}))
		}, "it declares no Form"},
		{"a form beside a JSON body", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Form = []FormPart{{Name: "file", File: true}} })), "its Req must be NoBody"},
		{"form part name repeated", func() {
			NewJSONOp[NoBody, opTestBody](with("POST /api/things", func(s *OpSpec) { s.Form = []FormPart{{Name: "file"}, {Name: "file"}} }))
		}, `form part "file" is declared twice`},
		{"JSON status not 2xx", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Status = http.StatusFound })), "status 302"},
		{"JSON status 204", jsonOp(with("POST /api/things", func(s *OpSpec) { s.Status = http.StatusNoContent })), "status 204"},
		{"JSON other status not 2xx", jsonOp(with("POST /api/things", func(s *OpSpec) { s.OtherStatuses = []int{http.StatusConflict} })), "status 409"},
		{"JSON other status repeats Status", jsonOp(with("POST /api/things", func(s *OpSpec) {
			s.Status = http.StatusCreated
			s.OtherStatuses = []int{http.StatusCreated}
		})), "status 201 is declared twice"},
		{"JSON other status repeats the default", jsonOp(with("POST /api/things", func(s *OpSpec) { s.OtherStatuses = []int{http.StatusOK} })), "status 200 is declared twice"},
		{"list status not 200", func() {
			NewListOp[opTestBody, opTestBody](with("POST /api/things/list", func(s *OpSpec) { s.Status = http.StatusCreated }))
		}, "a list answers 200 only"},
		{"list with other statuses", func() {
			NewListOp[opTestBody, opTestBody](with("POST /api/things/list", func(s *OpSpec) { s.OtherStatuses = []int{http.StatusCreated} }))
		}, "only a JSON op declares OtherStatuses"},
		{"NoContent status not 204", func() {
			NewNoContentOp[NoBody](with("DELETE /api/things/{id}", func(s *OpSpec) { s.Status = http.StatusOK }))
		}, "a NoContent op answers 204 only"},
		{"bytes status not 2xx", func() {
			NewBytesOp[NoBody](with("GET /api/files", func(s *OpSpec) { s.Status = http.StatusFound }), "image/png")
		}, "status 302 is not a 2xx"},
		{"bytes with no content type", func() { NewBytesOp[NoBody](with("GET /api/files", func(*OpSpec) {})) }, "at least one content type"},
		{"websocket with a status", func() {
			NewWebSocketOp(with("GET /api/stream", func(s *OpSpec) { s.Status = http.StatusSwitchingProtocols }))
		}, "declares no Status"},
		{"websocket that is not a GET", func() { NewWebSocketOp(with("POST /api/stream", func(*OpSpec) {})) }, "a websocket handshake is a GET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				got, _ := recover().(string)
				if !strings.Contains(got, tc.want) {
					t.Errorf("panic = %q, want one containing %q", got, tc.want)
				}
			}()
			tc.make()
		})
	}
}

func TestNewOp_ResolvesTheKindsDefaultStatus(t *testing.T) {
	readHints := Hints{ReadOnly: HintYes, OpenWorld: HintNo}
	cases := []struct {
		op   Operation
		want int
	}{
		{NewJSONOp[opTestBody, opTestBody](opTestSpec("POST /api/things")), http.StatusOK},
		{NewListOp[opTestBody, opTestBody](opTestSpec("POST /api/things/list")), http.StatusOK},
		{NewNoContentOp[NoBody](opTestSpec("DELETE /api/things/{id}")), http.StatusNoContent},
		{NewBytesOp[NoBody](OpSpec{Pattern: "GET /api/files/{path...}", ID: "get_file", PathParams: map[string]string{"path": "The file's path."}, Hints: readHints}, "image/png"), http.StatusOK},
		{NewWebSocketOp(OpSpec{Pattern: "GET /api/stream/{$}", ID: "open_stream", Hints: readHints}), 0},
	}
	for _, tc := range cases {
		if got := tc.op.Spec().Status; got != tc.want {
			t.Errorf("%s: Status = %d, want %d", tc.op.Spec().Pattern, got, tc.want)
		}
	}
}

func TestOp_MethodsRefuseWhatTheSpecDoesNotDeclare(t *testing.T) {
	noBody := NewJSONOp[NoBody, opTestBody](OpSpec{
		Pattern: "POST /api/things", ID: "do_thing", Hints: writeHints,
		Query:         []QueryParam{{Name: "q", Type: ParamString}},
		OtherStatuses: []int{http.StatusCreated},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/things?q=1&other=2", nil)
	cases := []struct {
		name string
		call func()
		want string
	}{
		{"Decode with NoBody", func() { noBody.Decode(httptest.NewRecorder(), req) }, "calls Decode, but its Req is NoBody"},
		{"DecodeBytes with NoBody", func() { noBody.DecodeBytes(httptest.NewRecorder(), nil) }, "calls DecodeBytes, but its Req is NoBody"},
		{"Query.Get of an undeclared name", func() { noBody.Query(req).Get("other") }, `reads query parameter "other"`},
		{"Query.Has of an undeclared name", func() { noBody.Query(req).Has("other") }, `reads query parameter "other"`},
		{"Query.All of an undeclared name", func() { noBody.Query(req).All("other") }, `reads query parameter "other"`},
		{"WriteStatus of an undeclared status", func() { noBody.WriteStatus(httptest.NewRecorder(), http.StatusAccepted, opTestBody{}) }, "writes status 202"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				got, _ := recover().(string)
				if !strings.Contains(got, tc.want) {
					t.Errorf("panic = %q, want one containing %q", got, tc.want)
				}
			}()
			tc.call()
		})
	}

	if got := noBody.Query(req).Get("q"); got != "1" {
		t.Errorf(`Query.Get("q") = %q, want "1"`, got)
	}
	rec := httptest.NewRecorder()
	noBody.WriteStatus(rec, http.StatusCreated, opTestBody{Name: "x"})
	if rec.Code != http.StatusCreated {
		t.Errorf("WriteStatus(201) wrote %d", rec.Code)
	}
}

func TestOp_DecodeAppliesTheSpecsBodyLimit(t *testing.T) {
	spec := opTestSpec("POST /api/things")
	spec.BodyLimit = 8
	op := NewJSONOp[opTestBody, opTestBody](spec)
	rec := httptest.NewRecorder()
	if _, ok := op.Decode(rec, httptest.NewRequest(http.MethodPost, "/api/things", strings.NewReader(`{"name":"longer than eight"}`))); ok {
		t.Fatal("Decode accepted a body over the limit")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestPatch_Read(t *testing.T) {
	type pair struct {
		A int `json:"a"`
	}
	type body struct {
		N Patch[int]  `json:"n"`
		P Patch[pair] `json:"p"`
	}
	readN := func(v *Validation, b body) (any, PatchState) { return b.N.Read(v, "n") }
	readP := func(v *Validation, b body) (any, PatchState) { return b.P.Read(v, "p") }
	cases := []struct {
		body      string
		read      func(*Validation, body) (any, PatchState)
		wantValue any
		wantState PatchState
		wantFault *ErrorItem
	}{
		{body: `{}`, read: readN, wantValue: 0, wantState: PatchAbsent},
		{body: `{"n": null}`, read: readN, wantValue: 0, wantState: PatchClear},
		{body: `{"n": 3}`, read: readN, wantValue: 3, wantState: PatchSet},
		{body: `{"n": "three"}`, read: readN, wantValue: 0, wantState: patchInvalid,
			wantFault: &ErrorItem{Reason: ReasonInvalidField, Message: "n must be a whole number, or null", Field: "n"}},
		{body: `{"p": {"a": 1, "b": 2}}`, read: readP, wantValue: pair{}, wantState: patchInvalid,
			wantFault: &ErrorItem{Reason: ReasonInvalidField, Message: `p has no field "b"`, Field: "p"}},
	}
	for _, tc := range cases {
		var b body
		if err := json.Unmarshal([]byte(tc.body), &b); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.body, err)
		}
		var v Validation
		got, state := tc.read(&v, b)
		if got != tc.wantValue || state != tc.wantState {
			t.Errorf("%s: Read = (%v, %d), want (%v, %d)", tc.body, got, state, tc.wantValue, tc.wantState)
		}
		items := v.Items()
		switch {
		case tc.wantFault == nil && len(items) != 0:
			t.Errorf("%s: faults %+v, want none", tc.body, items)
		case tc.wantFault != nil && (len(items) != 1 || items[0] != *tc.wantFault):
			t.Errorf("%s: faults %+v, want %+v", tc.body, items, *tc.wantFault)
		}
	}
}
