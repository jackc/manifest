package httpregistry_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/manifest/httpregistry"
	"github.com/stretchr/testify/require"
)

type bodyParams struct {
	Name   string         `json:"name"`
	Fields map[string]any `json:"fields"`
}

// A custom handler decoding with DecodeJSONBody answers every body as a
// binding with the same limit does.
func TestDecodeJSONBodyMatchesBinding(t *testing.T) {
	const limit = 64
	op := operation[bodyParams, bool]{call: func(context.Context, string, bodyParams) (bool, error) { return true, nil }}
	inputs, zero := httpregistry.FieldInputs[bodyParams]("body")
	bound := registryFor(httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, MaxBodyBytes: limit, Status: 200, Errors: errorWriter(), Inputs: inputs, Zero: zero}), "/").Handler()
	custom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p bodyParams
		if err := httpregistry.DecodeJSONBody(w, r, &p, limit); err != nil {
			errorWriter().Write(w, r, err)
			return
		}
		w.WriteHeader(200)
	})
	for _, body := range []string{
		`{"name":"a","fields":{"x":1}}`, ` {} `, `{"name":null}`,
		`{"other":1}`, `{"Name":"a"}`, `{"name":"a","name":"b"}`, `{"fields":{"x":1,"x":2}}`,
		"{\"name\":\"\xff\"}", `{} {}`, `null`, `[]`, ``, `{"name":3}`,
		`{"name":"` + strings.Repeat("a", limit) + `"}`, `{}` + strings.Repeat(" ", limit),
	} {
		t.Run(body, func(t *testing.T) {
			want := request(bound, "POST", "/", body).Code
			require.Equal(t, want, request(custom, "POST", "/", body).Code)
		})
	}
	require.Equal(t, 413, request(custom, "POST", "/", `{}`+strings.Repeat(" ", limit)).Code)
}

func TestDecodeJSONBody(t *testing.T) {
	decode := func(body string, maxBytes int64, options ...json.Options) (bodyParams, error) {
		var p bodyParams
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		err := httpregistry.DecodeJSONBody(httptest.NewRecorder(), r, &p, maxBytes, options...)
		return p, err
	}
	p, err := decode(`{"name":"a"}`, 0)
	require.NoError(t, err)
	require.Equal(t, bodyParams{Name: "a"}, p)

	// Zero means no limit.
	_, err = decode(`{"name":"`+strings.Repeat("a", 1<<20)+`"}`, 0)
	require.NoError(t, err)

	_, err = decode(`{"name":"`+strings.Repeat("a", 16)+`"}`, 16)
	var input *httpregistry.InputError
	require.ErrorAs(t, err, &input)
	var size *http.MaxBytesError
	require.ErrorAs(t, err, &size)

	_, err = decode(`{"other":1}`, 0)
	require.ErrorAs(t, err, &input)

	// Options follow the strict defaults, so they can relax them; the body
	// must still be an object.
	p, err = decode(`{"name":"a","other":1}`, 0, json.RejectUnknownMembers(false))
	require.NoError(t, err)
	require.Equal(t, "a", p.Name)
	_, err = decode(`null`, 0, json.RejectUnknownMembers(false))
	require.ErrorAs(t, err, &input)

	// A request without a body, as a client-side request may have, fails.
	r := httptest.NewRequest("POST", "/", nil)
	r.Body = nil
	require.ErrorAs(t, httpregistry.DecodeJSONBody(httptest.NewRecorder(), r, &p, 0), &input)

	require.Panics(t, func() { _, _ = decode(`{}`, -1) })
}

func TestRejectBody(t *testing.T) {
	reject := func(body string) error {
		return httpregistry.RejectBody(httptest.NewRequest("POST", "/", strings.NewReader(body)))
	}
	require.NoError(t, reject(""))
	var input *httpregistry.InputError
	require.ErrorAs(t, reject(" "), &input)
	require.ErrorAs(t, reject("{}"), &input)

	r := httptest.NewRequest("GET", "/", nil)
	r.Body = nil
	require.NoError(t, httpregistry.RejectBody(r))

	failing := httptest.NewRequest("POST", "/", errReader{})
	require.ErrorAs(t, httpregistry.RejectBody(failing), &input)
	require.ErrorIs(t, httpregistry.RejectBody(failing), errRead)
}

var errRead = errors.New("read failed")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errRead }
