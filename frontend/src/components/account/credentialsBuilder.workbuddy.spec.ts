import { describe, expect, it } from 'vitest'

import {
  buildWorkbuddyCredentials,
  workbuddyJwtExpiry
} from './credentialsBuilder'

/**
 * WorkBuddy 凭据组装：expires_at 由 access_token 的 JWT exp 自动推导。
 *
 * WHY：后端 workbuddyTokenNeedsRefresh 的判据是「ExpiresAt <= 0 → 不主动刷新」。
 * 本函数历史上一律不写 expires_at，导致手工粘贴的账号永不预刷新，token 到期只能撞 401
 * （2026-10-24 全量 401 事件的起点）。后端虽有读取侧 JWT 兜底，但写入侧补齐才能让
 * 落库数据本身完整（后台刷新与运维面板都直接读库）。
 */

/** 构造一个只带 exp 声明的 JWT（payload 用 base64url，与真实签发形态一致）。 */
function jwtWithExp(exp: number): string {
  const payload = btoa(JSON.stringify({ exp, sub: 'uid-1' }))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '')
  return `header.${payload}.signature`
}

const baseFields = {
  accessToken: '',
  refreshToken: '',
  deviceToken: '',
  uid: '',
  enterpriseId: '',
  nickname: '',
  realm: 'cn' as const,
  baseUrl: ''
}

describe('workbuddyJwtExpiry', () => {
  it('reads exp from a standard JWT payload', () => {
    const exp = 1_800_000_000
    expect(workbuddyJwtExpiry(jwtWithExp(exp))).toBe(exp)
  })

  it('normalises a millisecond epoch (13 digits) to seconds', () => {
    const exp = 1_800_000_000
    expect(workbuddyJwtExpiry(jwtWithExp(exp * 1000))).toBe(exp)
  })

  it('returns null instead of fabricating an expiry when it cannot be read', () => {
    // 非 JWT / 段数不足 / 非法 base64 / 无 exp / 非正 exp 都必须是 null。
    // 关键在于**不伪造**：写一个 0 或当前时间会让后端把健康 token 判成已过期。
    expect(workbuddyJwtExpiry('')).toBeNull()
    expect(workbuddyJwtExpiry('not-a-jwt')).toBeNull()
    expect(workbuddyJwtExpiry('a.b')).toBeNull()
    expect(workbuddyJwtExpiry('header.%%%.sig')).toBeNull()
    expect(workbuddyJwtExpiry(jwtWithExp(0))).toBeNull()
    expect(workbuddyJwtExpiry(jwtWithExp(-100))).toBeNull()
  })
})

describe('buildWorkbuddyCredentials', () => {
  it('writes expires_at derived from the access token', () => {
    const exp = 1_800_000_000
    const credentials = buildWorkbuddyCredentials({
      ...baseFields,
      accessToken: jwtWithExp(exp)
    })
    expect(credentials.expires_at).toBe(exp)
    expect(credentials.access_token).toBe(jwtWithExp(exp))
  })

  it('omits expires_at for a non-JWT token rather than writing a bogus value', () => {
    // 兼容粘贴任意形态 token 的场景：解不出 exp 就不写，保持「未知」语义
    // （后端对缺失值不主动刷新，是刻意的宽容行为，不是缺陷）。
    const credentials = buildWorkbuddyCredentials({
      ...baseFields,
      accessToken: 'opaque-token-without-dots'
    })
    expect(credentials).not.toHaveProperty('expires_at')
    expect(credentials.access_token).toBe('opaque-token-without-dots')
  })

  it('does not write expires_at when only a refresh token is provided', () => {
    const credentials = buildWorkbuddyCredentials({
      ...baseFields,
      refreshToken: 'rt-only'
    })
    expect(credentials).not.toHaveProperty('expires_at')
    expect(credentials.refresh_token).toBe('rt-only')
  })
})
