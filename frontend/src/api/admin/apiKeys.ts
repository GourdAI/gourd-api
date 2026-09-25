/**
 * Admin API Keys API endpoints
 * Handles API key management for administrators
 */

import { apiClient } from '../client'
import type { ApiKey } from '@/types'

export interface UpdateApiKeyGroupResult {
  api_key: ApiKey
  auto_granted_group_access: boolean
  granted_group_id?: number
  granted_group_name?: string
}

/**
 * Update an API key's group binding
 * @param id - API Key ID
 * @param groupId - Group ID (0 to unbind, positive to bind, null/undefined to skip)
 * @param groupIds - Optional full group set. When provided it wins over
 *   `groupId`; an empty array unbinds every group. The first entry is primary.
 * @returns Updated API key with auto-grant info
 */
export async function updateApiKeyGroup(
  id: number,
  groupId: number | null,
  groupIds?: number[]
): Promise<UpdateApiKeyGroupResult> {
  const payload: { group_id?: number; group_ids?: number[] } = {
    group_id: groupId === null ? 0 : groupId
  }
  if (groupIds !== undefined) {
    payload.group_ids = groupIds
  }
  const { data } = await apiClient.put<UpdateApiKeyGroupResult>(`/admin/api-keys/${id}`, payload)
  return data
}

export const apiKeysAPI = {
  updateApiKeyGroup
}

export default apiKeysAPI
