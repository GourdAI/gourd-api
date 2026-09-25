/**
 * Admin WorkBuddy API endpoints
 * Handles WorkBuddy OAuth device-authorization flows for administrators.
 *
 * 授权流程：generateWorkBuddyAuthUrl 生成设备授权链接（含 state）→ 用户在浏览器
 * 完成登录 → 前端轮询 exchangeWorkBuddyCode（pending → completed）取回凭据。
 */

import { apiClient } from '../client'

/** WorkBuddy 双域：国内 copilot.tencent.com / 国际 www.workbuddy.ai（裸域会被上游 301）。 */
export type WorkBuddyAuthRealm = 'cn' | 'global'

export interface WorkBuddyAuthUrlRequest {
  realm: WorkBuddyAuthRealm
  proxy_id?: number
}

export interface WorkBuddyAuthUrlResponse {
  auth_url: string
  state: string
  realm: WorkBuddyAuthRealm
}

export interface WorkBuddyExchangeCodeRequest {
  state: string
  realm: WorkBuddyAuthRealm
  proxy_id?: number
}

/** 兑换成功后返回的凭据信息（与后端 token 结构对齐）。 */
export interface WorkBuddyTokenInfo {
  access_token?: string
  refresh_token?: string
  expires_at?: number | string
  domain?: string
  realm?: WorkBuddyAuthRealm | string
  uid?: string
  enterprise_id?: string
  nickname?: string
  [key: string]: unknown
}

export interface WorkBuddyExchangePendingResponse {
  status: 'pending'
}

export interface WorkBuddyExchangeCompletedResponse {
  status: 'completed'
  token: WorkBuddyTokenInfo
}

export type WorkBuddyExchangeCodeResponse =
  | WorkBuddyExchangePendingResponse
  | WorkBuddyExchangeCompletedResponse

/** 积分套餐明细（对应后端 WorkBuddyCreditsPackage）。 */
export interface WorkBuddyCreditsPackage {
  package_name?: string
  remain?: number
  used?: number
  size?: number
  cycle_end_time?: string
}

/** 积分查询结果（对应后端 WorkBuddyCreditsResult）。 */
export interface WorkBuddyCreditsResult {
  success: boolean
  realm?: string
  remain?: number
  used?: number
  size?: number
  packs?: number
  packages?: WorkBuddyCreditsPackage[]
  fetched_at?: number
  today_checkin_status?: string
  /** 企业成员账号：个人 billing 资源池无额度可查（非「余额 0」）。 */
  not_applicable?: boolean
  /** 凭据携带 enterprise_id。 */
  enterprise?: boolean
  error?: string
}

/** 签到结果（对应后端 WorkBuddyCheckinResult）。 */
export interface WorkBuddyCheckinResult {
  success: boolean
  status: 'ok' | 'already' | 'fail' | 'skipped'
  detail?: string
  credits?: number | null
  realm?: string
  checked_at?: number
}

/** 活跃上报（含连登奖励链）运行结果（对应后端 WorkBuddyActivityRunResult，JSON 键为下游契约）。 */
export interface WorkBuddyActivityRunResult {
  success: boolean
  realm: string
  reports: number
  reports_requested: number
  streak_days: number
  streak_checked: boolean
  redeem_tier?: string
  credit_granted?: number
  lottery_drawn: boolean
  detail?: string
  ran_at: number
}

/** 猫猫旅行巡检运行结果（对应后端 WorkBuddyTravelRunResult，JSON 键为下游契约）。 */
export interface WorkBuddyTravelRunResult {
  success: boolean
  realm: string
  action: string
  detail?: string
  buddy_name?: string
  state?: string
  reward_credit?: number
  ran_at: number
}

export async function generateWorkBuddyAuthUrl(
  payload: WorkBuddyAuthUrlRequest
): Promise<WorkBuddyAuthUrlResponse> {
  const { data } = await apiClient.post<WorkBuddyAuthUrlResponse>(
    '/admin/workbuddy/oauth/auth-url',
    payload
  )
  return data
}

export async function exchangeWorkBuddyCode(
  payload: WorkBuddyExchangeCodeRequest
): Promise<WorkBuddyExchangeCodeResponse> {
  const { data } = await apiClient.post<WorkBuddyExchangeCodeResponse>(
    '/admin/workbuddy/oauth/exchange-code',
    payload
  )
  return data
}

/** 查询账号剩余积分（后端同时落 extra 快照，列表页下次加载可直接渲染）。 */
export async function queryWorkBuddyCredits(id: number): Promise<WorkBuddyCreditsResult> {
  const { data } = await apiClient.get<WorkBuddyCreditsResult>(
    `/admin/workbuddy/accounts/${id}/credits`
  )
  return data
}

/** 手动签到（幂等：今天已签到返回 status=already，仍是成功语义）。 */
export async function checkinWorkBuddyAccount(id: number): Promise<WorkBuddyCheckinResult> {
  const { data } = await apiClient.post<WorkBuddyCheckinResult>(
    `/admin/workbuddy/accounts/${id}/checkin`
  )
  return data
}

/** 手动跑活跃上报任务（后端同时落 extra 快照 workbuddy_activity）。 */
export async function runWorkBuddyActivity(id: number): Promise<WorkBuddyActivityRunResult> {
  const { data } = await apiClient.post<WorkBuddyActivityRunResult>(
    `/admin/workbuddy/accounts/${id}/activity`
  )
  return data
}

/** 手动跑猫猫旅行巡检（后端同时落 extra 快照 workbuddy_travel）。 */
export async function runWorkBuddyTravel(id: number): Promise<WorkBuddyTravelRunResult> {
  const { data } = await apiClient.post<WorkBuddyTravelRunResult>(
    `/admin/workbuddy/accounts/${id}/travel`
  )
  return data
}

export default {
  generateWorkBuddyAuthUrl,
  exchangeWorkBuddyCode,
  queryWorkBuddyCredits,
  checkinWorkBuddyAccount,
  runWorkBuddyActivity,
  runWorkBuddyTravel,
}
