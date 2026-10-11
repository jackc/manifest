package httpregistry_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/manifest/httpregistry"
	"github.com/stretchr/testify/require"
)

type metadata struct{ Public bool }

func compiler(spec httpregistry.Spec[metadata]) ([]httpregistry.Stage, error) {
	if spec.Metadata.Public {
		return nil, nil
	}
	return []httpregistry.Stage{{Name: "test-auth", Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "test" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}}}, nil
}
func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}
func TestRegistryPolicyAndRouting(t *testing.T) {
	r := httpregistry.New(compiler)
	calls := 0
	ep := httpregistry.Custom(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		fmt.Fprint(w, httpregistry.PathParam(req, "id"))
	}))
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/private/{id}"}, ep)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/public/{id}", Metadata: metadata{Public: true}}, ep)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/off/{id}", When: &httpregistry.Condition{Name: "feature", Enabled: false}}, ep)
	h := r.Handler()
	require.Equal(t, 401, request(h, "GET", "/private/42", "").Code)
	require.Equal(t, 0, calls)
	req := httptest.NewRequest("GET", "/private/42", nil)
	req.Header.Set("Authorization", "test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, "42", w.Body.String())
	require.Equal(t, "43", request(h, "GET", "/public/43", "").Body.String())
	require.Equal(t, 404, request(h, "GET", "/off/1", "").Code)
	require.Equal(t, 405, request(h, "POST", "/public/43", "").Code)
	require.Equal(t, 405, request(h, "HEAD", "/public/43", "").Code) // preserve explicit-method routing
	require.Equal(t, 404, request(h, "GET", "/public/43/", "").Code)
	inv := r.Inventory()
	require.Len(t, inv.Endpoints, 3)
	require.False(t, inv.Endpoints[0].Enabled)
	require.Equal(t, []string{"test-auth"}, inv.Endpoints[1].Stages)
	require.Nil(t, inv.Endpoints[2].Binding) // custom does not imply no action calls
	require.Panics(t, func() { r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/late"}, ep) })
	// Inventories cannot mutate library-owned registration state.
	inv.Endpoints[0].When.Enabled = true
	inv.Endpoints[1].Stages[0] = "other"
	require.False(t, r.Inventory().Endpoints[0].Enabled)
	require.Equal(t, "test-auth", r.Inventory().Endpoints[1].Stages[0])
}
func TestMountBoundaryAndPolicy(t *testing.T) {
	r := httpregistry.New(compiler)
	r.Mount(httpregistry.Spec[metadata]{Path: "/protocol"}, httpregistry.Custom(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.URL.Path) })))
	h := r.Handler()
	for _, path := range []string{"/protocol", "/protocol/", "/protocol/child"} {
		require.Equal(t, 401, request(h, "POST", path, "").Code)
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, path, w.Body.String())
	}
	require.Equal(t, 404, request(h, "POST", "/protocol-other", "").Code)
	require.Equal(t, "mount", r.Inventory().Endpoints[0].Kind)
	require.NotEmpty(t, r.Inventory().Endpoints[0].Gaps)
}
func TestInvalidRegistrations(t *testing.T) {
	ep := httpregistry.Custom(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, tc := range []struct {
		name     string
		register func(*httpregistry.Registry[metadata])
	}{
		{"duplicate", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a"}, ep)
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a"}, ep)
		}},
		{"equivalent-placeholder", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/{a}"}, ep)
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/{b}"}, ep)
		}},
		{"mount-overlap", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/m/a"}, ep)
			r.Mount(httpregistry.Spec[metadata]{Path: "/m"}, ep)
		}},
		{"missing-method", func(r *httpregistry.Registry[metadata]) { r.Handle(httpregistry.Spec[metadata]{Path: "/a"}, ep) }},
		{"unsupported-pattern", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/{id:[0-9]+}"}, ep)
		}},
		{"missing-target", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a"}, httpregistry.Endpoint{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Panics(t, func() { tc.register(httpregistry.New(compiler)) }) })
	}
	require.Panics(t, func() { httpregistry.New[metadata](nil) })
	require.Panics(t, func() {
		r := httpregistry.New(func(httpregistry.Spec[metadata]) ([]httpregistry.Stage, error) { return nil, fmt.Errorf("bad policy") })
		r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a"}, ep)
	})
}
func TestStageOrderAndInstanceIsolation(t *testing.T) {
	var order []string
	stage := func(name string) httpregistry.Stage {
		return httpregistry.Stage{Name: name, Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
				order = append(order, "after-"+name)
			})
		}}
	}
	r := httpregistry.New(func(httpregistry.Spec[metadata]) ([]httpregistry.Stage, error) {
		return []httpregistry.Stage{stage("policy")}, nil
	}, stage("outer"), stage("inner"))
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a"}, httpregistry.Custom(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "endpoint") })))
	request(r.Handler(), "GET", "/a", "")
	require.Equal(t, []string{"outer", "inner", "policy", "endpoint", "after-policy", "after-inner", "after-outer"}, order)
	require.Equal(t, []string{"outer", "inner", "policy"}, r.Inventory().Endpoints[0].Stages)
	other := httpregistry.New(compiler)
	require.Empty(t, other.Inventory().Endpoints)
	require.Equal(t, 404, request(other.Handler(), "GET", "/a", "").Code)
	order = nil
	request(r.Handler(), "GET", "/missing", "")
	require.Equal(t, []string{"outer", "inner", "after-inner", "after-outer"}, order)
}

func TestCustomFactoriesOnlyConstructEnabledEndpoints(t *testing.T) {
	r := httpregistry.New(compiler)
	calls := 0
	endpoint := httpregistry.CustomFactory(func() http.Handler {
		calls++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ready") })
	})
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/off", When: &httpregistry.Condition{Name: "off"}}, endpoint)
	r.Mount(httpregistry.Spec[metadata]{Path: "/off-mount", When: &httpregistry.Condition{Name: "off"}}, endpoint)
	require.Zero(t, calls)
	require.Len(t, r.Inventory().Endpoints, 2)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/on", Metadata: metadata{Public: true}}, endpoint)
	r.Mount(httpregistry.Spec[metadata]{Path: "/on-mount", Metadata: metadata{Public: true}}, endpoint)
	require.Equal(t, 2, calls)
	h := r.Handler()
	require.Equal(t, "ready", request(h, "GET", "/on", "").Body.String())
	require.Equal(t, "ready", request(h, "POST", "/on-mount/child", "").Body.String())
	require.Equal(t, 404, request(h, "GET", "/off", "").Code)
	require.Equal(t, 2, calls) // serving and inventory reads do not reconstruct handlers
	require.Panics(t, func() { httpregistry.CustomFactory(nil) })
	require.Panics(t, func() {
		invalid := httpregistry.New(compiler)
		invalid.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/"}, httpregistry.CustomFactory(func() http.Handler { return nil }))
	})
}
