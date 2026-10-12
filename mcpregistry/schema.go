package mcpregistry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Use the SDK's encoding/json wire semantics for SDK-owned metadata and raw
// result content. This does not choose an application's argument decoder.
func cloneTool(tool mcp.Tool) mcp.Tool {
	data, err := json.Marshal(tool)
	if err != nil {
		panic(fmt.Sprintf("mcpregistry: tool metadata: %v", err))
	}
	var copy mcp.Tool
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber() // Keep integer schema bounds and extension numbers exact.
	if err := decoder.Decode(&copy); err != nil {
		panic(fmt.Sprintf("mcpregistry: tool metadata: %v", err))
	}
	return copy
}

// Mirror only the SDK's schema inference for the inventory, never its execution
// pipeline. In particular, preserve nil schemas in the actual AddTool call so
// the SDK keeps its inferred-pointer-output zero-value behavior.
func describeTyped[In, Out any](tool *mcp.Tool) {
	if tool.InputSchema == nil {
		if reflect.TypeFor[In]() == reflect.TypeFor[any]() {
			tool.InputSchema = &jsonschema.Schema{Type: "object"}
		} else {
			tool.InputSchema = inferSchema[In]()
		}
	}
	if tool.OutputSchema == nil && reflect.TypeFor[Out]() != reflect.TypeFor[any]() {
		tool.OutputSchema = inferSchema[Out]()
	}
}

func inferSchema[T any]() *jsonschema.Schema {
	t := reflect.TypeFor[T]()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	schema, err := jsonschema.ForType(t, &jsonschema.ForOptions{})
	if err != nil {
		panic(fmt.Sprintf("mcpregistry: schema for %v: %v", t, err))
	}
	return schema
}

func jsonResult(value any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(data) == "null" {
		// As the SDK does for nil typed output, omit structured content. A raw
		// null would otherwise be sent as "structuredContent": null.
		return &mcp.CallToolResult{Content: []mcp.Content{}}, nil
	}
	return &mcp.CallToolResult{StructuredContent: json.RawMessage(data), Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
}
