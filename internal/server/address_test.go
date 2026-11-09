package server_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const root = "../.."

// compiledIn names each compiled in origin and the release and dev files
// that alone may declare it.
var compiledIn = map[string][2]string{
	"Address":     {"address.go", "address_dev.go"},
	"TroveOrigin": {"trove.go", "trove_dev.go"},
}

// declares reports whether the file declares or assigns an identifier named name.
func declares(f *ast.File, name string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for _, n := range x.Names {
				if n.Name == name {
					found = true
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					if l.Name == name {
						found = true
					}
				case *ast.SelectorExpr:
					if l.Sel.Name == name {
						found = true
					}
				}
			}
		case *ast.IncDecStmt:
			if s, ok := x.X.(*ast.SelectorExpr); ok && s.Sel.Name == name {
				found = true
			}
		}
		return true
	})
	return found
}

func TestReleaseAddressIsConstant(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		full, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		base := filepath.Base(path)
		inServer := filepath.Base(filepath.Dir(path)) == "server"
		// Each compiled in origin is declared in its two files and nowhere else.
		for name, files := range compiledIn {
			if declares(full, name) && (!inServer || (base != files[0] && base != files[1])) {
				t.Errorf("%s declares or assigns %s; only %s and %s may", path, name, files[0], files[1])
			}
		}
		if inServer {
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if p == "os" || p == "flag" {
					t.Errorf("%s imports %q; internal/server must not", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReleaseAddressValueIsFixed(t *testing.T) {
	src, err := os.ReadFile("address.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "//go:build !dev") {
		t.Error("address.go must carry the !dev build tag")
	}
	if !strings.Contains(s, `const Address = "https://mcp.smrit.ai"`) {
		t.Error("release Address must be the constant https://mcp.smrit.ai")
	}
}

func TestReleaseTroveOriginValueIsFixed(t *testing.T) {
	src, err := os.ReadFile("trove.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "//go:build !dev") {
		t.Error("trove.go must carry the !dev build tag")
	}
	if !strings.Contains(s, `const TroveOrigin = "https://trove.smrit.ai"`) {
		t.Error("release TroveOrigin must be the constant https://trove.smrit.ai")
	}
	dev, err := os.ReadFile("trove_dev.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dev), "//go:build dev") || !strings.Contains(string(dev), "var TroveOrigin string") {
		t.Error("trove_dev.go must carry the dev build tag and an unset TroveOrigin for ldflags")
	}
}

func TestMakefileDevSetsBothOrigins(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "-tags dev") {
			if !strings.Contains(line, "internal/server.Address=") || !strings.Contains(line, "internal/server.TroveOrigin=") {
				t.Fatalf("the dev build sets only part of the compiled in origins: %s", line)
			}
			return
		}
	}
	t.Fatal("the Makefile has no dev build")
}
