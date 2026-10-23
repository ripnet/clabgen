package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The jsonschema tag splits on commas, so a comma in its description= cuts
// the description short; such descriptions go in jsonschema_description.
func TestSchemaDescriptionsNotTruncated(t *testing.T) {
	keyword := func(s string) bool {
		k, _, ok := strings.Cut(s, "=")
		return ok && k != "" && !strings.ContainsAny(k, " ()")
	}
	for _, root := range []string{"../../protocols", "../../internal"} {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				fl, ok := n.(*ast.Field)
				if !ok || fl.Tag == nil {
					return true
				}
				raw, _ := strconv.Unquote(fl.Tag.Value)
				_, desc, ok := strings.Cut(reflect.StructTag(raw).Get("jsonschema"), "description=")
				if !ok {
					return true
				}
				for _, part := range strings.Split(desc, ",")[1:] {
					if !keyword(part) {
						t.Errorf("%s: description cut at a comma: %q", path, desc)
						break
					}
				}
				return true
			})
			return nil
		})
	}
}
