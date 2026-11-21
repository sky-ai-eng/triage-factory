package httpx

import (
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
)

// SchemaProvider is implemented by a type that encodes or decodes itself
// (MarshalJSON, UnmarshalJSON, MarshalText, UnmarshalText), whose wire shape
// its fields therefore do not describe. It returns the type's JSON Schema.
type SchemaProvider interface {
	OpenAPISchema() map[string]any
}

var (
	timeType            = reflect.TypeFor[time.Time]()
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
	schemaProviderType  = reflect.TypeFor[SchemaProvider]()
	patchFieldType      = reflect.TypeFor[patchField]()
	selfEncodingMethods = []struct {
		iface  reflect.Type
		method string
	}{
		{reflect.TypeFor[json.Marshaler](), "MarshalJSON"},
		{reflect.TypeFor[json.Unmarshaler](), "UnmarshalJSON"},
		{reflect.TypeFor[encoding.TextMarshaler](), "MarshalText"},
		{reflect.TypeFor[encoding.TextUnmarshaler](), "UnmarshalText"},
	}
)

// patchField is implemented by Patch[T] alone, and hands the schema builder
// the T it carries, which reflection cannot read off a generic type.
type patchField interface {
	patchElem() reflect.Type
}

func (Patch[T]) patchElem() reflect.Type { return reflect.TypeFor[T]() }

// direction is which side of a call a schema describes. Only a response
// schema lists required fields: what a response always carries is a fact,
// while which request fields a route requires is a rule the schema does not
// state yet.
type direction uint8

const (
	dirResponse direction = iota
	dirRequest
)

// schemaKey is one component: a named struct as one direction renders it.
type schemaKey struct {
	t   reflect.Type
	dir direction
}

// refSite is a $ref whose target name is assigned once every component is
// known, since a name depends on which other types share it.
type refSite struct {
	ref map[string]any
	key schemaKey
}

// schemaSet builds the schemas of a set of operations and the components
// they reference, and records each problem it finds on the way.
type schemaSet struct {
	order    []schemaKey
	bodies   map[schemaKey]map[string]any
	refs     []refSite
	problems map[string]bool
	// inline holds the structs being rendered in place on the current
	// path. Such a struct has no component to refer back to, so reaching it
	// again would never end.
	inline map[reflect.Type]bool
}

func newSchemaSet() *schemaSet {
	return &schemaSet{bodies: map[schemaKey]map[string]any{}, problems: map[string]bool{}, inline: map[reflect.Type]bool{}}
}

func (s *schemaSet) problem(format string, args ...any) {
	s.problems[fmt.Sprintf(format, args...)] = true
}

// schema renders t as encoding/json writes it. where names the place t
// appears, for the problems of an anonymous struct, which has no name of its
// own.
func (s *schemaSet) schema(t reflect.Type, dir direction, where string) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawMessageType:
		return map[string]any{}
	case t.Kind() == reflect.Pointer:
		// Before the method checks: a pointer has its element's methods, and
		// the element is the type that declares them.
		return nullable(s.schema(t.Elem(), dir, where))
	case t.Kind() == reflect.Interface:
		return map[string]any{}
	case t.Implements(patchFieldType):
		elem := reflect.Zero(t).Interface().(patchField).patchElem()
		return nullable(s.schema(elem, dir, where))
	}
	if provided, ok := providedSchema(t); ok {
		return provided
	}
	if method := selfEncoding(t); method != "" {
		s.problem("%s encodes itself through %s, so it must implement httpx.SchemaProvider", typeLabel(t, where), method)
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int64, reflect.Uint64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uintptr:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && selfEncoding(t.Elem()) == "" {
			return map[string]any{"type": []string{"string", "null"}, "format": "byte"}
		}
		return map[string]any{"type": []string{"array", "null"}, "items": s.schema(t.Elem(), dir, where)}
	case reflect.Array:
		return map[string]any{"type": "array", "items": s.schema(t.Elem(), dir, where)}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": s.schema(t.Elem(), dir, where)}
	case reflect.Struct:
		if t.Name() == "" || strings.Contains(t.Name(), "[") {
			if s.inline[t] {
				s.problem("%s refers to itself, which a generic struct cannot do here: it renders in place, with no component to refer to", typeLabel(t, where))
				return map[string]any{}
			}
			s.inline[t] = true
			defer delete(s.inline, t)
			return s.object(t, dir, where)
		}
		key := schemaKey{t, dir}
		// The key is present from the start, so nullable sees a $ref, and
		// finish sets its target.
		ref := map[string]any{"$ref": ""}
		s.refs = append(s.refs, refSite{ref: ref, key: key})
		if _, done := s.bodies[key]; !done {
			// Recorded before the body is built, so a type that refers to
			// itself ends at its own $ref.
			s.bodies[key] = nil
			s.order = append(s.order, key)
			s.bodies[key] = s.object(t, dir, where)
		}
		return ref
	}
	s.problem("%s is a %s, which encoding/json cannot encode", typeLabel(t, where), t.Kind())
	return map[string]any{}
}

// object renders a struct's fields as an object schema.
func (s *schemaSet) object(t reflect.Type, dir direction, where string) map[string]any {
	label := typeLabel(t, where)
	props := map[string]any{}
	var required []string
	for _, f := range jsonFields(t) {
		owner := label
		if f.owner != t {
			owner = typeLabel(f.owner, where)
		}
		if f.hidden {
			if dir == dirResponse {
				s.problem("%s.%s (json %q) is tagged spec:\"-\", which a response field may not be: encoding/json still writes it", owner, f.goName, f.name)
			}
			continue
		}
		if f.doc == "" {
			s.problem("%s.%s (json %q) has no doc tag", owner, f.goName, f.name)
		}
		var fs map[string]any
		if f.quoted {
			fs = quotedSchema(f.typ)
		}
		if fs == nil {
			fs = s.schema(f.typ, dir, owner+"."+f.goName)
		}
		// Every schema this builder returns is a fresh map, so the
		// description can be set on it in place. A $ref map in particular
		// must not be copied: its target is filled in by finish.
		if f.doc != "" {
			fs["description"] = f.doc
		}
		props[f.name] = fs
		if dir == dirResponse && !f.omit && !f.viaPointer {
			required = append(required, f.name)
		}
	}
	out := map[string]any{"type": "object"}
	if len(props) > 0 {
		out["properties"] = props
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// quotedSchema is the schema of a field with the ,string option, which
// encoding/json writes as a string when the field is a number or a boolean.
// It is nil for any other field, which the option leaves as it is.
func quotedSchema(t reflect.Type) map[string]any {
	pointer := t.Kind() == reflect.Pointer
	if pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
	default:
		return nil
	}
	if pointer {
		return map[string]any{"type": []string{"string", "null"}}
	}
	return map[string]any{"type": "string"}
}

// providedSchema calls t's SchemaProvider, on a value or a pointer receiver.
// The map is cloned, since a field's description is set on it.
func providedSchema(t reflect.Type) (map[string]any, bool) {
	switch {
	case t.Implements(schemaProviderType):
		return maps.Clone(reflect.Zero(t).Interface().(SchemaProvider).OpenAPISchema()), true
	case reflect.PointerTo(t).Implements(schemaProviderType):
		return maps.Clone(reflect.New(t).Interface().(SchemaProvider).OpenAPISchema()), true
	}
	return nil, false
}

// selfEncoding names the first of the four encoding methods t, or a pointer
// to it, implements, or is "" for none.
func selfEncoding(t reflect.Type) string {
	for _, m := range selfEncodingMethods {
		if t.Implements(m.iface) || reflect.PointerTo(t).Implements(m.iface) {
			return m.method
		}
	}
	return ""
}

// nullable widens s to admit null, as encoding/json writes a nil pointer,
// slice, or map. A schema with no type already admits it.
func nullable(s map[string]any) map[string]any {
	if len(s) == 0 {
		return s
	}
	if _, ref := s["$ref"]; !ref {
		switch typ := s["type"].(type) {
		case string:
			out := maps.Clone(s)
			out["type"] = []string{typ, "null"}
			return out
		case []string:
			if slices.Contains(typ, "null") {
				return s
			}
			out := maps.Clone(s)
			out["type"] = append(slices.Clone(typ), "null")
			return out
		}
	}
	return map[string]any{"oneOf": []any{s, map[string]any{"type": "null"}}}
}

// typeLabel names t in a problem: its package path and name, or where it
// appears when it is anonymous.
func typeLabel(t reflect.Type, where string) string {
	if t.Name() == "" {
		if where == "" {
			return t.String()
		}
		return where
	}
	return t.PkgPath() + "." + t.Name()
}

// finish names every component and fills in the $refs that point at it. A
// component is named for its type, first letter upper-cased. Two types that
// share a name are each prefixed with their package's last path element in
// PascalCase. A type a request and a response both reach renders twice, and
// the request's copy is suffixed Input.
func (s *schemaSet) finish() map[string]any {
	byBase := map[string][]reflect.Type{}
	dirs := map[reflect.Type]int{}
	for _, key := range s.order {
		base := upperFirst(key.t.Name())
		if !slices.Contains(byBase[base], key.t) {
			byBase[base] = append(byBase[base], key.t)
		}
		dirs[key.t]++
	}
	names := map[schemaKey]string{}
	owners := map[string]schemaKey{}
	components := map[string]any{}
	for _, key := range s.order {
		name := upperFirst(key.t.Name())
		if len(byBase[name]) > 1 {
			name = pascalCase(key.t.PkgPath()[strings.LastIndex(key.t.PkgPath(), "/")+1:]) + name
		}
		if key.dir == dirRequest && dirs[key.t] > 1 {
			name += "Input"
		}
		if prev, taken := owners[name]; taken {
			s.problem("%s and %s both render as the component %s", typeLabel(prev.t, ""), typeLabel(key.t, ""), name)
			continue
		}
		owners[name] = key
		names[key] = name
		components[name] = s.bodies[key]
	}
	for _, site := range s.refs {
		site.ref["$ref"] = "#/components/schemas/" + names[site.key]
	}
	return components
}

func (s *schemaSet) problemList() []string {
	return slices.Sorted(maps.Keys(s.problems))
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// pascalCase joins the alphanumeric runs of s, each with its first letter
// upper-cased: "multi_mode" is "MultiMode".
func pascalCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, p := range parts {
		parts[i] = upperFirst(p)
	}
	return strings.Join(parts, "")
}

// jsonField is one field of a struct as encoding/json encodes it.
type jsonField struct {
	name, goName, doc string
	typ               reflect.Type
	// owner is the struct that declares the field, which is the embedded
	// struct for a promoted one.
	owner reflect.Type
	// tagged is whether the json tag names the field; it settles a tie
	// between two fields of one name at one depth.
	tagged bool
	// omit is whether the field has omitempty or omitzero.
	omit bool
	// quoted is whether the field has the ,string option.
	quoted bool
	// viaPointer is whether the field is promoted through an embedded
	// pointer, which encoding/json leaves out entirely when it is nil.
	viaPointer bool
	// hidden is whether the field, or the embedded struct it is promoted
	// from, is tagged spec:"-". It still takes part in choosing which field
	// a name goes to, as encoding/json sees it.
	hidden bool
	index  []int
}

// jsonFields returns the fields encoding/json encodes for struct t, in its
// order: t's own fields and those promoted from embedded structs, with a
// name two fields share going to the shallower, then to the tagged one, and
// to neither when that leaves a tie.
func jsonFields(t reflect.Type) []jsonField {
	type level struct {
		typ        reflect.Type
		index      []int
		viaPointer bool
		hidden     bool
	}
	current := []level{}
	next := []level{{typ: t}}
	var count map[reflect.Type]int
	nextCount := map[reflect.Type]int{t: 1}
	visited := map[reflect.Type]bool{}
	var fields []jsonField
	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, lv := range current {
			if visited[lv.typ] {
				continue
			}
			visited[lv.typ] = true
			for i := range lv.typ.NumField() {
				sf := lv.typ.Field(i)
				ft := sf.Type
				if sf.Anonymous {
					if ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if !sf.IsExported() && ft.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				index := append(slices.Clone(lv.index), i)
				viaPointer := lv.viaPointer
				// spec:"-" marks a field the body decodes only so its route
				// can refuse it by name; it is no part of what a caller
				// sends.
				hidden := lv.hidden || sf.Tag.Get("spec") == "-"
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					optList := strings.Split(opts, ",")
					f := jsonField{
						name:       name,
						goName:     sf.Name,
						doc:        sf.Tag.Get("doc"),
						typ:        sf.Type,
						owner:      lv.typ,
						tagged:     name != "",
						omit:       slices.Contains(optList, "omitempty") || slices.Contains(optList, "omitzero"),
						quoted:     slices.Contains(optList, "string"),
						viaPointer: viaPointer,
						hidden:     hidden,
						index:      index,
					}
					if f.name == "" {
						f.name = sf.Name
					}
					fields = append(fields, f)
					if count[lv.typ] > 1 {
						// The struct is embedded twice at this depth, so its
						// fields tie with themselves and encoding/json drops
						// them; the duplicate makes the tie.
						fields = append(fields, f)
					}
					continue
				}
				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, level{typ: ft, index: index, viaPointer: viaPointer || sf.Type.Kind() == reflect.Pointer, hidden: hidden})
				}
			}
		}
	}

	slices.SortStableFunc(fields, func(a, b jsonField) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := len(a.index) - len(b.index); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	var out []jsonField
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		group := fields[i:j]
		if len(group) == 1 || len(group[0].index) != len(group[1].index) || group[0].tagged != group[1].tagged {
			out = append(out, group[0])
		}
		i = j
	}
	slices.SortFunc(out, func(a, b jsonField) int { return slices.Compare(a.index, b.index) })
	return out
}
