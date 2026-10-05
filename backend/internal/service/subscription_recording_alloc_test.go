package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 记账口径：请求已发生后钱必须记下来。
//
// 背景（2026-10-04 自查）：主计费路径 repo.Apply 原先只接受单个 SubscriptionID，
// 把整笔记到「最早到期」那份钱包；越界部分随该行过期一起消失 = 白送
// （实测 A(10,1天) + B(10,30天)、cost=15 时每人漏 5 USD）。
// 现在两条路径共用 AllocateSubscriptionRecordings，拆分口径不再分叉。

func TestAllocateSubscriptionRecordings(t *testing.T) {
	t.Run("装得下时与纯拆分口径逐字段一致", func(t *testing.T) {
		a := newWallet(70, walletF64(10), 0, time.Hour)
		b := newWallet(71, walletF64(10), 0, 30*24*time.Hour)
		subs := []*UserSubscription{a, b}

		pure, ok := AllocateSubscriptionUsage(subs, 15)
		require.True(t, ok)
		rec := AllocateSubscriptionRecordings(subs, 15)
		require.Equal(t, pure, rec, "降级路径与主路径必须完全同源，否则会分叉")
		require.Len(t, rec, 2, "A 记满 10、B 记 5")
		require.InDelta(t, 10.0, rec[0].Amount, 1e-9)
		require.InDelta(t, 5.0, rec[1].Amount, 1e-9)

		var sum float64
		for _, al := range rec {
			sum += al.Amount
		}
		require.InDelta(t, 15.0, sum, 1e-9, "拆分必须守恒")
	})

	t.Run("装不下时整笔记到首选钱包而不是丢钱", func(t *testing.T) {
		a := newWallet(72, walletF64(1), 0, time.Hour)
		b := newWallet(73, walletF64(1), 0, 24*time.Hour)
		rec := AllocateSubscriptionRecordings([]*UserSubscription{a, b}, 100)
		require.Len(t, rec, 1, "请求已发生，丢弃拆分等于白送")
		require.Equal(t, int64(72), rec[0].SubscriptionID,
			"记到先到期那份，让用量合法越界、后续请求被预检 429")
		require.InDelta(t, 100.0, rec[0].Amount, 1e-9)
	})

	t.Run("关键回归：越界记账不再随早到期行消失", func(t *testing.T) {
		// 这是 LEAK=5.00 的那组数据。修复前整笔 15 记到 A，A 到期后
		// 聚合可花从 5 变成 10（白送 5）；修复后 A 只记 10、B 记 5。
		a := newWallet(76, walletF64(10), 0, time.Hour)
		b := newWallet(77, walletF64(10), 0, 30*24*time.Hour)
		rec := AllocateSubscriptionRecordings([]*UserSubscription{a, b}, 15)
		require.Len(t, rec, 2, "必须拆分，否则 A 到期会把多记的用量一起带走")

		for _, al := range rec {
			if al.SubscriptionID == a.ID {
				a.TotalUsageUSD += al.Amount
			} else {
				b.TotalUsageUSD += al.Amount
			}
		}
		require.InDelta(t, 10.0, a.TotalUsageUSD, 1e-9, "A 只能被记满到自身上限")
		require.InDelta(t, 5.0, b.TotalUsageUSD, 1e-9, "余量落在后到期的 B")

		// A 到期后只剩 B：可花应为 5，而不是 10。
		after, ok := aggregateSubscriptionWallet([]UserSubscription{*b})
		require.True(t, ok)
		require.InDelta(t, 5.0, after.TotalLimit-after.TotalUsage, 1e-9,
			"A 到期后剩余可花必须等于 B 的真实余额（漏钱的判据）")
	})

	t.Run("零成本不产生分配", func(t *testing.T) {
		a := newWallet(74, walletF64(10), 0, time.Hour)
		require.Nil(t, AllocateSubscriptionRecordings([]*UserSubscription{a}, 0))
		require.Nil(t, AllocateSubscriptionRecordings([]*UserSubscription{a}, -3))
	})

	t.Run("无可用钱包行不 panic；全过期时仍归行而不丢钱", func(t *testing.T) {
		require.Nil(t, AllocateSubscriptionRecordings(nil, 5))
		require.Nil(t, AllocateSubscriptionRecordings([]*UserSubscription{nil}, 5))

		// 全部过期只可能出现在「预检后、记账前」这个窄窗口（取数 SQL 本身已过滤
		// expires_at > NOW()）。此时归到该行保留可追溯性，而不是静默丢弃这笔已发生的费用；
		// 且过期行不再参与 Σ可花，所以不会影响其它钱包的余额判定。
		expired := newWallet(75, walletF64(10), 0, -time.Minute)
		rec := AllocateSubscriptionRecordings([]*UserSubscription{expired}, 5)
		require.Len(t, rec, 1, "宁可归到已到期行，也不要把钱记成 0")
		require.Equal(t, int64(75), rec[0].SubscriptionID)
	})
}

func TestFirstBillableSubscription(t *testing.T) {
	t.Run("乱序入参也选到期最早的有限额钱包", func(t *testing.T) {
		// 不依赖调用方传入顺序：这是本次修正的核心。旧实现只取「切片里第一份
		// 命中项」，一旦某个调用方忘了 ORDER BY，钱就会被记到另一份钱包上。
		late := newWallet(80, walletF64(10), 0, 30*24*time.Hour)
		first := newWallet(81, walletF64(10), 0, time.Hour)
		free := newWallet(82, nil, 0, 2*time.Hour)
		got := FirstBillableSubscription([]*UserSubscription{late, free, first})
		require.NotNil(t, got)
		require.Equal(t, int64(81), got.ID, "先到期优先，且不限额钱包不得抢位")
	})

	t.Run("全不限额时退到到期最早的一份", func(t *testing.T) {
		f1 := newWallet(83, nil, 0, 5*time.Hour)
		f2 := newWallet(84, nil, 0, 4*time.Hour)
		got := FirstBillableSubscription([]*UserSubscription{f1, f2})
		require.NotNil(t, got)
		require.Equal(t, int64(84), got.ID)
	})

	t.Run("全部过期时仍返回一份以免静默丢钱", func(t *testing.T) {
		e1 := newWallet(85, walletF64(10), 0, -time.Hour)
		got := FirstBillableSubscription([]*UserSubscription{e1})
		require.NotNil(t, got, "预检后到期的快照：归到该行而不是丢弃")
		require.Equal(t, int64(85), got.ID)
	})

	t.Run("空入参返回nil", func(t *testing.T) {
		require.Nil(t, FirstBillableSubscription(nil))
		require.Nil(t, FirstBillableSubscription([]*UserSubscription{nil}))
	})
}
