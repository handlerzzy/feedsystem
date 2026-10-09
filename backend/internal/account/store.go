package account

import "context"

//go:generate mockgen -destination=mock/account_store_mock.go -package=mock github.com/handlerzzy/feedsystem/internal/account AccountStore

// AccountStore 是 AccountService 所需的账号持久化能力。
//
// 为什么定义接口：AccountService 的全部业务逻辑（密码校验、重复注册识别、
// token 吊销、refresh 轮换）都与 GORM 无关，却因为直接依赖 *AccountRepository
// 而必须连上 MySQL 才能验证。抽成接口后 service 层可用 mock 单测，
// 无需任何外部依赖。
//
// 接口按“消费方需要什么”来定义，不是照抄 AccountRepository 的全部方法：
// Rename 不在其中，因为 AccountService 只用 RenameWithToken。
//
// *AccountRepository 隐式实现本接口，因此调用方无需改动。
type AccountStore interface {
	CreateAccount(ctx context.Context, account *Account) error
	RenameWithToken(ctx context.Context, id uint, newUsername string, token string) error
	ChangePassword(ctx context.Context, id uint, newPassword string) error
	FindByID(ctx context.Context, id uint) (*Account, error)
	FindByUsername(ctx context.Context, username string) (*Account, error)
	Login(ctx context.Context, id uint, token, refreshToken string) error
	Logout(ctx context.Context, id uint) error
	UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error
	UpdateFields(ctx context.Context, id uint, updates map[string]interface{}) error
	UpdateToken(ctx context.Context, id uint, token string) error
}

// 编译期断言：GORM 实现必须始终满足该接口。
// 若 AccountRepository 的方法签名漂移，这里会立刻编译失败，
// 而不是等到某个测试莫名其妙地报错。
var _ AccountStore = (*AccountRepository)(nil)
