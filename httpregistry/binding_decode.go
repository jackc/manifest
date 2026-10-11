package httpregistry

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
)

// decode fills a fresh, addressable parameter struct using the compiled plan.
// Operation invocation and HTTP error responses are the caller's responsibility.
func (plan *bindingPlan) decode(w http.ResponseWriter, r *http.Request, params reflect.Value) error {
	if err := plan.decodeURLParameters(r, params); err != nil {
		return err
	}
	return plan.decodeBody(w, r, params)
}

func (plan *bindingPlan) decodeURLParameters(r *http.Request, params reflect.Value) error {
	var query url.Values
	if len(plan.queryNames) > 0 || plan.spec.RejectUnknownQuery {
		var err error
		query, err = url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return err
		}
		if plan.spec.RejectUnknownQuery {
			for name := range query {
				if !plan.queryNames[name] {
					return fmt.Errorf("unknown query parameter %q", name)
				}
			}
		}
	}
	// Preserve declaration order, including which malformed input fails first.
	for _, mapping := range plan.inputs {
		input := mapping.input
		destination := params.Field(mapping.parameterField)
		switch input.Source {
		case "path":
			if err := setValues(destination, []string{PathParam(r, input.Name)}); err != nil {
				return fmt.Errorf("path %s: %w", input.Name, err)
			}
		case "query":
			values, present := query[input.Name]
			if !present {
				if input.Required {
					return fmt.Errorf("missing query parameter %q", input.Name)
				}
				continue
			}
			if err := setValues(destination, values); err != nil {
				return fmt.Errorf("query %s: %w", input.Name, err)
			}
		}
	}
	return nil
}

func (plan *bindingPlan) decodeBody(w http.ResponseWriter, r *http.Request, params reflect.Value) error {
	if plan.spec.JSON == NoJSON {
		if plan.spec.RejectUnexpectedBody && r.Body != nil {
			data, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if len(data) > 0 {
				return fmt.Errorf("unexpected request body")
			}
			if err != nil && err != io.EOF {
				return err
			}
		}
		return nil
	}
	reader := r.Body
	if reader == nil {
		return io.EOF
	}
	if plan.spec.MaxBodyBytes > 0 {
		reader = http.MaxBytesReader(w, reader, plan.spec.MaxBodyBytes)
	}
	// Read the complete body so the byte limit also covers trailing whitespace.
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	// JSON v2 accepts null for a struct; the HTTP contract requires an object.
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	body := reflect.New(plan.bodyType)
	if err := json.Unmarshal(data, body.Interface(), json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	for bodyField, parameterField := range plan.bodyDestinations {
		params.Field(parameterField).Set(body.Elem().Field(bodyField))
	}
	return nil
}

// setValues handles presence and cardinality; setScalar handles conversion.
func setValues(destination reflect.Value, values []string) error {
	if destination.Kind() == reflect.Pointer {
		destination.Set(reflect.New(destination.Type().Elem()))
		return setValues(destination.Elem(), values)
	}
	if destination.Kind() == reflect.Slice {
		destination.Set(reflect.MakeSlice(destination.Type(), len(values), len(values)))
		for i, text := range values {
			if err := setScalar(destination.Index(i), text); err != nil {
				return err
			}
		}
		return nil
	}
	if len(values) != 1 {
		return fmt.Errorf("expected exactly one value")
	}
	return setScalar(destination, values[0])
}

func setScalar(destination reflect.Value, text string) error {
	switch destination.Kind() {
	case reflect.String:
		destination.SetString(text)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return err
		}
		destination.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(text, 10, destination.Type().Bits())
		if err != nil {
			return err
		}
		destination.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(text, 10, destination.Type().Bits())
		if err != nil {
			return err
		}
		destination.SetUint(parsed)
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(text, destination.Type().Bits())
		if err != nil {
			return err
		}
		destination.SetFloat(parsed)
	default:
		return fmt.Errorf("unsupported scalar")
	}
	return nil
}
