package protocol_test

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Follow production imports, including Linux files when a developer invokes
// this test elsewhere. A facade may not hide a storage dependency one hop away.
func TestTransitiveDependencyBoundaries(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	context := build.Default
	context.GOOS = "linux"
	cache := map[string][]string{}
	imports := func(pkg string) []string {
		if cached, ok := cache[pkg]; ok {
			return cached
		}
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, "pwnmesh/")))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var dependencies []string
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			matches, err := context.MatchFile(dir, entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !matches {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, dependency := range file.Imports {
				name, err := strconv.Unquote(dependency.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				dependencies = append(dependencies, name)
			}
		}
		cache[pkg] = dependencies
		return dependencies
	}
	for _, test := range []struct {
		name      string
		forbidden []string
		storage   bool
	}{
		{"protocol", []string{"board", "worker", "config"}, true},
		{"worker", []string{"board", "config"}, true},
		{"dispatcher", []string{"board", "worker"}, true},
		{"docker", []string{"board", "worker"}, true},
		{"board/model", []string{"board", "worker", "protocol", "provider", "agent", "server", "dispatcher", "docker", "config", "process", "tools"}, true},
		{"board/internal/projection", []string{"board", "board/internal/sqlstore", "worker", "protocol", "provider", "agent", "server", "dispatcher", "docker", "config", "process", "tools"}, true},
		{"worker/dag", []string{"board", "board/model", "worker", "protocol", "provider", "agent"}, true},
		{"worker/internal/phase", []string{"board", "board/model", "worker", "protocol", "provider", "agent", "config", "process", "tools"}, true},
		{"worker/internal/journal", []string{"board", "board/model", "worker", "protocol", "provider", "config", "process", "tools"}, true},
		{"board/internal/sqlstore", []string{"board", "worker", "protocol", "provider", "agent", "server", "dispatcher", "docker", "config"}, false},
		{"server", []string{"worker"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := map[string]bool{}
			var walk func(string, []string)
			walk = func(pkg string, chain []string) {
				if seen[pkg] {
					return
				}
				seen[pkg] = true
				for _, dependency := range imports(pkg) {
					path := append(append([]string{}, chain...), dependency)
					blocked := test.storage && (dependency == "database/sql" || strings.HasPrefix(dependency, "modernc.org/"))
					for _, name := range test.forbidden {
						blocked = blocked || dependency == "pwnmesh/internal/"+name
					}
					if blocked {
						t.Errorf("forbidden production dependency: %s", strings.Join(path, " -> "))
						continue
					}
					if strings.HasPrefix(dependency, "pwnmesh/") {
						walk(dependency, path)
					}
				}
			}
			pkg := "pwnmesh/internal/" + test.name
			walk(pkg, []string{pkg})
		})
	}
}
