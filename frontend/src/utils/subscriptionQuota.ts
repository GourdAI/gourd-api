import type { Group, UserSubscription } from '@/types'

const ONE_DAY_MS = 24 * 60 * 60 * 1000

/**
 * 订阅的生效额度（与后端 UserSubscription.EffectiveDailyLimit 同构）：
 * 1. 订阅自有额度（daily_limit_usd）优先，null/<=0 视为不限额；
 * 2. 仅当订阅确实归属该分组（group_id === group.id，且非个人订阅 0）时，才继承分组额度；
 * 3. 都不满足返回 null = 不限额。
 *
 * 注意：不可写成 `sub.daily_limit_usd ?? sub.group?.daily_limit_usd`，
 * 那会在「个人订阅挂在某个展示分组上」时错误继承分组额度。
 */
function effectiveSubLimit(
  sub: Pick<UserSubscription, 'group_id' | 'daily_limit_usd' | 'weekly_limit_usd' | 'monthly_limit_usd'>,
  group: Pick<Group, 'id' | 'daily_limit_usd' | 'weekly_limit_usd' | 'monthly_limit_usd'> | undefined | null,
  field: 'daily_limit_usd' | 'weekly_limit_usd' | 'monthly_limit_usd'
): number | null {
  const own = sub?.[field]
  if (own != null && own > 0) return own
  // 个人订阅（group_id=0）不继承任何分组额度
  if (!group || sub.group_id === 0 || sub.group_id !== group.id) return null
  const inherited = group[field]
  return inherited != null && inherited > 0 ? inherited : null
}

export const effectiveDailyLimit = (sub: UserSubscription, group?: Group | null) =>
  effectiveSubLimit(sub, group, 'daily_limit_usd')

export const effectiveWeeklyLimit = (sub: UserSubscription, group?: Group | null) =>
  effectiveSubLimit(sub, group, 'weekly_limit_usd')

export const effectiveMonthlyLimit = (sub: UserSubscription, group?: Group | null) =>
  effectiveSubLimit(sub, group, 'monthly_limit_usd')

/** 订阅是否不限额（三个窗口都没有生效额度） */
export function subscriptionHasNoLimit(sub: UserSubscription, group?: Group | null): boolean {
  return (
    effectiveDailyLimit(sub, group) == null &&
    effectiveWeeklyLimit(sub, group) == null &&
    effectiveMonthlyLimit(sub, group) == null
  )
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
