// Package jsonfields finds the fields encoding/json reads from a JSON object into a Go struct.
// The schema package describes them and the strict decoder checks arguments against them, so a
// schema and its decoder agree on every name and on which fields are required.
package jsonfields

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"
)

// Field is one field encoding/json reads from a JSON object into a struct.
type Field struct {
	Name     string
	Tagged   bool  // the name came from a json tag
	Index    []int // the field's index path from the outer struct
	Field    reflect.StructField
	Type     reflect.Type // the field's type, with an unnamed pointer followed
	Optional bool         // ",omitempty" or ",omitzero"
	Quoted   bool         // ",string" on a string, number, or boolean field
	// ViaUnexportedPtr names the unexported struct type of an embedded pointer on the field's
	// path, if any: encoding/json cannot allocate it, so decoding the field fails.
	ViaUnexportedPtr string
}

// Required reports whether the field is required: it is not a pointer, and its json tag has
// neither ",omitempty" nor ",omitzero". This is the one definition the schema's "required" list
// and the strict decoder's missing-field check share.
func (f Field) Required() bool {
	return !f.Optional && f.Field.Type.Kind() != reflect.Pointer
}

// TagNameError reports a json tag name encoding/json does not accept (one with a quote, a
// backslash, or another reserved character). encoding/json v1 falls back to the Go name there
// and its v2 implementation reads the name differently, so the field has no one JSON name.
type TagNameError struct {
	Struct reflect.Type
	Field  string // the Go field name
	Name   string // the tag name
}

func (e *TagNameError) Error() string {
	return fmt.Sprintf("%s: field %s has the json tag name %q, which encoding/json does not accept as a name", e.Struct, e.Field, e.Name)
}

// Of returns the fields encoding/json reads for struct type t, in index order. It is a port of
// encoding/json's typeFields: a breadth-first walk over embedded structs, then Go's dominance
// rules (the shallowest name wins, a json tag breaks a tie at one depth, and any other tie removes
// the name). A json tag name encoding/json does not accept is a *TagNameError.
func Of(t reflect.Type) ([]Field, error) {
	type level struct {
		typ              reflect.Type
		index            []int
		viaUnexportedPtr string
	}
	var current []level
	next := []level{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	var fields []Field

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, f := range current {
			if visited[f.typ] {
				continue
			}
			visited[f.typ] = true
			for i := 0; i < f.typ.NumField(); i++ {
				sf := f.typ.Field(i)
				if sf.Anonymous {
					et := sf.Type
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue // an embedded unexported non-struct type has no fields to promote
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if name != "" && !validTagName(name) {
					return nil, &TagNameError{Struct: f.typ, Field: sf.Name, Name: name}
				}
				index := append(slices.Clip(f.index), i)
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				quoted := false
				if hasOption(opts, "string") {
					switch ft.Kind() {
					case reflect.Bool,
						reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
						reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
						reflect.Float32, reflect.Float64,
						reflect.String:
						quoted = true
					}
				}
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					field := Field{
						Name: cmp.Or(name, sf.Name), Tagged: name != "", Index: index, Field: sf, Type: ft,
						Optional: hasOption(opts, "omitempty") || hasOption(opts, "omitzero"),
						Quoted:   quoted, ViaUnexportedPtr: f.viaUnexportedPtr,
					}
					fields = append(fields, field)
					if count[f.typ] > 1 {
						// The embedding struct is reached more than once at this depth: record a
						// second copy so the dominance pass sees the tie and removes the name.
						fields = append(fields, field)
					}
					continue
				}
				// An embedded struct whose tag names nothing: promote its fields next round.
				nextCount[ft]++
				if nextCount[ft] == 1 {
					via := f.viaUnexportedPtr
					if via == "" && sf.Type.Kind() == reflect.Pointer && !sf.IsExported() {
						via = ft.String()
					}
					next = append(next, level{typ: ft, index: index, viaUnexportedPtr: via})
				}
			}
		}
	}

	slices.SortFunc(fields, func(a, b Field) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.Index), len(b.Index)); c != 0 {
			return c
		}
		if a.Tagged != b.Tagged {
			if a.Tagged {
				return -1
			}
			return +1
		}
		return slices.Compare(a.Index, b.Index)
	})
	out := fields[:0]
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].Name == fields[i].Name {
			j++
		}
		// The first field of a name dominates, unless the second ties it on depth and tagging.
		if j-i == 1 || len(fields[i].Index) != len(fields[i+1].Index) || fields[i].Tagged != fields[i+1].Tagged {
			out = append(out, fields[i])
		}
		i = j
	}
	slices.SortFunc(out, func(a, b Field) int { return slices.Compare(a.Index, b.Index) })
	return out, nil
}

// validTagName reports whether encoding/json accepts s as a field name in a json tag.
func validTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
			// Backslash and quote are reserved; other punctuation is allowed.
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// hasOption reports whether a json tag's comma-separated options include opt.
func hasOption(opts, opt string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == opt {
			return true
		}
	}
	return false
}
