package account_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/handlerzzy/feedsystem/internal/account"
	"github.com/handlerzzy/feedsystem/internal/account/mock"
	"github.com/handlerzzy/feedsystem/internal/apierror"

	"go.uber.org/mock/gomock"
)

// TestServiceErrorsCarryHTTPStatus 固化本次错误契约收口的核心不变量：
// 服务层返回的错误必须同时满足两件事——
//  1. 分类器能判出正确的 HTTP 状态码（apierror.ClassifyHTTPStatus）；
//  2. 对外文案与改造前逐字一致（apierror.PublicMessage 就是客户端将看到的内容）。
//
// 为什么必须断言状态码、而不只是"错误非空"：改造前这些 errors.New 裸错误的
// 分类结果全部是 500 —— 那正是本批次要修的问题。只断言 err != nil
// 在改造前后都成立，无法区分有没有修好。
func TestServiceErrorsCarryHTTPStatus(t *testing.T) {
	cases := []struct {
		name       string
		wantStatus int
		wantMsg    string
		sentinel   error // 非 nil 时额外断言 errors.Is 仍然成立
		setup      func(store *mock.MockAccountStore)
		call       func(svc *account.AccountService) error
	}{
		{
			name:       "CreateAccount 空白用户名 -> 400",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "username is required",
			call: func(svc *account.AccountService) error {
				return svc.CreateAccount(context.Background(), &account.Account{
					Username: "   ",
					Password: "secret123",
				})
			},
		},
		{
			name:       "CreateAccount 唯一键冲突 -> 409 且仍可用 errors.Is 识别",
			wantStatus: http.StatusConflict,
			wantMsg:    "username already exists",
			sentinel:   account.ErrUsernameTaken,
			setup: func(store *mock.MockAccountStore) {
				store.EXPECT().CreateAccount(gomock.Any(), gomock.Any()).Return(dupUsernameErr())
			},
			call: func(svc *account.AccountService) error {
				return svc.CreateAccount(context.Background(), &account.Account{
					Username: "alice",
					Password: "secret123",
				})
			},
		},
		{
			name:       "Rename 新用户名为空 -> 400 且仍可用 errors.Is 识别",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "new_username is required",
			sentinel:   account.ErrNewUsernameRequired,
			call: func(svc *account.AccountService) error {
				_, err := svc.Rename(context.Background(), 1, "")
				return err
			},
		},
		{
			name:       "Rename 唯一键冲突 -> 409 且仍可用 errors.Is 识别",
			wantStatus: http.StatusConflict,
			wantMsg:    "username already exists",
			sentinel:   account.ErrUsernameTaken,
			setup: func(store *mock.MockAccountStore) {
				store.EXPECT().RenameWithToken(gomock.Any(), uint(1), "bob", gomock.Any()).
					Return(dupUsernameErr())
			},
			call: func(svc *account.AccountService) error {
				_, err := svc.Rename(context.Background(), 1, "bob")
				return err
			},
		},
		{
			name:       "UpdateProfile 无可更新字段 -> 400",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "nothing to update",
			call: func(svc *account.AccountService) error {
				return svc.UpdateProfile(context.Background(), 1, &account.UpdateProfileRequest{})
			},
		},
		{
			name:       "RefreshAccessToken 空 token -> 400",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "refresh token is empty",
			call: func(svc *account.AccountService) error {
				_, _, _, err := svc.RefreshAccessToken(context.Background(), "")
				return err
			},
		},
		{
			name:       "RefreshAccessToken 无效 token -> 401",
			wantStatus: http.StatusUnauthorized,
			wantMsg:    "invalid refresh token",
			call: func(svc *account.AccountService) error {
				_, _, _, err := svc.RefreshAccessToken(context.Background(), "never-issued")
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store := mock.NewMockAccountStore(ctrl)
			if tc.setup != nil {
				tc.setup(store)
			}
			// cache 传 nil：RefreshAccessToken 会直接走"视为无效"分支，
			// 无需 miniredis 即可验证。
			svc := account.NewAccountService(store, nil)

			err := tc.call(svc)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if got := apierror.ClassifyHTTPStatus(err); got != tc.wantStatus {
				t.Errorf("ClassifyHTTPStatus(%v) = %d, want %d", err, got, tc.wantStatus)
			}
			// PublicMessage 与 Respond 写入响应体的内容一致：文案必须逐字不变。
			if got := apierror.PublicMessage(err); got != tc.wantMsg {
				t.Errorf("PublicMessage(%v) = %q, want %q", err, got, tc.wantMsg)
			}
			if got := err.Error(); got != tc.wantMsg {
				t.Errorf("err.Error() = %q, want %q (对外文案必须逐字保留)", got, tc.wantMsg)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, %v) = false, want true", tc.sentinel)
			}
		})
	}
}

// TestSentinelErrorsAreBothIdentifiableAndCoded 固化两个哨兵错误的新形态：
// 它们既要能被 errors.Is 识别（既有代码与测试依赖），又要自带状态码，
// 使得 handler 不必再硬编码 409/400；被 %w 包装后两者仍需成立。
func TestSentinelErrorsAreBothIdentifiableAndCoded(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"ErrUsernameTaken", account.ErrUsernameTaken, http.StatusConflict, "username already exists"},
		{"ErrNewUsernameRequired", account.ErrNewUsernameRequired, http.StatusBadRequest, "new_username is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apierror.ClassifyHTTPStatus(tc.err); got != tc.wantCode {
				t.Errorf("ClassifyHTTPStatus = %d, want %d", got, tc.wantCode)
			}
			if got := tc.err.Error(); got != tc.wantMsg {
				t.Errorf("err.Error() = %q, want %q", got, tc.wantMsg)
			}
			if !errors.Is(tc.err, tc.err) {
				t.Error("哨兵错误必须能被 errors.Is 以自身命中")
			}
			// 文案相同但值不同的错误不得被误判：errors.Is 靠的是值本身，不是字符串。
			if errors.Is(errors.New(tc.wantMsg), tc.err) {
				t.Errorf("文案相同的新错误 %q 不应被识别为哨兵", tc.wantMsg)
			}

			wrapped := fmt.Errorf("service layer: %w", tc.err)
			if !errors.Is(wrapped, tc.err) {
				t.Error("哨兵被 %w 包装后必须仍可识别")
			}
			if got := apierror.ClassifyHTTPStatus(wrapped); got != tc.wantCode {
				t.Errorf("包装后的状态码 = %d, want %d", got, tc.wantCode)
			}
			if got := apierror.PublicMessage(wrapped); got != tc.wantMsg {
				t.Errorf("包装后的对外文案 = %q, want %q", got, tc.wantMsg)
			}
		})
	}
}

// TestUnclassifiedServerErrorStaysServerSide 确认改造没有把"未知错误"
// 也伪装成客户端错误：真正的服务端故障仍是 500，且对外只给通用文案。
// 这正是 A/C 类站点改用 apierror.Respond 后 handler 的实际行为。
func TestUnclassifiedServerErrorStaysServerSide(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockAccountStore(ctrl)
	svc := account.NewAccountService(store, nil)

	dbErr := errors.New("connection refused: dial tcp 10.0.0.5:3306")
	store.EXPECT().CreateAccount(gomock.Any(), gomock.Any()).Return(dbErr)

	err := svc.CreateAccount(context.Background(), &account.Account{
		Username: "alice",
		Password: "secret123",
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got := apierror.ClassifyHTTPStatus(err); got != http.StatusInternalServerError {
		t.Errorf("ClassifyHTTPStatus = %d, want 500", got)
	}
	if got := apierror.PublicMessage(err); got != "internal server error" {
		t.Errorf("PublicMessage = %q, want the generic message（内部细节只能进日志）", got)
	}
	if errors.Is(err, account.ErrUsernameTaken) {
		t.Error("数据库连接错误不得被误判为用户名冲突")
	}
}
