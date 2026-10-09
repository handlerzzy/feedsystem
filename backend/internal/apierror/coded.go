package apierror

import (
	"errors"
	"net/http"
)

// CodedError 是同时携带 HTTP 状态码与对外文案的错误。
//
// 为什么需要它：哨兵错误（ErrNotFound 等）只能表达"哪一类"问题，表达不了
// "具体是什么"。于是服务层里出现大量这样的写法：
//
//	return errors.New("video not found")
//
// 语义上是 404，但分类器不认识它，最终落成 500 并把原文回显。
// 若简单替换成 ErrNotFound，状态码对了、文案却从 "video not found"
// 退化成 "not found" —— 对调用方是信息丢失，对已文档化的响应是契约变更。
//
// CodedError 让服务层一次性声明两件事，且文案可以保持逐字不变：
//
//	return apierror.NotFound("video not found")   // 404 {"error":"video not found"}
//
// 它与哨兵错误互补而非替代：需要在多处用 errors.Is 判断"是不是同一类问题"
// 时仍应使用哨兵；只需要"这个错误对应哪个状态码、该说什么"时用 CodedError。
type CodedError struct {
	Status  int
	Message string

	cause error // 可选的底层原因，只用于日志，不对外
}

func (e *CodedError) Error() string { return e.Message }

// Unwrap 让 errors.Is/As 能穿透到底层原因。
func (e *CodedError) Unwrap() error { return e.cause }

// New 构造一个指定状态码与文案的错误。
func New(status int, message string) error {
	return &CodedError{Status: status, Message: message}
}

// Wrap 在保留状态码与对外文案的同时携带底层原因（原因只进日志）。
func Wrap(cause error, status int, message string) error {
	return &CodedError{Status: status, Message: message, cause: cause}
}

func BadRequest(message string) error   { return New(http.StatusBadRequest, message) }
func Unauthorized(message string) error { return New(http.StatusUnauthorized, message) }
func Forbidden(message string) error    { return New(http.StatusForbidden, message) }
func NotFound(message string) error     { return New(http.StatusNotFound, message) }
func Conflict(message string) error     { return New(http.StatusConflict, message) }

// CodedStatus 返回 err 携带的显式状态码；err 不是 CodedError 时返回 0 与 false。
func CodedStatus(err error) (int, bool) {
	var ce *CodedError
	if errors.As(err, &ce) && ce.Status != 0 {
		return ce.Status, true
	}
	return 0, false
}
