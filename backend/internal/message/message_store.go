package message

import (
	"context"

	"github.com/handlerzzy/feedsystem/internal/account"
)

//go:generate mockgen -destination=mock/message_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/message MessageStore
//go:generate mockgen -destination=mock/account_lookup_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/message AccountLookup

// MessageStore 是 message.Service 所需的私信持久化能力。
//
// 为什么定义接口：本包的私信收发业务规则——校验接收者存在、拒绝空内容、
// 把发送方 ID 强绑到 token 主体而不是请求体、限制会话列表条数——与 GORM 无关，
// 却因为直接依赖 *Repository 而必须连上 MySQL 才能验证。抽成接口后可用 mock 单测：
// “接收者不存在时绝不落库”“空内容绝不落库”这类不变量可以直接用
// “未设置期望 => 被调用即失败”来证明，不需要数据库。
//
// 注意当前代码结构：这些规则写在 Handler 的方法里，Service 本身只有两个依赖字段
// （它是依赖注入的载体）。所以单测的注入点是 NewService 的参数，
// 由本包测试驱动 Handler 来验证规则，见 service_test.go 顶部说明。
//
// 接口按“消费方需要什么”定义：只包含实际被调用的 Send 与 List。
// *Repository 隐式实现本接口，因此 internal/http/router.go 无需改动。
type MessageStore interface {
	Send(ctx context.Context, m *Message) error
	List(ctx context.Context, userID, peerID uint, limit int) ([]Message, error)
}

// AccountLookup 是 message.Service 所需的账号查询能力，也是本包内的“窄接口”。
//
// 为什么在 message 包里声明描述 account 包类型的接口：接收者校验只需要
// FindByID 一个方法，却因为直接依赖 *account.AccountRepository 而把整个
// 账号仓库（注册/改名/改密/token 轮换……）拉进依赖面，单测必须构造真实 DB。
// Go 的接口是隐式实现的，所以消费方声明接口是合法且惯用的做法：
// *account.AccountRepository 自动满足 AccountLookup，router.go 无需改动。
//
// 刻意不包含 ChangePassword / Login 等写方法：校验接收者不应该有任何写账号的能力。
type AccountLookup interface {
	FindByID(ctx context.Context, id uint) (*account.Account, error)
}

// 编译期断言：GORM 实现必须始终满足这两个接口。
// 若 Repository / account.AccountRepository 的方法签名漂移，这里会立刻编译失败，
// 而不是等到某个测试莫名其妙地报错。
var (
	_ MessageStore  = (*Repository)(nil)
	_ AccountLookup = (*account.AccountRepository)(nil)
)
