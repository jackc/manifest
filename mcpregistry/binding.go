package mcpregistry

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Operation is structural and enters the application's normal action pipeline.
// This package imports neither manifest/action nor any application package.
type Operation[D, P, R any] interface {
	Name() string
	Description() string
	Call(context.Context, D, P) (R, error)
}

// RawOperation ties the invoked operation to its descriptive identity. The
// application adapter owns decoding and validation; arguments are never remarshaled.
type RawOperation[D any] interface {
	Name() string
	Description() string
	InvokeJSON(context.Context, D, []byte) (any, error)
}

// ErrorPolicy translates operation failures into client-safe tool errors. SDK
// input validation and context-stage failures do not pass through this policy.
// A mapper returning nil is reported to the client as an internal error.
type ErrorPolicy struct {
	Name string
	Map  func(context.Context, error) error
}

type Binding struct {
	Namespace string
	Errors    ErrorPolicy
}

// ResultPolicy formats a raw operation's result, optionally providing custom
// text alongside structured data. Errors must be safe to expose to clients.
type ResultPolicy struct {
	Name   string
	Format func(context.Context, any) (*mcp.CallToolResult, error)
}

type RawBinding struct {
	Binding
	MaxArgumentsBytes int          // zero means no binding-level limit
	Result            ResultPolicy // zero value returns structured JSON and the same JSON as text; null results have neither
}

func Bind[D, P, R any](operation Operation[D, P, R], dependencies D, binding Binding) Endpoint {
	if operation == nil || operation.Name() == "" {
		panic("mcpregistry: operation is required")
	}
	validateErrors(binding.Errors)
	ep := Typed(func(ctx context.Context, _ *mcp.CallToolRequest, params P) (*mcp.CallToolResult, R, error) {
		result, err := operation.Call(ctx, dependencies, params)
		if err != nil {
			var zero R
			return nil, zero, mapError(ctx, binding.Errors, err)
		}
		return nil, result, nil
	})
	ep.description.Kind = "action"
	ep.description.Binding = &BindingDescription{Namespace: binding.Namespace, Action: operation.Name(), Description: operation.Description(), Errors: binding.Errors.Name, Result: "sdk-typed"}
	ep.description.Gaps = []string{"operation internals, custom codecs, error policy behavior, and transitive action calls are not inspected"}
	return ep
}

// BindRaw advertises explicit schemas but delegates their enforcement to the
// operation. This supports inputs whose original number tokens and extension
// fields must survive, and operations reporting individual row validation errors.
func BindRaw[D any](operation RawOperation[D], dependencies D, binding RawBinding) Endpoint {
	if operation == nil || operation.Name() == "" {
		panic("mcpregistry: operation is required")
	}
	validateErrors(binding.Errors)
	if binding.MaxArgumentsBytes < 0 {
		panic("mcpregistry: negative argument limit")
	}
	result := binding.Result
	if result.Name == "" && result.Format == nil {
		result = ResultPolicy{Name: "json-text-and-structured", Format: func(_ context.Context, value any) (*mcp.CallToolResult, error) { return jsonResult(value) }}
	}
	if result.Name == "" || result.Format == nil {
		panic("mcpregistry: result policy name and formatter are required")
	}
	ep := Custom(func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		if binding.MaxArgumentsBytes > 0 && len(args) > binding.MaxArgumentsBytes {
			return toolError(fmt.Errorf("arguments exceed %d bytes", binding.MaxArgumentsBytes)), nil
		}
		value, err := operation.InvokeJSON(ctx, dependencies, args)
		if err != nil {
			return toolError(mapError(ctx, binding.Errors, err)), nil
		}
		out, err := result.Format(ctx, value)
		if err != nil {
			return toolError(mapError(ctx, binding.Errors, err)), nil
		}
		return out, nil
	})
	ep.description.Kind = "raw-action"
	ep.description.Arguments = "raw-json; operation-owned decoding and validation"
	ep.description.Binding = &BindingDescription{Namespace: binding.Namespace, Action: operation.Name(), Description: operation.Description(), Errors: binding.Errors.Name, Result: result.Name, MaxArgumentsBytes: binding.MaxArgumentsBytes}
	ep.description.Gaps = []string{"advertised schemas are not enforced by the binding; operation validation, result formatting, error policy, and transitive action calls are not inspected"}
	return ep
}

// Typed retains the SDK's generic schema inference, argument validation, output
// validation, and content handling. Handler internals are opaque to the registry.
func Typed[In, Out any](handler mcp.ToolHandlerFor[In, Out]) Endpoint {
	if handler == nil {
		panic("mcpregistry: nil typed handler")
	}
	return Endpoint{
		description: Description{Kind: "custom-typed", Arguments: "sdk-typed", Gaps: []string{"handler internals and transitive action calls are not inspected"}},
		describe:    describeTyped[In, Out],
		install: func(server *mcp.Server, tool *mcp.Tool, stages []Stage) {
			mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
				ctx, err := prepare(ctx, req, stages)
				if err != nil {
					var zero Out
					return nil, zero, err
				}
				return handler(ctx, req, in)
			})
		},
	}
}

// Custom retains a raw SDK handler, including custom content and protocol
// errors. Explicit schemas are required as by Server.AddTool; schema validation,
// decoding, error translation, and result construction belong to the handler.
func Custom(handler mcp.ToolHandler) Endpoint {
	if handler == nil {
		panic("mcpregistry: nil custom handler")
	}
	return Endpoint{
		description: Description{Kind: "custom", Arguments: "raw-json; handler-owned decoding and validation", Gaps: []string{"handler internals, schema enforcement, and transitive action calls are not inspected"}},
		install: func(server *mcp.Server, tool *mcp.Tool, stages []Stage) {
			server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				ctx, err := prepare(ctx, req, stages)
				if err != nil {
					// Match the SDK's typed handling: protocol errors stay protocol errors.
					if wireErr, ok := err.(*jsonrpc.Error); ok {
						return nil, wireErr
					}
					return toolError(err), nil
				}
				return handler(ctx, req)
			})
		},
	}
}

func validateErrors(policy ErrorPolicy) {
	if policy.Name == "" || policy.Map == nil {
		panic("mcpregistry: error policy name and mapper are required")
	}
}

func mapError(ctx context.Context, policy ErrorPolicy, err error) error {
	if err = policy.Map(ctx, err); err == nil {
		return errInternal
	}
	return err
}

func toolError(err error) *mcp.CallToolResult {
	result := &mcp.CallToolResult{}
	result.SetError(err)
	return result
}
