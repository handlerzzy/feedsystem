package apierror_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHandlerEchoesRawServerError 是一道防回归门禁：禁止把原始错误直接回显
// 给客户端，只要该响应的状态码可能落在 5xx。
//
// 为什么需要它——这是一个真实发生过的漏检：第三批的收尾报告声称"500 站点归零"，
// 但它统计的是字面量 `c.JSON(500,`（结果为 0）。真实情况是
// `http.StatusInternalServerError` 有 27 处，另有 65 处形如
//
//	c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
//
// 的站点——**分类器只决定状态码，消息永远取 err.Error()**。于是只要分类落到
// 5xx，SQL 语句与表名、文件路径、上游地址这类内部细节就照样吐给客户端：
// 状态码是对的，泄漏依然发生。用字面量去 grep，整类问题都会被漏掉。
//
// 本门禁改用 AST 判断语义：只要出现 `c.JSON(<status>, gin.H{"error": <x>.Error()})`
// 且 <status> 不是"确定小于 500"的常量，就判为泄漏。正确的写法是：
//
//	apierror.Respond(c, err)                      // 状态码交给分类器，5xx 只回通用文案
//	apierror.RespondWith(c, status, msg, cause)   // 显式状态码与文案，cause 只进日志
//
// 已知边界：自定义的状态码常量（非 net/http 且非字面量）无法在此判定，
// 会被保守地判为违规。遇到时请改用 Respond/RespondWith。
func TestNoHandlerEchoesRawServerError(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// mock 是生成的；testdata 与本门禁无关；隐藏目录（.git/.run 等）跳过。
			if name == "mock" || name == "testdata" || name == "node_modules" ||
				(strings.HasPrefix(name, ".") && name != "." && name != "..") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// 解析不了的文件（理论上不该有）不静默跳过，避免门禁形同虚设。
			offenders = append(offenders, fmt.Sprintf("%s: 无法解析，门禁未覆盖 (%v)", path, perr))
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// gin.Context 上所有"把 body 交给客户端"的方法都要覆盖。
			// 只查 c.JSON 会漏掉 AbortWithStatusJSON 这类写法——
			// 它们同样会把 err.Error() 回显出去（且 Abort 变体还多一层
			// "必须保留 Abort 语义"的约束，更容易被漏掉）。
			if !responseMethods[sel.Sel.Name] {
				return true
			}
			for _, pos := range detectRawErrorEchoes(call) {
				p := fset.Position(pos)
				rel, rerr := filepath.Rel(root, p.Filename)
				if rerr != nil {
					rel = p.Filename
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d (%s)", rel, p.Line, sel.Sel.Name))
			}
			return true
		})
		return nil
	})

	if walkErr != nil {
		t.Fatalf("遍历源码失败: %v", walkErr)
	}
	if len(offenders) > 0 {
		t.Errorf("发现 %d 处把原始错误直接回显给客户端的响应：\n  %s\n\n"+
			"请改用统一出口：\n"+
			"  apierror.Respond(c, err)                          // 状态码交给分类器，文案 curated\n"+
			"  apierror.RespondBinding(c, err)                   // 绑定失败：固定 400 + curated 文案\n"+
			"  apierror.RespondWith(c, status, msg, cause)        // 显式状态码与文案，cause 只进日志\n"+
			"原始错误只应进日志，不应进响应体（4xx 也一样：绑定错误的原文含 Go 类型与字段名）。",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// responseMethods 列出 gin.Context 上"把响应体交给客户端"的方法。
// 门禁必须覆盖全部，否则换一种写法就能绕过。
var responseMethods = map[string]bool{
	"JSON":                true,
	"AbortWithStatusJSON": true,
	"IndentedJSON":        true,
	"SecureJSON":          true,
	"JSONP":               true,
	"AsciiJSON":           true,
	"PureJSON":            true,
}

// detectRawErrorEchoes 判断一次响应调用是否把原始错误回显给了客户端。
//
// 抽成独立函数是为了让门禁本身**可被自测**：门禁是安全防线，它自己写坏了
// （比如漏掉某种响应方法、或错误地放行某类状态码）会让整类泄漏静默复活。
// 见 TestGateDetectsRawErrorEchoes。
//
// 判定规则：只要是 gin.H{"error": <x>.Error()} 就违规，**不看状态码**。
//
// 这条规则收紧过一次。原先是"状态码明确小于 500 的 4xx 允许回显原文"，
// 理由是"4xx 的原文对调用方可操作"。该理由对**开发者自己构造的哨兵**成立，
// 但对**绑定错误**不成立——它们的原文是给开发者看的：
//
//	json: cannot unmarshal number into Go struct field .tag_name of type string
//	Key: 'InitChunkUploadRequest.FileSize' Error:Field validation for 'FileSize'
//	    failed on the 'required' tag
//
// 带着 Go 结构体名与字段名。而 8 个绑定站点恰好都写 `c.JSON(400, ...)`，
// 于是全部从旧规则的豁口漏了过去。收紧之后没有豁口：4xx 也要给 curated 文案，
// 绑定失败请用 apierror.RespondBinding（固定 400 + 统一文案）。
func detectRawErrorEchoes(call *ast.CallExpr) []token.Pos {
	if len(call.Args) != 2 {
		return nil
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	// gin.Context 上所有"把 body 交给客户端"的方法都要覆盖。
	// 只查 c.JSON 会漏掉 AbortWithStatusJSON 这类写法——
	// 它们同样会把 err.Error() 回显出去（且 Abort 变体还多一层
	// "必须保留 Abort 语义"的约束，更容易被漏掉）。
	if !responseMethods[sel.Sel.Name] {
		return nil
	}
	if !echoesRawError(call.Args[1]) {
		return nil
	}
	return []token.Pos{call.Pos()}
}

// TestGateDetectsRawErrorEchoes 是门禁的自测：证明它既能抓到违规，也不会误报。
//
// 没有这一节的话，门禁失效是**静默**的——它永远绿，而所有人以为这类泄漏
// 已经被守住了。这正是它要防的那种失败模式，所以必须先守住它自己。
func TestGateDetectsRawErrorEchoes(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantHit bool
	}{
		{
			name:    "5xx 回显原文",
			src:     `package p; func f(c *gin.Context, err error) { c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()}) }`,
			wantHit: true,
		},
		{
			name:    "4xx 字面量回显原文（旧规则的豁口）",
			src:     `package p; func f(c *gin.Context, err error) { c.JSON(400, gin.H{"error": err.Error()}) }`,
			wantHit: true,
		},
		{
			name:    "4xx 常量回显原文（旧规则的豁口）",
			src:     `package p; func f(c *gin.Context, err error) { c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()}) }`,
			wantHit: true,
		},
		{
			name:    "Abort 变体同样要覆盖",
			src:     `package p; func f(c *gin.Context, err error) { c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()}) }`,
			wantHit: true,
		},
		{
			name:    "分类器决定状态码也算违规（消息仍取原文）",
			src:     `package p; func f(c *gin.Context, err error) { c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()}) }`,
			wantHit: true,
		},
		{
			name:    "统一出口：合规",
			src:     `package p; func f(c *gin.Context, err error) { apierror.Respond(c, err) }`,
			wantHit: false,
		},
		{
			name:    "绑定专用出口：合规",
			src:     `package p; func f(c *gin.Context, err error) { apierror.RespondBinding(c, err) }`,
			wantHit: false,
		},
		{
			name:    "字面量文案：合规（安全由构造保证）",
			src:     `package p; func f(c *gin.Context) { c.JSON(400, gin.H{"error": "tag_name is required"}) }`,
			wantHit: false,
		},
		{
			// 哨兵的 .Error() 同样判违规：门禁只看形状，分不清"这是精心写好的
			// 安全文案"还是"这是刚从数据库捞出来的错误"。宁严勿宽——正解是走
			// RespondWith 显式给出文案（见下一条），代价只有一行。
			name:    "哨兵 .Error() 也算违规（门禁分不清哨兵与任意错误）",
			src:     `package p; func f(c *gin.Context) { c.JSON(http.StatusServiceUnavailable, gin.H{"error": errChunkCacheUnavailable.Error()}) }`,
			wantHit: true,
		},
		{
			name:    "哨兵文案走 RespondWith：合规",
			src:     `package p; func f(c *gin.Context) { apierror.RespondWith(c, http.StatusServiceUnavailable, errChunkCacheUnavailable.Error(), errChunkCacheUnavailable) }`,
			wantHit: false,
		},
		{
			name:    "只把错误写日志：合规",
			src:     `package p; func f(err error) { logging.L().Error("boom", zap.Error(err)) }`,
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "synthetic.go", tc.src, 0)
			if err != nil {
				t.Fatalf("解析测试源码失败: %v", err)
			}
			hits := 0
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					hits += len(detectRawErrorEchoes(call))
				}
				return true
			})
			if tc.wantHit && hits == 0 {
				t.Error("门禁漏掉了这处违规——这类泄漏会静默复活")
			}
			if !tc.wantHit && hits > 0 {
				t.Errorf("误报 %d 处；门禁误报会逼人加豁免，最终形同虚设", hits)
			}
		})
	}
}

// echoesRawError 判断表达式是否为 gin.H{"error": <x>.Error()}。
// 只看结构，因此注释、文档字符串里的示例不会误报。
func echoesRawError(expr ast.Expr) bool {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok || len(lit.Elts) != 1 {
		return false
	}
	kv, ok := lit.Elts[0].(*ast.KeyValueExpr)
	if !ok {
		return false
	}
	key, ok := kv.Key.(*ast.BasicLit)
	if !ok || key.Kind != token.STRING || key.Value != `"error"` {
		return false
	}
	call, ok := kv.Value.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" {
		return false
	}
	// 接收者形如 err 或 x.err
	switch sel.X.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	}
	return false
}

// constantStatus 尽力把状态码表达式求值为整数常量。
// 返回 (值, 是否已知)。表达式（如 apierror.ClassifyHTTPStatus(err)）视为未知，
