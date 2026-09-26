/**
 * Admin Trae API endpoints
 * Handles Trae IDE proxy (字节 Trae) credit queries and daily check-ins for administrators.
 *
 * 探测口径：所有上游探测失败都以 HTTP 200 + success:false + error 返回（与 workbuddy 一致），
 * 调用方必须以 body.success 判成败，不能依赖 HTTP 状态码。
 */

import { apiClient } from '../client'

/** Trae 双域：国内 cn / 国际 global。 */
export type TraeRealm = 'cn' | 'global'

/** 权益包明细（对应后端 TraeCreditsPackage）。 */
export interface TraeCreditsPackage {
  entitlement_id?: string
  kind?: 'checkin' | 'plan' | 'other'
  /** 注意：Trae 积分为浮点数（与 WorkBuddy 的整数积分不同）。 */
  limit: number
  used: number
  remain: number
  expire_at?: number
  expired?: boolean
}

/** 积分查询结果（对应后端 TraeCreditsResult）。 */
export interface TraeCreditsResult {
  success: boolean
  realm: TraeRealm | string
  /** 上游 status 接口口径的当前总积分。 */
  credits: number
  /** 未过期权益包聚合剩余。 */
  remain: number
  used: number
  size: number
  packs: number
  /** 签到入口是否可用。 */
  checkable: boolean
  /** 今日是否已签到（上游权威读数）。 */
  checked_in: boolean
  packages?: TraeCreditsPackage[]
  fetched_at: number
  today_checkin_status?: 'ok' | 'already' | 'fail' | 'skipped'
  /** access token 到期时刻（epoch 秒，缺省=未知）：列表页告警读数。 */
  token_expires_at?: number
  /** refresh token 到期时刻（epoch 秒）：过期后无法自动续期，必须重登。 */
  refresh_expires_at?: number
  error?: string
}

/** 签到结果（对应后端 TraeCheckinResult）。 */
export interface TraeCheckinResult {
  success: boolean
  status: 'ok' | 'already' | 'fail' | 'skipped'
  detail?: string
  credits: number
  has_credits?: boolean
  realm?: string
  checked_at: number
}

/** 换票请求（refresh_token → 新 access_token / refresh_token）。 */
export interface TraeExchangeTokenRequest {
  realm: TraeRealm | string
  refresh_token: string
  client_id?: string
  proxy_id?: number
}

/** 换票结果（对应后端 TraeExchangeResult；refresh_token 为轮换后的新值）。 */
export interface TraeExchangeResult {
  success: boolean
  realm: string
  access_token?: string
  refresh_token?: string
  expires_at?: number
  refresh_expires_at?: number
  uid?: string
  nickname?: string
  base_url?: string
  billing_base_url?: string
  oauth_base_url?: string
  credentials?: Record<string, unknown>
  error?: string
}

/**
 * 浏览器登录会话（auth-url 响应）。与 Qoder 的关键差异：Trae 授权页强制回调到
 * `http://127.0.0.1:<port>/authorize`，回调只会打到**用户本机**、后端永远收不到，
 * 因此没有轮询——必须由用户把浏览器地址栏整串粘回，再调 submitTraeOAuthCallback 换票。
 */
export interface TraeAuthUrlResult {
  login_id: string
  login_url: string
  /** 回调地址前缀（形如 http://127.0.0.1:xxxx/authorize），用于粘贴框 placeholder 提示。 */
  callback_url_prefix: string
  /** 会话到期时刻（epoch 秒），驱动前端倒计时。 */
  expires_at: number
  needs_paste: boolean
}

/** 提交回调地址后的换票结果（credentials 为 snake_case，键可能缺省）。 */
export interface TraeLoginSubmitResult {
  status: 'completed'
  credentials: Record<string, string>
  account_name: string
}

/** 发起浏览器登录（生成授权链接与会话）。 */
export async function startTraeOAuthLogin(payload: {
  realm: TraeRealm
  client_id?: string
  proxy_id?: number
}): Promise<TraeAuthUrlResult> {
  const { data } = await apiClient.post<TraeAuthUrlResult>('/admin/trae/oauth/auth-url', payload)
  return data
}

/**
 * 提交用户粘贴回来的回调地址，由后端换票并回填凭据。
 * 错误码语义：400 TRAE_LOGIN_CALLBACK_INVALID / TRAE_LOGIN_TRACE_MISMATCH /
 * TRAE_LOGIN_NO_CODE / TRAE_LOGIN_CALLBACK_ERROR（链接贴错、不完整或被上游拒绝）、
 * 410 TRAE_LOGIN_EXPIRED（会话过期，需重新发起）、404 TRAE_LOGIN_NOT_FOUND（会话
 * 不存在：服务重启或已被消费）、502 TRAE_LOGIN_GUIDANCE_FAILED /
 * TRAE_LOGIN_EXCHANGE_FAILED（上游故障）。
 */
export async function submitTraeOAuthCallback(payload: {
  login_id: string
  callback_url: string
}): Promise<TraeLoginSubmitResult> {
  const { data } = await apiClient.post<TraeLoginSubmitResult>('/admin/trae/oauth/submit', payload)
  return data
}

/** 放弃登录会话（关弹窗/切平台时调用，失败可静默忽略）。 */
export async function cancelTraeOAuthLogin(loginId: string): Promise<void> {
  await apiClient.post('/admin/trae/oauth/cancel', { login_id: loginId })
}

/** 查询账号剩余积分（后端同时落 extra 快照，列表页下次加载可直接渲染）。 */
export async function queryTraeCredits(id: number): Promise<TraeCreditsResult> {
  const { data } = await apiClient.get<TraeCreditsResult>(
    `/admin/trae/accounts/${id}/credits`
  )
  return data
}

/** 手动签到（幂等：今天已签到返回 status=already，仍是成功语义）。 */
export async function checkinTraeAccount(id: number): Promise<TraeCheckinResult> {
  const { data } = await apiClient.post<TraeCheckinResult>(
    `/admin/trae/accounts/${id}/checkin`
  )
  return data
}

/** 用 refresh_token 换取新的 access_token / refresh_token（后者必须覆盖旧值）。 */
export async function exchangeTraeToken(
  payload: TraeExchangeTokenRequest
): Promise<TraeExchangeResult> {
  const { data } = await apiClient.post<TraeExchangeResult>(
    '/admin/trae/oauth/exchange-token',
    payload
  )
  return data
}

export default {
  queryTraeCredits,
  checkinTraeAccount,
  exchangeTraeToken,
  startTraeOAuthLogin,
  submitTraeOAuthCallback,
  cancelTraeOAuthLogin,
}
