package execution

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestUseCasesDoNotOwnTransportStorageOrTransactionFinalization(t *testing.T) {
	files := token.NewFileSet()
	packages, err := parser.ParseDir(files, ".", func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				name, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if name == "database/sql" || name == "net/http" || strings.Contains(name, "sqlstore") || strings.Contains(name, "sqlite") || strings.HasPrefix(name, "pwnmesh/internal/worker") || name == "pwnmesh/internal/server" {
					t.Errorf("%s: execution use cases must use Board operations, not %s", files.Position(imp.Pos()), name)
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if ok && (selector.Sel.Name == "Commit" || selector.Sel.Name == "Rollback") {
					t.Errorf("%s: only Store may finalize a transaction", files.Position(call.Pos()))
				}
				return true
			})
		}
	}
}
