import { describe, expect, it } from 'vitest'

import {
  QODER_CN_BASE_URL,
  QODER_GLOBAL_BASE_URL,
  QODER_REALM_OPTIONS,
  buildQoderCredentials,
  defaultQoderBaseUrl,
  isQoderPlatform,
  resolveQoderRealm,
  validateQoderCredentials
} from './credentialsBuilder'

describe('qoder credentials', () => {
  it('builds snake_case credentials and skips empty optional fields', () => {
    const credentials = buildQoderCredentials({
      accessToken: ' q-token ',
      refreshToken: '',
      expiresAt: '',
      deviceToken: 'q-device',
      uid: ' q-uid ',
      realm: 'global',
      baseUrl: ' https://api1.qoder.sh ',
      nickname: ''
    })

    expect(credentials).toEqual({
      access_token: 'q-token',
      device_token: 'q-device',
      realm: 'global',
      uid: 'q-uid',
      base_url: 'https://api1.qoder.sh'
    })
  })

  it('normalizes expires_at to a unix-seconds number and skips invalid input', () => {
    const valid = buildQoderCredentials({
      accessToken: 'q-token',
      refreshToken: '',
      expiresAt: ' 1893456000 ',
      deviceToken: '',
      uid: '',
      realm: 'cn',
      baseUrl: '',
      nickname: ''
    })
    expect(valid.expires_at).toBe(1893456000)

    const invalid = buildQoderCredentials({
      accessToken: 'q-token',
      refreshToken: '',
      expiresAt: 'not-a-number',
      deviceToken: '',
      uid: '',
      realm: 'cn',
      baseUrl: '',
      nickname: ''
    })
    expect('expires_at' in invalid).toBe(false)
  })

  it('keeps optional nickname when provided', () => {
    const credentials = buildQoderCredentials({
      accessToken: 'q-token',
      refreshToken: 'q-refresh',
      expiresAt: '',
      deviceToken: '',
      uid: '',
      realm: 'cn',
      baseUrl: '',
      nickname: ' Qoder 工作区 '
    })
    expect(credentials.nickname).toBe('Qoder 工作区')
    expect(credentials.refresh_token).toBe('q-refresh')
    // 与 buildWorkbuddyCredentials 同构：baseUrl 留空时不写入 base_url，
    // 默认值由表单批次调用 defaultQoderBaseUrl 填充。
    expect('base_url' in credentials).toBe(false)
  })

  it('requires access_token, accepting dt- / drt- / pt- tokens', () => {
    expect(validateQoderCredentials('')).toBe(false)
    expect(validateQoderCredentials('   ')).toBe(false)
    expect(validateQoderCredentials('q-token')).toBe(true)
    expect(validateQoderCredentials('dt-abc123')).toBe(true)
    expect(validateQoderCredentials('drt-abc123')).toBe(true)
    expect(validateQoderCredentials('pt-abc123')).toBe(true)
  })

  it('resolves realm defaults and CN/global base urls', () => {
    expect(resolveQoderRealm(undefined)).toBe('cn')
    expect(resolveQoderRealm('cn')).toBe('cn')
    expect(resolveQoderRealm('global')).toBe('global')
    expect(resolveQoderRealm('bogus')).toBe('cn')
    expect(defaultQoderBaseUrl()).toBe(QODER_CN_BASE_URL)
    expect(defaultQoderBaseUrl('cn')).toBe(QODER_CN_BASE_URL)
    expect(defaultQoderBaseUrl('global')).toBe(QODER_GLOBAL_BASE_URL)
    expect(QODER_CN_BASE_URL).toBe('https://gateway.qoder.com.cn')
    expect(QODER_GLOBAL_BASE_URL).toBe('https://api1.qoder.sh')
  })

  it('exposes realm options mirroring the workbuddy shape', () => {
    expect(QODER_REALM_OPTIONS).toEqual([
      { value: 'cn', labelKey: 'cn' },
      { value: 'global', labelKey: 'global' }
    ])
  })

  it('detects the qoder platform', () => {
    expect(isQoderPlatform('qoder')).toBe(true)
    expect(isQoderPlatform('workbuddy')).toBe(false)
    expect(isQoderPlatform('')).toBe(false)
  })
})
