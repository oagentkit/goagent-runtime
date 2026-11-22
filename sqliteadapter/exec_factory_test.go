// exec_factory_test.go —— 执行位置的构造点只有一个（go/ast 判据）。
//
// # 判据
//
// 生产代码里 `exec{...}` 复合字面量**只允许出现在共享工厂所在的文件**
// （定义 Store.execIn 的那个文件）；别处出现 ⇒ 某个面手搓了一份执行位置 ⇒
// 迟早有一份漏 clock ⇒ 注入时钟在该面上静默失效 ⇒ 时钟口径漂移且门禁不红
// （Task 2 评审 M2、Task 3 评审同型）。
//
// # 为什么走 go/ast 不走字符串匹配
//
// 复合字面量有多行 / 单行等书写形态，且注释里出现 exec{ 也会误报——仓库纪律：
// 结构性判定走 AST，不逐行看文本。
//
// # 为什么要钉住（非空转）
//
// 「没有字面量 ⇒ 判据无对象」会让这条用例变成空转：所以同时判「共享工厂必须
// 存在」且「构造事务内执行位置的地方就是工厂所在的地方」。
package sqliteadapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecLiteralsOnlyInSharedFactoryFile 生产代码里的执行位置只从共享工厂来。
func TestExecLiteralsOnlyInSharedFactoryFile(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读包目录失败: %v", err)
	}

	factoryFile := ""
	litFiles := map[string]bool{}
	txLitFiles := map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s 失败: %v", name, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv != nil && len(node.Recv.List) == 1 &&
					methodRecvTypeName(node.Recv.List[0].Type) == "Store" &&
					node.Name.Name == "execIn" {
					factoryFile = name
				}
			case *ast.CompositeLit:
				id, ok := node.Type.(*ast.Ident)
				if !ok || id.Name != "exec" {
					return true
				}
				litFiles[name] = true
				if compositeHasKey(node, "tx") {
					txLitFiles[name] = true
				}
			}
			return true
		})
	}

	if factoryFile == "" {
		t.Fatalf("共享工厂 Store.execIn 不存在——执行位置的构造点判据失去依据（静默空转）")
	}
	for name := range litFiles {
		if name != factoryFile {
			t.Fatalf("生产代码里的 exec 字面量只允许出现在共享工厂所在文件（%s），%s 里出现了一处 ⇒ 某个面手搓了执行位置 ⇒ 迟早漏 clock ⇒ 注入时钟静默失效且门禁不红", factoryFile, name)
		}
	}
	for name := range txLitFiles {
		if name != factoryFile {
			t.Fatalf("构造事务内执行位置（带 tx 的 exec 字面量）的地方必须就是共享工厂所在文件（%s），实得 %s", factoryFile, name)
		}
	}
	if len(txLitFiles) == 0 {
		t.Fatalf("共享工厂所在文件里没有构造事务内执行位置——判据对象消失（本用例变成空转）")
	}
}

// methodRecvTypeName 取接收者类型名（*Store → "Store"）。
func methodRecvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return methodRecvTypeName(t.X)
	}
	return ""
}

// compositeHasKey 判断复合字面量里有没有某个键（exec{db: ..., tx: ...} 的 tx）。
func compositeHasKey(cl *ast.CompositeLit, key string) bool {
	for _, el := range cl.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
			return true
		}
	}
	return false
}
