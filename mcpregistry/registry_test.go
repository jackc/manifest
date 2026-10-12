package mcpregistry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/manifest/mcpregistry"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type metadata struct{ Private bool }
type callerKey struct{}

func registry(stages ...mcpregistry.Stage) *mcpregistry.Registry[metadata] {
	return mcpregistry.New(&mcp.Implementation{Name: "test", Version: "1"}, nil,
		func(mcpregistry.Spec[metadata]) ([]mcpregistry.Stage, error) { return nil, nil }, stages...)
}

func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any, meta mcp.Meta) *mcp.CallToolResult {
	t.Helper()
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
	require.NoError(t, err)
	return result
}

func wireJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}

type params struct {
	Value string `json:"value" jsonschema:"a value"`
}
type output struct {
	Value  string `json:"value"`
	Caller string `json:"caller"`
}
type host struct {
	calls int
	seen  []string
}
type operation struct{}

func (operation) Name() string        { return "internal_action" }
func (operation) Description() string { return "An application's own action pipeline." }
func (operation) Call(ctx context.Context, h *host, p params) (output, error) {
	h.calls++
	caller, _ := ctx.Value(callerKey{}).(string)
	h.seen = append(h.seen, caller)
	if p.Value == "fail" {
		return output{}, errors.New("database address is secret")
	}
	if err := ctx.Err(); err != nil {
		return output{}, err
	}
	return output{Value: strings.TrimSpace(p.Value), Caller: caller}, nil
}

func binding() mcpregistry.Binding {
	return mcpregistry.Binding{Namespace: "example/core", Errors: mcpregistry.ErrorPolicy{
		Name: "safe-errors", Map: func(context.Context, error) error { return errors.New("safe failure") },
	}}
}

func TestTypedBindingIdentityPolicyErrorsAndInventory(t *testing.T) {
	var order []string
	r := mcpregistry.New(&mcp.Implementation{Name: "test", Version: "1"}, nil,
		func(spec mcpregistry.Spec[metadata]) ([]mcpregistry.Stage, error) {
			require.True(t, spec.Metadata.Private)
			return []mcpregistry.Stage{{Name: "identity", Prepare: func(ctx context.Context, req *mcp.CallToolRequest) (context.Context, error) {
				order = append(order, "identity")
				caller, _ := req.Params.Meta["caller"].(string)
				if caller == "" {
					return nil, errors.New("authentication required")
				}
				return context.WithValue(ctx, callerKey{}, caller), nil
			}}}, nil
		}, mcpregistry.Stage{Name: "global", Prepare: func(ctx context.Context, _ *mcp.CallToolRequest) (context.Context, error) {
			order = append(order, "global")
			return ctx, nil
		}})
	h := &host{}
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "external_tool", Title: "External", Description: "Tool-specific guidance."}, Metadata: metadata{Private: true}}, mcpregistry.Bind(operation{}, h, binding()))
	inv := r.Inventory()
	require.Zero(t, h.calls)
	require.Equal(t, "internal_action", inv.Tools[0].Binding.Action)
	require.Equal(t, "external_tool", inv.Tools[0].Tool.Name)
	require.Equal(t, []string{"global", "identity"}, inv.Tools[0].Stages)
	server := r.Server()
	require.Same(t, server, r.Server())
	cs := connect(t, server)
	listed, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.JSONEq(t, wireJSON(t, inv.Tools[0].Tool), wireJSON(t, listed.Tools[0]))
	for _, caller := range []string{"alice", "bob"} {
		result := call(t, cs, "external_tool", params{Value: " trimmed "}, mcp.Meta{"caller": caller})
		require.False(t, result.IsError)
		require.Equal(t, map[string]any{"value": "trimmed", "caller": caller}, result.StructuredContent)
	}
	require.Equal(t, []string{"alice", "bob"}, h.seen)
	require.Equal(t, []string{"global", "identity", "global", "identity"}, order)
	denied := call(t, cs, "external_tool", params{Value: "x"}, nil)
	require.True(t, denied.IsError)
	require.Contains(t, wireJSON(t, denied), "authentication required")
	require.Equal(t, 2, h.calls)
	invalid := call(t, cs, "external_tool", map[string]any{"value": 3}, mcp.Meta{"caller": "alice"})
	require.True(t, invalid.IsError)
	require.Equal(t, 2, h.calls)
	failed := call(t, cs, "external_tool", params{Value: "fail"}, mcp.Meta{"caller": "alice"})
	require.True(t, failed.IsError)
	require.Contains(t, wireJSON(t, failed), "safe failure")
	require.NotContains(t, wireJSON(t, failed), "secret")
	// Calling a differently named tool cannot dynamically expose an action.
	_, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "internal_action", Arguments: params{}})
	require.Error(t, err)
	require.Panics(t, func() {
		r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "late"}}, mcpregistry.Bind(operation{}, h, binding()))
	})
}

func TestTypedSchemasMatchSDKAndPointerOutputBehavior(t *testing.T) {
	r := registry()
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "pointer"}}, mcpregistry.Typed(func(context.Context, *mcp.CallToolRequest, *params) (*mcp.CallToolResult, *output, error) {
		return nil, nil, nil
	}))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "any"}}, mcpregistry.Typed(func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, nil, nil
	}))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "slice"}}, mcpregistry.Typed(func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, []string, error) {
		return nil, []string{"ok"}, nil
	}))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "explicit", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "default": "defaulted"}}}, OutputSchema: map[string]any{"type": "object"}}}, mcpregistry.Typed(func(_ context.Context, _ *mcp.CallToolRequest, p params) (*mcp.CallToolResult, output, error) {
		return nil, output{Value: p.Value}, nil
	}))
	cs := connect(t, r.Server())
	listed, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	inv := r.Inventory()
	require.Len(t, listed.Tools, len(inv.Tools))
	for i, entry := range inv.Tools {
		require.JSONEq(t, wireJSON(t, entry.Tool), wireJSON(t, listed.Tools[i]))
	}
	result := call(t, cs, "pointer", params{Value: "x"}, nil)
	require.False(t, result.IsError)
	require.Equal(t, map[string]any{"value": "", "caller": ""}, result.StructuredContent)
	result = call(t, cs, "explicit", map[string]any{}, nil)
	require.False(t, result.IsError)
	require.Equal(t, "defaulted", result.StructuredContent.(map[string]any)["value"])
	result = call(t, cs, "slice", map[string]any{}, nil)
	require.False(t, result.IsError)
	require.Equal(t, []any{"ok"}, result.StructuredContent)
}

type rawHost struct {
	received []byte
	calls    int
}
type rawOperation struct{}

func (rawOperation) Name() string        { return "finance_import" }
func (rawOperation) Description() string { return "Preserve source records." }
func (rawOperation) InvokeJSON(ctx context.Context, h *rawHost, args []byte) (any, error) {
	h.calls++
	h.received = append([]byte(nil), args...)
	switch string(args) {
	case `{"fail":true}`:
		return nil, errors.New("secret")
	case `{"void":true}`:
		return nil, nil
	case `{"nil_pointer":true}`:
		return (*output)(nil), nil
	}
	return map[string]any{"accepted": true}, nil
}

func TestRawBindingPreservesTokensAndOwnsNeitherSchemaValidationNorFormatting(t *testing.T) {
	r := registry()
	h := &rawHost{}
	tool := mcp.Tool{Name: "finance", InputSchema: map[string]any{"type": "object", "required": []string{"deliberately_absent"}}, OutputSchema: map[string]any{"type": "object"}, Meta: mcp.Meta{"openai/fileParams": []string{"files"}}}
	r.Register(mcpregistry.Spec[metadata]{Tool: tool}, mcpregistry.BindRaw(rawOperation{}, h, mcpregistry.RawBinding{Binding: binding(), MaxArgumentsBytes: 256, Result: mcpregistry.ResultPolicy{Name: "finance-summary", Format: func(_ context.Context, value any) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: value, Content: []mcp.Content{&mcp.TextContent{Text: "Imported records."}}}, nil
	}}}))
	cs := connect(t, r.Server())
	args := json.RawMessage(`{"amount":1.23000000000000000001,"id":9007199254740993,"unknown":{"extra":true}}`)
	result := call(t, cs, "finance", args, nil)
	require.False(t, result.IsError)
	require.Equal(t, []byte(args), h.received)
	require.Equal(t, "Imported records.", result.Content[0].(*mcp.TextContent).Text)
	require.Equal(t, map[string]any{"accepted": true}, result.StructuredContent)
	require.Contains(t, r.Inventory().Tools[0].Arguments, "operation-owned")
	require.Equal(t, 256, r.Inventory().Tools[0].Binding.MaxArgumentsBytes)
	listed, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.JSONEq(t, wireJSON(t, tool), wireJSON(t, listed.Tools[0]))
	result = call(t, cs, "finance", map[string]any{"big": strings.Repeat("x", 257)}, nil)
	require.True(t, result.IsError)
	require.Equal(t, 1, h.calls)
	result = call(t, cs, "finance", json.RawMessage(`{"fail":true}`), nil)
	require.True(t, result.IsError)
	require.NotContains(t, wireJSON(t, result), "secret")
	require.Contains(t, wireJSON(t, result), "safe failure")
}

func TestRawDefaultResultAndCustomEscapeHatch(t *testing.T) {
	r := registry()
	spec := mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "raw", InputSchema: map[string]any{"type": "object"}}}
	r.Register(spec, mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: binding()}))
	spec.Tool.Name = "custom"
	r.Register(spec, mcpregistry.Custom(func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "custom result"}}, Meta: mcp.Meta{"extension": "preserved"}}, nil
	}))
	cs := connect(t, r.Server())
	result := call(t, cs, "raw", map[string]any{}, nil)
	require.False(t, result.IsError)
	require.JSONEq(t, wireJSON(t, result.StructuredContent), result.Content[0].(*mcp.TextContent).Text)
	result = call(t, cs, "custom", map[string]any{}, nil)
	require.Equal(t, "custom result", result.Content[0].(*mcp.TextContent).Text)
	require.Equal(t, "preserved", result.Meta["extension"])
	require.Nil(t, r.Inventory().Tools[0].Binding)
	require.NotEmpty(t, r.Inventory().Tools[0].Gaps)
}

func TestSnapshotsCannotMutateServedCatalog(t *testing.T) {
	r := registry()
	schema := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "integer", "maximum": int64(9007199254740993)}}}
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true}
	meta := mcp.Meta{"extension": map[string]any{"name": "original"}}
	spec := mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "tool", InputSchema: schema, Annotations: annotations, Meta: meta}}
	r.Register(spec, mcpregistry.Custom(func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}))
	before := wireJSON(t, r.Inventory())
	require.Contains(t, before, "9007199254740993", "cloning schemas must preserve exact integer bounds")
	schema["type"] = "string"
	annotations.ReadOnlyHint = false
	meta["extension"].(map[string]any)["name"] = "changed"
	inv := r.Inventory()
	inv.Tools[0].Tool.InputSchema.(map[string]any)["type"] = "array"
	inv.Tools[0].Tool.Annotations.ReadOnlyHint = false
	inv.Tools[0].Gaps[0] = "changed"
	require.JSONEq(t, before, wireJSON(t, r.Inventory()))
	cs := connect(t, r.Server())
	listed, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.JSONEq(t, wireJSON(t, r.Inventory().Tools[0].Tool), wireJSON(t, listed.Tools[0]))
}

func TestRegistriesAreIndependent(t *testing.T) {
	a, b := registry(), registry()
	first, second := &host{}, &host{}
	spec := mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "same"}}
	a.Register(spec, mcpregistry.Bind(operation{}, first, binding()))
	b.Register(spec, mcpregistry.Bind(operation{}, second, binding()))
	call(t, connect(t, a.Server()), "same", params{Value: "a"}, nil)
	require.Equal(t, 1, first.calls)
	require.Zero(t, second.calls)
	call(t, connect(t, b.Server()), "same", params{Value: "b"}, nil)
	require.Equal(t, 1, second.calls)
}

func TestInvalidDeclarations(t *testing.T) {
	ep := mcpregistry.Typed(func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	for _, name := range []string{"", "white space", "slash/name", strings.Repeat("a", 129)} {
		t.Run(name, func(t *testing.T) {
			require.Panics(t, func() { registry().Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: name}}, ep) })
		})
	}
	r := registry()
	spec := mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "duplicate"}}
	r.Register(spec, ep)
	require.Panics(t, func() { r.Register(spec, ep) })
	require.Panics(t, func() { registry().Register(spec, mcpregistry.Endpoint{}) })
	require.Panics(t, func() { mcpregistry.New[metadata](nil, nil, nil) })
	require.Panics(t, func() { registry(mcpregistry.Stage{Name: "missing-function"}) })
	require.Panics(t, func() { mcpregistry.Bind(operation{}, &host{}, mcpregistry.Binding{}) })
	require.Panics(t, func() {
		mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: binding(), MaxArgumentsBytes: -1})
	})
	require.Panics(t, func() {
		mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: binding(), Result: mcpregistry.ResultPolicy{Name: "missing-function"}})
	})
	bad := mcpregistry.New(&mcp.Implementation{Name: "test", Version: "1"}, nil, func(mcpregistry.Spec[metadata]) ([]mcpregistry.Stage, error) {
		return nil, errors.New("invalid policy")
	})
	require.Panics(t, func() { bad.Register(spec, ep) })
}

func textOf(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, result.Content, 1)
	return result.Content[0].(*mcp.TextContent).Text
}

func TestStagesBehaveAlikeForEveryEndpointKind(t *testing.T) {
	r := registry(mcpregistry.Stage{Name: "gate", Prepare: func(ctx context.Context, req *mcp.CallToolRequest) (context.Context, error) {
		switch req.Params.Meta["mode"] {
		case "deny":
			return nil, errors.New("denied")
		case "protocol":
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "protocol refusal"}
		case "nil-context":
			return nil, nil
		}
		return ctx, nil
	}})
	h, rh := &host{}, &rawHost{}
	var typedCalls, customCalls int
	object := map[string]any{"type": "object"}
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "bind"}}, mcpregistry.Bind(operation{}, h, binding()))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "typed"}}, mcpregistry.Typed(func(context.Context, *mcp.CallToolRequest, params) (*mcp.CallToolResult, output, error) {
		typedCalls++
		return nil, output{}, nil
	}))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "raw", InputSchema: object}}, mcpregistry.BindRaw(rawOperation{}, rh, mcpregistry.RawBinding{Binding: binding(), MaxArgumentsBytes: 32}))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "custom", InputSchema: object}}, mcpregistry.Custom(func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		customCalls++
		return &mcp.CallToolResult{Content: []mcp.Content{}}, nil
	}))
	calls := func() []int { return []int{h.calls, typedCalls, rh.calls, customCalls} }
	cs := connect(t, r.Server())
	for _, name := range []string{"bind", "typed", "raw", "custom"} {
		t.Run(name, func(t *testing.T) {
			before := calls()
			denied := call(t, cs, name, params{Value: "x"}, mcp.Meta{"mode": "deny"})
			require.True(t, denied.IsError)
			require.Equal(t, "denied", textOf(t, denied))
			broken := call(t, cs, name, params{Value: "x"}, mcp.Meta{"mode": "nil-context"})
			require.True(t, broken.IsError)
			require.Equal(t, "internal error", textOf(t, broken))
			_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: params{Value: "x"}, Meta: mcp.Meta{"mode": "protocol"}})
			var wireErr *jsonrpc.Error
			require.ErrorAs(t, err, &wireErr)
			require.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
			require.Equal(t, before, calls(), "refused calls must not reach the handler")
			require.False(t, call(t, cs, name, params{Value: "x"}, nil).IsError, "the server keeps serving after a wiring bug")
		})
	}
	// Raw stages run before the argument cap, so unauthenticated callers learn nothing about it.
	oversized := call(t, cs, "raw", params{Value: strings.Repeat("x", 64)}, mcp.Meta{"mode": "deny"})
	require.Equal(t, "denied", textOf(t, oversized))
}

func TestNilMappedErrorsAreInternalErrors(t *testing.T) {
	broken := mcpregistry.Binding{Errors: mcpregistry.ErrorPolicy{Name: "broken", Map: func(context.Context, error) error { return nil }}}
	r := registry()
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "typed"}}, mcpregistry.Bind(operation{}, &host{}, broken))
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "raw", InputSchema: map[string]any{"type": "object"}}}, mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: broken}))
	cs := connect(t, r.Server())
	result := call(t, cs, "typed", params{Value: "fail"}, nil)
	require.True(t, result.IsError)
	require.Equal(t, "internal error", textOf(t, result))
	result = call(t, cs, "raw", json.RawMessage(`{"fail":true}`), nil)
	require.True(t, result.IsError)
	require.Equal(t, "internal error", textOf(t, result))
	require.False(t, call(t, cs, "typed", params{Value: "ok"}, nil).IsError, "the server keeps serving after a wiring bug")
}

func TestRawDefaultResultOmitsNullStructuredContent(t *testing.T) {
	r := registry()
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "raw", InputSchema: map[string]any{"type": "object"}}}, mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: binding()}))
	server := r.Server()
	var sent []string
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if method == "tools/call" {
				sent = append(sent, wireJSON(t, result))
			}
			return result, err
		}
	})
	cs := connect(t, server)
	for _, args := range []string{`{"void":true}`, `{"nil_pointer":true}`} {
		result := call(t, cs, "raw", json.RawMessage(args), nil)
		require.False(t, result.IsError)
		require.Nil(t, result.StructuredContent)
		require.Empty(t, result.Content)
	}
	require.Len(t, sent, 2)
	for _, wire := range sent {
		require.NotContains(t, wire, "structuredContent")
		require.Contains(t, wire, `"content":[]`)
	}
}

func TestRawFormatterFailuresUseErrorPolicy(t *testing.T) {
	r := registry()
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "raw", InputSchema: map[string]any{"type": "object"}}}, mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: binding(), Result: mcpregistry.ResultPolicy{Name: "broken-formatter", Format: func(context.Context, any) (*mcp.CallToolResult, error) {
		return nil, errors.New("formatter secret")
	}}}))
	result := call(t, connect(t, r.Server()), "raw", map[string]any{}, nil)
	require.True(t, result.IsError)
	require.Equal(t, "safe failure", textOf(t, result))
}

func TestSDKDeclarationErrorsAreRegistryPanics(t *testing.T) {
	custom := mcpregistry.Custom(func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	raw := mcpregistry.BindRaw(rawOperation{}, &rawHost{}, mcpregistry.RawBinding{Binding: binding()})
	typed := mcpregistry.Typed(func(context.Context, *mcp.CallToolRequest, params) (*mcp.CallToolResult, output, error) {
		return nil, output{}, nil
	})
	panicMessage := func(f func()) (message string) {
		defer func() {
			if p := recover(); p != nil {
				message = fmt.Sprint(p)
			}
		}()
		f()
		return ""
	}
	r := registry()
	for name, endpoint := range map[string]mcpregistry.Endpoint{"custom": custom, "raw": raw} {
		message := panicMessage(func() { r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: name}}, endpoint) })
		require.Equal(t, "mcpregistry: "+name+": raw and custom endpoints require an explicit input schema", message)
	}
	for name, endpoint := range map[string]mcpregistry.Endpoint{"custom": custom, "typed": typed} {
		message := panicMessage(func() {
			r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: name, InputSchema: map[string]any{"type": "string"}}}, endpoint)
		})
		require.True(t, strings.HasPrefix(message, "mcpregistry: "), message)
		require.Contains(t, message, `must have type "object"`)
	}
	// A rejected declaration reserves nothing.
	r.Register(mcpregistry.Spec[metadata]{Tool: mcp.Tool{Name: "custom", InputSchema: map[string]any{"type": "object"}}}, custom)
	require.Len(t, r.Inventory().Tools, 1)
}
