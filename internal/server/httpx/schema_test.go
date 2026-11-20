package httpx

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type schemaLeaf struct {
	Name string `json:"name" doc:"The leaf's name."`
}

type schemaEmbedded struct {
	Inner string `json:"inner" doc:"A field promoted from the embedded struct."`
}

type schemaPointerEmbedded struct {
	Maybe string `json:"maybe" doc:"A field promoted through an embedded pointer."`
}

type schemaGeneric[T any] struct {
	Value T `json:"value" doc:"The value."`
}

type schemaFields struct {
	schemaEmbedded
	*schemaPointerEmbedded

	Plain    string      `json:"plain" doc:"Always written."`
	Optional string      `json:"optional,omitempty" doc:"Left out when empty."`
	Zero     time.Time   `json:"zero,omitzero" doc:"Left out when zero."`
	Count    int         `json:"count,string" doc:"A number written as a string."`
	Leaf     *schemaLeaf `json:"leaf" doc:"A nullable reference."`
	Inline   struct {
		A int `json:"a" doc:"An inline field."`
	} `json:"inline" doc:"An anonymous struct."`
	Wrapped  schemaGeneric[int] `json:"wrapped" doc:"A generic struct."`
	Skipped  string             `json:"-"`
	Refused  *int               `json:"refused" spec:"-"`
	unexport string
}

// schemaSelfEncoding encodes itself and describes its own schema.
type schemaSelfEncoding struct{}

func (schemaSelfEncoding) MarshalJSON() ([]byte, error) { return []byte(`"x"`), nil }
func (schemaSelfEncoding) OpenAPISchema() map[string]any {
	return map[string]any{"type": "string", "description": "Provided."}
}

// schemaUndescribed encodes itself and describes nothing.
type schemaUndescribed struct{}

func (*schemaUndescribed) UnmarshalText([]byte) error { return nil }

type schemaUndocumented struct {
	Bare string `json:"bare"`
}

// Location shares its name with time.Location.
type Location struct {
	Here string `json:"here" doc:"Where."`
}

type schemaBothLocations struct {
	Ours   Location       `json:"ours" doc:"Ours."`
	Theirs *time.Location `json:"theirs" doc:"Theirs."`
}

func TestSchema_FollowsEncodingJSON(t *testing.T) {
	_ = schemaFields{}.unexport
	cases := []struct {
		name       string
		t          reflect.Type
		dir        direction
		want       string
		components string
		problems   []string
	}{
		{name: "string", t: reflect.TypeFor[string](), want: `{"type":"string"}`},
		{name: "bool", t: reflect.TypeFor[bool](), want: `{"type":"boolean"}`},
		{name: "int", t: reflect.TypeFor[int](), want: `{"type":"integer"}`},
		{name: "int64", t: reflect.TypeFor[int64](), want: `{"type":"integer","format":"int64"}`},
		{name: "uint64", t: reflect.TypeFor[uint64](), want: `{"type":"integer","format":"int64"}`},
		{name: "float", t: reflect.TypeFor[float32](), want: `{"type":"number"}`},
		{name: "time", t: reflect.TypeFor[time.Time](), want: `{"type":"string","format":"date-time"}`},
		{name: "bytes", t: reflect.TypeFor[[]byte](), want: `{"type":["string","null"],"format":"byte"}`},
		{name: "raw message", t: reflect.TypeFor[json.RawMessage](), want: `{}`},
		{name: "interface", t: reflect.TypeFor[any](), want: `{}`},
		{name: "map", t: reflect.TypeFor[map[string]int](), want: `{"type":["object","null"],"additionalProperties":{"type":"integer"}}`},
		{name: "slice", t: reflect.TypeFor[[]string](), want: `{"type":["array","null"],"items":{"type":"string"}}`},
		{name: "array", t: reflect.TypeFor[[2]bool](), want: `{"type":"array","items":{"type":"boolean"}}`},
		{name: "pointer", t: reflect.TypeFor[*int](), want: `{"type":["integer","null"]}`},
		{name: "pointer to a slice", t: reflect.TypeFor[*[]int](), want: `{"type":["array","null"],"items":{"type":"integer"}}`},
		{name: "patch", t: reflect.TypeFor[Patch[[]string]](), want: `{"type":["array","null"],"items":{"type":"string"}}`},
		{
			name: "pointer to a named struct", t: reflect.TypeFor[*schemaLeaf](),
			want:       `{"oneOf":[{"$ref":"#/components/schemas/SchemaLeaf"},{"type":"null"}]}`,
			components: `{"SchemaLeaf":{"type":"object","properties":{"name":{"type":"string","description":"The leaf's name."}},"required":["name"]}}`,
		},
		{
			name: "response struct", t: reflect.TypeFor[schemaFields](),
			want: `{"$ref":"#/components/schemas/SchemaFields"}`,
			components: `{
				"SchemaLeaf":{"type":"object","properties":{"name":{"type":"string","description":"The leaf's name."}},"required":["name"]},
				"SchemaFields":{"type":"object","properties":{
					"inner":{"type":"string","description":"A field promoted from the embedded struct."},
					"maybe":{"type":"string","description":"A field promoted through an embedded pointer."},
					"plain":{"type":"string","description":"Always written."},
					"optional":{"type":"string","description":"Left out when empty."},
					"zero":{"type":"string","format":"date-time","description":"Left out when zero."},
					"count":{"type":"string","description":"A number written as a string."},
					"leaf":{"oneOf":[{"$ref":"#/components/schemas/SchemaLeaf"},{"type":"null"}],"description":"A nullable reference."},
					"inline":{"type":"object","properties":{"a":{"type":"integer","description":"An inline field."}},"required":["a"],"description":"An anonymous struct."},
					"wrapped":{"type":"object","properties":{"value":{"type":"integer","description":"The value."}},"required":["value"],"description":"A generic struct."}
				},"required":["inner","plain","count","leaf","inline","wrapped"]}
			}`,
		},
		{
			name: "request struct", t: reflect.TypeFor[schemaLeaf](), dir: dirRequest,
			want:       `{"$ref":"#/components/schemas/SchemaLeaf"}`,
			components: `{"SchemaLeaf":{"type":"object","properties":{"name":{"type":"string","description":"The leaf's name."}}}}`,
		},
		{
			name: "self-encoding with a schema", t: reflect.TypeFor[*schemaSelfEncoding](),
			want: `{"type":["string","null"],"description":"Provided."}`,
		},
		{
			name: "self-encoding without a schema", t: reflect.TypeFor[schemaUndescribed](),
			want:     `{}`,
			problems: []string{httpxPath + ".schemaUndescribed encodes itself through UnmarshalText, so it must implement httpx.SchemaProvider"},
		},
		{
			name: "undocumented field", t: reflect.TypeFor[schemaUndocumented](),
			want:       `{"$ref":"#/components/schemas/SchemaUndocumented"}`,
			components: `{"SchemaUndocumented":{"type":"object","properties":{"bare":{"type":"string"}},"required":["bare"]}}`,
			problems:   []string{httpxPath + `.schemaUndocumented.Bare (json "bare") has no doc tag`},
		},
		{
			name: "two types of one name", t: reflect.TypeFor[schemaBothLocations](),
			want: `{"$ref":"#/components/schemas/SchemaBothLocations"}`,
			components: `{
				"HttpxLocation":{"type":"object","properties":{"here":{"type":"string","description":"Where."}},"required":["here"]},
				"TimeLocation":{"type":"object"},
				"SchemaBothLocations":{"type":"object","properties":{
					"ours":{"$ref":"#/components/schemas/HttpxLocation","description":"Ours."},
					"theirs":{"oneOf":[{"$ref":"#/components/schemas/TimeLocation"},{"type":"null"}],"description":"Theirs."}
				},"required":["ours","theirs"]}
			}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSchemaSet()
			got := s.schema(tc.t, tc.dir, "")
			components := s.finish()
			assertJSON(t, "schema", got, tc.want)
			if tc.components == "" {
				tc.components = "{}"
			}
			assertJSON(t, "components", components, tc.components)
			if problems := s.problemList(); !slices.Equal(problems, tc.problems) {
				t.Errorf("problems = %q, want %q", problems, tc.problems)
			}
		})
	}
}

// TestSchema_RequestAndResponseRenderApart: a type both sides reach renders
// once per side, since only the response lists required fields.
func TestSchema_RequestAndResponseRenderApart(t *testing.T) {
	s := newSchemaSet()
	resp := s.schema(reflect.TypeFor[schemaLeaf](), dirResponse, "")
	req := s.schema(reflect.TypeFor[schemaLeaf](), dirRequest, "")
	components := s.finish()
	assertJSON(t, "response", resp, `{"$ref":"#/components/schemas/SchemaLeaf"}`)
	assertJSON(t, "request", req, `{"$ref":"#/components/schemas/SchemaLeafInput"}`)
	assertJSON(t, "components", components, `{
		"SchemaLeaf":{"type":"object","properties":{"name":{"type":"string","description":"The leaf's name."}},"required":["name"]},
		"SchemaLeafInput":{"type":"object","properties":{"name":{"type":"string","description":"The leaf's name."}}}
	}`)
}

type renderListRequest struct {
	Search string `json:"search" doc:"Text to match."`
	PageRequest
}

func TestOpenAPI_RendersEachKind(t *testing.T) {
	read := Hints{ReadOnly: HintYes, OpenWorld: HintNo}
	path := map[string]string{"id": "The thing's id."}
	entries := []SpecEntry{
		{Op: NewListOp[renderListRequest, schemaLeaf](OpSpec{Pattern: "POST /api/things/list", ID: "list_things", Summary: "List things", Description: "Lists things.", Hints: read})},
		{Op: NewJSONOp[NoBody, schemaLeaf](OpSpec{Pattern: "GET /api/things/{id}", ID: "get_thing", PathParams: path, Hints: read,
			Query: []QueryParam{{Name: "at", Type: ParamDateTime, Doc: "When."}}})},
		{Op: NewJSONOp[schemaLeaf, schemaLeaf](OpSpec{Pattern: "PUT /api/things/{id}", ID: "put_thing", PathParams: path, Hints: writeHints,
			Status: http.StatusCreated, OtherStatuses: []int{http.StatusOK}})},
		{Op: NewNoContentOp[NoBody](OpSpec{Pattern: "DELETE /api/things/{id}", ID: "delete_thing", PathParams: path,
			Hints: Hints{ReadOnly: HintNo, Destructive: HintYes, Idempotent: HintYes, OpenWorld: HintNo}})},
		{Op: NewBytesOp[NoBody](OpSpec{Pattern: "GET /api/files/{path...}", ID: "get_file", PathParams: map[string]string{"path": "The path."}, Hints: read}, "image/png", "image/gif")},
		{Op: NewWebSocketOp(OpSpec{Pattern: "GET /api/stream", ID: "open_stream", Hints: read})},
		{Op: NewJSONOp[NoBody, schemaUndocumented](OpSpec{Pattern: "GET /api/ping", ID: "ping", Hints: read}), Raw: true},
		{Op: NewNoContentOp[NoBody](OpSpec{Pattern: "POST /api/uploads", ID: "upload", Hints: writeHints, Form: []FormPart{
			{Name: "note", Doc: "A note."},
			{Name: "file", File: true, Doc: "The file."},
			{Name: "tags", Multiple: true, Doc: "Tags."},
			{Name: "images", File: true, Multiple: true, Doc: "Images."},
		}})},
	}
	doc, problems := OpenAPI(entries)
	again, _ := OpenAPI(entries)
	if string(doc) != string(again) || !strings.HasSuffix(string(doc), "}\n") {
		t.Fatal("rendering is not byte-identical across calls, or lacks its trailing newline")
	}
	if want := []string{httpxPath + `.schemaUndocumented.Bare (json "bare") has no doc tag`}; !slices.Equal(problems, want) {
		t.Errorf("problems = %q, want %q", problems, want)
	}
	var spec map[string]any
	if err := json.Unmarshal(doc, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	op := func(path, method string) map[string]any {
		t.Helper()
		got, _ := lookup(spec, "paths", path, method).(map[string]any)
		if got == nil {
			t.Fatalf("no %s %s in paths", method, path)
		}
		return got
	}
	checks := []struct {
		name string
		got  any
		want string
	}{
		{"header", map[string]any{"openapi": spec["openapi"], "info": spec["info"]}, `{"openapi":"3.1.0","info":{"title":"Triage Factory API","version":"unstable"}}`},
		{"list", op("/api/things/list", "post"), `{
			"operationId":"list_things","summary":"List things","description":"Lists things.",
			"requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/RenderListRequest"}}}},
			"responses":{"default":{"$ref":"#/components/responses/Error"},"200":{"description":"OK","content":{"application/json":{"schema":{
				"type":"object","required":["items","total_count"],"properties":{
					"items":{"type":"array","items":{"$ref":"#/components/schemas/SchemaLeaf"},"description":"The rows of this page, as an empty array when it has none."},
					"next_page_token":{"type":"string","description":"The page_token that reads the next page, absent on the last page."},
					"total_count":{"type":["integer","null"],"description":"How many rows match the request's filters across every page, or null for a list whose upstream cannot count them."}
				}}}}}},
			"security":[{"session":[]},{"token":[]}],
			"x-hints":{"readOnlyHint":true,"openWorldHint":false}
		}`},
		{"get parameters and no body", map[string]any{"parameters": op("/api/things/{id}", "get")["parameters"], "requestBody": op("/api/things/{id}", "get")["requestBody"]}, `{
			"parameters":[
				{"name":"id","in":"path","required":true,"description":"The thing's id.","schema":{"type":"string"}},
				{"name":"at","in":"query","required":false,"description":"When.","schema":{"type":"string","format":"date-time"}}
			],"requestBody":null}`},
		{"JSON statuses", lookup(op("/api/things/{id}", "put"), "responses"), `{
			"default":{"$ref":"#/components/responses/Error"},
			"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"#/components/schemas/SchemaLeaf"}}}},
			"201":{"description":"Created","content":{"application/json":{"schema":{"$ref":"#/components/schemas/SchemaLeaf"}}}}}`},
		{"no content", map[string]any{"responses": op("/api/things/{id}", "delete")["responses"], "x-hints": op("/api/things/{id}", "delete")["x-hints"]}, `{
			"responses":{"default":{"$ref":"#/components/responses/Error"},"204":{"description":"No Content"}},
			"x-hints":{"readOnlyHint":false,"destructiveHint":true,"idempotentHint":true,"openWorldHint":false}}`},
		{"bytes under a wildcard", lookup(op("/api/files/{path}", "get"), "responses", "200"), `{"description":"OK","content":{
			"image/png":{"schema":{"type":"string","format":"binary"}},"image/gif":{"schema":{"type":"string","format":"binary"}}}}`},
		{"websocket", lookup(op("/api/stream", "get"), "responses"), `{"default":{"$ref":"#/components/responses/Error"},"101":{"description":"Switching Protocols"}}`},
		{"raw", lookup(op("/api/ping", "get"), "security"), `[]`},
		{"form", op("/api/uploads", "post")["requestBody"], `{"required":true,"content":{"multipart/form-data":{"schema":{"type":"object","properties":{
			"note":{"type":"string","description":"A note."},
			"file":{"type":"string","format":"binary","description":"The file."},
			"tags":{"type":"array","items":{"type":"string"},"description":"Tags."},
			"images":{"type":"array","items":{"type":"string","format":"binary"},"description":"Images."}
		}}}}}`},
		{"error response", lookup(spec, "components", "responses", "Error", "content"), `{"application/json":{"schema":{"$ref":"#/components/schemas/ErrorEnvelope"}}}`},
		{"security schemes", lookup(spec, "components", "securitySchemes"), `{
			"session":{"type":"apiKey","in":"cookie","name":"sid","description":"The session cookie a browser sign-in sets."},
			"token":{"type":"http","scheme":"bearer","description":"An API token, sent as Authorization: Bearer tf_…."}}`},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { assertJSON(t, c.name, c.got, c.want) })
	}
}

// lookup walks a decoded JSON document by object keys, nil where a key is
// missing.
func lookup(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

// assertJSON compares got, as JSON, against want's JSON, ignoring layout
// and key order.
func assertJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", what, err)
	}
	var g, w any
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("%s: unmarshal got: %v", what, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: unmarshal want: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s =\n  %s\nwant\n  %s", what, raw, strings.Join(strings.Fields(want), " "))
	}
}
