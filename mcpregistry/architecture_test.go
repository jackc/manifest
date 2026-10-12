package mcpregistry_test

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLibraryHasNoApplicationOrOtherManifestDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			for _, prefix := range []string{"github.com/jackc/logger4life/", "github.com/jackc/fam/", "github.com/jackc/manifest/"} {
				require.False(t, strings.HasPrefix(path, prefix), "%s imports %s", entry.Name(), path)
			}
		}
	}
}
