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

// declares reports whether the file declares or assigns an identifier named Address.
func declaresAddress(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for _, name := range x.Names {
				if name.Name == "Address" {
					found = true
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					if l.Name == "Address" {
						found = true
					}
				case *ast.SelectorExpr:
					if l.Sel.Name == "Address" {
						found = true
					}
				}
			}
		case *ast.IncDecStmt:
			if s, ok := x.X.(*ast.SelectorExpr); ok && s.Sel.Name == "Address" {
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
		if declaresAddress(full) && base != "address_dev.go" && base != "address.go" {
			t.Errorf("%s declares or assigns Address; only address.go and address_dev.go may", path)
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
