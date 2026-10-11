package httpregistry_test

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLibraryHasNoApplicationDependencies(t *testing.T) {
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
			require.False(t, strings.HasPrefix(path, "github.com/jackc/logger4life/"), "%s imports application code %s", entry.Name(), path)
			require.False(t, strings.HasPrefix(path, "github.com/jackc/manifest/"), "%s imports another Manifest package %s", entry.Name(), path)
		}
	}
}
