package action

import (
	"context"
	"errors"
	"fmt"
)

var errNoDispatcher = errors.New("action: dependencies supply no dispatcher")

// Catalog is a registry whose actions find their dispatcher through each
// call's dependencies. Its actions are called as Call(ctx, dependencies,
// params) and InvokeJSON(ctx, dependencies, data), the operation shapes the
// HTTP and MCP registries bind, so applications need no wrapper type to
// supply the dispatcher. A Catalog must not be copied after first use.
type Catalog[D, M any] struct {
	registry   Registry[D, M]
	dispatcher func(D) *Dispatcher[D, M]
	validate   func(Info[M]) error
}

// CatalogOptions configures NewCatalog.
type CatalogOptions[D, M any] struct {
	// Dispatcher returns the dispatcher for a call's dependencies, which must
	// be built from this catalog with NewDispatcher. It is required.
	Dispatcher func(D) *Dispatcher[D, M]
	// Validate, when set, checks each definition's name, description, and
	// metadata before Define registers it; Define panics with its error.
	Validate func(Info[M]) error
}

func NewCatalog[D, M any](opts CatalogOptions[D, M]) *Catalog[D, M] {
	if opts.Dispatcher == nil {
		panic("action: catalog dispatcher is required")
	}
	return &Catalog[D, M]{dispatcher: opts.Dispatcher, validate: opts.Validate}
}

// Define validates and registers an action, panicking on an invalid
// definition, an empty name, a missing handler, or a duplicate name.
func (c *Catalog[D, M]) Define[P, R any](def Definition[D, M, P, R]) *CatalogAction[D, M, P, R] {
	if c.validate != nil {
		if err := c.validate(definitionInfo[M]{def.Name, def.Description, def.Metadata}); err != nil {
			panic(fmt.Sprintf("action: invalid definition %q: %v", def.Name, err))
		}
	}
	return &CatalogAction[D, M, P, R]{Action: c.registry.Define(def), catalog: c}
}

// NewDispatcher returns a dispatcher for this catalog's actions. The first
// middleware is the outermost wrapper.
func (c *Catalog[D, M]) NewDispatcher(middleware ...Middleware[M]) *Dispatcher[D, M] {
	return NewDispatcher(&c.registry, middleware...)
}

func (c *Catalog[D, M]) Lookup(name string) (AnyAction[D, M], bool) { return c.registry.Lookup(name) }

// Actions returns a snapshot sorted by action name.
func (c *Catalog[D, M]) Actions() []AnyAction[D, M] { return c.registry.Catalog() }

// Inventory returns a snapshot sorted by action name; see Registry.Inventory.
func (c *Catalog[D, M]) Inventory() Inventory[M] { return c.registry.Inventory() }

// InvokeJSON invokes the named action through the dependencies' dispatcher;
// see Dispatcher.InvokeJSON.
func (c *Catalog[D, M]) InvokeJSON(ctx context.Context, dependencies D, name string, data []byte) (any, error) {
	d, err := c.dispatch(dependencies)
	if err != nil {
		return nil, err
	}
	return d.InvokeJSON(ctx, dependencies, name, data)
}

func (c *Catalog[D, M]) dispatch(dependencies D) (*Dispatcher[D, M], error) {
	d := c.dispatcher(dependencies)
	if d == nil {
		return nil, errNoDispatcher
	}
	return d, nil
}

// CatalogAction is an action defined in a Catalog. It is also an AnyAction;
// the embedded Action's Call, which takes an explicit dispatcher, remains
// reachable as a.Action.Call.
type CatalogAction[D, M, P, R any] struct {
	*Action[D, M, P, R]
	catalog *Catalog[D, M]
}

// Call invokes the action through the dependencies' dispatcher.
func (a *CatalogAction[D, M, P, R]) Call(ctx context.Context, dependencies D, p P) (R, error) {
	d, err := a.catalog.dispatch(dependencies)
	if err != nil {
		var zero R
		return zero, err
	}
	return a.Action.Call(ctx, d, dependencies, p)
}

// InvokeJSON invokes the action with a JSON parameter document through the
// dependencies' dispatcher, decoding it as Dispatcher.InvokeJSON does.
func (a *CatalogAction[D, M, P, R]) InvokeJSON(ctx context.Context, dependencies D, data []byte) (any, error) {
	d, err := a.catalog.dispatch(dependencies)
	if err != nil {
		return nil, err
	}
	p := new(P)
	if err := decodeParams(a.Name(), data, p); err != nil {
		return nil, err
	}
	return a.Invoke(ctx, d, dependencies, p)
}

// definitionInfo describes a definition to a catalog's validator before an
// action exists for it.
type definitionInfo[M any] struct {
	name, description string
	metadata          M
}

func (i definitionInfo[M]) Name() string        { return i.name }
func (i definitionInfo[M]) Description() string { return i.description }
func (i definitionInfo[M]) Metadata() M         { return i.metadata }
