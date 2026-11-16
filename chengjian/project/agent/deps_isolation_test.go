package agent

// deps_isolation_test.go 是 T-25 的守卫（实例依赖接线 / 多实例隔离）。
//
// 背景：一次独立审查提出「agent 的工具/提示词等仍依赖捕获宿主的包级可变回调」。
// 复核结论是**这条具体判断不成立**——agent 包内没有任何包级可变回调，
// 所有依赖都是每运行时通过 SetXxx 注入的字段。但「现在没有」和「以后不会有」
// 是两件事，而这类回归一旦发生极难发现：它不会编译失败，只会在某个多任务
// 并发场景下让两个任务互相看见对方的工具或提示词。
//
// 所以这里把它变成一条会失败的测试，而不是一段文档里的承诺。
//
// 为什么这几个包在扫描范围内：它们都是「每运行时实例化、可并存多个」的东西
// ——planner/worker 各有一份、每个任务一套判据状态、每个任务一个身份存储。
// 包级可变状态一旦出现在这些包里，多实例隔离就自动失效。
// 不在范围内的是纯配置/纯函数包（如 recon、guard 的常量表），它们没有
// 「实例」概念，包级只读数据是合理的。
//
// 用 AST 而不是正则：正则分不清「函数内的 var」和「包级 var」，而前者
// （`var err error`、`var out []string`）在每个函数里都有。误报的守卫
// 等于没有守卫——它会被加进忽略列表，然后连真的违规也一起忽略。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// perRuntimePkgs 是必须保持「无包级可变函数状态」的包。
var perRuntimePkgs = []string{
	"agent", "planning", "webtest", "authstate", "capability", "observation",
}

// pkgFile 是一个待扫描的源文件。
type pkgFile struct {
	path string
	file *ast.File
	fset *token.FileSet
}

// scanPerRuntimePkgs 解析范围内的所有非测试源文件。
func scanPerRuntimePkgs(t *testing.T) []pkgFile {
	t.Helper()
	out := []pkgFile{}
	for _, pkg := range perRuntimePkgs {
		dir := filepath.Join("..", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读取包目录 %s 失败: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", path, err)
			}
			out = append(out, pkgFile{path: path, file: f, fset: fset})
		}
	}
	return out
}

// isFuncTyped 判断一个包级 var 是否持有函数值。
func isFuncTyped(spec *ast.ValueSpec) bool {
	if _, ok := spec.Type.(*ast.FuncType); ok {
		return true
	}
	for _, v := range spec.Values {
		if _, ok := v.(*ast.FuncLit); ok {
			return true
		}
	}
	return false
}

// packageVarNames 收集包级 var 名，供 Set 方法检查使用。
func packageVarNames(pf pkgFile) map[string]bool {
	names := map[string]bool{}
	for _, decl := range pf.file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				names[n.Name] = true
			}
		}
	}
	return names
}

// TestNoPackageLevelMutableCallbacks 钉住「依赖只能通过每运行时的字段注入」。
func TestNoPackageLevelMutableCallbacks(t *testing.T) {
	violations := []string{}
	for _, pf := range scanPerRuntimePkgs(t) {
		for _, decl := range pf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || !isFuncTyped(vs) {
					continue
				}
				for _, n := range vs.Names {
					violations = append(violations, pf.fset.Position(n.Pos()).String()+"  var "+n.Name)
				}
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("检测到包级函数变量（会让多实例隔离失效：两个任务共享同一个回调）:\n%s\n"+
			"改法：做成结构体字段，由 SetXxx 在每次运行时注入——现有代码全部是这种形态。",
			strings.Join(violations, "\n"))
	}
}

// TestSettersDoNotWritePackageState 钉住「注入只作用于自己的接收者」。
//
// 守的是一个很容易被顺手破坏的形状：某个 SetXxx 为了给测试提供默认值，
// 顺手同时写了一个包级变量。编译完全通过，但所有实例都会读到同一个值，
// 而且测试里看不出来（测试通常只建一个实例）。
func TestSettersDoNotWritePackageState(t *testing.T) {
	for _, pf := range scanPerRuntimePkgs(t) {
		pkgVars := packageVarNames(pf)
		if len(pkgVars) == 0 {
			continue
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				return true
			}
			if !strings.HasPrefix(fn.Name.Name, "Set") {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				as, ok := inner.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok {
						continue
					}
					// 接收者字段赋值是 *ast.SelectorExpr，不会走到这里；
					// 裸标识符只可能是局部变量或包级变量。
					if pkgVars[id.Name] {
						t.Errorf("%s 里的 %s 直接写了包级变量 %q —— 注入必须只作用于接收者，否则多实例共享",
							pf.fset.Position(as.Pos()), fn.Name.Name, id.Name)
					}
				}
				return true
			})
			return true
		})
	}
}
