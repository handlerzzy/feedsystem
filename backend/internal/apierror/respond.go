package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/handlerzzy/feedsystem/internal/logging"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

// Logger 是服务端错误详情的唯一出口。
//
// 保留成变量是为了让日志实现可替换：当前用标准库 log，接入结构化日志
// （zap）后只需在这里换一次，不必修改任何调用点，也不会有 handler 漏改。
var Logger = log.Printf

// genericMessage 是 5xx 响应体里对外的统一文案。
//
// 为什么不回显原始错误：5xx 意味着"服务端出了问题"，此时的 err 往往携带
// 内部细节——SQL 语句与表名、文件系统路径、上游地址、连接串片段。
// 调用方对这些既无从处理，也不该看到。
const genericMessage = "internal server error"

// PublicMessage 返回可安全回显给客户端的消息。
//
// 规则：
//   - 显式声明了文案的 4xx（CodedError）：用它自己的文案。这些都是开发者特意
//     为调用方写的（"username already exists"、"forbidden"），属于对外契约。
//   - 5xx：一律返回 genericMessage，原文只进日志。
//   - 已知的标准库/绑定错误：换成可操作的说法（见 friendlyMessage）。**不能**
//     原样回显——它们的原文是给开发者看的，含 Go 结构体名与字段名。
//   - 其余 4xx：原样回显 err.Error()。走到这里的都是项目自己构造的哨兵。
//
// 注意不要为了"更整齐"把 4xx 也改成哨兵的规范文案：那会破坏既有响应契约
// （例如重复注册当前返回 "username already exists"，而不是 "conflict"）。
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	// 显式声明了文案的错误（CodedError）直接用它自己的文案：
	// 它是服务层特意为调用方写的，比通用文案或 err.Error() 都更准确。
	var ce *CodedError
	if errors.As(err, &ce) && ce.Status < http.StatusInternalServerError {
		return ce.Message
	}
	if ClassifyHTTPStatus(err) >= http.StatusInternalServerError {
		return genericMessage
	}
	// 少数来自标准库的错误，原文对调用方毫无意义（"EOF"、"unexpected EOF"），
	// 但它们对应的客户端问题是明确且可操作的，换成人类读得懂的说法。
	if msg, ok := friendlyMessage(err); ok {
		return msg
	}
	return err.Error()
}

// friendlyMessage 把标准库与绑定层的晦涩原文换成可操作的提示。
// 只覆盖确定无疑的几种；拿不准的一律返回 false，交给调用方按原样回显。
func friendlyMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, io.EOF):
		return "request body is required", true
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "request body is incomplete", true
	}
	return bindingMessage(err)
}

// bindingMessage 把 gin 绑定/校验失败的错误换成**不含内部信息**的文案。
//
// 为什么必须换：这些错误的 err.Error() 是给开发者看的，不是给调用方看的。
// 实测原样回显出来的是：
//
//	json: cannot unmarshal number into Go struct field .tag_name of type string
//	Key: 'InitChunkUploadRequest.FileSize' Error:Field validation for 'FileSize'
//	    failed on the 'required' tag
//
// 里面带着 Go 结构体名（InitChunkUploadRequest）与字段名（FileSize）——把内部
// 类型结构透给了客户端，且是英文的、面向开发者的措辞。8 个站点此前都是这么写的，
// 因为门禁只拦 5xx，而这里状态码是字面量 400。
//
// 状态码仍由 ClassifyHTTPStatus 决定（这三类都映射到 400），这里只负责文案。
//
// 字段名取的是 json tag 而非 Go 字段名，依赖 router 里注册的 RegisterTagNameFunc
// （见 registerJSONFieldNames）。没有它 Field() 会退回 Go 字段名，就又把内部
// 名字漏出去了。
func bindingMessage(err error) (string, bool) {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return "invalid request body: malformed JSON", true
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		// Field 是 JSON 路径（客户端自己写的键），不是 Go 字段名。
		if typeErr.Field != "" {
			return fmt.Sprintf("invalid request body: field %q has the wrong type", typeErr.Field), true
		}
		return "invalid request body", true
	}

	var validationErrs validator.ValidationErrors
	if errors.As(err, &validationErrs) && len(validationErrs) > 0 {
		// 只报第一条：调用方一次修一个字段就够了，列全反而难读；
		// 多条同时出错时逐条修完再提交也是常见做法。
		fieldErr := validationErrs[0]
		if fieldErr.Tag() == "required" {
			return fmt.Sprintf("invalid request body: field %q is required", fieldErr.Field()), true
		}
		return fmt.Sprintf("invalid request body: field %q failed %q",
			fieldErr.Field(), fieldErr.Tag()), true
	}

	return "", false
}

// Respond 是 handler 的统一错误出口：分类状态码、回显安全消息、记录原始错误。
//
// 它替换的是这个在项目里出现近百次的模式：
//
//	c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
//
// 该模式的问题在于：ClassifyHTTPStatus 只决定状态码，消息永远取 err.Error()。
// 于是只要分类落到 500，SQL 错误、文件路径这类内部细节就照样吐给客户端——
// 状态码是对的，泄漏依然发生。
//
// 适用范围仅限**状态码由分类器决定**的站点。若某处原本硬编码了状态码
// （例如 account handler 里因为错误没有哨兵而写死的 409），改用本函数会把
// 状态码改成分类结果（500），属于行为回归——那种情况请用 RespondWith。
func Respond(c *gin.Context, err error) {
	if c == nil {
		return
	}
	if err == nil {
		// 用 nil 调用 Respond 属于调用方的编程错误：既没有可回显的错误，
		// 也不该返回 200 + 错误体。明确落成 500 并留下线索。
		Logger("apierror.Respond called with nil error (path=%s)", c.Request.URL.Path)
		c.JSON(http.StatusInternalServerError, gin.H{"error": genericMessage})
		return
	}

	status := ClassifyHTTPStatus(err)
	if status >= http.StatusInternalServerError {
		logServerError(c, status, PublicMessage(err), err)
	}
	c.JSON(status, gin.H{"error": PublicMessage(err)})
}

// RespondWith 用**显式**状态码与**显式**对外文案响应，cause 只用于日志。
//
// 两种场景需要它：
//
//  1. 错误没有哨兵，状态码由 handler 写死。例如重复注册返回 account.ErrUsernameTaken，
//     分类器不认识它（会落 500），而 handler 明确知道这是 409。
//  2. 对外文案是固定的可操作提示，比通用文案更有用。例如分片上传失败时
//     "failed to save chunk" 既安全又能指明是哪个环节——这类站点**不该**
//     退化成 "internal server error"。
//
// cause 允许为 nil（纯客户端错误、没有底层原因时）。
func RespondWith(c *gin.Context, status int, publicMsg string, cause error) {
	if c == nil {
		return
	}
	if status >= http.StatusInternalServerError {
		if cause != nil {
			logServerError(c, status, publicMsg, cause)
		} else {
			// 没有 cause 的 5xx 仍然要留一条记录：否则线上只剩"500 了"，
			// 连哪个入口、什么文案都无从查起。
			Logger("request failed: method=%s path=%s request_id=%s status=%d msg=%q (no cause attached)",
				c.Request.Method, c.Request.URL.Path,
				logging.RequestIDFrom(c.Request.Context()), status, publicMsg)
		}
	}
	c.JSON(status, gin.H{"error": publicMsg})
}

// logServerError 记录 5xx 的服务端详情。
//
// 刻意同时记录**对外文案**与**底层原因**：排查时最常见的问题是
// "客户端说看到了 X，日志里只有 Y"，两者对不上就无从关联。
//
// 另外带上 request_id：客户端报障时提供响应头 X-Request-ID，
// 就能直接检索出这整条请求的日志，不必按时间戳人肉对齐。
func logServerError(c *gin.Context, status int, publicMsg string, err error) {
	Logger("request failed: method=%s path=%s request_id=%s status=%d msg=%q err=%v",
		c.Request.Method, c.Request.URL.Path,
		logging.RequestIDFrom(c.Request.Context()), status, publicMsg, err)
}

// RespondBinding 是"请求体绑定失败"站点的统一出口：状态码恒为 400，文案仍走
// 统一出口，绝不回显 err.Error()。
//
// 为什么状态码不让 ClassifyHTTPStatus 决定：绑定失败**按定义**就是客户端错误。
// 分类器只认识几类具体错误（*json.SyntaxError、*json.UnmarshalTypeError、
// validator.ValidationErrors、io.EOF/io.ErrUnexpectedEOF），gin 将来若引入新的
// 绑定错误类型（例如 *http.MaxBytesError），走分类器会把它兜成 500 + 通用文案：
// 状态码从"你的请求有问题"变成"服务端出错了"，客户端于是去重试而不是修请求，
// 同时污染服务端日志。固定 400 对新类型才是安全的默认值。
//
// 但状态码硬编码**不等于**文案也要用原文。此前这些站点写的是
// c.JSON(400, gin.H{"error": err.Error()})，实测回显出来的是：
//
//	json: cannot unmarshal number into Go struct field .tag_name of type string
//	Key: 'InitChunkUploadRequest.FileSize' Error:Field validation for 'FileSize'
//	    failed on the 'required' tag
//
// 带着 Go 结构体名与字段名，把内部类型结构透给了客户端。所以这里固定 400，
// 文案则优先取已知类型的 curated 版本；不认识的类型退回一句通用但可操作的
// 提示，而不是把原文漏出去。
func RespondBinding(c *gin.Context, err error) {
	if c == nil {
		return
	}
	if err == nil {
		// 用 nil 调用属于调用方的编程错误：既没有可回显的错误，也不该返回 200。
		Logger("apierror.RespondBinding called with nil error (path=%s)", c.Request.URL.Path)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// 不认识的类型用这个兜底，因此任何情况下都不会回显 err.Error()。
	msg := "invalid request body"
	if m, ok := friendlyMessage(err); ok {
		msg = m
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": msg})
}
