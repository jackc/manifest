package httpregistry

import (
	"reflect"
	"slices"
	"strings"
)

// FieldInputs declares every parameter field of P as an input from source
// ("query" or "body"), named as encoding/json names it: the tag's name, or
// the Go field name when the tag gives none. Fields promoted from embedded
// structs are included. Fields tagged "-" are returned as zero fields
// instead. Tag options such as omitempty are ignored. Options adjust the
// result: ZeroFields leaves named fields at zero, and FieldsFromPath takes
// the fields a route's placeholders name from its path.
//
// The result is ordinary declaration data: Bind validates it and the
// inventory lists every input. It suits applications whose operations take
// one flat parameter document from a single source.
func FieldInputs[P any](source string, options ...FieldOption) (inputs []Input, zero []string) {
	var adjust fieldOptions
	for _, option := range options {
		option(&adjust)
	}
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
	// Zero fields are removed before placeholders are matched, so a
	// placeholder cannot name one.
	for _, field := range adjust.zero {
		i := slices.IndexFunc(inputs, func(in Input) bool { return in.Field == field })
		if i < 0 {
			panic("httpregistry: FieldInputs: no input for parameter field " + field)
		}
		inputs = slices.Delete(inputs, i, i+1)
		zero = append(zero, field)
	}
	if adjust.path != nil {
		names, _, _ := pathShape(*adjust.path)
		for _, name := range names {
			i := slices.IndexFunc(inputs, func(in Input) bool { return in.Name == name })
			if i < 0 {
				panic("httpregistry: FieldInputs: no input for path placeholder " + name)
			}
			inputs[i].Source = "path"
		}
	}
	return inputs, zero
}

// FieldOption adjusts the inputs FieldInputs derives.
type FieldOption func(*fieldOptions)

type fieldOptions struct {
	zero []string
	path *string
}

// ZeroFields leaves the named parameter fields at zero: they are returned
// as zero fields instead of inputs, after any "-"-tagged fields and in the
// order named. Fields are named as Go names them, as in Binding.Zero. A
// route uses it to refuse, as unknown, a field its operation accepts from
// other callers. FieldInputs panics if a name is not one of its inputs.
func ZeroFields(fields ...string) FieldOption {
	fields = slices.Clone(fields)
	return func(o *fieldOptions) { o.zero = append(o.zero, fields...) }
}

// FieldsFromPath takes the fields named by the placeholders of a route path,
// {name} or a final {name...}, from the path instead of source. A field
// matches a placeholder by its input name. FieldInputs panics if the path is
// invalid, a placeholder names no input, or the option is given twice.
func FieldsFromPath(path string) FieldOption {
	return func(o *fieldOptions) {
		if o.path != nil {
			panic("httpregistry: FieldInputs: more than one path")
		}
		o.path = &path
	}
}
