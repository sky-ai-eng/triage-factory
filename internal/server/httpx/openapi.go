package httpx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// SpecEntry is one mounted operation, and whether it was mounted raw: with no
// session in front of it.
type SpecEntry struct {
	Op  Operation
	Raw bool
}

// OpenAPI renders the operations as an OpenAPI 3.1.0 document, in JSON with
// sorted keys, a two-space indent, and a trailing newline, so the same
// operations always render the same bytes. problems lists each field that has
// no doc tag and each type that encodes itself without a SchemaProvider; the
// document is rendered whether or not there are any.
func OpenAPI(entries []SpecEntry) (doc []byte, problems []string) {
	schemas := newSchemaSet()
	paths := map[string]map[string]any{}
	// rendered maps each OpenAPI method and path to the pattern that took
	// it: {name...} renders as {name} and {$} is dropped, so two mux
	// patterns can meet at one.
	rendered := map[string]string{}
	for _, e := range entries {
		spec, shape := e.Op.Spec(), e.Op.shape()
		method, path, _ := strings.Cut(spec.Pattern, " ")
		key := openAPIPath(path)
		if prev, taken := rendered[method+" "+key]; taken {
			schemas.problem("%s and %s both render as %s %s in the spec", prev, spec.Pattern, method, key)
			continue
		}
		rendered[method+" "+key] = spec.Pattern
		op := map[string]any{
			"operationId": spec.ID,
			"summary":     spec.Summary,
			"description": spec.Description,
			"responses":   responses(schemas, spec, shape),
			"x-hints":     hintsObject(spec.Hints),
		}
		var params []any
		for _, name := range pathParams(path) {
			params = append(params, map[string]any{
				"name": name, "in": "path", "required": true,
				"description": spec.PathParams[name],
				"schema":      map[string]any{"type": "string"},
			})
		}
		for _, q := range spec.Query {
			params = append(params, map[string]any{
				"name": q.Name, "in": "query", "required": false,
				"description": q.Doc,
				"schema":      paramSchema(q.Type),
			})
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		switch {
		case shape.req != nil:
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/json": map[string]any{"schema": schemas.schema(shape.req, dirRequest, "")},
				},
			}
		case len(spec.Form) > 0:
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"multipart/form-data": map[string]any{"schema": formSchema(spec.Form)},
				},
			}
		}
		if e.Raw {
			op["security"] = []any{}
		} else {
			op["security"] = []any{
				map[string]any{"session": []any{}},
				map[string]any{"token": []any{}},
			}
		}
		if paths[key] == nil {
			paths[key] = map[string]any{}
		}
		paths[key][strings.ToLower(method)] = op
	}

	errorSchema := schemas.schema(reflect.TypeFor[errorEnvelope](), dirResponse, "")
	components := map[string]any{
		"schemas": schemas.finish(),
		"responses": map[string]any{
			"Error": map[string]any{
				"description": "The request was refused or failed. The body lists every fault found.",
				"content": map[string]any{
					"application/json": map[string]any{"schema": errorSchema},
				},
			},
		},
		"securitySchemes": map[string]any{
			"session": map[string]any{
				"type": "apiKey", "in": "cookie", "name": "__Host-sid",
				"description": "The session cookie a browser sign-in sets. A deployment served over plain HTTP names it sid.",
			},
			"token": map[string]any{
				"type": "http", "scheme": "bearer",
				"description": "An API token, sent as Authorization: Bearer tf_….",
			},
		},
	}
	document := map[string]any{
		"openapi":    "3.1.0",
		"info":       map[string]any{"title": "Triage Factory API", "version": "unstable"},
		"paths":      paths,
		"components": components,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(document); err != nil {
		// The document is maps, slices, strings, and booleans.
		panic("httpx: encode OpenAPI document: " + err.Error())
	}
	return buf.Bytes(), schemas.problemList()
}

// responses renders an op's success responses and its default error.
func responses(schemas *schemaSet, spec OpSpec, shape opShape) map[string]any {
	out := map[string]any{"default": map[string]any{"$ref": "#/components/responses/Error"}}
	describe := func(status int) map[string]any {
		return map[string]any{"description": http.StatusText(status)}
	}
	jsonContent := func(schema map[string]any) map[string]any {
		return map[string]any{"application/json": map[string]any{"schema": schema}}
	}
	switch shape.kind {
	case kindJSON:
		for _, status := range append([]int{spec.Status}, spec.OtherStatuses...) {
			r := describe(status)
			r["content"] = jsonContent(schemas.schema(shape.resp, dirResponse, ""))
			out[strconv.Itoa(status)] = r
		}
	case kindList:
		r := describe(http.StatusOK)
		r["content"] = jsonContent(listEnvelope(schemas.schema(shape.resp, dirResponse, "")))
		out[strconv.Itoa(http.StatusOK)] = r
	case kindNoContent:
		out[strconv.Itoa(http.StatusNoContent)] = describe(http.StatusNoContent)
	case kindBytes:
		content := map[string]any{}
		for _, ct := range shape.contentTypes {
			content[ct] = map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}
		}
		r := describe(spec.Status)
		r["content"] = content
		out[strconv.Itoa(spec.Status)] = r
	case kindWebSocket:
		out[strconv.Itoa(http.StatusSwitchingProtocols)] = describe(http.StatusSwitchingProtocols)
	}
	return out
}

// formSchema is a multipart body's schema: one property per part.
func formSchema(parts []FormPart) map[string]any {
	props := map[string]any{}
	for _, part := range parts {
		schema := map[string]any{"type": "string"}
		if part.File {
			schema["format"] = "binary"
		}
		if part.Multiple {
			schema = map[string]any{"type": "array", "items": schema}
		}
		schema["description"] = part.Doc
		props[part.Name] = schema
	}
	return map[string]any{"type": "object", "properties": props}
}

// listEnvelope is the body listResponse writes around a page of items.
func listEnvelope(item map[string]any) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array", "items": item,
				"description": "The rows of this page, as an empty array when it has none.",
			},
			"next_page_token": map[string]any{
				"type":        "string",
				"description": "The page_token that reads the next page, absent on the last page.",
			},
			"total_count": map[string]any{
				"type":        []string{"integer", "null"},
				"description": "How many rows match the request's filters across every page, or null for a list whose upstream cannot count them.",
			},
		},
		"required": []string{"items", "total_count"},
	}
}

func paramSchema(t ParamType) map[string]any {
	switch t {
	case ParamInteger:
		return map[string]any{"type": "integer"}
	case ParamBoolean:
		return map[string]any{"type": "boolean"}
	case ParamDateTime:
		return map[string]any{"type": "string", "format": "date-time"}
	}
	return map[string]any{"type": "string"}
}

// hintsObject renders the set hints under MCP's names.
func hintsObject(h Hints) map[string]any {
	out := map[string]any{}
	for name, hint := range map[string]Hint{
		"readOnlyHint":    h.ReadOnly,
		"destructiveHint": h.Destructive,
		"idempotentHint":  h.Idempotent,
		"openWorldHint":   h.OpenWorld,
	} {
		if hint != HintUnset {
			out[name] = hint == HintYes
		}
	}
	return out
}

// openAPIPath is a mux path as OpenAPI spells it: a {name...} segment is
// {name}, and the {$} end anchor is dropped, leaving the trailing slash it
// follows.
func openAPIPath(path string) string {
	path = strings.TrimSuffix(path, "{$}")
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}") {
			segs[i] = strings.TrimSuffix(seg, "...}") + "}"
		}
	}
	return strings.Join(segs, "/")
}
