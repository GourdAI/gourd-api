import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { WorkBuddyAuthRealm, WorkBuddyTokenInfo } from '@/api/admin/workbuddy'
import { extractApiErrorMessage, extractI18nErrorMessage } from '@/utils/apiError'

/** 轮询间隔（毫秒）：设备授权登录通常 10~60 秒完成，2.5s 兼顾及时性与请求量。 */
export const WORKBUDDY_OAUTH_POLL_INTERVAL_MS = 2500
/** 轮询总超时（毫秒）：超过后自动停止并提示，避免无限轮询。 */
export const WORKBUDDY_OAUTH_POLL_TIMEOUT_MS = 5 * 60 * 1000

/** pollForToken 单次轮询的结果：pending / completed（含 token）/ null（失败）。 */
export type WorkBuddyPollResult =
  | { status: 'pending' }
  | { status: 'completed'; token: WorkBuddyTokenInfo }

export function useWorkBuddyOAuth() {
  const appStore = useAppStore()
  const { t } = useI18n()

  const authUrl = ref('')
  const state = ref('')
  const realm = ref<WorkBuddyAuthRealm>('cn')
  const loading = ref(false)
  const error = ref('')

  // 轮询控制：定时器与超时计时器句柄（组件卸载时须 stopPolling 清理）。
  let pollTimer: ReturnType<typeof setTimeout> | null = null
  let timeoutTimer: ReturnType<typeof setTimeout> | null = null
  const polling = ref(false)

  const clearTimers = () => {
    if (pollTimer !== null) {
      clearTimeout(pollTimer)
      pollTimer = null
    }
    if (timeoutTimer !== null) {
      clearTimeout(timeoutTimer)
      timeoutTimer = null
    }
  }

  /** 停止轮询并清理所有定时器（幂等）。 */
  const stopPolling = () => {
    clearTimers()
    polling.value = false
  }

  const resetState = () => {
    stopPolling()
    authUrl.value = ''
    state.value = ''
    loading.value = false
    error.value = ''
  }

  const generateAuthUrl = async (
    targetRealm: WorkBuddyAuthRealm,
    proxyId?: number | null
  ): Promise<boolean> => {
    loading.value = true
    authUrl.value = ''
    state.value = ''
    error.value = ''

    try {
      const payload: { realm: WorkBuddyAuthRealm; proxy_id?: number } = { realm: targetRealm }
      if (proxyId) payload.proxy_id = proxyId

      const response = await adminAPI.workbuddy.generateWorkBuddyAuthUrl(payload)
      authUrl.value = response.auth_url
      state.value = response.state
      realm.value = (response.realm as WorkBuddyAuthRealm) || targetRealm
      return true
    } catch (err: any) {
      error.value = extractApiErrorMessage(
        err,
        t('admin.accounts.workbuddy.oauthFailedToGenerateUrl')
      )
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  /**
   * 单次轮询兑换授权码。
   * - 后端返回 pending → { status: 'pending' }
   * - 后端返回 completed → { status: 'completed', token }
   * - 请求失败 → null（error 已填充）
   */
  const pollForToken = async (
    targetRealm?: WorkBuddyAuthRealm,
    proxyId?: number | null
  ): Promise<WorkBuddyPollResult | null> => {
    const currentState = state.value
    if (!currentState) {
      error.value = t('admin.accounts.workbuddy.oauthMissingState')
      return null
    }

    try {
      const payload: { state: string; realm: WorkBuddyAuthRealm; proxy_id?: number } = {
        state: currentState,
        realm: targetRealm || realm.value,
      }
      if (proxyId) payload.proxy_id = proxyId

      const response = await adminAPI.workbuddy.exchangeWorkBuddyCode(payload)
      if (response.status === 'completed') {
        const token = (response.token || {}) as WorkBuddyTokenInfo
        // realm 保持一致：后端回传的 realm 优先，缺省沿用请求值。
        if (!token.realm) token.realm = payload.realm
        return { status: 'completed', token }
      }
      return { status: 'pending' }
    } catch (err: any) {
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.workbuddy.oauthErrors',
        t('admin.accounts.workbuddy.oauthFailedToExchange')
      )
      return null
    }
  }

  /**
   * 启动轮询循环：每 intervalMs 调一次 pollForToken，直到：
   * - onCompleted(token) 返回后停止（completed）
   * - onError(error) 返回后停止（连续请求失败）
   * - 超时（timeoutMs）后停止并回调 onTimeout
   * 组件侧负责在卸载/关闭时调用 stopPolling()。
   */
  const startPolling = (
    handlers: {
      onCompleted: (token: WorkBuddyTokenInfo) => void
      onTimeout?: () => void
      onError?: (message: string) => void
    },
    options: {
      realm?: WorkBuddyAuthRealm
      proxyId?: number | null
      intervalMs?: number
      timeoutMs?: number
    } = {}
  ) => {
    stopPolling()
    const intervalMs = options.intervalMs ?? WORKBUDDY_OAUTH_POLL_INTERVAL_MS
    const timeoutMs = options.timeoutMs ?? WORKBUDDY_OAUTH_POLL_TIMEOUT_MS
    const targetRealm = options.realm || realm.value

    polling.value = true

    timeoutTimer = setTimeout(() => {
      stopPolling()
      handlers.onTimeout?.()
    }, timeoutMs)

    const tick = async () => {
      if (!polling.value) return
      const result = await pollForToken(targetRealm, options.proxyId)
      if (!polling.value) return

      if (!result) {
        // 单次失败不立即终止：网络抖动容忍，交由超时兜底；仅上报错误消息。
        handlers.onError?.(error.value)
        error.value = ''
        pollTimer = setTimeout(tick, intervalMs)
        return
      }

      if (result.status === 'completed') {
        stopPolling()
        handlers.onCompleted(result.token)
        return
      }

      pollTimer = setTimeout(tick, intervalMs)
    }

    pollTimer = setTimeout(tick, intervalMs)
  }

  /**
   * 组装 WorkBuddy 凭据（snake_case，与后端约定一致）。
   * 仅写入后端返回的非空字段，避免空值覆盖。
   */
  const buildCredentials = (token: WorkBuddyTokenInfo): Record<string, unknown> => {
    const credentials: Record<string, unknown> = {}
    if (token.access_token) credentials.access_token = token.access_token
    if (token.refresh_token) credentials.refresh_token = token.refresh_token
    if (token.expires_at !== undefined && token.expires_at !== null) {
      credentials.expires_at = token.expires_at
    }
    if (token.domain) credentials.domain = token.domain
    if (token.realm) credentials.realm = token.realm
    if (token.uid) credentials.uid = token.uid
    if (token.enterprise_id) credentials.enterprise_id = token.enterprise_id
    if (token.nickname) credentials.nickname = token.nickname
    return credentials
  }

  return {
    // state
    authUrl,
    state,
    realm,
    loading,
    error,
    polling,
    // methods
    resetState,
    generateAuthUrl,
    pollForToken,
    startPolling,
    stopPolling,
    buildCredentials,
  }
}
