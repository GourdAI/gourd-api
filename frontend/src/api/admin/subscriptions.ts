/**
 * Admin Subscriptions API endpoints
 * Handles user subscription management for administrators
 */

import { apiClient } from '../client'
import type {
  UserSubscription,
  SubscriptionProgress,
  AssignSubscriptionRequest,
  BulkAssignSubscriptionRequest,
  ExtendSubscriptionRequest,
  PaginatedResponse
} from '@/types'

export type SubscriptionBulkAction = 'extend' | 'reset_quota' | 'revoke' | 'restore'

export interface SubscriptionBulkActionRequest {
  subscription_ids: number[]
  action: SubscriptionBulkAction
  /** 仅 action='extend' 使用；正整数延长，负整数缩短 */
  days?: number
}

export interface SubscriptionBulkActionResult {
  success_count: number
  failed_count: number
  results: Array<{ subscription_id: number; success: boolean; error?: string }>
}

export interface BulkAssignSubscriptionResult {
  success_count: number
  created_count: number
  reused_count: number
  failed_count: number
  subscriptions: UserSubscription[]
  errors: string[]
  statuses?: Record<string, 'created' | 'reused' | 'failed'>
}

export async function bulkAction(
  request: SubscriptionBulkActionRequest,
  idempotencyKey: string
): Promise<SubscriptionBulkActionResult> {
  const { data } = await apiClient.post<SubscriptionBulkActionResult>(
    '/admin/subscriptions/bulk-action',
    request,
    { headers: { 'Idempotency-Key': idempotencyKey } }
  )
  return data
}

/**
 * List all subscriptions with pagination
 *
 * 订阅 = 个人额度钱包，不绑定分组，因此没有 group_id / platform 筛选（
 * 后端 List 已改为按 user_id / plan_id / status 寻址）。
 * @param page - Page number (default: 1)
 * @param pageSize - Items per page (default: 20)
 * @param filters - Optional filters (status, user_id, plan_id, sort_by, sort_order)
 * @returns Paginated list of subscriptions
 */
export async function list(
  page: number = 1,
  pageSize: number = 20,
  filters?: {
    status?: 'active' | 'expired' | 'revoked' | 'suspended'
    user_id?: number
    plan_id?: number
    sort_by?: string
    sort_order?: 'asc' | 'desc'
  },
  options?: {
    signal?: AbortSignal
  }
): Promise<PaginatedResponse<UserSubscription>> {
  const { data } = await apiClient.get<PaginatedResponse<UserSubscription>>(
    '/admin/subscriptions',
    {
      params: {
        page,
        page_size: pageSize,
        ...filters
      },
      signal: options?.signal
    }
  )
  return data
}

/**
 * Get subscription by ID
 * @param id - Subscription ID
 * @returns Subscription details
 */
export async function getById(id: number): Promise<UserSubscription> {
  const { data } = await apiClient.get<UserSubscription>(`/admin/subscriptions/${id}`)
  return data
}

/**
 * Get subscription progress
 * @param id - Subscription ID
 * @returns Subscription progress with usage stats
 */
export async function getProgress(id: number): Promise<SubscriptionProgress> {
  const { data } = await apiClient.get<SubscriptionProgress>(`/admin/subscriptions/${id}/progress`)
  return data
}

/**
 * Assign subscription to user
 * @param request - Assignment request
 * @returns Created subscription
 */
export async function assign(request: AssignSubscriptionRequest): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>('/admin/subscriptions/assign', request)
  return data
}

/**
 * Bulk assign subscriptions to multiple users
 * @param request - Bulk assignment request
 * @returns Per-user assignment outcomes and created or reused subscriptions
 */
export async function bulkAssign(
  request: BulkAssignSubscriptionRequest
): Promise<BulkAssignSubscriptionResult> {
  const { data } = await apiClient.post<BulkAssignSubscriptionResult>(
    '/admin/subscriptions/bulk-assign',
    request
  )
  return data
}

/**
 * Extend subscription validity
 * @param id - Subscription ID
 * @param request - Extension request with days
 * @returns Updated subscription
 */
export async function extend(
  id: number,
  request: ExtendSubscriptionRequest
): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>(
    `/admin/subscriptions/${id}/extend`,
    request
  )
  return data
}

/**
 * Revoke subscription
 * @param id - Subscription ID
 * @returns Success confirmation
 */
export async function revoke(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>(`/admin/subscriptions/${id}/revoke`)
  return data
}

/**
 * Restore revoked subscription
 * @param id - Subscription ID
 * @returns Restored subscription
 */
export async function restore(id: number): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>(`/admin/subscriptions/${id}/restore`)
  return data
}

/**
 * Reset the used amount of a subscription's total quota pool.
 *
 * 总额池只有一份，不存在日/周/月分档重置，因此本接口无请求体参数。
 * @param id - Subscription ID
 * @returns Updated subscription
 */
export async function resetQuota(id: number): Promise<UserSubscription> {
  const { data } = await apiClient.post<UserSubscription>(
    `/admin/subscriptions/${id}/reset-quota`,
  )
  return data
}

/**
 * List subscriptions by user
 * @param userId - User ID
 * @param page - Page number
 * @param pageSize - Items per page
 * @returns Paginated list of user's subscriptions
 */
export async function listByUser(
  userId: number,
  page: number = 1,
  pageSize: number = 20
): Promise<PaginatedResponse<UserSubscription>> {
  const { data } = await apiClient.get<PaginatedResponse<UserSubscription>>(
    `/admin/users/${userId}/subscriptions`,
    {
      params: { page, page_size: pageSize }
    }
  )
  return data
}

export const subscriptionsAPI = {
  list,
  getById,
  getProgress,
  assign,
  bulkAssign,
  bulkAction,
  extend,
  revoke,
  restore,
  resetQuota,
  listByUser
}

export default subscriptionsAPI
