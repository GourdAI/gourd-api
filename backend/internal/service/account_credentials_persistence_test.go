//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// persistAccountCredentials 是凭据写入的唯一汇聚点（token 刷新 / 身份自愈 / 订阅补全
// 全部经此）。管理端「同步上游模型」的 preview 入口用**未落库**的凭据现拼一个
// Account{ID:0}，而 Trae 换票、Qoder jobToken 交换、WorkBuddy 刷新、Qoder 身份自愈
// 都会在拉目录途中调用本函数。
//
// 没有 ID<=0 守卫时，写库表现为「UPDATE accounts ... WHERE id = 0」命中 0 行 →
// ErrAccountNotFound：白跑一个事务（还连带 scheduler outbox），且部分链路会把这个
// 错误冒泡成「同步失败」，让一次本来成功的目录拉取前功尽弃。

// previewCredentialRepo 记录所有写调用，用于断言「一次都没发生」。
type previewCredentialRepo struct {
	mockAccountRepoForGemini
	credentialWrites []int64
	extraWrites      []int64
}

func (r *previewCredentialRepo) UpdateCredentials(_ context.Context, id int64, _ map[string]any) error {
	r.credentialWrites = append(r.credentialWrites, id)
	if id <= 0 {
		// 真实仓储在这里是「命中 0 行 → ErrAccountNotFound」，桩保持一致。
		return ErrAccountNotFound
	}
	return nil
}

func (r *previewCredentialRepo) UpdateExtra(_ context.Context, id int64, _ map[string]any) error {
	r.extraWrites = append(r.extraWrites, id)
	return nil
}

func (r *previewCredentialRepo) Update(_ context.Context, account *Account) error {
	r.credentialWrites = append(r.credentialWrites, account.ID)
	return nil
}

func TestPersistAccountCredentials_TemporaryAccountNeverWrites(t *testing.T) {
	repo := &previewCredentialRepo{}
	temp := &Account{
		// ID 恒为 0：preview 的临时账号没有对应数据库行。
		Platform:    PlatformWorkbuddy,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "old"},
	}

	require.NoError(t, persistAccountCredentials(context.Background(), repo, temp, map[string]any{
		"access_token":  "rotated",
		"refresh_token": "rotated-rt",
	}))

	require.Empty(t, repo.credentialWrites, "临时账号不得发起任何凭据写库")
	require.Empty(t, repo.extraWrites, "临时账号不得发起任何 extra 写库")
	// 内存凭据必须更新：本次请求（以及紧随其后的目录请求）要用轮换后的新票。
	require.Equal(t, "rotated", temp.GetCredential("access_token"))
	require.Equal(t, "rotated-rt", temp.GetCredential("refresh_token"))
}

func TestPersistAccountCredentials_NegativeIDAlsoSkips(t *testing.T) {
	repo := &previewCredentialRepo{}
	temp := &Account{
		ID:          -7,
		Platform:    PlatformTrae,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{},
	}

	require.NoError(t, persistAccountCredentials(context.Background(), repo, temp, map[string]any{"access_token": "x"}))
	require.Empty(t, repo.credentialWrites)
	require.Equal(t, "x", temp.GetCredential("access_token"))
}

func TestPersistAccountCredentials_SavedAccountStillWrites(t *testing.T) {
	// 反向护栏：守卫只针对临时账号。已落库账号的轮换票必须照常持久化，
	// 否则网关下次请求会拿着过期票反复撞 401。
	repo := &previewCredentialRepo{}
	saved := &Account{
		ID:          4242,
		Platform:    PlatformTrae,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"access_token": "old"},
	}

	require.NoError(t, persistAccountCredentials(context.Background(), repo, saved, map[string]any{"access_token": "new"}))
	require.Equal(t, []int64{4242}, repo.credentialWrites, "真实账号必须写库一次")
	require.Equal(t, "new", saved.GetCredential("access_token"))
}
