package httpregistry_test

import (
	"encoding/json/v2"
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
		{"rest-not-final", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/{a...}/b"}, ep)
		}},
		{"rest-without-name", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a/{...}"}, ep)
		}},
		{"equivalent-rest", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a/{x...}"}, ep)
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a/{y...}"}, ep)
		}},
		{"rest-overlaps-mount", func(r *httpregistry.Registry[metadata]) {
			r.Mount(httpregistry.Spec[metadata]{Path: "/m/a/b"}, ep)
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/m/{rest...}"}, ep)
		}},
		{"mount-below-rest", func(r *httpregistry.Registry[metadata]) {
			r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/{rest...}"}, ep)
			r.Mount(httpregistry.Spec[metadata]{Path: "/m/a"}, ep)
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

func TestUnmatchedRequests(t *testing.T) {
	type reply struct {
		path    string
		allowed []string
	}
	var replies []reply
	header := httpregistry.Stage{Name: "global-header", Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Global", "yes")
			next.ServeHTTP(w, r)
		})
	}}
	r := httpregistry.New(compiler, header)
	ok := httpregistry.Custom(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/items/{id}", Metadata: metadata{Public: true}}, ok)
	r.Handle(httpregistry.Spec[metadata]{Method: "DELETE", Path: "/items/{id}", Metadata: metadata{Public: true}}, ok)
	r.Handle(httpregistry.Spec[metadata]{Method: "POST", Path: "/items/{id}", When: &httpregistry.Condition{Name: "off"}}, ok)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/off", When: &httpregistry.Condition{Name: "off"}}, ok)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/private"}, ok)
	r.Mount(httpregistry.Spec[metadata]{Path: "/protocol", Metadata: metadata{Public: true}}, ok)
	require.Nil(t, r.Inventory().Unmatched)
	r.SetUnmatched(httpregistry.Unmatched{Name: "test-unmatched", Handle: func(w http.ResponseWriter, req *http.Request, allowed []string) {
		replies = append(replies, reply{req.URL.Path, allowed})
		w.WriteHeader(418)
	}})
	h := r.Handler()

	for _, tc := range []struct {
		method, path string
		allowed      []string
	}{
		{"GET", "/missing", []string{}},
		{"GET", "/items/1/", []string{}},
		{"GET", "/off", []string{}},                         // disabled endpoints are not installed
		{"POST", "/items/1", []string{"GET", "DELETE"}},     // the disabled POST is not allowed
		{"HEAD", "/items/1", []string{"GET", "DELETE"}},     // GET does not imply HEAD
		{"PROPFIND", "/items/1", []string{"GET", "DELETE"}}, // unknown to the router
		{"PROPFIND", "/missing", []string{}},
		{"GET", "/items/a%2Fb/x", []string{}},
		{"POST", "/items/a%2Fb", []string{"GET", "DELETE"}}, // routed on the escaped path, as the router does
		// The router refuses methods it does not know before routing, even
		// under a mount, which itself accepts every method.
		{"PROPFIND", "/protocol/any", []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE"}},
	} {
		replies = nil
		w := request(h, tc.method, tc.path, "")
		require.Equal(t, 418, w.Code, "%s %s", tc.method, tc.path)
		require.Equal(t, "yes", w.Header().Get("X-Global"), "global stages wrap unmatched requests")
		require.Len(t, replies, 1)
		require.Equal(t, tc.allowed, replies[0].allowed, "%s %s", tc.method, tc.path)
	}
	// Matched requests, including endpoint policy refusals and mounts, never
	// reach the unmatched handler.
	replies = nil
	require.Equal(t, "ok", request(h, "GET", "/items/1", "").Body.String())
	require.Equal(t, "ok", request(h, "GET", "/items/a%2Fb", "").Body.String())
	require.Equal(t, 401, request(h, "GET", "/private", "").Code)
	require.Equal(t, "ok", request(h, "DELETE", "/protocol/any", "").Body.String())
	require.Empty(t, replies)

	inv := r.Inventory()
	require.Equal(t, &httpregistry.UnmatchedEntry{Name: "test-unmatched", Stages: []string{"global-header"}}, inv.Unmatched)
	inv.Unmatched.Stages[0] = "changed"
	require.Equal(t, "global-header", r.Inventory().Unmatched.Stages[0])
	require.Panics(t, func() {
		r.SetUnmatched(httpregistry.Unmatched{Name: "late", Handle: func(http.ResponseWriter, *http.Request, []string) {}})
	})
}

func TestUnmatchedDeclarations(t *testing.T) {
	handle := func(http.ResponseWriter, *http.Request, []string) {}
	require.Panics(t, func() { httpregistry.New(compiler).SetUnmatched(httpregistry.Unmatched{Handle: handle}) })
	require.Panics(t, func() { httpregistry.New(compiler).SetUnmatched(httpregistry.Unmatched{Name: "x"}) })
	require.Panics(t, func() {
		r := httpregistry.New(compiler)
		r.SetUnmatched(httpregistry.Unmatched{Name: "x", Handle: handle})
		r.SetUnmatched(httpregistry.Unmatched{Name: "y", Handle: handle})
	})
	// Without a handler, the router's replies and the inventory's JSON are as before.
	r := httpregistry.New(compiler)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/a", Metadata: metadata{Public: true}}, httpregistry.Custom(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	h := r.Handler()
	require.Equal(t, 404, request(h, "GET", "/missing", "").Code)
	w := request(h, "POST", "/a", "")
	require.Equal(t, 405, w.Code)
	require.Equal(t, "GET", w.Header().Get("Allow"))
	data, err := json.Marshal(r.Inventory())
	require.NoError(t, err)
	require.NotContains(t, string(data), "unmatched")
}

func TestTrailingPlaceholder(t *testing.T) {
	r := httpregistry.New(func(httpregistry.Spec[metadata]) ([]httpregistry.Stage, error) {
		// Stages read placeholders by name, as handlers do.
		return []httpregistry.Stage{{Name: "header", Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("X-Name", httpregistry.PathParam(req, "name"))
				next.ServeHTTP(w, req)
			})
		}}}, nil
	})
	echo := httpregistry.Custom(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "%s|%s", httpregistry.PathParam(req, "id"), httpregistry.PathParam(req, "name"))
	}))
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/files/{id}/{name...}"}, echo)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/files/{id}/index"}, httpregistry.Custom(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "literal")
	})))
	h := r.Handler()
	for path, want := range map[string]string{
		"/files/7/a/b.txt": "7|a/b.txt",
		"/files/7/":        "7|",
		"/files/7/a/":      "7|a/",
		"/files/7/index":   "literal", // A literal route takes precedence.
	} {
		w := request(h, "GET", path, "")
		require.Equal(t, 200, w.Code, path)
		require.Equal(t, want, w.Body.String(), path)
	}
	w := request(h, "GET", "/files/7/a/b.txt", "")
	require.Equal(t, "a/b.txt", w.Header().Get("X-Name"))
	require.Equal(t, 404, request(h, "GET", "/files/7", "").Code)
	require.Equal(t, 405, request(h, "POST", "/files/7/a", "").Code)
	inv := r.Inventory()
	require.Equal(t, "/files/{id}/{name...}", inv.Endpoints[1].Path)
}

func TestMatchedPath(t *testing.T) {
	var matched []string
	logger := httpregistry.Stage{Name: "log", Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			require.Empty(t, httpregistry.MatchedPath(req)) // Not yet routed.
			next.ServeHTTP(w, req)
			matched = append(matched, httpregistry.MatchedPath(req))
		})
	}}
	r := httpregistry.New(compiler, logger)
	ok := httpregistry.Custom(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/items/{id}", Metadata: metadata{Public: true}}, ok)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/private"}, ok)
	r.Handle(httpregistry.Spec[metadata]{Method: "GET", Path: "/files/{name...}", Metadata: metadata{Public: true}}, ok)
	r.Mount(httpregistry.Spec[metadata]{Path: "/protocol", Metadata: metadata{Public: true}}, ok)
	h := r.Handler()
	for _, path := range []string{"/items/1", "/private", "/files/a/b", "/protocol/x", "/missing"} {
		request(h, "GET", path, "")
	}
	request(h, "POST", "/items/1", "")
	// A stage's refusal still names the route; unmatched requests name none.
	require.Equal(t, []string{"/items/{id}", "/private", "/files/{name...}", "/protocol", "", ""}, matched)
	require.Empty(t, httpregistry.MatchedPath(httptest.NewRequest("GET", "/items/1", nil)))
}
