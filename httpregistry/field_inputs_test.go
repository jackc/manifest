package httpregistry_test

import (
	"context"
	"testing"

	"github.com/jackc/manifest/httpregistry"
	"github.com/stretchr/testify/require"
)

type sharedFields struct {
	Note string `json:"note,omitempty"`
}

type fieldInputParams struct {
	ID       string `json:"id"`
	Untagged int
	Internal string `json:"-"`
	Dash     string `json:"-,"`
	sharedFields
}

type taggedEmbedding struct {
	sharedFields `json:"shared"`
}

type omittedEmbedding struct {
	sharedFields `json:"-"`
}

func TestFieldInputs(t *testing.T) {
	inputs, zero := httpregistry.FieldInputs[fieldInputParams]("body")
	require.Equal(t, []httpregistry.Input{
		{Source: "body", Name: "id", Field: "ID"},
		{Source: "body", Name: "Untagged", Field: "Untagged"},
		{Source: "body", Name: "-", Field: "Dash"},
		{Source: "body", Name: "note", Field: "Note"},
	}, inputs)
	require.Equal(t, []string{"Internal"}, zero)

	require.Panics(t, func() { httpregistry.FieldInputs[taggedEmbedding]("body") })
	require.Panics(t, func() { httpregistry.FieldInputs[omittedEmbedding]("body") })
	require.Panics(t, func() { httpregistry.FieldInputs[string]("query") })
}

// The derived declarations bind a strict body end to end. Bind still
// validates them: "-" is not a usable body name.
func TestFieldInputsBind(t *testing.T) {
	type params struct {
		ID       string `json:"id"`
		Internal string `json:"-"`
		sharedFields
	}
	var got params
	op := operation[params, bool]{call: func(_ context.Context, _ string, p params) (bool, error) { got = p; return true, nil }}
	inputs, zero := httpregistry.FieldInputs[params]("body")
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), Inputs: inputs, Zero: zero})
	h := registryFor(ep, "/").Handler()
	require.Equal(t, 200, request(h, "POST", "/", `{"id":"a","note":"hi"}`).Code)
	require.Equal(t, params{ID: "a", sharedFields: sharedFields{Note: "hi"}}, got)
	require.Equal(t, 400, request(h, "POST", "/", `{"id":"a","Internal":"x"}`).Code)

	inputs, zero = httpregistry.FieldInputs[fieldInputParams]("body")
	require.Panics(t, func() {
		httpregistry.Bind(operation[fieldInputParams, bool]{}, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), Inputs: inputs, Zero: zero})
	})
}

type routeParams struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Revision int    `json:"revision"`
	Key      string `json:"idempotency_key"`
	Internal string `json:"-"`
}

func TestFieldInputsOptions(t *testing.T) {
	inputs, zero := httpregistry.FieldInputs[routeParams]("body",
		httpregistry.ZeroFields("Key", "Revision"),
		httpregistry.FieldsFromPath("/files/{id}/{path...}"))
	require.Equal(t, []httpregistry.Input{
		{Source: "path", Name: "id", Field: "ID"},
		{Source: "path", Name: "path", Field: "Path"},
	}, inputs)
	require.Equal(t, []string{"Internal", "Key", "Revision"}, zero)

	// Options apply in a fixed order, whatever order they are given in, and
	// ZeroFields accumulates.
	inputs2, zero2 := httpregistry.FieldInputs[routeParams]("body",
		httpregistry.FieldsFromPath("/files/{id}/{path...}"),
		httpregistry.ZeroFields("Key"), httpregistry.ZeroFields("Revision"))
	require.Equal(t, inputs, inputs2)
	require.Equal(t, zero, zero2)

	// A placeholder matches an input name, not a Go field name; a path with
	// none changes nothing.
	inputs, _ = httpregistry.FieldInputs[routeParams]("query", httpregistry.FieldsFromPath("/keys/{idempotency_key}"))
	require.Equal(t, httpregistry.Input{Source: "path", Name: "idempotency_key", Field: "Key"}, inputs[3])
	inputs, _ = httpregistry.FieldInputs[routeParams]("query", httpregistry.FieldsFromPath("/files"))
	require.Equal(t, "query", inputs[0].Source)

	for name, options := range map[string][]httpregistry.FieldOption{
		"unknown zero field":       {httpregistry.ZeroFields("Missing")},
		"json name as zero field":  {httpregistry.ZeroFields("revision")},
		"already zero":             {httpregistry.ZeroFields("Internal")},
		"repeated zero field":      {httpregistry.ZeroFields("Key", "Key")},
		"unknown placeholder":      {httpregistry.FieldsFromPath("/files/{missing}")},
		"Go name as placeholder":   {httpregistry.FieldsFromPath("/files/{ID}")},
		"placeholder of zero":      {httpregistry.ZeroFields("ID"), httpregistry.FieldsFromPath("/files/{id}")},
		"invalid path":             {httpregistry.FieldsFromPath("files/{id}")},
		"invalid placeholder":      {httpregistry.FieldsFromPath("/files/{path...}/x")},
		"repeated placeholder":     {httpregistry.FieldsFromPath("/files/{id}/{id}")},
		"more than one path given": {httpregistry.FieldsFromPath("/files/{id}"), httpregistry.FieldsFromPath("/files/{id}")},
	} {
		t.Run(name, func(t *testing.T) {
			require.Panics(t, func() { httpregistry.FieldInputs[routeParams]("body", options...) })
		})
	}
}

// Derived path and zero fields bind like declared ones: the route serves
// them, refuses zero fields as unknown, and the inventory lists them.
func TestFieldInputsOptionsBind(t *testing.T) {
	var got routeParams
	op := operation[routeParams, bool]{call: func(_ context.Context, _ string, p routeParams) (bool, error) { got = p; return true, nil }}
	path := "/files/{id}/{path...}"
	inputs, zero := httpregistry.FieldInputs[routeParams]("body", httpregistry.ZeroFields("Key"), httpregistry.FieldsFromPath(path))
	r := registryFor(httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), Inputs: inputs, Zero: zero}), path)
	h := r.Handler()
	require.Equal(t, 200, request(h, "POST", "/files/a/b/c.txt", `{"revision":2}`).Code)
	require.Equal(t, routeParams{ID: "a", Path: "b/c.txt", Revision: 2}, got)
	require.Equal(t, 400, request(h, "POST", "/files/a/b", `{"idempotency_key":"k"}`).Code)
	require.Equal(t, 400, request(h, "POST", "/files/a/b", `{"id":"b"}`).Code)

	binding := r.Inventory().Endpoints[0].Binding
	require.Equal(t, inputs, binding.Inputs)
	require.Equal(t, []string{"Internal", "Key"}, binding.Zero)
}
