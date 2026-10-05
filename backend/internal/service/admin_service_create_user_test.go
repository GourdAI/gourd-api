//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAdminService_CreateUser_Success(t *testing.T) {
	repo := &userRepoStub{nextID: 10}
	svc := &adminServiceImpl{userRepo: repo}
	balance := 12.5

	input := &CreateUserInput{
		Email:         "user@test.com",
		Password:      "strong-pass",
		Username:      "tester",
		Notes:         "note",
		Balance:       &balance,
		Concurrency:   7,
		AllowedGroups: []int64{3, 5},
	}

	user, err := svc.CreateUser(context.Background(), input)
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, int64(10), user.ID)
	require.Equal(t, input.Email, user.Email)
	require.Equal(t, input.Username, user.Username)
	require.Equal(t, input.Notes, user.Notes)
	require.Equal(t, balance, user.Balance)
	require.Equal(t, input.Concurrency, user.Concurrency)
	require.Equal(t, input.AllowedGroups, user.AllowedGroups)
	require.Equal(t, RoleUser, user.Role)
	require.Equal(t, StatusActive, user.Status)
	require.True(t, user.CheckPassword(input.Password))
	require.Len(t, repo.created, 1)
	require.Equal(t, user, repo.created[0])
}

func TestAdminService_CreateUser_UsesDefaultBalanceWhenBalanceOmitted(t *testing.T) {
	repo := &userRepoStub{nextID: 11}
	cfg := &config.Config{
		Default: config.DefaultConfig{
			UserBalance: 0,
		},
	}
	settingService := NewSettingService(&settingRepoStub{values: map[string]string{
		SettingKeyDefaultBalance: "0.02",
	}}, cfg)
	svc := &adminServiceImpl{userRepo: repo, settingService: settingService}

	user, err := svc.CreateUser(context.Background(), &CreateUserInput{
		Email:    "default-balance@test.com",
		Password: "strong-pass",
	})

	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, 0.02, user.Balance)
	require.Len(t, repo.created, 1)
	require.Equal(t, 0.02, repo.created[0].Balance)
}

func TestAdminService_CreateUser_ExplicitZeroBalanceOverridesDefault(t *testing.T) {
	repo := &userRepoStub{nextID: 12}
	cfg := &config.Config{
		Default: config.DefaultConfig{
			UserBalance: 0,
		},
	}
	settingService := NewSettingService(&settingRepoStub{values: map[string]string{
		SettingKeyDefaultBalance: "0.02",
	}}, cfg)
	svc := &adminServiceImpl{userRepo: repo, settingService: settingService}
	balance := 0.0

	user, err := svc.CreateUser(context.Background(), &CreateUserInput{
		Email:    "zero-balance@test.com",
		Password: "strong-pass",
		Balance:  &balance,
	})

	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, 0.0, user.Balance)
	require.Len(t, repo.created, 1)
	require.Equal(t, 0.0, repo.created[0].Balance)
}

func TestAdminService_CreateUser_EmailExists(t *testing.T) {
	repo := &userRepoStub{createErr: ErrEmailExists}
	svc := &adminServiceImpl{userRepo: repo}

	_, err := svc.CreateUser(context.Background(), &CreateUserInput{
		Email:    "dup@test.com",
		Password: "password",
	})
	require.ErrorIs(t, err, ErrEmailExists)
	require.Empty(t, repo.created)
}

func TestAdminService_CreateUser_CreateError(t *testing.T) {
	createErr := errors.New("db down")
	repo := &userRepoStub{createErr: createErr}
	svc := &adminServiceImpl{userRepo: repo}

	_, err := svc.CreateUser(context.Background(), &CreateUserInput{
		Email:    "user@test.com",
		Password: "password",
	})
	require.ErrorIs(t, err, createErr)
	require.Empty(t, repo.created)
}

func TestAdminService_CreateUser_AssignsDefaultSubscriptions(t *testing.T) {
	repo := &userRepoStub{nextID: 21}
	assigner := &defaultSubscriptionAssignerStub{}
	cfg := &config.Config{
		Default: config.DefaultConfig{
			UserBalance:     0,
			UserConcurrency: 1,
		},
	}
	settingService := NewSettingService(&settingRepoStub{values: map[string]string{
		// 订阅钱包化：默认发放配置不再指向分组，而是「总额度 USD + 有效天数」。
		SettingKeyDefaultSubscriptions: `[{"total_limit_usd":5,"validity_days":30}]`,
	}}, cfg)
	svc := &adminServiceImpl{
		userRepo:           repo,
		settingService:     settingService,
		defaultSubAssigner: assigner,
	}

	_, err := svc.CreateUser(context.Background(), &CreateUserInput{
		Email:    "new-user@test.com",
		Password: "password",
	})
	require.NoError(t, err)
	require.Len(t, assigner.calls, 1)
	require.Equal(t, int64(21), assigner.calls[0].UserID)
	// 发放时记录的「来源」：订阅已不绑定分组，来源只可能是钱包额度；
	// 保留这条断言以锁定「配置里的额度真的透传到了发放入参」。
	require.NotNil(t, assigner.calls[0].TotalLimitUSD)
	require.InDelta(t, 5, *assigner.calls[0].TotalLimitUSD, 0.0001)
	require.Nil(t, assigner.calls[0].PlanID, "默认发放不带套餐来源")
	require.Equal(t, 30, assigner.calls[0].ValidityDays)
}
