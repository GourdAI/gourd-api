/**
 * User Subscription API
 * API for regular users to view their own subscriptions and progress
 */

import { apiClient } from './client'
import type { UserSubscription, SubscriptionProgress } from '@/types'

/**
 * Subscription summary for user dashboard
 *
 * 字段与后端 handler.SubscriptionSummaryItem / summary 信封逐一对应
 * （backend/internal/handler/subscription_handler.go:16-28,169-173）。
 * 订阅已改为「个人额度钱包」：单一总额池，无日/周/月分档，也不携带分组信息。
 * total_limit_usd / remaining_usd 为 null 表示不限额。
 */
export interface SubscriptionSummary {
  active_count: number
  total_used_usd: number
  total_limit_usd: number | null
  unlimited: boolean
  subscriptions: Array<{
    id: number
    plan_id?: number | null
    name: string
    status: string
    expires_at?: string | null
    total_limit_usd?: number | null
    total_usage_usd?: number
    remaining_usd?: number | null
    unlimited: boolean
  }>
}

/**
 * Get list of current user's subscriptions
 */
export async function getMySubscriptions(): Promise<UserSubscription[]> {
  const response = await apiClient.get<UserSubscription[]>('/subscriptions')
  return response.data
}

/**
 * Get current user's active subscriptions
 */
export async function getActiveSubscriptions(): Promise<UserSubscription[]> {
  const response = await apiClient.get<UserSubscription[]>('/subscriptions/active')
  return response.data
}

/**
 * Get progress for all user's active subscriptions
 */
export async function getSubscriptionsProgress(): Promise<SubscriptionProgress[]> {
  const response = await apiClient.get<SubscriptionProgress[]>('/subscriptions/progress')
  return response.data
}

/**
 * Get subscription summary for dashboard display
 */
export async function getSubscriptionSummary(): Promise<SubscriptionSummary> {
  const response = await apiClient.get<SubscriptionSummary>('/subscriptions/summary')
  return response.data
}

/**
 * Get progress for a specific subscription
 */
export async function getSubscriptionProgress(
  subscriptionId: number
): Promise<SubscriptionProgress> {
  const response = await apiClient.get<SubscriptionProgress>(
    `/subscriptions/${subscriptionId}/progress`
  )
  return response.data
}

/**
 * 可自助领取的套餐（免费额度类）。
 * 后端只返回 for_sale=true 且 self_claimable=true 的套餐，
 * already_claimed 表示当前用户已持有同一套餐（前端据此置灰按钮）。
 */
export interface ClaimableSubscriptionPlan {
  id: number
  name: string
  description: string
  total_limit_usd: number | null
  /** none = 一次性总额池；daily/weekly/monthly = 每周期重发额度 */
  reset_period: 'none' | 'daily' | 'weekly' | 'monthly'
  validity_days: number
  validity_unit: string
  features: string
  sort_order: number
  already_claimed: boolean
}

/** 领取结果：already_claimed=true 表示命中了幂等（未新建、未叠加额度） */
export interface ClaimSubscriptionResult {
  subscription: UserSubscription
  already_claimed: boolean
}

/**
 * Get plans the current user may self-claim
 */
export async function getClaimablePlans(): Promise<ClaimableSubscriptionPlan[]> {
  const response = await apiClient.get<ClaimableSubscriptionPlan[]>('/subscriptions/claimable')
  return response.data
}

/**
 * Self-claim a free subscription plan.
 * 同一套餐重复领取是幂等的（不叠加额度），因此可以安全重试。
 */
export async function claimPlan(planId: number): Promise<ClaimSubscriptionResult> {
  const response = await apiClient.post<ClaimSubscriptionResult>('/subscriptions/claim', {
    plan_id: planId
  })
  return response.data
}

export default {
  getMySubscriptions,
  getActiveSubscriptions,
  getSubscriptionsProgress,
  getSubscriptionSummary,
  getSubscriptionProgress,
  getClaimablePlans,
  claimPlan
}
