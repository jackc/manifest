package mcpregistry

import "github.com/modelcontextprotocol/go-sdk/mcp"

// ReadOnlyAnnotations returns new annotations for a tool that only reads
// application state: read-only, not destructive, idempotent, and closed-world.
// They are hints to clients, not evidence of the handler's behavior.
func ReadOnlyAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: new(false),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
	}
}
