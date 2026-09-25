/**
 * Admin Qoder API endpoints
 * Handles Qoder OAuth authorization flows for administrators.
 *
 * 授权流程：generateQoderAuthUrl 生成 qoder.com 授权链接（含 login_id）→ 用户在浏览器
 * 完成登录 → 前端调用 exchangeQoderCode 取回凭据。
 */

import { apiClient } from '../client'

/** Qoder 双域：国内 gateway.qoder.com.cn / 国际 api1.qoder.sh。 */
export type QoderAuthRealm = 'cn' | 'global'

export interface QoderAuthUrlResponse {
  login_url: string
  login_id: string
}

export interface QoderAuthExchangeResponse {
  credentials: Record<string, string>
  account_name?: string
}

export async function generateQoderAuthUrl(
  realm: QoderAuthRealm
): Promise<QoderAuthUrlResponse> {
  const { data } = await apiClient.post<QoderAuthUrlResponse>(
    '/admin/qoder/oauth/auth-url',
    { realm }
  )
  return data
}

export async function exchangeQoderCode(
  loginId: string,
  realm: QoderAuthRealm
): Promise<QoderAuthExchangeResponse> {
  const { data } = await apiClient.post<QoderAuthExchangeResponse>(
    '/admin/qoder/oauth/exchange-code',
    { login_id: loginId, realm }
  )
  return data
}

/** 单个专属资源包摘要（dedicatedResourcePackages 条目）。 */
export interface QoderCreditPack {
  name?: string
  remain: number
  used: number
  total: number
  status?: string
  expires_at?: number
  available: boolean
}

/** 积分余额 + 活动状态查询结果（后端同时落 extra 快照）。 */
export interface QoderCreditsResult {
  success: boolean
  realm?: string
  user_type?: string
  /** 汇总余额 = 套餐内 + 资源包（活动赠送计入资源包）。 */
  remaining: number
  used: number
  total: number
  plan_remain: number
  plan_total: number
  addon_remain: number
  addon_total: number
  packs: number
  packages?: QoderCreditPack[]
  quota_exceeded: boolean
  plan_expires_at?: number
  fetched_at: number
  /** 本轮是否有可领取的活动。 */
  claimable: boolean
  claim_amount?: number
  claim_campaign_key?: string
  claim_campaign_end_at?: number
  /** 本轮领取状态（already 等），空串表示未领。 */
  today_claim_status?: string
  round?: string
  error?: string
}

/** 领取结果（状态机：ok / already / fail / skipped）。 */
export interface QoderCheckinResult {
  success: boolean
  status: 'ok' | 'already' | 'fail' | 'skipped'
  detail?: string
  credits?: number
  amount?: number
  grant_id?: string
  expires_at?: string
  realm?: string
  round?: string
  checked_at: number
}

/** 查询账号积分余额与活动可领取状态。 */
export async function queryQoderCredits(id: number): Promise<QoderCreditsResult> {
  const { data } = await apiClient.get<QoderCreditsResult>(`/admin/qoder/accounts/${id}/credits`)
  return data
}

/**
 * 手动领取本轮活动 Credits。
 * 幂等：同一轮重复领取上游返回 replayed，本接口回报 status=already（仍为成功语义）。
 */
export async function checkinQoderAccount(id: number): Promise<QoderCheckinResult> {
  const { data } = await apiClient.post<QoderCheckinResult>(`/admin/qoder/accounts/${id}/checkin`)
  return data
}

export const qoderAPI = {
  generateQoderAuthUrl,
  exchangeQoderCode,
  queryQoderCredits,
  checkinQoderAccount
}

export default qoderAPI
