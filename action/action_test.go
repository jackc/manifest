package action_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/manifest/action"
)

// These application types deliberately have no dependency on Logger4Life's
// core or its action policies.
type (
	dependencies struct{ prefix string }
	metadata     struct{ Category string }
	params       struct {
		Value string `json:"value"`
	}
)

func TestIndependentRegistriesAndDispatchers(t *testing.T) {
	var first, second action.Registry[dependencies, metadata]
	define := func(registry *action.Registry[dependencies, metadata], category string) *action.Action[dependencies, metadata, params, string] {
		return registry.Define(action.Definition[dependencies, metadata, params, string]{
			Name: "echo", Description: "Return a prefixed value.", Metadata: metadata{Category: category},
			Handler: func(_ context.Context, deps dependencies, p params) (string, error) {
				return deps.prefix + p.Value, nil
			},
		})
	}
	a := define(&first, "first")
	b := define(&second, "second")
	for _, tc := range []struct {
		registry *action.Registry[dependencies, metadata]
		action   *action.Action[dependencies, metadata, params, string]
		category string
	}{{&first, a, "first"}, {&second, b, "second"}} {
		found, ok := tc.registry.Lookup("echo")
		if !ok || found != tc.action || found.Metadata().Category != tc.category {
			t.Fatalf("Lookup(echo) = %v, %t", found, ok)
		}
		catalog := tc.registry.Catalog()
		if len(catalog) != 1 || catalog[0] != tc.action {
			t.Fatalf("Catalog() = %v", catalog)
		}
	}

	// A single dispatcher can serve different dependencies on successive calls.
	var seen metadata
	dispatcher := action.NewDispatcher(&first,
		func(next action.Handler[metadata]) action.Handler[metadata] {
			return func(ctx context.Context, inv action.Invocation[metadata]) (any, error) {
				seen = inv.Action.Metadata()
				return next(ctx, inv)
			}
		})
	for _, prefix := range []string{"one:", "two:", "one:"} {
		deps := dependencies{prefix: prefix}
		seen = metadata{}
		got, err := a.Call(context.Background(), dispatcher, deps, params{Value: "typed"})
		if err != nil || got != prefix+"typed" || seen.Category != "first" {
			t.Fatalf("Call() = %q, %v; metadata = %#v", got, err, seen)
		}
		seen = metadata{}
		result, err := dispatcher.InvokeJSON(context.Background(), deps, "echo", []byte(`{"value":"json"}`))
		if err != nil || result != prefix+"json" || seen.Category != "first" {
			t.Fatalf("InvokeJSON() = %v, %v; metadata = %#v", result, err, seen)
		}
	}
	// Dispatchers sharing a registry may have different middleware.
	seen = metadata{}
	plain := action.NewDispatcher(&first)
	if got, err := a.Call(context.Background(), plain, dependencies{prefix: "plain:"}, params{Value: "value"}); err != nil || got != "plain:value" {
		t.Fatalf("Call through second dispatcher = %q, %v", got, err)
	}
	if seen != (metadata{}) {
		t.Fatal("second dispatcher used the first dispatcher's middleware")
	}
	if _, ok := first.Lookup("missing"); ok {
		t.Fatal("Lookup found an unregistered action")
	}
}

type validatedParams struct {
	validationCalls *int
}

func (p *validatedParams) Validate() error {
	*p.validationCalls++
	return nil
}

func TestInvocationRejectsAnotherRegistryBeforeExecution(t *testing.T) {
	var first, second action.Registry[dependencies, metadata]
	var handlerCalls, middlewareCalls, validationCalls int
	firstAction := first.Define(action.Definition[dependencies, metadata, validatedParams, string]{
		Name: "same_name",
		Handler: func(context.Context, dependencies, validatedParams) (string, error) {
			handlerCalls++
			return "first", nil
		},
	})
	second.Define(action.Definition[dependencies, metadata, params, string]{
		Name: "same_name",
		Handler: func(_ context.Context, deps dependencies, p params) (string, error) {
			return deps.prefix + p.Value, nil
		},
	})
	dispatcher := action.NewDispatcher(&second,
		func(next action.Handler[metadata]) action.Handler[metadata] {
			return func(ctx context.Context, inv action.Invocation[metadata]) (any, error) {
				middlewareCalls++
				return next(ctx, inv)
			}
		})
	ctx := context.Background()
	deps := dependencies{prefix: "second:"}
	p := validatedParams{validationCalls: &validationCalls}
	if got, err := firstAction.Call(ctx, dispatcher, deps, p); !errors.Is(err, action.ErrWrongRegistry) || got != "" {
		t.Fatalf("Call with wrong registry = %q, %v", got, err)
	}
	var erased action.AnyAction[dependencies, metadata] = firstAction
	for _, raw := range []any{&p, "wrong parameter type"} {
		if got, err := erased.Invoke(ctx, dispatcher, deps, raw); !errors.Is(err, action.ErrWrongRegistry) || got != nil {
			t.Fatalf("Invoke with wrong registry = %v, %v", got, err)
		}
	}
	if handlerCalls != 0 || middlewareCalls != 0 || validationCalls != 0 {
		t.Fatalf("wrong registry executed handler=%d middleware=%d validation=%d", handlerCalls, middlewareCalls, validationCalls)
	}
	// JSON lookup resolves the second registry's action despite the shared name.
	got, err := dispatcher.InvokeJSON(ctx, deps, "same_name", []byte(`{"value":"json"}`))
	if err != nil || got != "second:json" || middlewareCalls != 1 || handlerCalls != 0 {
		t.Fatalf("InvokeJSON = %v, %v; middleware=%d first handler=%d", got, err, middlewareCalls, handlerCalls)
	}
	if _, err := dispatcher.InvokeJSON(ctx, deps, "missing", []byte(`{}`)); !errors.Is(err, action.ErrUnknownAction) {
		t.Fatalf("InvokeJSON with unknown action = %v", err)
	}
	// The action still executes normally through its own registry.
	got, err = firstAction.Call(ctx, action.NewDispatcher(&first), deps, p)
	if err != nil || got != "first" || handlerCalls != 1 || validationCalls != 1 {
		t.Fatalf("Call with own registry = %v, %v; handler=%d validation=%d", got, err, handlerCalls, validationCalls)
	}
}

func TestDuplicateNamesAreRejectedWithinARegistry(t *testing.T) {
	var registry action.Registry[struct{}, struct{}]
	def := action.Definition[struct{}, struct{}, struct{}, struct{}]{
		Name: "duplicate",
		Handler: func(context.Context, struct{}, struct{}) (struct{}, error) {
			return struct{}{}, nil
		},
	}
	registry.Define(def)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	registry.Define(def)
}

func TestInvokeJSONRejectsInvalidInputBeforeDispatch(t *testing.T) {
	var registry action.Registry[struct{}, struct{}]
	handlerCalls, middlewareCalls := 0, 0
	registry.Define(action.Definition[struct{}, struct{}, params, string]{
		Name: "json",
		Handler: func(_ context.Context, _ struct{}, p params) (string, error) {
			handlerCalls++
			return p.Value, nil
		},
	})
	dispatcher := action.NewDispatcher(&registry, func(next action.Handler[struct{}]) action.Handler[struct{}] {
		return func(ctx context.Context, inv action.Invocation[struct{}]) (any, error) {
			middlewareCalls++
			return next(ctx, inv)
		}
	})
	for _, data := range []string{
		`{"value":"first"}{"value":"second"}`,
		`{"value":"first"} null`,
		`{"value":"first"} true`,
		`{"value":"first"} garbage`,
		`{"value":"first"} {`,
		`{"Value":"case alias"}`,
		`{"extra":true}`,
		`{"value":"first","value":"second"}`,
		`{"value":"\ud800"}`,
		"{\"value\":\"\xff\"}",
	} {
		t.Run(data, func(t *testing.T) {
			_, err := dispatcher.InvokeJSON(context.Background(), struct{}{}, "json", []byte(data))
			var validation *action.ValidationError
			if !errors.As(err, &validation) || validation.Action != "json" {
				t.Fatalf("expected named validation error, got %v", err)
			}
			if handlerCalls != 0 || middlewareCalls != 0 {
				t.Fatal("malformed input entered the invocation pipeline")
			}
		})
	}
	result, err := dispatcher.InvokeJSON(context.Background(), struct{}{}, "json", []byte("{\"value\":\"ok\"} \n\t"))
	if err != nil || result != "ok" || handlerCalls != 1 || middlewareCalls != 1 {
		t.Fatalf("trailing whitespace should be accepted: result=%v error=%v calls=%d/%d", result, err, handlerCalls, middlewareCalls)
	}
}
