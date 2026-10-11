package httpregistry

import (
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
	bodyDestinations []int // body field index -> parameter field index
}

// inputMapping keeps a declared input beside its resolved destination instead
// of relying on two parallel slices remaining in sync.
type inputMapping struct {
	input          Input
	parameterField int
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
	resolveField := func(name string) int {
		field, ok := parameterType.FieldByName(name)
		if !ok || len(field.Index) != 1 || !field.IsExported() || field.Anonymous {
			panic("httpregistry: unsupported parameter field " + name)
		}
		if assignedFields[name] {
			panic("httpregistry: multiple sources for " + name)
		}
		assignedFields[name] = true
		return field.Index[0]
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
		fieldType := parameterType.Field(parameterField).Type
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
	for index := 0; index < parameterType.NumField(); index++ {
		name := parameterType.Field(index).Name
		if !assignedFields[name] {
			panic("httpregistry: unmapped parameter field " + name)
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
	parameterPointer := reflect.PointerTo(parameterType)
	if parameterPointer.Implements(reflect.TypeFor[json.Unmarshaler]()) ||
		parameterPointer.Implements(reflect.TypeFor[json.UnmarshalerFrom]()) {
		panic("httpregistry: custom parameter unmarshalling is unsupported")
	}
}

func validateURLField(input Input, fieldType reflect.Type) {
	scalarType := fieldType
	if scalarType.Kind() == reflect.Pointer {
		scalarType = scalarType.Elem()
	}
	if scalarType.Kind() == reflect.Slice && input.Source == "query" {
		scalarType = scalarType.Elem()
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
		Errors:               plan.spec.Errors.Name,
	}
}
