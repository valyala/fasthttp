package fasthttp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestErrorStringsLowercase(t *testing.T) {
	fset := token.NewFileSet()

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch path {
			case ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Errorf("%s: parse failed: %v", path, err)
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if isErrorStringCall(n) && len(n.Args) > 0 {
					checkErrorStringExpr(t, fset, n.Args[0])
				}
			case *ast.FuncDecl:
				if isErrorMethod(n) {
					checkErrorMethod(t, fset, n)
					return false
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isErrorStringCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "errors" && sel.Sel.Name == "New" ||
		pkg.Name == "fmt" && sel.Sel.Name == "Errorf"
}

func isErrorMethod(fn *ast.FuncDecl) bool {
	if fn.Name.Name != "Error" || fn.Type.Params.NumFields() != 0 || fn.Type.Results.NumFields() != 1 {
		return false
	}
	ident, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
	return ok && ident.Name == "string"
}

func checkErrorMethod(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, result := range ret.Results {
			checkErrorStringExpr(t, fset, result)
			call, ok := result.(*ast.CallExpr)
			if ok && isFmtSprintfCall(call) && len(call.Args) > 0 {
				checkErrorStringExpr(t, fset, call.Args[0])
			}
		}
		return true
	})
}

func isFmtSprintfCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "fmt" && sel.Sel.Name == "Sprintf"
}

func checkErrorStringExpr(t *testing.T, fset *token.FileSet, expr ast.Expr) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return
		}
		s, err := strconv.Unquote(expr.Value)
		if err != nil {
			t.Errorf("%s: cannot unquote %s: %v", fset.Position(expr.Pos()), expr.Value, err)
			return
		}
		if containsUppercaseASCII(s) {
			t.Errorf("%s: error string %q contains uppercase ASCII", fset.Position(expr.Pos()), s)
		}
	case *ast.BinaryExpr:
		checkErrorStringExpr(t, fset, expr.X)
		checkErrorStringExpr(t, fset, expr.Y)
	case *ast.ParenExpr:
		checkErrorStringExpr(t, fset, expr.X)
	}
}

func containsUppercaseASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			return true
		}
	}
	return false
}
