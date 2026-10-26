package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEveryRPCSharesTheConfigRead holds every RPC entry point to one config
// read: an exported Runtime method that uses its context must pass it through
// kandev.ShareConfig, or each adapter call it makes pays its own GetConfig
// round trip again.
func TestEveryRPCSharesTheConfigRead(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			method, ok := decl.(*ast.FuncDecl)
			if !ok || !isRuntimeRPC(method) {
				continue
			}
			checked++
			require.Truef(t, callsShareConfig(method.Body), "%s: Runtime.%s uses its context without kandev.ShareConfig",
				fset.Position(method.Pos()), method.Name.Name)
		}
	}
	require.NotZero(t, checked, "found no Runtime RPC methods; has the receiver been renamed?")
}

// isRuntimeRPC reports an exported method on *Runtime whose first parameter
// is a named context.
func isRuntimeRPC(method *ast.FuncDecl) bool {
	if method.Recv == nil || !method.Name.IsExported() || len(method.Recv.List) != 1 {
		return false
	}
	star, ok := method.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if receiver, ok := star.X.(*ast.Ident); !ok || receiver.Name != "Runtime" {
		return false
	}
	params := method.Type.Params.List
	if len(params) == 0 || len(params[0].Names) == 0 || params[0].Names[0].Name == "_" {
		return false
	}
	selector, ok := params[0].Type.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "Context"
}

func callsShareConfig(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "ShareConfig" {
			found = true
		}
		return !found
	})
	return found
}
