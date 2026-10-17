package modelrpc

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWorkerAndModelWireDoNotDependOnProviderCredentials(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var inspect func(string)
	inspect = func(pkg string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		files := token.NewFileSet()
		packages, err := parser.ParseDir(files, filepath.Join(root, strings.TrimPrefix(pkg, "pwnmesh/")), func(info os.FileInfo) bool {
			return !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range packages {
			for _, file := range p.Files {
				for _, imp := range file.Imports {
					name, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					switch name {
					case "pwnmesh/internal/provider", "pwnmesh/internal/kimi", "pwnmesh/internal/modelconfig", "pwnmesh/internal/modelauth":
						t.Errorf("%s: worker model requests must not depend on %s", files.Position(imp.Pos()), name)
						continue
					}
					if strings.HasPrefix(name, "pwnmesh/") {
						inspect(name)
					}
				}
			}
		}
	}
	for _, pkg := range []string{"pwnmesh/internal/worker", "pwnmesh/internal/modelrpc", "pwnmesh/internal/modelwire"} {
		inspect(pkg)
	}
}
