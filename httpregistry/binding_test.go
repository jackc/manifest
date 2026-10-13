package httpregistry_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/manifest/httpregistry"
	"github.com/stretchr/testify/require"
)

type operation[P, R any] struct {
	call func(context.Context, string, P) (R, error)
}

func (operation[P, R]) Name() string        { return "test-operation" }
func (operation[P, R]) Description() string { return "An operation for testing." }
func (o operation[P, R]) Call(ctx context.Context, d string, p P) (R, error) {
	return o.call(ctx, d, p)
}
func errorWriter() httpregistry.ErrorWriter {
	return httpregistry.ErrorWriter{Name: "test-errors", Write: func(w http.ResponseWriter, r *http.Request, err error) {
		var input *httpregistry.InputError
		if errors.As(err, &input) {
			var size *http.MaxBytesError
			if errors.As(err, &size) {
				http.Error(w, "large", 413)
			} else {
				http.Error(w, "input", 400)
			}
			return
		}
		http.Error(w, "operation", 422)
	}}
}
func registryFor(ep httpregistry.Endpoint, path string) *httpregistry.Registry[metadata] {
	r := httpregistry.New(compiler)
	r.Handle(httpregistry.Spec[metadata]{Method: "POST", Path: path, Metadata: metadata{Public: true}}, ep)
	return r
}
func TestBindingMapsTypedParameters(t *testing.T) {
	type params struct {
		ID     string
		Limit  *int
		Tags   []string
		Note   string
		Fields map[string]any
	}
	var got params
	calls := 0
	op := operation[params, params]{call: func(ctx context.Context, d string, p params) (params, error) {
		require.Equal(t, "dependencies", d)
		calls++
		got = p
		return p, nil
	}}
	ep := httpregistry.Bind(op, "dependencies", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 201, MaxBodyBytes: 4096, RejectUnknownQuery: true, Errors: errorWriter(), Inputs: []httpregistry.Input{
		{Source: "path", Name: "id", Field: "ID"}, {Source: "query", Name: "limit", Field: "Limit"}, {Source: "query", Name: "tag", Field: "Tags"}, {Source: "body", Name: "note", Field: "Note"}, {Source: "body", Name: "fields", Field: "Fields"},
	}})
	r := registryFor(ep, "/items/{id}")
	w := request(r.Handler(), "POST", "/items/abc?limit=0&tag=a&tag=b", `{"note":"hello","fields":{"x":1}}`)
	require.Equal(t, 201, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Equal(t, 1, calls)
	require.Equal(t, "abc", got.ID)
	require.NotNil(t, got.Limit)
	require.Zero(t, *got.Limit)
	require.Equal(t, []string{"a", "b"}, got.Tags)
	require.Equal(t, float64(1), got.Fields["x"])
	require.Equal(t, "hello", got.Note)
	w = request(r.Handler(), "POST", "/items/abc", `{}`)
	require.Equal(t, 201, w.Code)
	require.Nil(t, got.Limit)
	inv := r.Inventory()
	require.Equal(t, "test-operation", inv.Endpoints[0].Binding.Action)
	require.Len(t, inv.Endpoints[0].Binding.Inputs, 5)
	inv.Endpoints[0].Binding.Inputs[0].Field = "other"
	require.Equal(t, "ID", r.Inventory().Endpoints[0].Binding.Inputs[0].Field)
}
func TestStrictBindingRejectsAmbiguityBeforeInvocation(t *testing.T) {
	type params struct {
		ID     string
		Limit  int
		Fields map[string]any
	}
	calls := 0
	op := operation[params, bool]{call: func(context.Context, string, params) (bool, error) { calls++; return true, nil }}
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, MaxBodyBytes: 128, RejectUnknownQuery: true, Errors: errorWriter(), Inputs: []httpregistry.Input{
		{Source: "path", Name: "id", Field: "ID"}, {Source: "query", Name: "limit", Field: "Limit"}, {Source: "body", Name: "fields", Field: "Fields"},
	}})
	h := registryFor(ep, "/items/{id}").Handler()
	for _, tc := range []struct {
		name, query, body string
		status            int
	}{
		{"body-path-conflict", "", `{"id":"abc"}`, 400},
		{"unknown-field", "", `{"other":1}`, 400},
		{"case-alias", "", `{"Fields":{}}`, 400},
		{"duplicate-key", "", `{"fields":{},"fields":{}}`, 400},
		{"nested-duplicate", "", `{"fields":{"x":1,"x":2}}`, 400},
		{"array-nested-duplicate", "", `{"fields":{"x":[{"a":1,"a":2}]}}`, 400},
		{"escaped-duplicate", "", `{"fields":{"x":1,"\u0078":2}}`, 400},
		{"invalid-utf8", "", "{\"fields\":{\"x\":\"\xff\"}}", 400},
		{"invalid-surrogate", "", `{"fields":{"x":"\ud800"}}`, 400},
		{"trailing", "", `{} {}`, 400}, {"null", "", `null`, 400}, {"array", "", `[]`, 400}, {"empty", "", "", 400},
		{"invalid-type", "", `{"fields":3}`, 400}, {"too-large", "", `{"fields":{"x":"` + strings.Repeat("a", 128) + `"}}`, 413},
		{"repeated-scalar", "?limit=1&limit=1", `{}`, 400}, {"invalid-scalar", "?limit=a", `{}`, 400}, {"unknown-query", "?id=abc", `{}`, 400},
		{"malformed-query", "?limit=%xx", `{}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.status, request(h, "POST", "/items/abc"+tc.query, tc.body).Code)
		})
	}
	require.Zero(t, calls)
}
func TestStrictBindingAllowsOmittedAndNullFields(t *testing.T) {
	type params struct {
		ID     string
		Note   string
		Fields map[string]any
	}
	op := operation[params, params]{call: func(_ context.Context, _ string, p params) (params, error) { return p, nil }}
	h := registryFor(httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 201, Errors: errorWriter(), Inputs: []httpregistry.Input{{Source: "path", Name: "id", Field: "ID"}, {Source: "body", Name: "note", Field: "Note"}, {Source: "body", Name: "fields", Field: "Fields"}}}), "/{id}").Handler()
	for _, body := range []string{`{}`, `{"note":null,"fields":null}`, " \n\t{} \r\n"} {
		w := request(h, "POST", "/trusted?ignored=yes", body)
		require.Equal(t, 201, w.Code)
		var p params
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
		require.Equal(t, "trusted", p.ID)
		require.Empty(t, p.Note)
		require.Empty(t, p.Fields)
	}
}
func TestBindingErrorsAndAuthBeforeDecode(t *testing.T) {
	type params struct{ Note string }
	calls := 0
	op := operation[params, bool]{call: func(context.Context, string, params) (bool, error) { calls++; return false, fmt.Errorf("domain error") }}
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), Inputs: []httpregistry.Input{{Source: "body", Name: "note", Field: "Note"}}})
	r := httpregistry.New(compiler)
	r.Handle(httpregistry.Spec[metadata]{Method: "POST", Path: "/"}, ep)
	h := r.Handler()
	require.Equal(t, 401, request(h, "POST", "/", `not json`).Code)
	require.Zero(t, calls)
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"note":"hi"}`))
	req.Header.Set("Authorization", "test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 422, w.Code)
	require.Equal(t, 1, calls)
}
func TestInvalidBindings(t *testing.T) {
	type params struct{ ID string }
	op := operation[params, bool]{call: func(context.Context, string, params) (bool, error) { return true, nil }}
	base := httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 200, Errors: errorWriter(), Inputs: []httpregistry.Input{{Source: "path", Name: "id", Field: "ID"}}}
	for _, tc := range []struct {
		name string
		edit func(*httpregistry.Binding)
	}{
		{"overlap", func(b *httpregistry.Binding) {
			b.Inputs = append(b.Inputs, httpregistry.Input{Source: "query", Name: "id", Field: "ID"})
		}},
		{"missing-field", func(b *httpregistry.Binding) { b.Inputs = nil }},
		{"unknown-field", func(b *httpregistry.Binding) {
			b.Inputs = []httpregistry.Input{{Source: "path", Name: "id", Field: "Other"}}
		}},
		{"no-policy", func(b *httpregistry.Binding) { b.JSON = "" }},
		{"unsupported-policy", func(b *httpregistry.Binding) { b.JSON = "legacy-single-decode" }},
		{"no-errors", func(b *httpregistry.Binding) { b.Errors = httpregistry.ErrorWriter{} }},
		{"negative-limit", func(b *httpregistry.Binding) { b.MaxBodyBytes = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) { b := base; tc.edit(&b); require.Panics(t, func() { httpregistry.Bind(op, "", b) }) })
	}
	ep := httpregistry.Bind(op, "", base)
	require.Panics(t, func() { registryFor(ep, "/{other}") })
	require.Panics(t, func() { registryFor(ep, "/literal") })
}

func TestBindingOptionalInputsAndBodyPolicy(t *testing.T) {
	type params struct {
		Count  int
		Unused string
	}
	got := params{}
	op := operation[params, bool]{call: func(_ context.Context, _ string, p params) (bool, error) { got = p; return true, nil }}
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 200, Errors: errorWriter(), RejectUnexpectedBody: true, Zero: []string{"Unused"}, Inputs: []httpregistry.Input{{Source: "query", Name: "count", Field: "Count", Required: true}}})
	h := registryFor(ep, "/").Handler()
	require.Equal(t, 400, request(h, "POST", "/", "").Code)
	require.Equal(t, 400, request(h, "POST", "/?count=3", "body").Code)
	require.Equal(t, 200, request(h, "POST", "/?count=3", "").Code)
	require.Equal(t, params{Count: 3}, got)
}

func TestStrictBodyLimitIncludesTrailingWhitespace(t *testing.T) {
	op := operation[struct{}, bool]{call: func(context.Context, string, struct{}) (bool, error) {
		t.Fatal("operation must not run")
		return false, nil
	}}
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), MaxBodyBytes: 16})
	require.Equal(t, 413, request(registryFor(ep, "/").Handler(), "POST", "/", "{}"+strings.Repeat(" ", 32)).Code)
}

type customJSONParams struct{}

func (*customJSONParams) UnmarshalJSON([]byte) error { return nil }

type customJSONFromParams struct{}

func (*customJSONFromParams) UnmarshalJSONFrom(*jsontext.Decoder) error { return nil }

func TestBindingRejectsCustomParameterUnmarshalling(t *testing.T) {
	spec := httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter()}
	t.Run("UnmarshalJSON", func(t *testing.T) {
		require.PanicsWithValue(t, "httpregistry: custom parameter unmarshalling is unsupported", func() {
			httpregistry.Bind(operation[customJSONParams, bool]{}, "", spec)
		})
	})
	t.Run("UnmarshalJSONFrom", func(t *testing.T) {
		require.PanicsWithValue(t, "httpregistry: custom parameter unmarshalling is unsupported", func() {
			httpregistry.Bind(operation[customJSONFromParams, bool]{}, "", spec)
		})
	})
}

// hexID is an array type that decodes itself from text, like uuid.UUID.
type hexID [2]byte

func (h *hexID) UnmarshalText(text []byte) error {
	if len(text) != 4 {
		return errors.New("want four hex digits")
	}
	v, err := strconv.ParseUint(string(text), 16, 16)
	if err != nil {
		return err
	}
	h[0], h[1] = byte(v>>8), byte(v)
	return nil
}

// shouted is a string kind whose own decoding wins over conversion by kind.
type shouted string

func (s *shouted) UnmarshalText(text []byte) error {
	*s = shouted(strings.ToUpper(string(text)))
	return nil
}

func TestBindingDecodesTextValues(t *testing.T) {
	type params struct {
		ID     hexID
		Others []hexID
		Maybe  *hexID
		Name   shouted
	}
	var got params
	op := operation[params, bool]{call: func(_ context.Context, _ string, p params) (bool, error) { got = p; return true, nil }}
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 200, Errors: errorWriter(), Inputs: []httpregistry.Input{
		{Source: "path", Name: "id", Field: "ID"}, {Source: "query", Name: "other", Field: "Others"},
		{Source: "query", Name: "maybe", Field: "Maybe"}, {Source: "query", Name: "name", Field: "Name"},
	}})
	h := registryFor(ep, "/items/{id}").Handler()
	require.Equal(t, 200, request(h, "POST", "/items/0a0b?other=0102&other=ffff&maybe=1234&name=quiet", "").Code)
	require.Equal(t, params{ID: hexID{0x0a, 0x0b}, Others: []hexID{{1, 2}, {0xff, 0xff}}, Maybe: &hexID{0x12, 0x34}, Name: "QUIET"}, got)
	got = params{}
	require.Equal(t, 200, request(h, "POST", "/items/0a0b", "").Code)
	require.Nil(t, got.Maybe)
	for _, target := range []string{"/items/0a0", "/items/0a0b?other=xyzw", "/items/0a0b?maybe=1&maybe=2"} {
		require.Equal(t, 400, request(h, "POST", target, "").Code, target)
	}
}

type promoted struct {
	Note  string
	Count int
}

type pointerEmbedding struct{ *promoted }

type customEmbedded struct{ customJSONParams }

type ambiguousCustomEmbedded struct {
	customJSONParams
	otherCustomJSON
}

type otherCustomJSON struct{}

func (*otherCustomJSON) UnmarshalJSON([]byte) error { return nil }

func TestBindingPromotesEmbeddedFields(t *testing.T) {
	type params struct {
		ID string
		promoted
	}
	var got params
	op := operation[params, bool]{call: func(_ context.Context, _ string, p params) (bool, error) { got = p; return true, nil }}
	inputs := []httpregistry.Input{{Source: "path", Name: "id", Field: "ID"}, {Source: "body", Name: "note", Field: "Note"}, {Source: "query", Name: "count", Field: "Count"}}
	ep := httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), Inputs: inputs})
	r := registryFor(ep, "/items/{id}")
	require.Equal(t, 200, request(r.Handler(), "POST", "/items/a?count=3", `{"note":"hi"}`).Code)
	require.Equal(t, params{ID: "a", promoted: promoted{Note: "hi", Count: 3}}, got)
	require.Equal(t, "Note", r.Inventory().Endpoints[0].Binding.Inputs[1].Field)

	require.PanicsWithValue(t, "httpregistry: unmapped parameter field Count", func() {
		httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter(), Inputs: inputs[:2]})
	})
	spec := httpregistry.Binding{JSON: httpregistry.StrictJSON, Status: 200, Errors: errorWriter()}
	require.PanicsWithValue(t, "httpregistry: unsupported embedded field promoted", func() {
		httpregistry.Bind(operation[pointerEmbedding, bool]{}, "", spec)
	})
	// A promoted UnmarshalJSON makes the parameters themselves custom.
	require.PanicsWithValue(t, "httpregistry: custom parameter unmarshalling is unsupported", func() {
		httpregistry.Bind(operation[customEmbedded, bool]{}, "", spec)
	})
	// Two such methods cancel out by ambiguity; the embedded field is still refused.
	require.PanicsWithValue(t, "httpregistry: unsupported embedded field customJSONParams", func() {
		httpregistry.Bind(operation[ambiguousCustomEmbedded, bool]{}, "", spec)
	})
}

func TestBindingEncoder(t *testing.T) {
	type params struct{}
	result := map[string]any{"b": 1, "a": nil}
	op := operation[params, map[string]any]{call: func(context.Context, string, params) (map[string]any, error) { return result, nil }}
	encoder := httpregistry.Encoder{Name: "test-encoder", Encode: func(v any) ([]byte, error) {
		if v.(map[string]any)["fail"] != nil {
			return nil, errors.New("cannot encode")
		}
		return []byte(`"encoded"`), nil
	}}
	r := registryFor(httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 201, Errors: errorWriter(), Encoder: encoder}), "/")
	w := request(r.Handler(), "POST", "/", "")
	require.Equal(t, 201, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Equal(t, "\"encoded\"\n", w.Body.String())
	require.Equal(t, "test-encoder", r.Inventory().Endpoints[0].Binding.Encoder)

	// Encoding finishes before the status is committed.
	result["fail"] = true
	require.Equal(t, 422, request(r.Handler(), "POST", "/", "").Code)

	// The default is JSON v2, and the inventory leaves the encoder unnamed.
	delete(result, "fail")
	r = registryFor(httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 200, Errors: errorWriter()}), "/")
	require.JSONEq(t, `{"a":null,"b":1}`, request(r.Handler(), "POST", "/", "").Body.String())
	require.Empty(t, r.Inventory().Endpoints[0].Binding.Encoder)
	data, err := json.Marshal(r.Inventory().Endpoints[0].Binding)
	require.NoError(t, err)
	require.NotContains(t, string(data), "encoder")

	for _, half := range []httpregistry.Encoder{{Name: "named-only"}, {Encode: encoder.Encode}} {
		require.Panics(t, func() {
			httpregistry.Bind(op, "", httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 200, Errors: errorWriter(), Encoder: half})
		})
	}
}

func TestBindingTrailingPlaceholder(t *testing.T) {
	type params struct{ ID, Name string }
	var got params
	op := operation[params, params]{call: func(_ context.Context, _ string, p params) (params, error) { got = p; return p, nil }}
	ep := httpregistry.Bind(op, "dependencies", httpregistry.Binding{JSON: httpregistry.NoJSON, Status: 200, Errors: errorWriter(), Inputs: []httpregistry.Input{
		{Source: "path", Name: "id", Field: "ID"}, {Source: "path", Name: "name", Field: "Name"},
	}})
	r := registryFor(ep, "/items/{id}/{name...}")
	require.Equal(t, 200, request(r.Handler(), "POST", "/items/abc/x/y", "").Code)
	require.Equal(t, params{ID: "abc", Name: "x/y"}, got)
	require.Panics(t, func() { registryFor(ep, "/items/{id}/{other...}") })
}
