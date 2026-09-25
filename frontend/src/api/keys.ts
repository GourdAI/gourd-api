/**
 * API Keys management endpoints
 * Handles CRUD operations for user API keys
 */

import { apiClient } from './client'
import type { ApiKey, CreateApiKeyRequest, UpdateApiKeyRequest, PaginatedResponse } from '@/types'

/**
 * Normalize the accepted group inputs (array, single ID or `null`) into a clean
 * ID list. Order is preserved because the first entry becomes the primary group.
 */
export function normalizeGroupIds(groups?: number[] | number | null): number[] {
  if (groups === null || groups === undefined) return []
  const list = Array.isArray(groups) ? groups : [groups]
  return [...new Set(list.filter((id) => Number.isFinite(id) && id > 0))]
}

/**
 * List all API keys for current user
 * @param page - Page number (default: 1)
 * @param pageSize - Items per page (default: 10)
 * @param filters - Optional filter parameters
 * @param options - Optional request options
 * @returns Paginated list of API keys
 */
export async function list(
  page: number = 1,
  pageSize: number = 10,
  filters?: {
    search?: string
    status?: string
    group_id?: number | string
    sort_by?: string
    sort_order?: 'asc' | 'desc'
  },
  options?: {
    signal?: AbortSignal
  }
): Promise<PaginatedResponse<ApiKey>> {
  const { data } = await apiClient.get<PaginatedResponse<ApiKey>>('/keys', {
    params: { page, page_size: pageSize, ...filters },
    signal: options?.signal
  })
  return data
}

/**
 * Get API key by ID
 * @param id - API key ID
 * @returns API key details
 */
export async function getById(id: number): Promise<ApiKey> {
  const { data } = await apiClient.get<ApiKey>(`/keys/${id}`)
  return data
}

/**
 * Create new API key
 * @param name - Key name
 * @param groups - Target groups. Accepts an array (preferred), a single group ID
 *   (legacy calls), or `null`/`undefined` to leave the key without a group.
 * @param customKey - Optional custom key value
 * @param ipWhitelist - Optional IP whitelist
 * @param ipBlacklist - Optional IP blacklist
 * @param quota - Optional quota limit in USD (0 = unlimited)
 * @param expiresInDays - Optional days until expiry (undefined = never expires)
 * @param rateLimitData - Optional rate limit fields
 * @returns Created API key
 */
export async function create(
  name: string,
  groups?: number[] | number | null,
  customKey?: string,
  ipWhitelist?: string[],
  ipBlacklist?: string[],
  quota?: number,
  expiresInDays?: number,
  rateLimitData?: { rate_limit_5h?: number; rate_limit_1d?: number; rate_limit_7d?: number }
): Promise<ApiKey> {
  const payload: CreateApiKeyRequest = { name }
  const groupIds = normalizeGroupIds(groups)
  if (groupIds.length > 0) {
    payload.group_ids = groupIds
    // Keep the primary group in sync for older clients/backends.
    payload.group_id = groupIds[0]
  }
  if (customKey) {
    payload.custom_key = customKey
  }
  if (ipWhitelist && ipWhitelist.length > 0) {
    payload.ip_whitelist = ipWhitelist
  }
  if (ipBlacklist && ipBlacklist.length > 0) {
    payload.ip_blacklist = ipBlacklist
  }
  if (quota !== undefined && quota > 0) {
    payload.quota = quota
  }
  if (expiresInDays !== undefined && expiresInDays > 0) {
    payload.expires_in_days = expiresInDays
  }
  if (rateLimitData?.rate_limit_5h && rateLimitData.rate_limit_5h > 0) {
    payload.rate_limit_5h = rateLimitData.rate_limit_5h
  }
  if (rateLimitData?.rate_limit_1d && rateLimitData.rate_limit_1d > 0) {
    payload.rate_limit_1d = rateLimitData.rate_limit_1d
  }
  if (rateLimitData?.rate_limit_7d && rateLimitData.rate_limit_7d > 0) {
    payload.rate_limit_7d = rateLimitData.rate_limit_7d
  }

  const { data } = await apiClient.post<ApiKey>('/keys', payload)
  return data
}

/**
 * Update API key
 * @param id - API key ID
 * @param updates - Fields to update. Pass `group_ids` to replace the whole group
 *   set (`[]` unbinds every group) or `group_id` for the legacy single-group form.
 * @returns Updated API key
 */
export async function update(id: number, updates: UpdateApiKeyRequest): Promise<ApiKey> {
  const { data } = await apiClient.put<ApiKey>(`/keys/${id}`, updates)
  return data
}

export interface BulkUpdateApiKeysResult {
  succeededIds: number[]
  failures: Array<{ id: number; error: unknown }>
}

/** Reuse per-key validation and permissions, with at most five requests in flight. */
export async function bulkUpdate(
  ids: number[],
  updates: UpdateApiKeyRequest
): Promise<BulkUpdateApiKeysResult> {
  const uniqueIds = [...new Set(ids)]
  const result: BulkUpdateApiKeysResult = { succeededIds: [], failures: [] }
  for (let offset = 0; offset < uniqueIds.length; offset += 5) {
    const batch = uniqueIds.slice(offset, offset + 5)
    const responses = await Promise.allSettled(batch.map((id) => update(id, updates)))
    responses.forEach((response, index) => {
      if (response.status === 'fulfilled') {
        result.succeededIds.push(batch[index])
      } else {
        result.failures.push({ id: batch[index], error: response.reason })
      }
    })
  }
  return result
}

/**
 * Delete API key
 * @param id - API key ID
 * @returns Success confirmation
 */
export async function deleteKey(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/keys/${id}`)
  return data
}

/**
 * Toggle API key status (active/inactive)
 * @param id - API key ID
 * @param status - New status
 * @returns Updated API key
 */
export async function toggleStatus(id: number, status: 'active' | 'inactive'): Promise<ApiKey> {
  return update(id, { status })
}

export const keysAPI = {
  list,
  getById,
  create,
  update,
  bulkUpdate,
  delete: deleteKey,
  toggleStatus
}

export default keysAPI
