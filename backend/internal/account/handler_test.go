package account_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/account/mock"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/logging"

	"github.com/gin-gonic/gin"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

// callHandler 在真实的 *gin.Context（gin.CreateTestContext）上直接调用 handler，
// 返回状态码与解析后的 JSON body。
//
// 为什么不走 engine + ServeHTTP：这里要验证的是单个 handler 的错误出口，
// 直接调用可以避免路由与中间件的行为混进断言。
func callHandler(t *testing.T, req *http.Request, accountID *uint, h gin.HandlerFunc) (int, map[string]string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	if accountID != nil {
		c.Set("accountID", *accountID)
	}
	h(c)

	body := map[string]string{}
	if raw := w.Body.Bytes(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("响应体不是 JSON 对象: %v (raw=%q)", err, w.Body.String())
		}
	}
	return w.Code, body
}

// jsonCall 构造 JSON 请求并调用 handler。
func jsonCall(t *testing.T, h gin.HandlerFunc, method, path, body string, accountID *uint) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return callHandler(t, req, accountID, h)
}

// captureAPILogs 把 apierror.Logger 换成收集器，返回收集到的日志切片。
// 测试结束自动还原；本包内的测试不并行，因此替换全局变量是安全的。
func captureAPILogs(t *testing.T) *[]string {
	t.Helper()
	logs := &[]string{}
	prev := apierror.Logger
	apierror.Logger = func(format string, args ...any) {
		*logs = append(*logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { apierror.Logger = prev })
	return logs
}

func uintPtr(v uint) *uint { return &v }

// ---------- CreateAccount ----------

// 重复注册：改造前 handler 硬编码 409 兜住 ErrUsernameTaken；
// 现在 ErrUsernameTaken 自带 409，handler 走统一出口，契约必须完全不变。
func TestCreateAccountHandlerDuplicateUsername(t *testing.T) {
	logs := captureAPILogs(t)

	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	store.EXPECT().CreateAccount(gomock.Any(), gomock.Any()).Return(dupUsernameErr())
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.CreateAccount, http.MethodPost, "/account/register",
		`{"username":"alice","password":"secret123"}`, nil)

	if code != http.StatusConflict {
		t.Errorf("status = %d, want %d", code, http.StatusConflict)
	}
	if body["error"] != "username already exists" {
		t.Errorf("error = %q, want %q", body["error"], "username already exists")
	}
	if len(*logs) != 0 {
		t.Errorf("4xx 不应写服务端错误日志, got %v", *logs)
	}
}

// 空白用户名的 400 是 handler 自己写死的（错误没有哨兵），本次改造不得触碰。
func TestCreateAccountHandlerBlankUsername(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	// 未设置期望：服务层不应被调用。
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.CreateAccount, http.MethodPost, "/account/register",
		`{"username":"   ","password":"secret123"}`, nil)

	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", code, http.StatusBadRequest)
	}
	if body["error"] != "username is required" {
		t.Errorf("error = %q, want %q", body["error"], "username is required")
	}
}

// ---------- Rename ----------

// 改造前 new_username 为空会落成 500（裸错误 + 分类器不认识），
// 现在 ErrNewUsernameRequired 自带 400。这是本次有意的行为修复。
func TestRenameHandlerEmptyNewUsername(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	// 未设置期望：校验在服务层最前面完成，不应触碰存储。
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.Rename, http.MethodPost, "/account/rename",
		`{"new_username":""}`, uintPtr(1))

	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", code, http.StatusBadRequest)
	}
	if body["error"] != "new_username is required" {
		t.Errorf("error = %q, want %q", body["error"], "new_username is required")
	}
}

func TestRenameHandlerDuplicateUsername(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	store.EXPECT().RenameWithToken(gomock.Any(), uint(1), "bob", gomock.Any()).
		Return(dupUsernameErr())
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.Rename, http.MethodPost, "/account/rename",
		`{"new_username":"bob"}`, uintPtr(1))

	if code != http.StatusConflict {
		t.Errorf("status = %d, want %d", code, http.StatusConflict)
	}
	if body["error"] != "username already exists" {
		t.Errorf("error = %q, want %q", body["error"], "username already exists")
	}
}

// 账号不存在是本次**未**改动的站点：它有自己的固定文案 "account not found"，
// 不能退化成 err.Error()（"record not found"）。此测试作为回归护栏。
func TestRenameHandlerAccountNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	store.EXPECT().RenameWithToken(gomock.Any(), uint(1), "bob", gomock.Any()).
		Return(gorm.ErrRecordNotFound)
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.Rename, http.MethodPost, "/account/rename",
		`{"new_username":"bob"}`, uintPtr(1))

	if code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", code, http.StatusNotFound)
	}
	if body["error"] != "account not found" {
		t.Errorf("error = %q, want %q", body["error"], "account not found")
	}
}

// ---------- UpdateProfile ----------

// 改造前 "nothing to update" 会被分类成 500 并原样回显，现在应为 400。
func TestUpdateProfileHandlerNothingToUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	// 未设置期望：没有任何字段可更新时不应触碰存储。
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.UpdateProfile, http.MethodPost, "/account/updateProfile",
		`{}`, uintPtr(1))

	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", code, http.StatusBadRequest)
	}
	if body["error"] != "nothing to update" {
		t.Errorf("error = %q, want %q", body["error"], "nothing to update")
	}
}

// ---------- Refresh ----------

// Refresh 的 401 是 handler 自己写死并固定文案的（所有失败一律 "invalid refresh token"），
// 本次改造按要求未触碰它。服务层现在返回带码错误，但对外响应必须一字不变。
func TestRefreshHandlerKeepsExplicitUnauthorized(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	code, body := jsonCall(t, h.Refresh, http.MethodPost, "/account/refresh",
		`{"refresh_token":"never-issued"}`, nil)

	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", code, http.StatusUnauthorized)
	}
	if body["error"] != "invalid refresh token" {
		t.Errorf("error = %q, want %q", body["error"], "invalid refresh token")
	}
}

// ---------- 5xx 不再回显原始错误 ----------

// 身份缺失（受保护路由上没有 accountID）的契约：401 + 干净文案，且留下 Warn。
//
// 这里走过一次反复。改造前该路径回显裸错误 "accountID not found" 且状态码 500；
// 后来改成 500 + 通用文案并断言"原文进日志"。但 500 是错的语义：调用方没有身份
// 是"你没通过认证"，不是"服务端挂了"——拿到 500 的客户端会去重试，而正解是重新
// 登录。message 包早就为此断言了 401（"未登录时返回 401，不会退化成 0 号账号
// 发送"），account 这边却给了 500，同一份错误两种语义。
//
// 现在统一成 401 + "authentication required"。走到这条路径其实意味着路由漏挂了
// 认证中间件，是服务端接线错误；对外给 401，对内留一条 Warn，这样接线错误仍然
// 可见——否则它会在日志里完全静默。
func TestLogoutHandlerMissingIdentityIs401AndLogged(t *testing.T) {
	// 这条 Warn 走的是 zap（logging.L()），不是 apierror.Logger——
	// 后者只承载 5xx 的服务端详情，401 不属于它。
	core, logs := observer.New(zapcore.WarnLevel)
	prev := logging.SetLogger(zap.New(core))
	t.Cleanup(func() { logging.SetLogger(prev) })

	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	// 不注入 accountID，制造 getAccountID 失败。
	code, body := jsonCall(t, h.Logout, http.MethodPost, "/account/logout", `{}`, nil)

	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401（调用方没有身份，不是服务端错误）", code)
	}
	if body["error"] != "authentication required" {
		t.Errorf("error = %q, want %q", body["error"], "authentication required")
	}
	if strings.Contains(body["error"], "accountID") {
		t.Error("内部细节不得出现在响应体里")
	}
	// 接线错误必须可见。
	var found bool
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "缺少身份") {
			found = true
		}
	}
	if !found {
		t.Errorf("身份缺失必须留下 Warn 线索，否则接线错误会完全静默; got %v", logs.All())
	}
}

// uploadAvatar 的 os.MkdirAll 失败是 C 类站点之一：改造前返回
// 500 + err.Error()（内含 .run/uploads/... 路径），改造后只给通用文案，
// 路径只进日志。
func TestUploadAvatarHandlerServerErrorIsLoggedNotLeaked(t *testing.T) {
	// 让相对路径 .run 落在临时目录：既不污染工作区，也无需清理。
	t.Chdir(t.TempDir())
	logs := captureAPILogs(t)

	// 在目标目录位置放一个普通文件，MkdirAll 会以 "not a directory" 失败。
	avatarDir := filepath.Join(".run", "uploads", "avatars")
	if err := os.MkdirAll(avatarDir, 0o755); err != nil {
		t.Fatalf("prepare avatar dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(avatarDir, "7"), []byte("x"), 0o644); err != nil {
		t.Fatalf("prepare blocking file: %v", err)
	}

	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	// 未设置期望：MkdirAll 已在 UpdateAvatar 之前失败。
	h := account.NewAccountHandler(account.NewAccountService(store, nil))

	req := multipartUpload(t, "/account/uploadAvatar", "a.png", []byte("png-bytes"))
	code, body := callHandler(t, req, uintPtr(7), h.UploadAvatar)

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body=%v)", code, http.StatusInternalServerError, body)
	}
	if body["error"] != "internal server error" {
		t.Errorf("error = %q, want the generic message", body["error"])
	}
	if strings.Contains(body["error"], ".run") || strings.Contains(body["error"], "not a directory") {
		t.Error("文件系统路径/细节不得出现在响应体里")
	}
	if len(*logs) == 0 || !strings.Contains(strings.Join(*logs, "\n"), ".run") {
		t.Errorf("原始错误（含路径）必须写进日志, got %v", *logs)
	}
}

// multipartUpload 构造一个带 "file" 字段的 multipart 请求。
func multipartUpload(t *testing.T, path, filename string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}
