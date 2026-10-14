package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestProductionServerUsesSemanticBoardOperations(t *testing.T) {
	files := token.NewFileSet()
	packages, err := parser.ParseDir(files, ".", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if path == "database/sql" || strings.Contains(path, "sqlite") {
					t.Errorf("%s: Server must use Board operations, not a SQL dependency", files.Position(imp.Pos()))
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch selector.Sel.Name {
				case "Exec", "ExecContext", "QueryRow", "QueryRowContext", "QueryContext", "Prepare", "PrepareContext", "Commit", "Rollback":
					t.Errorf("%s: raw transaction operation belongs in Board", files.Position(call.Pos()))
				case "Query":
					// URL.Query has no arguments; SQL queries require a statement.
					if len(call.Args) != 0 {
						t.Errorf("%s: raw SQL query belongs in Board", files.Position(call.Pos()))
					}
				}
				return true
			})
		}
	}
}
