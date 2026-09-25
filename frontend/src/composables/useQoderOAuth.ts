import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { QoderAuthRealm } from '@/api/admin/qoder'
import { extractApiErrorMessage, extractI18nErrorMessage } from '@/utils/apiError'

/** 轮询间隔（毫秒）：设备授权登录通常 10~60 秒完成，2.5s 兼顾及时性与请求量。 */
export const QODER_OAUTH_POLL_INTERVAL_MS = 2500
/** 轮询总超时（毫秒）：超过后自动停止并提示，避免无限轮询。 */
export const QODER_OAUTH_POLL_TIMEOUT_MS = 5 * 60 * 1000

/** pollForCredentials 单次轮询的结果：pending / completed（含凭据）/ null（失败）。 */
export type QoderPollResult =
  | { status: 'pending' }
  | { status: 'completed'; credentials: Record<string, string>; accountName: string }

/**
 * Qoder OAuth 设备授权登录 composable（镜像 useWorkBuddyOAuth）。
 * 与 WorkBuddy 的差异：后端以 login_id（而非 state）关联会话，且 exchange
 * 直接返回 snake_case credentials（无独立 pending/status 字段）——凭据非空
 * 即视为 completed，否则视为 pending 继续轮询。
 */
export function useQoderOAuth() {
  const appStore = useAppStore()
  const { t } = useI18n()

  const loginUrl = ref('')
  const loginId = ref('')
  const realm = ref<QoderAuthRealm>('cn')
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
    loginUrl.value = ''
    loginId.value = ''
    loading.value = false
    error.value = ''
  }

  const generateAuthUrl = async (targetRealm: QoderAuthRealm): Promise<boolean> => {
    loading.value = true
    loginUrl.value = ''
    loginId.value = ''
    error.value = ''

    try {
      const response = await adminAPI.qoder.generateQoderAuthUrl(targetRealm)
      loginUrl.value = response.login_url
      loginId.value = response.login_id
      realm.value = targetRealm
      return true
    } catch (err: any) {
      error.value = extractApiErrorMessage(err, t('admin.accounts.qoder.oauthFailed'))
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  /**
   * 单次轮询兑换凭据。
   * - 后端返回空凭据（用户尚未完成登录）→ { status: 'pending' }
   * - 后端返回 credentials → { status: 'completed', credentials, accountName }
   * - 请求失败 → null（error 已填充）
   */
  const pollForCredentials = async (
    targetRealm?: QoderAuthRealm
  ): Promise<QoderPollResult | null> => {
    const currentLoginId = loginId.value
    if (!currentLoginId) {
      error.value = t('admin.accounts.qoder.oauthFailed')
      return null
    }

    try {
      const response = await adminAPI.qoder.exchangeQoderCode(
        currentLoginId,
        targetRealm || realm.value
      )
      const credentials = response.credentials || {}
      if (Object.keys(credentials).length > 0) {
        return {
          status: 'completed',
          credentials,
          accountName: response.account_name || ''
        }
      }
      return { status: 'pending' }
    } catch (err: any) {
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.qoder.oauthErrors',
        t('admin.accounts.qoder.oauthFailed')
      )
      return null
    }
  }

  /**
   * 启动轮询循环：每 intervalMs 调一次 pollForCredentials，直到：
   * - onCompleted(credentials, accountName) 返回后停止（completed）
   * - onError(error) 返回后停止（连续请求失败）
   * - 超时（timeoutMs）后停止并回调 onTimeout
   * 组件侧负责在卸载/关闭时调用 stopPolling()。
   */
  const startPolling = (
    handlers: {
      onCompleted: (credentials: Record<string, string>, accountName: string) => void
      onTimeout?: () => void
      onError?: (message: string) => void
    },
    options: {
      realm?: QoderAuthRealm
      intervalMs?: number
      timeoutMs?: number
    } = {}
  ) => {
    stopPolling()
    const intervalMs = options.intervalMs ?? QODER_OAUTH_POLL_INTERVAL_MS
    const timeoutMs = options.timeoutMs ?? QODER_OAUTH_POLL_TIMEOUT_MS
    const targetRealm = options.realm || realm.value

    polling.value = true

    timeoutTimer = setTimeout(() => {
      stopPolling()
      handlers.onTimeout?.()
    }, timeoutMs)

    const tick = async () => {
      if (!polling.value) return
      const result = await pollForCredentials(targetRealm)
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
        handlers.onCompleted(result.credentials, result.accountName)
        return
      }

      pollTimer = setTimeout(tick, intervalMs)
    }

    pollTimer = setTimeout(tick, intervalMs)
  }

  return {
    // state
    loginUrl,
    loginId,
    realm,
    loading,
    error,
    polling,
    // methods
    resetState,
    generateAuthUrl,
    pollForCredentials,
    startPolling,
    stopPolling,
  }
}
