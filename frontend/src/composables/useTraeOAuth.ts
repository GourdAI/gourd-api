import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { TraeRealm } from '@/api/admin/trae'
import { extractApiErrorMessage, extractI18nErrorMessage } from '@/utils/apiError'

/**
 * 会话兜底有效期（毫秒）：后端未回 expires_at 时按 15 分钟计（与后端会话 TTL 同口径）。
 * Trae 浏览器登录会话比 Qoder 短得多——授权链接一次性，过期只能重新发起。
 */
export const TRAE_OAUTH_SESSION_TTL_MS = 15 * 60 * 1000
/** 倒计时刷新间隔（毫秒）：只用于渲染 mm:ss，不参与任何请求判定。 */
export const TRAE_OAUTH_COUNTDOWN_TICK_MS = 1000

/** submitCallback 的成功结果：凭据（snake_case）+ 后端推导出的账号名。 */
export type TraeOAuthSubmitResult = {
  credentials: Record<string, string>
  accountName: string
}

/**
 * Trae 真 OAuth 浏览器登录 composable（镜像 useQoderOAuth 的结构与错误处理）。
 *
 * 与 Qoder / WorkBuddy 的关键差异：**没有轮询**。Trae 授权页强制要求回调地址是
 * `http://127.0.0.1:<port>/authorize`，回调只会打到用户本机端口，后端永远收不到，
 * 所以流程是「点登录 → 新标签完成登录 → 浏览器停在 127.0.0.1（显示无法访问属正常）
 * → 用户把地址栏整串粘回 → 点提交 → 后端拿 code 换票」。
 * 会话 15 分钟过期，用 expires_at 驱动一个倒计时；超时后提示重新发起并禁用提交。
 */
export function useTraeOAuth() {
  const appStore = useAppStore()
  const { t } = useI18n()

  const loginUrl = ref('')
  const loginId = ref('')
  // 回调地址前缀（形如 http://127.0.0.1:xxxx/authorize）：粘贴框 placeholder 提示用。
  const callbackUrlPrefix = ref('')
  // 用户粘回的浏览器地址栏整串（组件侧 v-model，410 时由本 composable 清空）。
  const pastedUrl = ref('')
  const realm = ref<TraeRealm>('cn')
  const loading = ref(false)
  const submitting = ref(false)
  const error = ref('')

  // 会话到期时刻（epoch 毫秒）与倒计时读数；定时器仅驱动倒计时渲染。
  const expiresAtMs = ref(0)
  const remainingMs = ref(0)
  let countdownTimer: ReturnType<typeof setInterval> | null = null

  const clearCountdown = () => {
    if (countdownTimer !== null) {
      clearInterval(countdownTimer)
      countdownTimer = null
    }
  }

  /** 会话是否仍有效（已发起且未过期）。 */
  const sessionActive = computed(() => Boolean(loginId.value) && remainingMs.value > 0)
  /** 会话是否已过期（发起过且倒计时归零）——用于禁用提交并提示重新发起。 */
  const expired = computed(() => Boolean(loginId.value) && remainingMs.value <= 0)
  /** 剩余时间 mm:ss（倒计时渲染，超过 60 分钟也按分钟累加，不会出现 h 位）。 */
  const countdownText = computed(() => {
    const total = Math.max(0, Math.ceil(remainingMs.value / 1000))
    const minutes = Math.floor(total / 60)
    const seconds = total % 60
    return `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`
  })

  const tick = () => {
    remainingMs.value = Math.max(0, expiresAtMs.value - Date.now())
    if (remainingMs.value === 0) {
      // 归零即停表：过期后不再空转定时器，提交按钮由 expired 禁用。
      clearCountdown()
      error.value = t('admin.accounts.trae.oauth.expired')
    }
  }

  /** 按 expires_at 启动倒计时（幂等：先清旧表）。 */
  const startCountdown = () => {
    clearCountdown()
    tick()
    if (remainingMs.value > 0) {
      countdownTimer = setInterval(tick, TRAE_OAUTH_COUNTDOWN_TICK_MS)
    }
  }

  const stopCountdown = () => {
    clearCountdown()
  }

  const resetState = () => {
    clearCountdown()
    loginUrl.value = ''
    loginId.value = ''
    callbackUrlPrefix.value = ''
    pastedUrl.value = ''
    loading.value = false
    submitting.value = false
    error.value = ''
    expiresAtMs.value = 0
    remainingMs.value = 0
  }

  /**
   * 发起浏览器登录：向后端申请授权链接与会话。
   * 成功 → true（调用方据此 window.open(loginUrl)）；失败 → false（error 已填充并 toast）。
   */
  const startLogin = async (targetRealm: TraeRealm, proxyId?: number | null): Promise<boolean> => {
    // in-flight guard：按钮 disabled 依赖 DOM 异步更新，快速连点仍会进来第二个调用；
    // 并发两个会话会让后到的响应覆盖 login_id（先建会话泄漏，粘回旧链接报 TRACE_MISMATCH）。
    if (loading.value) return false
    clearCountdown()
    loading.value = true
    error.value = ''
    loginUrl.value = ''
    loginId.value = ''
    callbackUrlPrefix.value = ''
    pastedUrl.value = ''
    expiresAtMs.value = 0
    remainingMs.value = 0

    try {
      const payload: { realm: TraeRealm; proxy_id?: number } = { realm: targetRealm }
      if (proxyId) payload.proxy_id = proxyId

      const response = await adminAPI.trae.startTraeOAuthLogin(payload)
      loginUrl.value = response.login_url || ''
      loginId.value = response.login_id || ''
      callbackUrlPrefix.value = response.callback_url_prefix || ''
      realm.value = targetRealm
      // 会话过期时刻由后端签发（epoch 秒）；缺省或异常值时回落 TTL 兜底，
      // 否则倒计时会立刻归零把用户挡在门外。
      const issued = Number(response.expires_at)
      expiresAtMs.value =
        Number.isFinite(issued) && issued > 0 ? issued * 1000 : Date.now() + TRAE_OAUTH_SESSION_TTL_MS
      startCountdown()
      return Boolean(loginId.value)
    } catch (err: any) {
      error.value = extractApiErrorMessage(err, t('admin.accounts.trae.oauth.startFailed'))
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  /**
   * 提交用户粘贴回来的回调地址，由后端换票。
   * - 成功 → { credentials, accountName }（会话即结束：停表并清空 login_id）
   * - 失败 → null（error 已填充；410 会话过期会同时清空粘贴框并置为过期态）
   */
  const submitCallback = async (
    callbackUrl?: string
  ): Promise<TraeOAuthSubmitResult | null> => {
    // in-flight guard：重复提交时第二个请求会因会话已被一次性消费而拿到 404，
    // 把第一次的「成功」状态覆盖成错误提示并清空粘贴框。
    if (submitting.value) return null
    const url = (callbackUrl ?? pastedUrl.value).trim()
    if (!url) {
      error.value = t('admin.accounts.trae.oauth.callbackMissing')
      return null
    }
    if (!loginId.value) {
      error.value = t('admin.accounts.trae.oauth.sessionMissing')
      return null
    }
    if (remainingMs.value <= 0) {
      error.value = t('admin.accounts.trae.oauth.expired')
      return null
    }

    submitting.value = true
    error.value = ''
    try {
      const response = await adminAPI.trae.submitTraeOAuthCallback({
        login_id: loginId.value,
        callback_url: url
      })
      const credentials = response.credentials || {}
      // 换票成功后会话作废：停表并清空 login_id / 粘贴框，避免关闭弹窗时再发一次
      // 多余 cancel，也让提交按钮自动回到禁用态（防用户重复提交同一回调链接）。
      clearCountdown()
      remainingMs.value = 0
      loginId.value = ''
      pastedUrl.value = ''
      return {
        credentials,
        accountName: response.account_name || ''
      }
    } catch (err: any) {
      error.value = extractI18nErrorMessage(
        err,
        t,
        'admin.accounts.trae.oauth.errors',
        t('admin.accounts.trae.oauth.submitFailed')
      )
      // 410 TRAE_LOGIN_EXPIRED（会话过期）/ 404 TRAE_LOGIN_NOT_FOUND（服务重启或已被消费）：
      // 粘贴的链接再贴也没用，清空粘贴框并停表，让 UI 明确引导「重新发起登录」。
      const status = Number((err as { status?: number })?.status)
      const reason = (err as { reason?: string })?.reason
      const sessionGone =
        status === 410 ||
        status === 404 ||
        reason === 'TRAE_LOGIN_EXPIRED' ||
        reason === 'TRAE_LOGIN_NOT_FOUND'
      if (sessionGone) {
        clearCountdown()
        remainingMs.value = 0
        pastedUrl.value = ''
      }
      return null
    } finally {
      submitting.value = false
    }
  }

  /**
   * 放弃登录会话（关闭弹窗 / 切换平台 / 组件卸载时调用）。
   * 后端取消失败不影响前端收尾，故整段静默吞掉异常。
   */
  const cancelLogin = async (): Promise<void> => {
    const currentLoginId = loginId.value
    clearCountdown()
    if (!currentLoginId) return
    loginId.value = ''
    remainingMs.value = 0
    try {
      await adminAPI.trae.cancelTraeOAuthLogin(currentLoginId)
    } catch {
      // 静默：会话本身 15 分钟后自然过期，取消失败无需打扰用户。
    }
  }

  // 组件侧负责在关闭弹窗 / 卸载时调用 stopCountdown() 与 cancelLogin()（与
  // useQoderOAuth 让调用方 stopPolling 的纪律保持一致，不在 composable 里挂生命周期）。

  return {
    // state
    loginUrl,
    loginId,
    callbackUrlPrefix,
    pastedUrl,
    realm,
    loading,
    submitting,
    error,
    remainingMs,
    sessionActive,
    expired,
    countdownText,
    // methods
    resetState,
    startLogin,
    submitCallback,
    cancelLogin,
    stopCountdown,
  }
}
