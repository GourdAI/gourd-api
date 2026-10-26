/**
 * Admin TLS Fingerprint Profile API endpoints
 * Handles TLS fingerprint profile CRUD for administrators
 */

import { apiClient } from '../client'

/**
 * TLS fingerprint profile interface
 */
export interface TLSFingerprintProfile {
  id: number
  name: string
  description: string | null
  enable_grease: boolean
  cipher_suites: number[]
  curves: number[]
  point_formats: number[]
  signature_algorithms: number[]
  alpn_protocols: string[]
  supported_versions: number[]
  key_share_groups: number[]
  psk_modes: number[]
  extensions: number[]
  created_at: string
  updated_at: string
}

/**
 * Create profile request
 */
export interface CreateProfileRequest {
  name: string
  description?: string | null
  enable_grease?: boolean
  cipher_suites?: number[]
  curves?: number[]
  point_formats?: number[]
  signature_algorithms?: number[]
  alpn_protocols?: string[]
  supported_versions?: number[]
  key_share_groups?: number[]
  psk_modes?: number[]
  extensions?: number[]
}

/**
 * Update profile request
 */
export interface UpdateProfileRequest {
  name?: string
  description?: string | null
  enable_grease?: boolean
  cipher_suites?: number[]
  curves?: number[]
  point_formats?: number[]
  signature_algorithms?: number[]
  alpn_protocols?: string[]
  supported_versions?: number[]
  key_share_groups?: number[]
  psk_modes?: number[]
  extensions?: number[]
}

export async function list(): Promise<TLSFingerprintProfile[]> {
  const { data } = await apiClient.get<TLSFingerprintProfile[]>('/admin/tls-fingerprint-profiles')
  return data
}

/**
 * 各平台的 TLS 指纹默认开关（平台级配置 gateway.<platform>.tls_fingerprint_enabled）。
 *
 * 新建账号表单需要它：新建时没有已存在账号、拿不到账号级三态回显，若不知道「该平台
 * 默认开启」就会把表单显示成 OFF，而创建完实际在伪装（WorkBuddy/Trae/Qoder）。
 */
export async function platformDefaults(): Promise<Record<string, boolean>> {
  const { data } = await apiClient.get<{ platform_defaults?: Record<string, boolean> }>(
    '/admin/tls-fingerprint-platform-defaults',
  )
  return data?.platform_defaults ?? {}
}

export async function getById(id: number): Promise<TLSFingerprintProfile> {
  const { data } = await apiClient.get<TLSFingerprintProfile>(`/admin/tls-fingerprint-profiles/${id}`)
  return data
}

export async function create(profileData: CreateProfileRequest): Promise<TLSFingerprintProfile> {
  const { data } = await apiClient.post<TLSFingerprintProfile>('/admin/tls-fingerprint-profiles', profileData)
  return data
}

export async function update(id: number, updates: UpdateProfileRequest): Promise<TLSFingerprintProfile> {
  const { data } = await apiClient.put<TLSFingerprintProfile>(`/admin/tls-fingerprint-profiles/${id}`, updates)
  return data
}

export async function deleteProfile(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/tls-fingerprint-profiles/${id}`)
  return data
}

export const tlsFingerprintProfileAPI = {
  list,
  getById,
  create,
  update,
  delete: deleteProfile,
  platformDefaults
}

export default tlsFingerprintProfileAPI
