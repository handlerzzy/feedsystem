package account

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/apierror"
	"github.com/handlerzzy/feedsystem/internal/auth"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"strconv"
	"strings"
	"time"

	rediscache "github.com/handlerzzy/feedsystem/internal/middleware/redis"

	"github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type AccountService struct {
	accountRepository AccountStore
	cache             *rediscache.Client
}

// ErrUsernameTaken 与 ErrNewUsernameRequired 既是可被 errors.Is 识别的哨兵，
// 又自带 HTTP 状态码与对外文案（CodedError）：handler 无需再硬编码 409/400，
// 分类器即可给出正确状态，且文案与改造前逐字一致。
var (
	ErrUsernameTaken       = apierror.Conflict("username already exists")
	ErrNewUsernameRequired = apierror.BadRequest("new_username is required")
)

// refreshTokenTTL 是 refresh token 在 Redis 中的存活时间，由启动流程通过
// SetRefreshTokenTTL 注入（默认 7 天，与 auth 的默认 refresh TTL 一致）。
var refreshTokenTTL = 7 * 24 * time.Hour

// SetRefreshTokenTTL 由启动流程调用，覆盖 refresh token 的默认有效期。
func SetRefreshTokenTTL(d time.Duration) error {
	if d <= 0 {
		return errors.New("refresh token ttl must be positive")
	}
	refreshTokenTTL = d
	return nil
}

func NewAccountService(accountRepository AccountStore, cache *rediscache.Client) *AccountService {
	return &AccountService{accountRepository: accountRepository, cache: cache}
}

func (as *AccountService) CreateAccount(ctx context.Context, account *Account) error {
	if strings.TrimSpace(account.Username) == "" {
		return apierror.BadRequest("username is required")
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(account.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	account.Password = string(passwordHash)
	if err := as.accountRepository.CreateAccount(ctx, account); err != nil {
		// 用户名唯一键冲突属于客户端错误（重复注册），不能落成 500。
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return ErrUsernameTaken
		}
		return err
	}
	return nil
}

func (as *AccountService) Rename(ctx context.Context, accountID uint, newUsername string) (string, error) {
	if newUsername == "" {
		return "", ErrNewUsernameRequired
	}

	token, err := auth.GenerateToken(accountID, newUsername)
	if err != nil {
		return "", err
	}

	if err := as.accountRepository.RenameWithToken(ctx, accountID, newUsername, token); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return "", ErrUsernameTaken
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", err
		}
		return "", err
	}
	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", accountID), []byte(token), 24*time.Hour); err != nil {
			// 缓存只是加速路径，写失败不影响改名结果，故为 Warn。
			logging.Ctx(ctx).Warn("failed to set cache",
				zap.Uint("account_id", accountID), zap.Error(err))
		}
	}
	return token, nil
}

// ChangePasswordByID 按账号 ID 改密码；成功后登出该账号（吊销其 token）
func (as *AccountService) ChangePasswordByID(ctx context.Context, accountID uint, oldPassword, newPassword string) error {
	account, err := as.FindByID(ctx, accountID)
	if err != nil {
		return err
	}
	return as.changePassword(ctx, account, oldPassword, newPassword)
}

// changePassword 为两条入口共用的逻辑：校验旧密码 → 写入新 hash → 登出该账号
func (as *AccountService) changePassword(ctx context.Context, account *Account, oldPassword, newPassword string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(oldPassword)); err != nil {
		return err
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := as.accountRepository.ChangePassword(ctx, account.ID, string(passwordHash)); err != nil {
		return err
	}
	if err := as.Logout(ctx, account.ID); err != nil {
		return err
	}
	return nil
}

func (as *AccountService) FindByID(ctx context.Context, id uint) (*Account, error) {
	if account, err := as.accountRepository.FindByID(ctx, id); err != nil {
		return nil, err
	} else {
		return account, nil
	}
}

func (as *AccountService) FindByUsername(ctx context.Context, username string) (*Account, error) {
	if account, err := as.accountRepository.FindByUsername(ctx, username); err != nil {
		return nil, err
	} else {
		return account, nil
	}
}

func (as *AccountService) Login(ctx context.Context, username, password string) (string, string, error) {
	account, err := as.FindByUsername(ctx, username)
	if err != nil {
		return "", "", err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(password)); err != nil {
		return "", "", err
	}
	accessToken, err := auth.GenerateToken(account.ID, account.Username)
	if err != nil {
		return "", "", err
	}
	refreshToken, err := auth.GenerateRefreshToken(account.ID)
	if err != nil {
		return "", "", err
	}
	if err := as.accountRepository.Login(ctx, account.ID, accessToken, refreshToken); err != nil {
		return "", "", err
	}
	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", account.ID), []byte(accessToken), 24*time.Hour); err != nil {
			// 缓存只是加速路径，写失败不影响登录结果，故为 Warn。
			logging.Ctx(ctx).Warn("failed to set cache",
				zap.Uint("account_id", account.ID), zap.Error(err))
		}
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d:refresh", account.ID), []byte(refreshToken), refreshTokenTTL); err != nil {
			logging.Ctx(ctx).Warn("failed to set refresh cache",
				zap.Uint("account_id", account.ID), zap.Error(err))
		}
		// 注意：这条记录的 key 内嵌 refresh token，属凭证，故只记 account_id 不记 key。
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("refresh:%s", refreshToken), []byte(strconv.FormatUint(uint64(account.ID), 10)), refreshTokenTTL); err != nil {
			logging.Ctx(ctx).Warn("failed to set refresh lookup",
				zap.Uint("account_id", account.ID), zap.Error(err))
		}
	}
	return accessToken, refreshToken, nil
}

func (as *AccountService) Logout(ctx context.Context, accountID uint) error {
	account, err := as.FindByID(ctx, accountID)
	if err != nil {
		return err
	}
	if account.Token == "" {
		return nil
	}
	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		if err := as.cache.Del(cacheCtx, as.cache.Key("account:%d", account.ID)); err != nil {
			// 删缓存失败只会让旧 token 多活一会儿（DB 才是判定依据），故为 Warn。
			logging.Ctx(ctx).Warn("failed to del cache",
				zap.Uint("account_id", account.ID), zap.Error(err))
		}
		if err := as.cache.Del(cacheCtx, as.cache.Key("account:%d:refresh", account.ID)); err != nil {
			logging.Ctx(ctx).Warn("failed to del refresh cache",
				zap.Uint("account_id", account.ID), zap.Error(err))
		}
		if account.RefreshToken != "" {
			// 同上：key 内嵌 refresh token，只记 account_id。
			if err := as.cache.Del(cacheCtx, as.cache.Key("refresh:%s", account.RefreshToken)); err != nil {
				logging.Ctx(ctx).Warn("failed to del refresh lookup",
					zap.Uint("account_id", account.ID), zap.Error(err))
			}
		}
	}
	return as.accountRepository.Logout(ctx, account.ID)
}

func (as *AccountService) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	return as.accountRepository.UpdateAvatar(ctx, accountID, avatarURL)
}

func (as *AccountService) UpdateProfile(ctx context.Context, accountID uint, req *UpdateProfileRequest) error {
	updates := map[string]interface{}{}
	if req.Bio != "" {
		updates["bio"] = strings.TrimSpace(req.Bio)
	}
	if req.AvatarURL != "" {
		updates["avatar_url"] = strings.TrimSpace(req.AvatarURL)
	}
	if len(updates) == 0 {
		return apierror.BadRequest("nothing to update")
	}
	return as.accountRepository.UpdateFields(ctx, accountID, updates)
}

func (as *AccountService) RefreshAccessToken(ctx context.Context, refreshToken string) (string, uint, string, error) {
	if refreshToken == "" {
		return "", 0, "", apierror.BadRequest("refresh token is empty")
	}
	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		b, err := as.cache.GetBytes(cacheCtx, as.cache.Key("refresh:%s", refreshToken))
		if err == nil {
			idStr := string(b)
			id, parseErr := strconv.ParseUint(idStr, 10, 64)
			if parseErr == nil {
				account, err := as.FindByID(ctx, uint(id))
				if err == nil && account != nil && account.RefreshToken == refreshToken {
					newToken, err := auth.GenerateToken(account.ID, account.Username)
					if err != nil {
						return "", 0, "", err
					}
					if err := as.accountRepository.UpdateToken(ctx, account.ID, newToken); err != nil {
						return "", 0, "", err
					}
					if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", account.ID), []byte(newToken), 24*time.Hour); err != nil {
						// 回填缓存失败不影响本次刷新（新 token 已在 DB），故为 Warn。
						logging.Ctx(ctx).Warn("failed to set cache",
							zap.Uint("account_id", account.ID), zap.Error(err))
					}
					return newToken, account.ID, account.Username, nil
				}
			}
		}
	}
	// === 缓存未命中 / Redis 不可用 → 直接视为 refresh token 无效 ===
	// 设计取舍：不再提供 DB 兜底。原实现走 FindAll()（SELECT * FROM accounts
	// 无 WHERE 无 LIMIT）把整张表加载进内存再线性比对，且命中后不回填缓存，
	// 导致兜底路径每次请求都重复全表扫描；而该路径正是在 Redis 重启/key 被清
	// 时触发，即系统最脆弱的时候。
	// refresh:<token> 的 TTL（7 天）与 refresh token 生命周期相同，正常运行时
	// 不会被淘汰；凭证存储丢失时要求用户重新登录是合理的产品行为。
	return "", 0, "", apierror.Unauthorized("invalid refresh token")
}
