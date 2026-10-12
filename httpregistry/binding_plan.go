package httpregistry

import (
	"encoding"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// bindingPlan holds the immutable results of construction-time validation.
// It contains no operation or application dependencies and needs no type
// parameters: struct fields have already been resolved to reflection indices.
type bindingPlan struct {
	spec             Binding
	inputs           []inputMapping
	pathNames        []string
	queryNames       map[string]bool
	bodyType         reflect.Type
	bodyDestinations [][]int // body field index -> parameter field index path
}

// inputMapping keeps a declared input beside its resolved destination instead
// of relying on two parallel slices remaining in sync.
type inputMapping struct {
	input          Input
	parameterField []int // index path; longer than one for promoted fields
}

func compileBinding(parameterType reflect.Type, spec Binding) *bindingPlan {
	validateBindingOptions(spec)
	validateParameterType(parameterType)
	spec.Inputs = slices.Clone(spec.Inputs)
	spec.Zero = slices.Clone(spec.Zero)
	plan := &bindingPlan{
		spec:       spec,
		queryNames: map[string]bool{},
	}
	assignedFields := map[string]bool{}
	declaredSources := map[string]bool{}
	var bodyFields []reflect.StructField

	// Resolving a field also claims it, so no second source can overwrite it.
	// Fields promoted from embedded structs are named as Go promotes them.
	resolveField := func(name string) []int {
		field, ok := parameterType.FieldByName(name)
		if !ok || !field.IsExported() || field.Anonymous {
			panic("httpregistry: unsupported parameter field " + name)
		}
		if assignedFields[name] {
			panic("httpregistry: multiple sources for " + name)
		}
		assignedFields[name] = true
		return field.Index
	}
	for _, input := range spec.Inputs {
		if input.Name == "" || strings.ContainsAny(input.Name, "\"\\,\n\r\t") {
			panic("httpregistry: invalid input name")
		}
		parameterField := resolveField(input.Field)
		sourceKey := input.Source + ":" + input.Name
		if declaredSources[sourceKey] {
			panic("httpregistry: duplicate input " + sourceKey)
		}
		declaredSources[sourceKey] = true
		fieldType := parameterType.FieldByIndex(parameterField).Type
		switch input.Source {
		case "path", "query":
			if input.Source == "path" {
				plan.pathNames = append(plan.pathNames, input.Name)
				if input.Required {
					panic("httpregistry: path inputs are inherently required")
				}
			} else {
				plan.queryNames[input.Name] = true
			}
			validateURLField(input, fieldType)
		case "body":
			validateBodyInput(input, spec.JSON)
			// Decode only body-owned fields. A body-supplied path ID can therefore
			// never overwrite the value assigned by the path mapping.
			bodyFields = append(bodyFields, reflect.StructField{
				Name: fmt.Sprintf("F%d", len(bodyFields)),
				Type: fieldType,
				Tag:  reflect.StructTag("json:" + strconv.Quote(input.Name)),
			})
			plan.bodyDestinations = append(plan.bodyDestinations, parameterField)
		default:
			panic("httpregistry: unknown input source " + input.Source)
		}
		plan.inputs = append(plan.inputs, inputMapping{
			input:          input,
			parameterField: parameterField,
		})
	}
	for _, name := range spec.Zero {
		resolveField(name)
	}
	for _, field := range parameterFields(parameterType) {
		if !assignedFields[field.Name] {
			panic("httpregistry: unmapped parameter field " + field.Name)
		}
	}
	plan.bodyType = reflect.StructOf(bodyFields)
	return plan
}

func validateBindingOptions(spec Binding) {
	if spec.Status < 200 || spec.Status > 299 || spec.Status == http.StatusNoContent || spec.Status == http.StatusResetContent {
		panic("httpregistry: binding requires a JSON success status")
	}
	if spec.Errors.Name == "" || spec.Errors.Write == nil {
		panic("httpregistry: error writer is required")
	}
	if (spec.Encoder.Name == "") != (spec.Encoder.Encode == nil) {
		panic("httpregistry: encoder name and function are required together")
	}
	if spec.MaxBodyBytes < 0 {
		panic("httpregistry: negative body limit")
	}
	if spec.JSON != NoJSON && spec.JSON != StrictJSON {
		panic("httpregistry: explicit JSON policy is required")
	}
	if spec.JSON == NoJSON && spec.MaxBodyBytes != 0 {
		panic("httpregistry: body limits require JSON input")
	}
	if spec.JSON != NoJSON && spec.RejectUnexpectedBody {
		panic("httpregistry: unexpected-body policy only applies without JSON input")
	}
}

func validateParameterType(parameterType reflect.Type) {
	if parameterType.Kind() != reflect.Struct {
		panic("httpregistry: parameters must be a struct")
	}
	// Custom parameter unmarshalling would bypass the declared field mapping.
	if customJSON(parameterType) {
		panic("httpregistry: custom parameter unmarshalling is unsupported")
	}
	parameterFields(parameterType)
}

func customJSON(t reflect.Type) bool {
	pointer := reflect.PointerTo(t)
	return pointer.Implements(reflect.TypeFor[json.Unmarshaler]()) ||
		pointer.Implements(reflect.TypeFor[json.UnmarshalerFrom]())
}

// parameterFields lists the fields a binding must account for: top-level
// fields and those promoted from embedded structs, but not the embedded
// structs themselves. Only struct values may be embedded: a pointer would
// need allocating, and custom unmarshalling would bypass the mapping.
// Fields Go cannot promote (ambiguous or shadowed) are not listed; JSON
// ignores them too, so they stay at their zero values.
func parameterFields(parameterType reflect.Type) []reflect.StructField {
	var fields []reflect.StructField
	for _, field := range reflect.VisibleFields(parameterType) {
		if !field.Anonymous {
			fields = append(fields, field)
			continue
		}
		if field.Type.Kind() != reflect.Struct || customJSON(field.Type) {
			panic("httpregistry: unsupported embedded field " + field.Name)
		}
	}
	return fields
}

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

// decodesText reports whether a type parses itself from a path or query
// value, as uuid.UUID and time.Time do. Such types take precedence over
// conversion by kind.
func decodesText(t reflect.Type) bool { return reflect.PointerTo(t).Implements(textUnmarshalerType) }

func validateURLField(input Input, fieldType reflect.Type) {
	scalarType := fieldType
	if scalarType.Kind() == reflect.Pointer {
		scalarType = scalarType.Elem()
	}
	if !decodesText(scalarType) && scalarType.Kind() == reflect.Slice && input.Source == "query" {
		scalarType = scalarType.Elem()
	}
	if decodesText(scalarType) {
		return
	}
	switch scalarType.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
	default:
		panic("httpregistry: unsupported path/query field " + input.Field)
	}
}

func validateBodyInput(input Input, policy JSONPolicy) {
	// "-" means ignore this field to encoding/json; it cannot be a wire name.
	if input.Name == "-" {
		panic("httpregistry: invalid JSON field name")
	}
	for _, char := range input.Name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
			panic("httpregistry: unsupported JSON field name")
		}
	}
	if policy == NoJSON || input.Required {
		panic("httpregistry: body fields require JSON; requiredness belongs to operation validation")
	}
}

func (plan *bindingPlan) describe(actionName, actionDescription string) BindingDescription {
	return BindingDescription{
		Namespace:            plan.spec.Namespace,
		Action:               actionName,
		Description:          actionDescription,
		Inputs:               plan.spec.Inputs,
		Zero:                 plan.spec.Zero,
		JSON:                 plan.spec.JSON,
		MaxBodyBytes:         plan.spec.MaxBodyBytes,
		RejectUnknownQuery:   plan.spec.RejectUnknownQuery,
		RejectUnexpectedBody: plan.spec.RejectUnexpectedBody,
		Status:               plan.spec.Status,
		Response:             "json: complete operation result",
		Encoder:              plan.spec.Encoder.Name,
		Errors:               plan.spec.Errors.Name,
	}
}
