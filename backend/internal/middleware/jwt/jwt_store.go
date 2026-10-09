package jwt

import (
	"context"
	"github.com/handlerzzy/feedsystem/internal/account"
)

//go:generate mockgen -destination=mock/account_lookup_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/middleware/jwt AccountLookup

// AccountLookup 是 JWTAuth / SoftJWTAuth / CheckAndBind 所需的账号查询能力。
//
// 为什么定义接口：这几个函数真正的职责是“鉴权不变量”——无 token / 格式错 /
// 签名错 / 过期一律 401；签名有效但 token 已被吊销（logout、改名、改密）也
// 必须 401；SoftJWTAuth 在无 token 时必须放行；CheckAndBind 只校验+绑定、
// 不负责 c.Next()。要验证这些性质只需要一个账号查询入口，却被
// *account.AccountRepository 绑死到 MySQL 上，导致安全性质长期没有自动化
// 保护。抽成接口后可用 mock 精确构造“账号已登出（Token 为空）”“缓存与 DB
// token 不一致”“DB 里查不到该账号”等场景。
//
// 接口按消费方实际需要裁剪：三个函数只调用 FindByID，
// 因此这里只有这一个方法（改名后的 token 校验复用同一入口）。
//
// Go 接口是隐式实现，*account.AccountRepository 自动满足本接口，
// 因此 internal/http/router.go 与 internal/worker/ssehub.go 均无需改动。
type AccountLookup interface {
	FindByID(ctx context.Context, id uint) (*account.Account, error)
}

// 编译期断言：真实实现必须始终满足该接口。
// 若 AccountRepository.FindByID 的签名漂移，这里会立刻编译失败，
// 而不是等到某个鉴权测试莫名其妙地报错。
var _ AccountLookup = (*account.AccountRepository)(nil)
