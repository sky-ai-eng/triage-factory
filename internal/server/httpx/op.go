package httpx

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// --- declared operations ---
//
// An operation declares one /api route in a single value beside its handler:
// the mux pattern, the request and response types, the caller-facing summary
// and description, the path and query parameters, and MCP's tool hints. The
// server mounts the route through it, the handler decodes, reads its query,
// and writes its success body through it, and OpenAPI renders the spec from
// the same values, so the spec describes what the handler does rather than a
// second account of it that can drift.
//
// The constructors panic on a malformed declaration. Every op is a package
// variable, so the panic happens at init and fails every test in the
// package, which is when a programming error should surface.

// OpSpec is the caller-facing declaration of one route.
type OpSpec struct {
	// Pattern is the ServeMux pattern, method included: "POST /api/tasks/list".
	Pattern string
	// ID is the operationId: lower snake case, unique across the API.
	ID string
	// Summary is an imperative phrase in sentence case, at most 60
	// characters, with no trailing period.
	Summary string
	// Description says what the route does and returns, from the caller's
	// side, in one to three sentences.
	Description string
	// PathParams documents each {name} and {name...} segment of Pattern.
	PathParams map[string]string
	// Query lists every query parameter the handler reads.
	Query []QueryParam
	// Status is the success status; 0 is the kind's default.
	Status int
	// OtherStatuses are further 2xx statuses a JSON op writes with the same
	// body.
	OtherStatuses []int
	// BodyLimit is the request body cap in bytes; 0 is DecodeJSONStrict's
	// default.
	BodyLimit int64
	// Form documents the parts of a multipart/form-data body, which the
	// handler reads itself. Only an op whose Req is NoBody takes one.
	Form  []FormPart
	Hints Hints
}

// QueryParam documents one query parameter.
type QueryParam struct {
	Name string
	Type ParamType
	Doc  string
}

// FormPart documents one part of a multipart/form-data body.
type FormPart struct {
	Name string
	// File is whether the part is a file; otherwise it is text.
	File bool
	// Multiple is whether the part may repeat.
	Multiple bool
	Doc      string
}

// ParamType is a query parameter's schema type. The zero value is no type,
// which the constructors refuse.
type ParamType uint8

const (
	ParamString ParamType = iota + 1
	ParamInteger
	ParamBoolean
	ParamDateTime
)

// Hint is one of MCP's tool annotations: unset, yes, or no.
type Hint uint8

const (
	HintUnset Hint = iota
	HintYes
	HintNo
)

// Hints are MCP's tool annotations with MCP's meanings. ReadOnly and
// OpenWorld are always set. Destructive and Idempotent are set exactly when
// ReadOnly is no, because MCP defines them only for tools that write.
type Hints struct{ ReadOnly, Destructive, Idempotent, OpenWorld Hint }

// NoBody is the Req of a route that reads no request body.
type NoBody struct{}

// Operation is what a mount takes. Its unexported method means only httpx's
// op types implement it.
type Operation interface {
	Spec() OpSpec
	shape() opShape
}

// opKind is which of the five op types an Operation is.
type opKind uint8

const (
	kindJSON opKind = iota
	kindList
	kindNoContent
	kindBytes
	kindWebSocket
)

// opShape is what OpenAPI needs from an op beyond its OpSpec: its kind, the
// types it decodes and writes, and a bytes op's content types. req is nil
// for a route that reads no body; resp is a JSON op's Resp and a list op's
// Item, and nil otherwise.
type opShape struct {
	kind         opKind
	req, resp    reflect.Type
	contentTypes []string
}

var (
	patternRE = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) (/api/\S*)$`)
	opIDRE    = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
)

// opCore is the part every op shares: the validated spec and the set of
// query names its handler may read.
type opCore struct {
	spec  OpSpec
	query map[string]bool
}

// Spec returns the op's declaration, with Status resolved to the kind's
// default where the declaration left it 0.
func (o *opCore) Spec() OpSpec { return o.spec }

// Query reads the request's query parameters, readable only by the names the
// spec declares. A query string with a pair that does not parse is refused,
// rather than read without that pair as URL.Query reads it: a filter
// dropped that way widens the read. On a refusal the error is already
// written and ok is false.
func (o *opCore) Query(w http.ResponseWriter, r *http.Request) (q QueryValues, ok bool) {
	values, v := parseQueryStrict(r.URL.RawQuery)
	if v.Flush(w, http.StatusBadRequest) {
		return QueryValues{}, false
	}
	return QueryValues{values: values, declared: o.query, pattern: o.spec.Pattern}, true
}

// parseQueryStrict parses a raw query string as url.ParseQuery does, and
// records every pair it cannot read instead of skipping it.
func parseQueryStrict(raw string) (url.Values, *Validation) {
	values := url.Values{}
	v := &Validation{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		rawName, rawValue, _ := strings.Cut(pair, "=")
		name, nameErr := url.QueryUnescape(rawName)
		value, valueErr := url.QueryUnescape(rawValue)
		switch {
		case nameErr != nil || strings.Contains(rawName, ";"):
			v.Add(ErrorItem{Reason: ReasonInvalidParam, Message: fmt.Sprintf(
				"the query string's pair %q has a name that is not percent-encoded, or contains a semicolon; parameters are joined with &", pair)})
		case strings.Contains(rawValue, ";"):
			v.Param(name, fmt.Sprintf("%s contains a semicolon; parameters are joined with &, and a semicolon in a value is written %%3B", name))
		case valueErr != nil:
			v.Param(name, fmt.Sprintf("%s is not percent-encoded correctly", name))
		default:
			values[name] = append(values[name], value)
		}
	}
	return values, v
}

// QueryValues is a request's query string, read through the names its op
// declares. Reading any other name panics, so a handler cannot read a
// parameter the spec does not list.
type QueryValues struct {
	values   url.Values
	declared map[string]bool
	pattern  string
}

// Get returns the first value of name, or "" when it is absent.
func (q QueryValues) Get(name string) string {
	q.check(name)
	return q.values.Get(name)
}

// Has reports whether name is present.
func (q QueryValues) Has(name string) bool {
	q.check(name)
	return q.values.Has(name)
}

// All returns every value of name, in request order.
func (q QueryValues) All(name string) []string {
	q.check(name)
	return q.values[name]
}

func (q QueryValues) check(name string) {
	if !q.declared[name] {
		panic(fmt.Sprintf("httpx: %s reads query parameter %q, which its OpSpec does not declare", q.pattern, name))
	}
}

// bodyOp adds the body decoders to every op but the websocket's.
type bodyOp[Req any] struct {
	opCore
}

// Decode strictly decodes the request body into a Req, as DecodeJSONStrict
// does, under the spec's BodyLimit when it sets one. On a failure the error
// is already written and ok is false.
func (o *bodyOp[Req]) Decode(w http.ResponseWriter, r *http.Request) (req Req, ok bool) {
	o.mustRead("Decode")
	// The decode is its own statement: in `return req, f(&req)` the spec
	// leaves unordered whether req is read before f writes it.
	if o.spec.BodyLimit > 0 {
		ok = DecodeJSONStrictLimit(w, r, &req, o.spec.BodyLimit)
	} else {
		ok = DecodeJSONStrict(w, r, &req)
	}
	return req, ok
}

// DecodeBytes strictly decodes a body already read into a Req, as
// DecodeJSONStrictBytes does. The body was read under BodyBytes's default
// cap, so an op that declares a BodyLimit decodes with Decode.
func (o *bodyOp[Req]) DecodeBytes(w http.ResponseWriter, body []byte) (req Req, ok bool) {
	o.mustRead("DecodeBytes")
	if o.spec.BodyLimit > 0 {
		panic(fmt.Sprintf("httpx: %s calls DecodeBytes, but its BodyLimit applies only to Decode", o.spec.Pattern))
	}
	ok = DecodeJSONStrictBytes(w, body, &req)
	return req, ok
}

func (o *bodyOp[Req]) mustRead(method string) {
	if reflect.TypeFor[Req]() == noBodyType {
		panic(fmt.Sprintf("httpx: %s calls %s, but its Req is NoBody", o.spec.Pattern, method))
	}
}

// reqType is Req for the spec, or nil when the route reads no body.
func reqType[Req any]() reflect.Type {
	if t := reflect.TypeFor[Req](); t != noBodyType {
		return t
	}
	return nil
}

var noBodyType = reflect.TypeFor[NoBody]()

// JSONOp is a route that answers a JSON body.
type JSONOp[Req, Resp any] struct {
	bodyOp[Req]
}

// NewJSONOp declares a route that answers a Resp as JSON. Status defaults to
// 200.
func NewJSONOp[Req, Resp any](spec OpSpec) *JSONOp[Req, Resp] {
	return &JSONOp[Req, Resp]{bodyOp[Req]{newCore(spec, kindJSON, reqType[Req](), 0)}}
}

func (o *JSONOp[Req, Resp]) shape() opShape {
	return opShape{kind: kindJSON, req: reqType[Req](), resp: reflect.TypeFor[Resp]()}
}

// Write writes resp with the spec's Status.
func (o *JSONOp[Req, Resp]) Write(w http.ResponseWriter, resp Resp) {
	WriteJSON(w, o.spec.Status, resp)
}

// WriteStatus writes resp with status, which must be the spec's Status or one
// of its OtherStatuses.
func (o *JSONOp[Req, Resp]) WriteStatus(w http.ResponseWriter, status int, resp Resp) {
	if status != o.spec.Status && !slices.Contains(o.spec.OtherStatuses, status) {
		panic(fmt.Sprintf("httpx: %s writes status %d, which its OpSpec does not declare", o.spec.Pattern, status))
	}
	WriteJSON(w, status, resp)
}

// ListOp is a list read: a page of Items in the list envelope.
type ListOp[Req, Item any] struct {
	bodyOp[Req]
}

// NewListOp declares a list read. It answers 200 only.
func NewListOp[Req, Item any](spec OpSpec) *ListOp[Req, Item] {
	return &ListOp[Req, Item]{bodyOp[Req]{newCore(spec, kindList, reqType[Req](), 0)}}
}

func (o *ListOp[Req, Item]) shape() opShape {
	return opShape{kind: kindList, req: reqType[Req](), resp: reflect.TypeFor[Item]()}
}

// Write writes an offset-paged page, as WriteList does.
func (o *ListOp[Req, Item]) Write(w http.ResponseWriter, page Page, items []Item, total int) {
	WriteList(w, page, items, total)
}

// WriteKeyset writes a keyset-paged page, as WriteListKeyset does.
func (o *ListOp[Req, Item]) WriteKeyset(w http.ResponseWriter, page Page, items []Item, total int, nextKey []string) {
	WriteListKeyset(w, page, items, total, nextKey)
}

// WriteProxy writes a page an upstream counted nothing for, as
// WriteProxyList does.
func (o *ListOp[Req, Item]) WriteProxy(w http.ResponseWriter, page Page, items []Item, nextCursor string) {
	WriteProxyList(w, page, items, nextCursor)
}

// WriteScannedProxy writes a page of a scanned proxy list, as
// WriteScannedProxyList does.
func (o *ListOp[Req, Item]) WriteScannedProxy(w http.ResponseWriter, page Page, items []Item, next string) {
	WriteScannedProxyList(w, page, items, next)
}

// NoContentOp is a route that answers 204 with no body.
type NoContentOp[Req any] struct {
	bodyOp[Req]
}

// NewNoContentOp declares a route that answers 204.
func NewNoContentOp[Req any](spec OpSpec) *NoContentOp[Req] {
	return &NoContentOp[Req]{bodyOp[Req]{newCore(spec, kindNoContent, reqType[Req](), 0)}}
}

func (o *NoContentOp[Req]) shape() opShape {
	return opShape{kind: kindNoContent, req: reqType[Req]()}
}

// Write writes the 204.
func (o *NoContentOp[Req]) Write(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// BytesOp is a route whose handler writes a body of one of the declared
// content types itself.
type BytesOp[Req any] struct {
	bodyOp[Req]
	contentTypes []string
}

// NewBytesOp declares a route that answers raw bytes of one of contentTypes.
// Status defaults to 200.
func NewBytesOp[Req any](spec OpSpec, contentTypes ...string) *BytesOp[Req] {
	return &BytesOp[Req]{
		bodyOp:       bodyOp[Req]{newCore(spec, kindBytes, reqType[Req](), len(contentTypes))},
		contentTypes: slices.Clone(contentTypes),
	}
}

func (o *BytesOp[Req]) shape() opShape {
	return opShape{kind: kindBytes, req: reqType[Req](), contentTypes: o.contentTypes}
}

// WebSocketOp is a websocket handshake. Its handler upgrades the connection
// itself.
type WebSocketOp struct {
	opCore
}

// NewWebSocketOp declares a websocket handshake, a GET that answers 101.
func NewWebSocketOp(spec OpSpec) *WebSocketOp {
	return &WebSocketOp{newCore(spec, kindWebSocket, nil, 0)}
}

func (o *WebSocketOp) shape() opShape { return opShape{kind: kindWebSocket} }

// newCore validates spec for an op of kind and resolves its default status.
// req is the op's Req, nil for NoBody, and contentTypes how many content
// types a bytes op declared.
func newCore(spec OpSpec, kind opKind, req reflect.Type, contentTypes int) opCore {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("httpx: op %q (%s): %s", spec.ID, spec.Pattern, fmt.Sprintf(format, args...)))
	}
	m := patternRE.FindStringSubmatch(spec.Pattern)
	if m == nil {
		fail("Pattern must be <GET|POST|PUT|PATCH|DELETE> /api/…")
	}
	method, path := m[1], m[2]
	if !opIDRE.MatchString(spec.ID) {
		fail("ID must be lower snake case, matching %s", opIDRE)
	}

	segments := pathParams(path)
	if len(segments) != len(spec.PathParams) {
		fail("PathParams must document exactly the path's parameters %v", segments)
	}
	for _, name := range segments {
		if _, ok := spec.PathParams[name]; !ok {
			fail("PathParams must document exactly the path's parameters %v", segments)
		}
	}

	query := make(map[string]bool, len(spec.Query))
	for _, q := range spec.Query {
		if query[q.Name] {
			fail("query parameter %q is declared twice", q.Name)
		}
		if q.Type == 0 {
			fail("query parameter %q has no Type", q.Name)
		}
		query[q.Name] = true
	}

	h := spec.Hints
	switch {
	case h.ReadOnly == HintUnset || h.OpenWorld == HintUnset:
		fail("Hints must set ReadOnly and OpenWorld")
	case h.ReadOnly == HintNo && (h.Destructive == HintUnset || h.Idempotent == HintUnset):
		fail("Hints must set Destructive and Idempotent on an op that is not read-only")
	case h.ReadOnly == HintYes && (h.Destructive != HintUnset || h.Idempotent != HintUnset):
		fail("Hints must leave Destructive and Idempotent unset on a read-only op")
	}

	if method == http.MethodGet && req != nil {
		fail("a GET reads no body, so its Req must be NoBody")
	}

	if len(spec.Form) > 0 {
		if method == http.MethodGet {
			fail("a GET reads no body, so it declares no Form")
		}
		if req != nil {
			fail("a Form body is read by the handler, so its Req must be NoBody")
		}
	}
	parts := map[string]bool{}
	for _, part := range spec.Form {
		if parts[part.Name] {
			fail("form part %q is declared twice", part.Name)
		}
		parts[part.Name] = true
	}

	if kind != kindJSON && len(spec.OtherStatuses) > 0 {
		fail("only a JSON op declares OtherStatuses")
	}
	switch kind {
	case kindJSON:
		if spec.Status == 0 {
			spec.Status = http.StatusOK
		}
		seen := map[int]bool{}
		for _, status := range append([]int{spec.Status}, spec.OtherStatuses...) {
			if !carriesBody(status) {
				fail("status %d is not a 2xx that carries a body; a 204 is a NoContent op", status)
			}
			if seen[status] {
				fail("status %d is declared twice", status)
			}
			seen[status] = true
		}
	case kindList:
		if spec.Status != 0 && spec.Status != http.StatusOK {
			fail("a list answers 200 only")
		}
		spec.Status = http.StatusOK
	case kindNoContent:
		if spec.Status != 0 && spec.Status != http.StatusNoContent {
			fail("a NoContent op answers 204 only")
		}
		spec.Status = http.StatusNoContent
	case kindBytes:
		if spec.Status == 0 {
			spec.Status = http.StatusOK
		}
		if !carriesBody(spec.Status) {
			fail("status %d is not a 2xx that carries a body", spec.Status)
		}
		if contentTypes == 0 {
			fail("a bytes op declares at least one content type")
		}
	case kindWebSocket:
		if method != http.MethodGet {
			fail("a websocket handshake is a GET")
		}
		if spec.Status != 0 {
			fail("a websocket op declares no Status; the handshake answers 101")
		}
	}
	return opCore{spec: spec, query: query}
}

// carriesBody reports whether status is a 2xx a response body may follow:
// 204 and 205 carry none.
func carriesBody(status int) bool {
	return status >= 200 && status <= 299 && status != http.StatusNoContent && status != http.StatusResetContent
}

// pathParams returns the names of path's {name} and {name...} segments, in
// order. {$} anchors the end of a pattern and names nothing.
func pathParams(path string) []string {
	var names []string
	for _, seg := range strings.Split(path, "/") {
		inner, ok := strings.CutPrefix(seg, "{")
		if !ok {
			continue
		}
		inner, ok = strings.CutSuffix(inner, "}")
		if !ok || inner == "$" {
			continue
		}
		names = append(names, strings.TrimSuffix(inner, "..."))
	}
	return names
}

// WindowQuery declares the since and until parameters ParseWindow reads, for
// a statistic node's OpSpec.
func WindowQuery() []QueryParam {
	return []QueryParam{
		{Name: "since", Type: ParamString, Doc: "Start of the window, inclusive, as an RFC 3339 timestamp or a YYYY-MM-DD date read as UTC midnight; with since and until both absent the window is the current UTC calendar month up to now, and with only one of them given the other end is open."},
		{Name: "until", Type: ParamString, Doc: "End of the window, exclusive, as an RFC 3339 timestamp or a YYYY-MM-DD date read as UTC midnight, which must be after since when both are given."},
	}
}
