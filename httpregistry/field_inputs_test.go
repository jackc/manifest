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
