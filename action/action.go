// Package action provides typed action catalogs and middleware dispatch.
// Applications supply their dependencies and metadata; the package assigns
// no policy meaning to metadata.
package action

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

var ErrUnknownAction = errors.New("unknown action")

// ErrWrongRegistry means an action was invoked through a dispatcher for a
// different registry. No middleware, validation, or handler runs in this case.
var ErrWrongRegistry = errors.New("action belongs to a different registry")

type ValidationError struct {
	Action string
	Err    error
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %v", e.Action, e.Err) }
func (e *ValidationError) Unwrap() error { return e.Err }

// Info is the descriptive view of an action used by middleware and other
// observers. It does not expose invocation or depend on application services.
type Info[M any] interface {
	Name() string
	Description() string
	Metadata() M
}

type Invocation[M any] struct {
	Action Info[M]
	Params any
}
type (
	Handler[M any]    func(context.Context, Invocation[M]) (any, error)
	Middleware[M any] func(Handler[M]) Handler[M]
)

// AnyAction is the type-erased view used by catalogs and dynamic callers.
// D is the application dependency type and M is its metadata type.
type AnyAction[D, M any] interface {
	Info[M]
	NewParams() any
	Invoke(context.Context, *Dispatcher[D, M], D, any) (any, error)
}

type Definition[D, M, P, R any] struct {
	Name        string
	Description string
	Metadata    M
	Handler     func(context.Context, D, P) (R, error)
}

type Action[D, M, P, R any] struct {
	registry *Registry[D, M]
	def      Definition[D, M, P, R]
}

// Registry owns a catalog independently of other registries. Its zero value
// is ready for use. A Registry must not be copied after first use.
type Registry[D, M any] struct {
	mu      sync.RWMutex
	actions map[string]AnyAction[D, M]
}

// Define registers an action, panicking on an empty name, missing handler,
// or duplicate name within this registry.
func (registry *Registry[D, M]) Define[P, R any](def Definition[D, M, P, R]) *Action[D, M, P, R] {
	if def.Name == "" || def.Handler == nil {
		panic("action: action name and handler are required")
	}
	a := &Action[D, M, P, R]{registry: registry, def: def}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.actions[def.Name]; exists {
		panic("action: duplicate action " + def.Name)
	}
	if registry.actions == nil {
		registry.actions = make(map[string]AnyAction[D, M])
	}
	registry.actions[def.Name] = a
	return a
}

func (r *Registry[D, M]) Lookup(name string) (AnyAction[D, M], bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[name]
	return a, ok
}

// Catalog returns a snapshot sorted by action name.
func (r *Registry[D, M]) Catalog() []AnyAction[D, M] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a := slices.Collect(maps.Values(r.actions))
	slices.SortFunc(a, func(a, b AnyAction[D, M]) int { return strings.Compare(a.Name(), b.Name()) })
	return a
}
func (a *Action[D, M, P, R]) Name() string        { return a.def.Name }
func (a *Action[D, M, P, R]) Description() string { return a.def.Description }
func (a *Action[D, M, P, R]) Metadata() M         { return a.def.Metadata }
func (a *Action[D, M, P, R]) NewParams() any      { return new(P) }

// Call invokes the action with this call's dependencies and parameters. The
// dispatcher must use the registry in which the action was defined.
func (a *Action[D, M, P, R]) Call(ctx context.Context, d *Dispatcher[D, M], dependencies D, p P) (R, error) {
	var zero R
	v, err := a.Invoke(ctx, d, dependencies, &p)
	if err != nil {
		return zero, err
	}
	r, ok := v.(R)
	if !ok {
		return zero, fmt.Errorf("action: action %q returned %T", a.Name(), v)
	}
	return r, nil
}

func (a *Action[D, M, P, R]) Invoke(ctx context.Context, d *Dispatcher[D, M], dependencies D, raw any) (any, error) {
	if a.registry != d.registry {
		return nil, ErrWrongRegistry
	}
	p, ok := raw.(*P)
	if !ok {
		return nil, fmt.Errorf("action: action %q parameters have type %T", a.Name(), raw)
	}
	inner := func(ctx context.Context, inv Invocation[M]) (any, error) {
		if v, ok := any(p).(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				return nil, &ValidationError{Action: a.Name(), Err: err}
			}
		}
		return a.def.Handler(ctx, dependencies, *p)
	}
	return d.run(ctx, Invocation[M]{Action: a, Params: p}, inner)
}

// Dispatcher binds a registry to middleware. Application dependencies are
// supplied on each invocation and are not retained by the dispatcher.
// A registry can have multiple dispatchers with different middleware.
// The first middleware is the outermost wrapper.
type Dispatcher[D, M any] struct {
	registry   *Registry[D, M]
	middleware []Middleware[M]
}

func NewDispatcher[D, M any](registry *Registry[D, M], middleware ...Middleware[M]) *Dispatcher[D, M] {
	return &Dispatcher[D, M]{registry: registry, middleware: append([]Middleware[M](nil), middleware...)}
}

func (d *Dispatcher[D, M]) run(ctx context.Context, inv Invocation[M], h Handler[M]) (any, error) {
	// The terminal handler captures this invocation's dependencies and parameters.
	// Reusing a composed chain would require a different call-state contract.
	for i := len(d.middleware) - 1; i >= 0; i-- {
		h = d.middleware[i](h)
	}
	return h(ctx, inv)
}

// InvokeJSON resolves an action and decodes parameters before entering
// middleware. JSON v2 rejects unknown fields, duplicate names, invalid Unicode,
// and trailing data, and matches field names case-sensitively. Parameter
// validation runs inside middleware, as it does for typed calls.
func (d *Dispatcher[D, M]) InvokeJSON(ctx context.Context, dependencies D, name string, data []byte) (any, error) {
	a, ok := d.registry.Lookup(name)
	if !ok {
		return nil, ErrUnknownAction
	}
	p := a.NewParams()
	if err := json.Unmarshal(data, p, json.RejectUnknownMembers(true)); err != nil {
		return nil, &ValidationError{Action: name, Err: err}
	}
	return a.Invoke(ctx, d, dependencies, p)
}
