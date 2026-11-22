package main

// 参考实现不得手抄内核词表（issue #358）。
//
// examples/ 是「给宿主抄的契约」：宿主会把这里的装配整段抄走。参考实现若把
// contract 的终态判据抄成本地 switch，这份手抄当前恰好一致——contract 新增
// 一个终态时它不会跟着变，也没有任何测试会红。故用 AST 判据钉住：
//
//  1. 终态闸必须直接注入 contract.IsTerminalStatus（不得本地定义）；
//  2. 整个 examples/ 目录不得存在「本地手抄的终态判定函数」（返回 bool、
//     switch 里列了 ≥2 个 contract.Status* 取值）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGuardInjectsContractPredicate newRuntime 的终态闸必须直接引用
// contract.IsTerminalStatus。
//
// 反向验证：把手抄的 isTerminalStatus 改回来并注入，本用例转红。
func TestGuardInjectsContractPredicate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runtime.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 runtime.go: %v", err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "commitgate" || sel.Sel.Name != "NewGuard" {
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		apkg, ok := arg.X.(*ast.Ident)
		if ok && apkg.Name == "contract" && arg.Sel.Name == "IsTerminalStatus" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("commitgate.NewGuard 必须直接注入 contract.IsTerminalStatus——参考实现不得手抄终态词表")
	}
}

// TestExamplesHaveNoHandCopiedTerminalPredicate examples/ 全目录不得存在本地
// 手抄的终态判定函数（issue #358 验收 4：判据推广到 examples/ 全目录）。
func TestExamplesHaveNoHandCopiedTerminalPredicate(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !returnsBool(fn) {
				continue
			}
			if n := contractStatusCaseCount(fn); n >= 2 {
				t.Errorf("%s: 函数 %s 手抄了终态判定词表（switch 里 %d 个 contract.Status* 取值）——必须调用 contract.IsTerminalStatus",
					path, fn.Name.Name, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 examples/: %v", err)
	}
}

// returnsBool 函数是否只返回一个 bool。
func returnsBool(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}
	id, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "bool"
}

// contractStatusCaseCount 数函数体 switch case 里引用的 contract.Status* 个数。
func contractStatusCaseCount(fn *ast.FuncDecl) int {
	n := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		cc, ok := node.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range cc.List {
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "contract" && strings.HasPrefix(sel.Sel.Name, "Status") {
				n++
			}
		}
		return true
	})
	return n
}
