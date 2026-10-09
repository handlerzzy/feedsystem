package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/apierror"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

// 本文件覆盖 registerJSONFieldNames —— 它在 SetRouter 里注册，是全局 validator
// 的设置，因此**只能通过生产入口验证**：测试里自己 gin.New() 建的路由不会走
// SetRouter，也就验证不到它。
//
// 它值得单独测，是因为绑定错误文案里出现的字段名直接来自 validator：
// 没有这个注册，Field() 返回 Go 字段名（FileSize），拼进 4xx 文案就等于把内部
// 类型结构透给客户端。而这条链路（注册 -> validator -> apierror 文案）跨了三个
// 包，任何一环被改坏都不会有编译错误。

type fieldNameProbe struct {
	FileSize int64 `json:"file_size" binding:"required,min=1"`
}

// TestSetRouterRegistersJSONFieldNames 走**真实生产入口** SetRouter，
// 而不是直接调 registerJSONFieldNames。
//
// 这个区别很重要：直接调函数只能证明"函数本身有用"，证明不了"生产路径会调它"。
// 而这条链路的失败方式是静默的——漏调之后绑定错误文案会退回 Go 字段名
// （FileSize），接口照常 200/400，没有任何报错。所以必须从 SetRouter 进。
func TestSetRouterRegistersJSONFieldNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := SetRouter(Deps{}) // 不碰 DB/Redis/MQ：绑定在触达依赖之前就失败了

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/account/register",
		strings.NewReader(`{"username":"","password":""}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// 核心断言：不得出现 Go 侧的标识符。
	for _, leak := range []string{"FileSize", "CreateAccountRequest", "fieldNameProbe", "Username", "Password"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应体泄漏了 Go 侧标识符 %q: %s\n"+
				"（多半是 SetRouter 没有调用 registerJSONFieldNames）", leak, body)
		}
	}
	// 并且应当是客户端自己的字段名，否则文案对调用方毫无用处。
	for _, want := range []string{"username", "required"} {
		if !strings.Contains(body, want) {
			t.Errorf("响应体应含 %q，实际: %s", want, body)
		}
	}
}

// TestWithoutTagNameFuncValidatorLeaksGoFieldNames 是上面那条的反证：
// 没有注册时 validator 报的就是 Go 字段名与啰嗦的开发者措辞。
//
// 把这个对照写下来，是为了让"为什么需要 registerJSONFieldNames"有据可查，
// 而不是一句注释。注意这里**不断言** PublicMessage 会把它洗掉——它洗不掉：
// 字段名直接来自 validator.Field()，注册与否决定了它是 file_size 还是 FileSize。
//
// 也就是说，文案的安全性依赖"SetRouter 一定注册过"这个前提。这个前提由
// TestSetRouterRegistersJSONFieldNames 从生产入口守着——它才是真正的防线，
// 本条只是解释防线为什么存在。
func TestWithoutTagNameFuncValidatorLeaksGoFieldNames(t *testing.T) {
	// 与 gin 的 binding.Validator 同样读 binding tag，但**不**注册 TagNameFunc：
	// 这正是注册之前的行为。
	raw := validator.New()
	raw.SetTagName("binding")
	err := raw.Struct(&fieldNameProbe{})
	if err == nil {
		t.Fatal("空 FileSize 应当校验失败")
	}

	// 原始文案长这样：Key: 'fieldNameProbe.FileSize' Error:Field validation
	// for 'FileSize' failed on the 'required' tag —— 类型名与字段名都在里面。
	if !strings.Contains(err.Error(), "FileSize") {
		t.Errorf("未注册时本应报出 Go 字段名 FileSize，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "fieldNameProbe") {
		t.Errorf("未注册时本应报出 Go 类型名 fieldNameProbe，实际: %v", err)
	}

	// PublicMessage 至少能去掉类型名与那串开发者措辞：它只保留 field/tag
	// 两段信息，不会把 err.Error() 整句带出去。
	msg := apierror.PublicMessage(err)
	if strings.Contains(msg, "fieldNameProbe") || strings.Contains(msg, "Error:Field validation") {
		t.Errorf("PublicMessage 不该带出类型名或 validator 的原始措辞，实际 %q", msg)
	}
	if !strings.HasPrefix(msg, "invalid request body") {
		t.Errorf("应当是 curated 文案形状，实际 %q", msg)
	}
}
