// Package httpregistry registers HTTP endpoints and inventories the same
// declarations used to serve requests. Metadata has application-defined meaning.
// Construction is single-threaded; Handler freezes registration before serving.
package httpregistry

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Stage couples an installed wrapper to its name. The name is a declaration,
// not proof of the wrapper's behavior. The first stage is outermost.
type Stage struct {
	Name string
	Wrap func(http.Handler) http.Handler
}

// Compiler interprets application metadata for every endpoint, including mounts
// and disabled declarations. It must not perform I/O or mutate metadata.
type Compiler[M any] func(Spec[M]) ([]Stage, error)

type Condition struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type Spec[M any] struct {
	Method   string     `json:"method,omitempty"` // empty only for mounts
	Path     string     `json:"path"`
	Metadata M          `json:"metadata"`
	When     *Condition `json:"when,omitzero"`
}

type Description struct {
	Kind    string              `json:"kind"`
	Binding *BindingDescription `json:"binding,omitzero"`
	Gaps    []string            `json:"gaps,omitempty"`
}

type Entry[M any] struct {
	Spec[M]
	Description
	Enabled bool     `json:"enabled"`
	Stages  []string `json:"stages"`
}

type Inventory[M any] struct {
	SchemaVersion int        `json:"schema_version"`
	Endpoints     []Entry[M] `json:"endpoints"`
}

// Endpoint is constructed by Custom, CustomFactory, or Bind. Its executable and
// descriptive parts cannot be changed independently by registration callers.
type Endpoint struct {
	handler     http.Handler
	factory     func() http.Handler
	description Description
	pathParams  []string
}

// Custom makes no claim about action calls, request decoding, or responses.
func Custom(handler http.Handler) Endpoint {
	if handler == nil {
		panic("httpregistry: nil handler")
	}
	return Endpoint{
		handler: handler,
		description: Description{
			Kind: "custom",
			Gaps: []string{"handler internals and transitive action calls are not inspected"},
		},
	}
}

// CustomFactory describes an opaque handler without constructing it yet.
// Registration calls the factory once for each enabled endpoint and never for
// disabled endpoints. Like Custom, this makes no claims about handler internals.
func CustomFactory(factory func() http.Handler) Endpoint {
	if factory == nil {
		panic("httpregistry: nil handler factory")
	}
	return Endpoint{
		factory: factory,
		description: Description{
			Kind: "custom",
			Gaps: []string{"handler internals and transitive action calls are not inspected"},
		},
	}
}

type Registry[M any] struct {
	compiler Compiler[M]
	global   []Stage
	mux      *chi.Mux
	entries  []Entry[M]
	keys     map[string]bool
	handler  http.Handler
}

// New requires an explicit metadata compiler, even when metadata is descriptive
// only (in that case return no stages). Global stages also wrap 404/405 replies.
func New[M any](compiler Compiler[M], global ...Stage) *Registry[M] {
	if compiler == nil {
		panic("httpregistry: metadata compiler is required")
	}
	validateStages(global)
	return &Registry[M]{
		compiler: compiler,
		global:   slices.Clone(global),
		mux:      chi.NewRouter(),
		keys:     map[string]bool{},
	}
}

func (r *Registry[M]) Handle(spec Spec[M], endpoint Endpoint) { r.register(spec, endpoint, false) }

// Mount delegates the exact path, its trailing-slash form, and descendants to
// the handler without stripping URL.Path. Supported protocol methods and inner
// routes are opaque. Mount paths must be literal, without a trailing slash.
func (r *Registry[M]) Mount(spec Spec[M], endpoint Endpoint) { r.register(spec, endpoint, true) }

func (r *Registry[M]) register(spec Spec[M], ep Endpoint, mount bool) {
	if r.handler != nil {
		panic("httpregistry: registration is frozen")
	}
	if ep.handler == nil && ep.factory == nil {
		panic("httpregistry: missing endpoint")
	}
	params, shape := pathShape(spec.Path)
	if mount {
		if spec.Method != "" || len(params) != 0 || strings.HasSuffix(spec.Path, "/") {
			panic("httpregistry: mounts require a literal path, no method or trailing slash")
		}
		if ep.description.Binding != nil {
			panic("httpregistry: cannot mount an action binding")
		}
		ep.description.Kind = "mount"
		ep.description.Gaps = []string{"mounted methods, inner routes, and transitive action calls are not inspected"}
	} else if !validMethod(spec.Method) {
		panic("httpregistry: unsupported HTTP method")
	}
	key := spec.Method + " " + shape
	if r.keys[key] {
		panic("httpregistry: duplicate route " + key)
	}
	// Forbid mount overlap rather than silently shadowing declarations.
	for _, entry := range r.entries {
		if (mount && within(entry.Path, spec.Path)) || (entry.Kind == "mount" && within(spec.Path, entry.Path)) {
			panic("httpregistry: overlapping mount " + spec.Path)
		}
	}
	if ep.description.Binding != nil && !sameStrings(params, ep.pathParams) {
		panic("httpregistry: route placeholders and binding path inputs differ")
	}
	if spec.When != nil {
		c := *spec.When
		if c.Name == "" {
			panic("httpregistry: condition name is required")
		}
		spec.When = &c
	}
	stages, err := r.compiler(spec)
	if err != nil {
		panic(fmt.Sprintf("httpregistry: %s %s: %v", spec.Method, spec.Path, err))
	}
	validateStages(stages)
	enabled := spec.When == nil || spec.When.Enabled
	names := make([]string, 0, len(r.global)+len(stages))
	for _, s := range r.global {
		names = append(names, s.Name)
	}
	for _, s := range stages {
		names = append(names, s.Name)
	}
	if enabled {
		if ep.factory != nil {
			ep.handler = ep.factory()
			if ep.handler == nil {
				panic("httpregistry: handler factory returned nil")
			}
		}
		h := wrap(ep.handler, stages)
		if mount {
			r.mux.Mount(spec.Path, h)
		} else {
			r.mux.Method(spec.Method, spec.Path, h)
		}
	}
	r.keys[key] = true
	r.entries = append(r.entries, Entry[M]{
		Spec:        spec,
		Description: ep.description,
		Enabled:     enabled,
		Stages:      names,
	})
}

// Handler freezes registration. Repeated calls return the same handler.
func (r *Registry[M]) Handler() http.Handler {
	if r.handler == nil {
		r.handler = wrap(r.mux, r.global)
	}
	return r.handler
}

// Inventory returns sorted copies of library-owned descriptions. Metadata is
// copied by value: callers must treat any referenced application data as immutable.
// Disabled entries describe the stages that would be installed if enabled.
func (r *Registry[M]) Inventory() Inventory[M] {
	entries := slices.Clone(r.entries)
	for i := range entries {
		e := &entries[i]
		e.Stages = slices.Clone(e.Stages)
		e.Gaps = slices.Clone(e.Gaps)
		if e.When != nil {
			c := *e.When
			e.When = &c
		}
		if e.Binding != nil {
			b := *e.Binding
			b.Inputs = slices.Clone(b.Inputs)
			b.Zero = slices.Clone(b.Zero)
			e.Binding = &b
		}
	}
	slices.SortFunc(entries, func(a, b Entry[M]) int {
		if n := strings.Compare(a.Path, b.Path); n != 0 {
			return n
		}
		return strings.Compare(a.Method, b.Method)
	})
	return Inventory[M]{SchemaVersion: 1, Endpoints: entries}
}

// PathParam reads an endpoint's matched path parameter without exposing the router.
func PathParam(r *http.Request, name string) string { return chi.URLParam(r, name) }

func wrap(h http.Handler, stages []Stage) http.Handler {
	for i := len(stages) - 1; i >= 0; i-- {
		h = stages[i].Wrap(h)
		if h == nil {
			panic("httpregistry: stage returned nil handler")
		}
	}
	return h
}
func validateStages(stages []Stage) {
	for _, s := range stages {
		if s.Name == "" || s.Wrap == nil {
			panic("httpregistry: stage name and wrapper are required")
		}
	}
}
func within(path, prefix string) bool {
	// A placeholder can intersect a literal mount prefix too.
	parts, base := strings.Split(path, "/"), strings.Split(prefix, "/")
	if len(parts) < len(base) {
		return false
	}
	for i, p := range base {
		if parts[i] != p && !strings.HasPrefix(parts[i], "{") {
			return false
		}
	}
	return true
}
func validMethod(s string) bool {
	switch s {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE":
		return true
	}
	return false
}
func sameStrings(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// Initially only literal segments and whole-segment {name} placeholders are
// supported. This keeps binding coverage and duplicate detection unambiguous.
func pathShape(path string) ([]string, string) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#*") || strings.Contains(path, "//") {
		panic("httpregistry: invalid path " + path)
	}
	parts := strings.Split(path, "/")
	var names []string
	for i, p := range parts {
		if strings.ContainsAny(p, "{}") {
			if len(p) < 3 || p[0] != '{' || p[len(p)-1] != '}' {
				panic("httpregistry: invalid placeholder")
			}
			name := p[1 : len(p)-1]
			for _, c := range name {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
					panic("httpregistry: invalid placeholder")
				}
			}
			if slices.Contains(names, name) {
				panic("httpregistry: repeated placeholder")
			}
			names = append(names, name)
			parts[i] = "{}"
		}
	}
	return names, strings.Join(parts, "/")
}
