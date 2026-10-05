import type { UserSubscription } from '@/types'

const ONE_DAY_MS = 24 * 60 * 60 * 1000

/**
 * 订阅生效额度（与后端 UserSubscription.EffectiveTotalLimit 同构）：
 * 总额度只长在钱包自身（发放时从套餐快照进订阅行），null / <=0 视为不限额。
 *
 * 订阅不绑定分组、不继承分组额度（2026-10-03 重构）：旧版的日/周/月三档
 * 以及「回退继承分组额度」逻辑已全部退役，因此本函数不再接受 group 入参。
 */
export function effectiveTotalLimit(
  sub: Pick<UserSubscription, 'total_limit_usd'> | null | undefined
): number | null {
  const own = sub?.total_limit_usd
  return own != null && own > 0 ? own : null
}

/** 订阅是否不限额（总额池未设有效额度） */
export function subscriptionHasNoLimit(sub: UserSubscription): boolean {
  return effectiveTotalLimit(sub) == null
}

/** 订阅总额池已用占比（0~100）；不限额返回 0 */
export function subscriptionUsagePercentage(sub: UserSubscription): number {
  const limit = effectiveTotalLimit(sub)
  if (!limit) return 0
  const percentage = ((sub.total_usage_usd || 0) / limit) * 100
  return Number.isFinite(percentage) ? percentage : 0
}

export type ExpirationDateRelation = 'expired' | 'today' | 'tomorrow' | 'later'

export type RemainingExpiryDuration =
  | { unit: 'days'; days: number }
  | { unit: 'hoursMinutes'; hours: number; minutes: number }

export interface RemainingDurationParts {
  days: number
  hours: number
  minutes: number
}

export function isOneTimeDailyQuota(
  subscription: Pick<UserSubscription, 'starts_at' | 'expires_at'>
): boolean {
  if (!subscription.starts_at || !subscription.expires_at) return false

  const startsAt = new Date(subscription.starts_at).getTime()
  const expiresAt = new Date(subscription.expires_at).getTime()

  if (!Number.isFinite(startsAt) || !Number.isFinite(expiresAt)) return false

  return expiresAt <= startsAt + ONE_DAY_MS
}

export function getRemainingDurationParts(
  targetAt: Date | string,
  now: Date = new Date()
): RemainingDurationParts | null {
  const targetTime = targetAt instanceof Date ? targetAt.getTime() : new Date(targetAt).getTime()
  const nowTime = now.getTime()

  if (!Number.isFinite(targetTime) || !Number.isFinite(nowTime)) return null

  const diffMs = targetTime - nowTime
  if (diffMs <= 0) return null

  const totalMinutes = Math.floor(diffMs / (1000 * 60))
  const days = Math.floor(totalMinutes / (24 * 60))
  const hours = Math.floor((totalMinutes % (24 * 60)) / 60)
  const minutes = totalMinutes % 60

  return { days, hours, minutes }
}

export function getExpirationDateRelation(
  targetAt: Date | string,
  now: Date = new Date()
): ExpirationDateRelation | null {
  const target = targetAt instanceof Date ? targetAt : new Date(targetAt)
  const targetTime = target.getTime()
  const nowTime = now.getTime()

  if (!Number.isFinite(targetTime) || !Number.isFinite(nowTime)) return null
  if (targetTime <= nowTime) return 'expired'

  const targetDay = Date.UTC(target.getFullYear(), target.getMonth(), target.getDate())
  const currentDay = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate())
  const calendarDays = Math.round((targetDay - currentDay) / ONE_DAY_MS)

  if (calendarDays === 0) return 'today'
  if (calendarDays === 1) return 'tomorrow'
  return 'later'
}

export function getRemainingExpiryDuration(
  targetAt: Date | string,
  now: Date = new Date()
): RemainingExpiryDuration | null {
  const targetTime = targetAt instanceof Date ? targetAt.getTime() : new Date(targetAt).getTime()
  const nowTime = now.getTime()

  if (!Number.isFinite(targetTime) || !Number.isFinite(nowTime)) return null

  const diffMs = targetTime - nowTime
  if (diffMs <= 0) return null
  if (diffMs >= ONE_DAY_MS) {
    return { unit: 'days', days: Math.ceil(diffMs / ONE_DAY_MS) }
  }

  const totalMinutes = Math.ceil(diffMs / (60 * 1000))
  return {
    unit: 'hoursMinutes',
    hours: Math.floor(totalMinutes / 60),
    minutes: totalMinutes % 60
  }
}
