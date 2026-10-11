package httpregistry

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"reflect"
)

// Operation is a structural interface: the library imports no application or
// action package. Implementations must enter their normal invocation pipeline.
// Name and Call come from the same handle, never a separate documentation list.
type Operation[D, P, R any] interface {
	Name() string
	Description() string
	Call(context.Context, D, P) (R, error)
}

type Input struct {
	Source   string `json:"source"` // path, query, or body
	Name     string `json:"name"`
	Field    string `json:"field"`             // exported, top-level Go parameter field
	Required bool   `json:"required,omitzero"` // query only; path is always present
}

type JSONPolicy string

const (
	NoJSON JSONPolicy = "none"
	// StrictJSON requires one object, exact top-level names, no unknown fields,
	// no duplicate keys (including nested objects), valid Unicode, and EOF
	// after the object. Decoding uses encoding/json/v2.
	StrictJSON JSONPolicy = "strict-object"
)

type ErrorWriter struct {
	Name  string
	Write func(http.ResponseWriter, *http.Request, error)
}

type Binding struct {
	Namespace            string // qualifies the action name when joining inventories
	Inputs               []Input
	Zero                 []string // parameter fields intentionally left at their zero values
	JSON                 JSONPolicy
	MaxBodyBytes         int64 // zero means no binding-level limit
	RejectUnknownQuery   bool
	RejectUnexpectedBody bool // for bindings without JSON input
	Status               int
	Errors               ErrorWriter
}

type BindingDescription struct {
	Namespace            string     `json:"namespace,omitempty"`
	Action               string     `json:"action"`
	Description          string     `json:"description"`
	Inputs               []Input    `json:"inputs"`
	Zero                 []string   `json:"zero_fields,omitempty"`
	JSON                 JSONPolicy `json:"json"`
	MaxBodyBytes         int64      `json:"max_body_bytes"`
	RejectUnknownQuery   bool       `json:"reject_unknown_query"`
	RejectUnexpectedBody bool       `json:"reject_unexpected_body"`
	Status               int        `json:"success_status"`
	Response             string     `json:"response"`
	Errors               string     `json:"error_policy"`
}

// InputError distinguishes transport decoding from operation errors. The
// application owns status codes and public messages for both.
type InputError struct{ Err error }

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }

// Bind compiles the request mapping once, then pairs its description with a
// typed invoke/respond handler. Cookie handling and result projections belong
// in custom handlers. Invalid plans panic during construction.
func Bind[D, P, R any](operation Operation[D, P, R], dependencies D, spec Binding) Endpoint {
	if operation == nil || operation.Name() == "" {
		panic("httpregistry: operation is required")
	}
	plan := compileBinding(reflect.TypeFor[P](), spec)
	description := plan.describe(operation.Name(), operation.Description())
	return Endpoint{
		handler:    operationHandler(operation, dependencies, plan),
		pathParams: plan.pathNames,
		description: Description{
			Kind:    "action",
			Binding: &description,
			Gaps: []string{
				"operation internals, custom field codecs, error writer behavior, and transitive action calls are not inspected",
			},
		},
	}
}

// operationHandler is the only request-time code that needs the operation's
// generic types. The decoder fills a fresh P, and the operation returns an R.
func operationHandler[D, P, R any](operation Operation[D, P, R], dependencies D, plan *bindingPlan) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var params P
		if err := plan.decode(w, r, reflect.ValueOf(&params).Elem()); err != nil {
			plan.spec.Errors.Write(w, r, &InputError{Err: err})
			return
		}
		result, err := operation.Call(r.Context(), dependencies, params)
		if err != nil {
			plan.spec.Errors.Write(w, r, err)
			return
		}
		data, err := json.Marshal(result)
		if err != nil {
			plan.spec.Errors.Write(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(plan.spec.Status)
		_, _ = w.Write(append(data, '\n'))
	})
}
