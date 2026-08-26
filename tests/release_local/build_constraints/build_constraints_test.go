package build_constraints

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckLingerHasSingleImplementationPerTarget(t *testing.T) {
	t.Parallel()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}
	daemonDir := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "..", "daemon"))

	targets := []struct {
		goos   string
		goarch string
		want   string
	}{
		{goos: "linux", goarch: "amd64", want: "systemd.go"},
		{goos: "darwin", goarch: "arm64", want: "launchd.go"},
		{goos: "windows", goarch: "amd64", want: "windows.go"},
		{goos: "freebsd", goarch: "amd64", want: "unsupported.go"},
	}

	entries, err := os.ReadDir(daemonDir)
	if err != nil {
		t.Fatalf("read daemon directory: %v", err)
	}

	for _, target := range targets {
		target := target
		t.Run(target.goos+"_"+target.goarch, func(t *testing.T) {
			ctx := build.Default
			ctx.GOOS = target.goos
			ctx.GOARCH = target.goarch
			ctx.CgoEnabled = false

			var implementations []string
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}

				matches, err := ctx.MatchFile(daemonDir, name)
				if err != nil {
					t.Fatalf("match build constraints for %s: %v", name, err)
				}
				if !matches {
					continue
				}

				path := filepath.Join(daemonDir, name)
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
				if err != nil {
					t.Fatalf("parse %s: %v", name, err)
				}
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if ok && fn.Recv == nil && fn.Name.Name == "CheckLinger" {
						implementations = append(implementations, name)
					}
				}
			}

			if len(implementations) != 1 || implementations[0] != target.want {
				t.Fatalf("CheckLinger implementations = %v, want exactly [%s]", implementations, target.want)
			}
		})
	}
}
