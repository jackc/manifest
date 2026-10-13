package action_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/manifest/action"
	"github.com/jackc/manifest/httpregistry"
	"github.com/jackc/manifest/mcpregistry"
)

// app is a dependency value that carries its own dispatcher, as an
// application's core does.
type app struct {
	actions *action.Dispatcher[*app, metadata]
	prefix  string
}

type checkedParams struct {
	Value string `json:"value"`
}

func (p checkedParams) Validate() error {
	if p.Value == "" {
		return errors.New("value is required")
	}
	return nil
}

func newCatalog() *action.Catalog[*app, metadata] {
	return action.NewCatalog(action.CatalogOptions[*app, metadata]{
		Dispatcher: func(a *app) *action.Dispatcher[*app, metadata] { return a.actions },
	})
}

func defineEcho(catalog *action.Catalog[*app, metadata], calls *int) *action.CatalogAction[*app, metadata, checkedParams, string] {
	return catalog.Define(action.Definition[*app, metadata, checkedParams, string]{
		Name: "echo", Description: "Return a prefixed value.", Metadata: metadata{Category: "public"},
		Handler: func(_ context.Context, a *app, p checkedParams) (string, error) {
			*calls++
			return a.prefix + p.Value, nil
		},
	})
}

// Catalog actions are bound directly, with no application wrapper.
var (
	_ httpregistry.Operation[*app, checkedParams, string] = (*action.CatalogAction[*app, metadata, checkedParams, string])(nil)
	_ mcpregistry.Operation[*app, checkedParams, string]  = (*action.CatalogAction[*app, metadata, checkedParams, string])(nil)
	_ mcpregistry.RawOperation[*app]                      = (*action.CatalogAction[*app, metadata, checkedParams, string])(nil)
	_ action.AnyAction[*app, metadata]                    = (*action.CatalogAction[*app, metadata, checkedParams, string])(nil)
)

func TestCatalogCallsUseTheDependenciesDispatcher(t *testing.T) {
	ctx := context.Background()
	catalog := newCatalog()
	var calls int
	echo := defineEcho(catalog, &calls)
	shout := func(next action.Handler[metadata]) action.Handler[metadata] {
		return func(ctx context.Context, inv action.Invocation[metadata]) (any, error) {
			result, err := next(ctx, inv)
			if s, ok := result.(string); ok {
				result = strings.ToUpper(s)
			}
			return result, err
		}
	}
	quiet := &app{prefix: "quiet:"}
	quiet.actions = catalog.NewDispatcher()
	loud := &app{prefix: "loud:"}
	loud.actions = catalog.NewDispatcher(shout)

	for _, tc := range []struct {
		deps *app
		want string
	}{{quiet, "quiet:x"}, {loud, "LOUD:X"}} {
		if got, err := echo.Call(ctx, tc.deps, checkedParams{Value: "x"}); err != nil || got != tc.want {
			t.Errorf("Call = %q, %v; want %q", got, err, tc.want)
		}
		if got, err := echo.InvokeJSON(ctx, tc.deps, []byte(`{"value":"x"}`)); err != nil || got != tc.want {
			t.Errorf("action InvokeJSON = %v, %v; want %q", got, err, tc.want)
		}
		if got, err := catalog.InvokeJSON(ctx, tc.deps, "echo", []byte(`{"value":"x"}`)); err != nil || got != tc.want {
			t.Errorf("catalog InvokeJSON = %v, %v; want %q", got, err, tc.want)
		}
	}
	if _, err := catalog.InvokeJSON(ctx, quiet, "missing", []byte(`{}`)); !errors.Is(err, action.ErrUnknownAction) {
		t.Errorf("unknown action error = %v", err)
	}
	var validation *action.ValidationError
	if _, err := echo.Call(ctx, quiet, checkedParams{}); !errors.As(err, &validation) {
		t.Errorf("invalid params error = %v, want ValidationError", err)
	}

	calls = 0
	missing := &app{prefix: "none:"}
	if _, err := echo.Call(ctx, missing, checkedParams{Value: "x"}); err == nil || calls != 0 {
		t.Fatalf("Call without a dispatcher = %v; handler calls %d", err, calls)
	}
	if _, err := echo.InvokeJSON(ctx, missing, []byte(`{"value":"x"}`)); err == nil || calls != 0 {
		t.Fatalf("InvokeJSON without a dispatcher = %v; handler calls %d", err, calls)
	}

	if got := catalog.Inventory(); len(got.Actions) != 1 || got.Actions[0].Name != "echo" {
		t.Fatalf("inventory = %+v", got)
	}
	if found, ok := catalog.Lookup("echo"); !ok || found.Name() != "echo" || len(catalog.Actions()) != 1 {
		t.Fatal("catalog does not list its action")
	}
}

func TestCatalogValidatesDefinitionsBeforeRegistering(t *testing.T) {
	catalog := action.NewCatalog(action.CatalogOptions[*app, metadata]{
		Dispatcher: func(a *app) *action.Dispatcher[*app, metadata] { return a.actions },
		Validate: func(info action.Info[metadata]) error {
			if info.Description() == "" || info.Metadata().Category == "" {
				return errors.New("description and category are required")
			}
			return nil
		},
	})
	var calls int
	defineEcho(catalog, &calls)
	func() {
		defer func() {
			if p := recover(); p == nil || !strings.Contains(p.(string), "category are required") {
				t.Fatalf("invalid definition panic = %v", p)
			}
		}()
		catalog.Define(action.Definition[*app, metadata, checkedParams, string]{
			Name: "undescribed", Description: "No category.",
			Handler: func(context.Context, *app, checkedParams) (string, error) { return "", nil },
		})
	}()
	if _, ok := catalog.Lookup("undescribed"); ok {
		t.Fatal("an invalid definition was registered")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("NewCatalog without a dispatcher did not panic")
		}
	}()
	action.NewCatalog(action.CatalogOptions[*app, metadata]{})
}

// Catalog actions decode JSON as Dispatcher.InvokeJSON does: strictly, with
// failures reported as ValidationError before any middleware runs.
func TestCatalogDecodesJSONStrictly(t *testing.T) {
	ctx := context.Background()
	catalog := newCatalog()
	var calls, middlewareCalls int
	echo := defineEcho(catalog, &calls)
	deps := &app{prefix: "strict:"}
	deps.actions = catalog.NewDispatcher(func(next action.Handler[metadata]) action.Handler[metadata] {
		return func(ctx context.Context, inv action.Invocation[metadata]) (any, error) {
			middlewareCalls++
			return next(ctx, inv)
		}
	})
	for _, data := range []string{`{"value":"x","extra":true}`, `{"Value":"x"}`, ``, `{"value":"x"} {}`} {
		var validation *action.ValidationError
		if _, err := echo.InvokeJSON(ctx, deps, []byte(data)); !errors.As(err, &validation) || validation.Action != "echo" {
			t.Errorf("action InvokeJSON(%q) = %v, want ValidationError", data, err)
		}
		if _, err := catalog.InvokeJSON(ctx, deps, "echo", []byte(data)); !errors.As(err, &validation) {
			t.Errorf("catalog InvokeJSON(%q) = %v, want ValidationError", data, err)
		}
	}
	if middlewareCalls != 0 || calls != 0 {
		t.Fatalf("undecodable calls reached middleware %d or handler %d", middlewareCalls, calls)
	}
}
