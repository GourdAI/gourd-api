/**
 * System API endpoints for admin operations
 */

import { apiClient } from '../client'

/**
 * Restart the service
 */
export async function restartService(): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>('/admin/system/restart')
  return data
}

export const systemAPI = {
  restartService
}

export default systemAPI
