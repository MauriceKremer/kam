package api

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestTransportOnlyImports enforces the API package's dependency rule
// (FeatureUnifiedApi.md §4.1): the transport layer implements no KAM
// capability and imports nothing from this repository except internal/models,
// the shared data types. Test files are exempt; they wire fakes and real
// rings deliberately.
func TestTransportOnlyImports(t *testing.T) {
	const allow = "kam.bytecoder.nl/internal/models"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "kam.bytecoder.nl/") && path != allow {
				t.Errorf("%s imports %q: the API package may import only %s from this repository", name, path, allow)
			}
		}
	}
}
