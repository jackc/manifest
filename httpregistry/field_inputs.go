package httpregistry

import (
	"reflect"
	"strings"
)

// FieldInputs declares every parameter field of P as an input from source
// ("query" or "body"), named as encoding/json names it: the tag's name, or
// the Go field name when the tag gives none. Fields promoted from embedded
// structs are included. Fields tagged "-" are returned as zero fields
// instead. Tag options such as omitempty are ignored.
//
// The result is ordinary declaration data: Bind validates it and the
// inventory lists every input. It suits applications whose operations take
// one flat parameter document from a single source.
func FieldInputs[P any](source string) (inputs []Input, zero []string) {
	parameterType := reflect.TypeFor[P]()
	if parameterType.Kind() != reflect.Struct {
		panic("httpregistry: parameters must be a struct")
	}
	for _, field := range reflect.VisibleFields(parameterType) {
		tag, tagged := field.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous {
			// JSON names or omits a tagged embedded struct instead of promoting
			// its fields; Bind promotes, so the two would disagree.
			if tagged && name != "" {
				panic("httpregistry: FieldInputs: embedded field " + field.Name + " has a JSON tag")
			}
			continue
		}
		if !field.IsExported() {
			continue // Bind reports it as unmapped.
		}
		switch {
		case tag == "-":
			zero = append(zero, field.Name)
			continue
		case name == "":
			name = field.Name
		}
		inputs = append(inputs, Input{Source: source, Name: name, Field: field.Name})
	}
	return inputs, zero
}
