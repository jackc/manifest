package mcpregistry_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/jackc/manifest/mcpregistry"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyAnnotations(t *testing.T) {
	a := mcpregistry.ReadOnlyAnnotations()
	data, err := json.Marshal(a)
	require.NoError(t, err)
	require.JSONEq(t, `{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false}`, string(data))

	// Each call returns its own value, so a caller may adjust one tool's.
	*a.DestructiveHint = true
	a.ReadOnlyHint = false
	require.Equal(t, mcpregistry.ReadOnlyAnnotations(), &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)})
}
