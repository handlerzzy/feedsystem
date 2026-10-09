//go:build tools

// Package tools 只为钉住代码生成工具链的版本，不参与任何构建产物。
//
// 为什么用一个只有 build tag 的文件：`go mod tidy` 认得这种写法——它会把
// 这两个生成器记进 go.mod（因此 `go install <pkg>` 装到的是钉住的版本），
// 而 `go build ./...` / `go vet ./...` / `go test ./...` 因为 build tag
// 永远看不到它们，不会往 API 与 Worker 的产物里多链接一个字节。
//
// 不这样做的话，"重新生成 proto stub"就变成"用当时恰好装在机器上的任意版本
// 插件生成"：产物里全是与版本相关的噪音 diff，而 CI 不装 buf，
// 没人会发现产物已经与 proto 不一致。
//
// 用法（在 backend/ 下）：
//
//	go generate ./tools   # 按 go.mod 钉住的版本校验生成器可编译
//	go install google.golang.org/protobuf/cmd/protoc-gen-go \
//	           google.golang.org/grpc/cmd/protoc-gen-go-grpc   # 装进 GOBIN
//	buf generate          # 生成 internal/aipb/ 下的 stub，并一起提交
package tools

import (
	// protoc-gen-go：生成 *.pb.go（消息类型）。
	// 版本需与 go.mod 里 google.golang.org/protobuf 一致——生成器比运行期库
	// 新时，产物会引用老库没有的符号，编译直接失败。
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"

	// protoc-gen-go-grpc：生成 *_grpc.pb.go（client stub）。
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
)

// 用 `go build -o /dev/null` 而不是 `go install`：后者在模块模式下会去解析
// 该包的最新版本，正是上面要避免的事情；前者遵守 go.mod 的版本约束，
// 只做"这个版本能不能编译"的校验。
//
//go:generate go build -o /dev/null google.golang.org/protobuf/cmd/protoc-gen-go
//go:generate go build -o /dev/null google.golang.org/grpc/cmd/protoc-gen-go-grpc
