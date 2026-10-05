package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 第三轮 review：管理员「只改额度」不得被 validity_days / notes 幂等冲突挡掉。
// 有效订阅分支既不续期也不写备注，却拿这两项做 400 闸门，
// 会让任何被续期过（ExpiresAt != StartsAt+N）或带备注的订阅彻底改不动额度。
//
// 钱包化后额度入口收敛为一个：AssignSubscriptionInput.TotalLimitUSD
// （日/周/月三档 DailyLimitUSD / WeeklyLimitUSD / MonthlyLimitUSD 已随窗口概念删除），
// 所以「带额度的调额度请求永不冲突」这条不变式只需盯住一个字段。
func TestDetectAssignSemanticConflict_QuotaOnlyRequestNeverConflicts(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	planID := int64(3)
	existing := &UserSubscription{
		ID: 1, UserID: 7, PlanID: &planID,
		StartsAt:  start,
		ExpiresAt: start.AddDate(0, 0, 60), // 按 60 天分配，且此后被续期过
		Status:    SubscriptionStatusActive,
		Notes:     "assigned via redeem code",
	}
	amount := 5.0
	zero := 0.0

	cases := []struct {
		name  string
		input *AssignSubscriptionInput
	}{
		{"前端默认30天与现有60天不符", &AssignSubscriptionInput{UserID: 7, PlanID: &planID, ValidityDays: 30, Notes: "same", TotalLimitUSD: &amount}},
		{"备注与现有不同", &AssignSubscriptionInput{UserID: 7, PlanID: &planID, ValidityDays: 60, Notes: "totally different", TotalLimitUSD: &amount}},
		{"只填总额度", &AssignSubscriptionInput{UserID: 7, PlanID: &planID, ValidityDays: 1, TotalLimitUSD: &amount}},
		{"填 0 表示改回不限额", &AssignSubscriptionInput{UserID: 7, PlanID: &planID, ValidityDays: 99, TotalLimitUSD: &zero}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, conflict := detectAssignSemanticConflict(existing, tc.input)
			require.False(t, conflict, "带额度的调额度请求不应报冲突，got reason=%q", reason)
		})
	}
}

// 反向守卫：不带额度的纯幂等重复分配，严格判定必须保留（防误覆盖他人订阅）。
func TestDetectAssignSemanticConflict_StillGuardsQuotalessAssign(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	planID := int64(3)
	existing := &UserSubscription{
		ID: 1, UserID: 7, PlanID: &planID,
		StartsAt: start, ExpiresAt: start.AddDate(0, 0, 30),
		Status: SubscriptionStatusActive, Notes: "same",
	}

	reason, conflict := detectAssignSemanticConflict(existing, &AssignSubscriptionInput{
		UserID: 7, PlanID: &planID, ValidityDays: 60, Notes: "same",
	})
	require.True(t, conflict)
	require.Equal(t, "validity_days_mismatch", reason)

	reason, conflict = detectAssignSemanticConflict(existing, &AssignSubscriptionInput{
		UserID: 7, PlanID: &planID, ValidityDays: 30, Notes: "other",
	})
	require.True(t, conflict)
	require.Equal(t, "notes_mismatch", reason)
}
