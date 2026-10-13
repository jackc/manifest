package mcphttp

import "github.com/modelcontextprotocol/go-sdk/mcp"

// StreamableHTTPOptions returns a new set of SDK handler options for an
// endpoint behind RequireBearer, a trusted reverse proxy, and tools that
// return one result without progress notifications. The caller may adjust the
// returned value.
func StreamableHTTPOptions() *mcp.StreamableHTTPOptions {
	return &mcp.StreamableHTTPOptions{
		// Required for protocol 2026-07-28. The SDK still handles legacy
		// initialize requests, without retaining HTTP session state.
		Stateless: true,
		// Each response is one JSON result rather than an event stream.
		JSONResponse: true,
		// Stop the handler's work when a modern client's request disconnects.
		PropagateRequestCancellation: true,
		// A trusted reverse proxy may forward the original public Host
		// header while connecting over loopback. The SDK's DNS-rebinding
		// protection rejects that combination, but the attack it guards
		// against is handled by the proxy's host routing and RequireBearer's
		// Origin check. Do not use these options without both.
		DisableLocalhostProtection: true,
	}
}
