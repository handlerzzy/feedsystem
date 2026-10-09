package apierror

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-playground/validator/v10"
	"gorm.io/gorm"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrValidation   = errors.New("validation error")
	ErrForbidden    = errors.New("forbidden") // 新增：归属校验失败用 403
	ErrNotFound     = errors.New("not found") // 新增：明确的资源不存在
	ErrConflict     = errors.New("conflict")  // 新增：唯一键冲突等
)

func ClassifyHTTPStatus(err error) int {
	// gin 的 binding 错误不实现 Is()，必须用 errors.As 按具体类型识别，
	// 否则非法 JSON / 类型错误 / 缺必填字段都会落进 default 变成 500。
	var ve validator.ValidationErrors
	var syn *json.SyntaxError
	var ute *json.UnmarshalTypeError

	// 显式声明了状态码的错误优先：它表达的是"这个具体错误对应哪个状态"，
	// 比按类别推断更精确。
	if code, ok := CodedStatus(err); ok {
		return code
	}

	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &ve), errors.As(err, &syn), errors.As(err, &ute):
		return http.StatusBadRequest
	// 空请求体与截断请求体：gin 的绑定返回 io.EOF / io.ErrUnexpectedEOF，
	// 而不是 json.SyntaxError。二者不实现 Is() 也不属于 validator 错误，
	// 若不显式覆盖就会落进 default 变成 500 —— 客户端没把 body 发全，
	// 却被告知"服务端出错"，会去重试而不是修请求，同时污染服务端日志。
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
