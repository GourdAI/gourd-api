<template>
  <div
    v-if="visible"
    data-test="trae-credits"
    class="min-w-[220px] space-y-1"
  >
    <!-- 积分行：快照/查询结果直接渲染（悬停看权益包明细与更新时间）。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <span
        data-test="trae-credits-remain"
        class="inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-gray-700 dark:text-gray-200"
        :title="creditsTooltip"
      >
        <span class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.trae.creditsRemain') }}</span>
        <span class="font-semibold tabular-nums">{{ formattedRemain }}</span>
      </span>
      <!-- 副行：已用 / 总额（两者任一存在才展示，避免把缺字段读成 0）。 -->
      <span
        v-if="usedOfTotalText"
        data-test="trae-credits-usage"
        class="text-[10px] leading-4 text-gray-400 dark:text-gray-500"
      >
        {{ usedOfTotalText }}
      </span>
      <!-- 有效积分包个数（已过期的不计入）。 -->
      <span
        v-if="packsLabel"
        data-test="trae-credits-packs"
        class="text-[10px] leading-4 text-gray-400 dark:text-gray-500"
      >
        {{ packsLabel }}
      </span>
      <span v-if="realm" class="text-[10px] leading-4 text-gray-400">
        {{ realm === 'global' ? 'global' : 'cn' }}
      </span>
      <!-- 凭据到期告警：Trae 没有「重新登录即自动续期」的旁路，refreshToken 一过期
           就必须人工到 Trae 客户端重登重取凭据，所以「还能用多久」必须在此可见。 -->
      <span
        v-if="tokenExpiryLabel"
        data-test="trae-token-expiry"
        class="whitespace-nowrap rounded px-1 py-0.5 text-[10px] font-medium leading-4"
        :class="tokenExpiryClass"
        :title="tokenExpiryTooltip"
      >
        {{ tokenExpiryLabel }}
      </span>
    </div>

    <!-- 操作行：查询 + 签到（今日已签到时签到按钮禁用并显示已签到态）。 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="trae-credits-refresh"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.trae.creditsRefreshTooltip')"
        @click="handleProbe()"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.trae.creditsRefresh') }}
      </button>
      <button
        type="button"
        data-test="trae-checkin"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-emerald-600 transition-colors hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
        :disabled="checkinLoading || checkinDisabled"
        :title="checkinButtonTooltip"
        @click="handleCheckin()"
      >
        <svg
          v-if="checkinLoading"
          class="h-2.5 w-2.5 animate-spin"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.trae.checkin') }}
      </button>
      <!-- 今日签到状态（上游读数 / 快照 / 本次操作结果）：ok|already 绿、fail 红、skipped 灰。 -->
      <span
        v-if="checkinStatusLabel"
        data-test="trae-checkin-status"
        class="text-[10px] leading-4"
        :class="checkinStatusClass"
        :title="checkinStatusDetail || undefined"
      >
        {{ checkinStatusLabel }}
      </span>
    </div>

    <!-- 错误行（查询失败保留快照只显示错误；文案截断 80 字符）。 -->
    <div
      v-if="error"
      data-test="trae-credits-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="error"
    >
      {{ truncatedError }}
    </div>
    <div
      v-if="checkinError"
      data-test="trae-checkin-error"
      class="truncate text-[10px] leading-4 text-red-600 dark:text-red-400"
      :title="checkinError"
    >
      {{ truncatedCheckinError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { TraeCreditsPackage, TraeCreditsResult } from '@/api/admin/trae'
import type { Account } from '@/types'

const props = defineProps<{
  account: Account
}>()

const { t } = useI18n()

const visible = computed(() => props.account.platform === 'trae')

const loading = ref(false)
const checkinLoading = ref(false)
const error = ref<string | null>(null)
const checkinError = ref<string | null>(null)

// 展示用的收敛类型：Result 全字段必填（后端契约），而 extra 快照可能缺字段，
// 故两者都归一到「字段可选」的视图类型，缺字段一律显示 '-' 而不是 0。
type TraeCreditsView = {
  success: boolean
  realm?: string
  credits?: number
  remain?: number
  used?: number
  size?: number
  packs?: number
  packages?: TraeCreditsPackage[]
  fetched_at?: number
  checkable?: boolean
  checked_in?: boolean
  today_checkin_status?: string
  token_expires_at?: number
  refresh_expires_at?: number
}

const data = ref<TraeCreditsView | null>(null)

// 签到结果的本地覆盖（本次操作后优先于 extra 快照显示）。
const localCheckinStatus = ref('')

// 快照过期窗口（与 WorkBuddyCreditsCell / QoderCreditsCell 同口径）。
const SNAPSHOT_STALE_MS = 15 * 60 * 1000

// 自动探测去抖窗口与最近一次自动探测时间（模块级，跨实例共享）。
const AUTO_PROBE_DEBOUNCE_MS = 5 * 60 * 1000
const lastAutoProbeAt = new Map<number, number>()

// 本地日期（YYYY-MM-DD）；与 WorkBuddyCreditsCell 同一生成方式。
const todayLocalDate = () => {
  const now = new Date()
  const month = String(now.getMonth() + 1).padStart(2, '0')
  const day = String(now.getDate()).padStart(2, '0')
  return `${now.getFullYear()}-${month}-${day}`
}

/**
 * Trae 积分是浮点数（与 WorkBuddy 的整数积分不同）：
 * 整数部分千分位、小数最多保留 1 位（2203.5264 → 2,203.5；2200 → 2,200）。
 */
const formatTraeCredits = (value: number): string => {
  if (!Number.isFinite(value)) return '-'
  const rounded = Math.round(value * 10) / 10
  const integral = Number.isInteger(rounded)
  return rounded.toLocaleString('en-US', {
    minimumFractionDigits: integral ? 0 : 1,
    maximumFractionDigits: 1
  })
}

// 后端 UpdateExtra 写入 trae_credits 的快照形状（字段可能缺失）。
type TraeCreditsSnapshotExtra = {
  credits?: number
  remain?: number
  used?: number
  size?: number
  packs?: number
  packages?: TraeCreditsPackage[]
  fetched_at?: number
  realm?: string
  checkable?: boolean
  checked_in?: boolean
  token_expires_at?: number
  refresh_expires_at?: number
}

// 从 account.extra 读持久化快照（免探测渲染）。
const snapshotData = computed<TraeCreditsView | null>(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.trae_credits
  if (!raw || typeof raw !== 'object') return null
  const snap = raw as TraeCreditsSnapshotExtra
  // 两个积分口径字段都没有就没有可展示的读数（不要拿 0 冒充）。
  if (typeof snap.remain !== 'number' && typeof snap.credits !== 'number') return null
  return {
    success: true,
    realm: typeof snap.realm === 'string' ? snap.realm : undefined,
    credits: typeof snap.credits === 'number' ? snap.credits : undefined,
    remain: typeof snap.remain === 'number' ? snap.remain : undefined,
    used: typeof snap.used === 'number' ? snap.used : undefined,
    size: typeof snap.size === 'number' ? snap.size : undefined,
    packs: typeof snap.packs === 'number' ? snap.packs : undefined,
    packages: Array.isArray(snap.packages) ? snap.packages : undefined,
    fetched_at: typeof snap.fetched_at === 'number' ? snap.fetched_at : undefined,
    checkable: typeof snap.checkable === 'boolean' ? snap.checkable : undefined,
    checked_in: typeof snap.checked_in === 'boolean' ? snap.checked_in : undefined,
    token_expires_at: typeof snap.token_expires_at === 'number' ? snap.token_expires_at : undefined,
    refresh_expires_at:
      typeof snap.refresh_expires_at === 'number' ? snap.refresh_expires_at : undefined
  }
})

// 后端查询结果优先，extra 快照兜底。
const resolved = computed<TraeCreditsView | null>(() => data.value || snapshotData.value)

// ---------------- 凭据到期告警 ----------------
// Trae 的 refreshToken 一旦过期，**没有任何自动续期途径**（必须人工在 Trae 客户端
// 重新登录再取凭据），账号会默默变成不可用。故到期时刻必须常驻列表可见。
// 取数优先级：查询结果 / 积分快照 > account.credentials（expires_at 不是敏感键，
// 会随详情下发且换票时回写，因此无快照也能显示）。
const SECONDS_PER_DAY = 86400
const SECONDS_PER_HOUR = 3600

const numOr = (a: number | undefined, b: number | undefined): number | undefined => {
  if (typeof a === 'number' && a > 0) return a
  if (typeof b === 'number' && b > 0) return b
  return undefined
}

const credentialNumber = (key: string): number | undefined => {
  const creds = props.account.credentials as Record<string, unknown> | undefined
  // 别名必须与后端 GetTraeCredentials 同口径：Trae 本地 storage.json 用过去式
  // expiredAt / refreshExpiredAt，traework2api 用 expiresAt / refreshExpiresAt。
  // 只读一种会把另一形态读成「无到期时间」，进而错过过期告警。
  const aliasGroups: Record<string, string[]> = {
    expires_at: ['expires_at', 'expiresAt', 'expiredAt'],
    refresh_expires_at: ['refresh_expires_at', 'refreshExpiresAt', 'refreshExpiredAt']
  }
  for (const alias of aliasGroups[key] ?? [key]) {
    const value = creds?.[alias]
    if (typeof value === 'number' && value > 0) return toEpochSeconds(value)
    // 后端可能以字符串承载 epoch（手动粘贴的 storage.json 字段）。
    if (typeof value === 'string' && value.trim() !== '') {
      const parsed = Number(value)
      if (Number.isFinite(parsed) && parsed > 0) return toEpochSeconds(parsed)
    }
  }
  return undefined
}

const tokenExpiresAt = computed(() =>
  numOr(resolved.value?.token_expires_at, credentialNumber('expires_at'))
)
const refreshExpiresAt = computed(() =>
  numOr(resolved.value?.refresh_expires_at, credentialNumber('refresh_expires_at'))
)

/** 归一为 epoch 秒：凭据里可能残留毫秒形态（上游 TokenExpireAt 即毫秒）。 */
const toEpochSeconds = (value: number): number => (value > 1e12 ? Math.floor(value / 1000) : value)

const tokenExpiryState = computed<'none' | 'expired' | 'soon' | 'upcoming' | 'renewable'>(() => {
  const now = Date.now() / 1000
  const token = tokenExpiresAt.value ? toEpochSeconds(tokenExpiresAt.value) : undefined
  const refresh = refreshExpiresAt.value ? toEpochSeconds(refreshExpiresAt.value) : undefined

  if (typeof refresh === 'number' && refresh <= now) return 'expired'
  // access token 过期但 refreshToken 仍在：后端会在出站前自动换票，不必报警。
  if (typeof token === 'number' && token <= now) {
    return typeof refresh === 'number' ? 'renewable' : 'expired'
  }
  const effective = typeof refresh === 'number' ? refresh : token
  if (typeof effective !== 'number') return 'none'
  const remaining = effective - now
  if (remaining <= 72 * SECONDS_PER_HOUR) return 'soon'
  if (remaining <= 14 * SECONDS_PER_DAY) return 'upcoming'
  return 'none'
})

const tokenExpiryLabel = computed(() => {
  const now = Date.now() / 1000
  const refresh = refreshExpiresAt.value ? toEpochSeconds(refreshExpiresAt.value) : undefined
  const token = tokenExpiresAt.value ? toEpochSeconds(tokenExpiresAt.value) : undefined
  const effective = typeof refresh === 'number' ? refresh : token
  const days =
    typeof effective === 'number' ? Math.max(0, Math.ceil((effective - now) / SECONDS_PER_DAY)) : 0
  const hours =
    typeof effective === 'number' ? Math.max(0, Math.ceil((effective - now) / SECONDS_PER_HOUR)) : 0

  switch (tokenExpiryState.value) {
    case 'expired':
      return t('admin.accounts.trae.tokenExpired')
    case 'renewable':
      return t('admin.accounts.trae.tokenRenewable')
    case 'soon':
      return t('admin.accounts.trae.tokenExpiringInHours', { hours })
    case 'upcoming':
      return t('admin.accounts.trae.tokenExpiringInDays', { days })
    default:
      return ''
  }
})

const tokenExpiryClass = computed(() => {
  switch (tokenExpiryState.value) {
    case 'expired':
      return 'bg-red-50 text-red-600 dark:bg-red-900/30 dark:text-red-400'
    case 'soon':
      return 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400'
    case 'renewable':
    case 'upcoming':
      return 'bg-gray-100 text-gray-500 dark:bg-gray-800 dark:text-gray-400'
    default:
      return ''
  }
})

const formatEpoch = (value?: number) =>
  typeof value === 'number' && value > 0 ? new Date(toEpochSeconds(value) * 1000).toLocaleString() : '-'

const tokenExpiryTooltip = computed(() => {
  const lines = [
    `${t('admin.accounts.trae.tokenExpiresAt')}: ${formatEpoch(tokenExpiresAt.value)}`,
    `${t('admin.accounts.trae.refreshTokenExpiresAt')}: ${formatEpoch(refreshExpiresAt.value)}`
  ]
  if (tokenExpiryState.value === 'expired') {
    lines.push(t('admin.accounts.trae.tokenExpiredHint'))
  }
  return lines.join('\n')
})

// 主数字用 remain（未过期权益包聚合剩余），缺 remain 时回落 credits（上游 status 口径）。
const primaryCredits = computed(() => {
  const r = resolved.value
  if (!r) return undefined
  if (typeof r.remain === 'number') return r.remain
  if (typeof r.credits === 'number') return r.credits
  return undefined
})

const formattedRemain = computed(() => {
  const value = primaryCredits.value
  return typeof value === 'number' ? formatTraeCredits(value) : '-'
})

const usedOfTotalText = computed(() => {
  const r = resolved.value
  if (!r) return ''
  if (typeof r.used !== 'number' && typeof r.size !== 'number') return ''
  const used = typeof r.used === 'number' ? formatTraeCredits(r.used) : '-'
  const size = typeof r.size === 'number' ? formatTraeCredits(r.size) : '-'
  return t('admin.accounts.trae.creditsUsedOfTotal', { used, size })
})

const realm = computed(() => resolved.value?.realm || '')

// 未过期权益包（已过期的不计入展示与计数）。
const activePackages = computed<TraeCreditsPackage[]>(() => {
  const packages = resolved.value?.packages
  if (!Array.isArray(packages)) return []
  return packages.filter((pkg) => pkg.expired !== true)
})

const packsLabel = computed(() => {
  const packages = resolved.value?.packages
  const count = Array.isArray(packages)
    ? activePackages.value.length
    : typeof resolved.value?.packs === 'number'
      ? resolved.value.packs
      : 0
  if (count <= 0) return ''
  return t('admin.accounts.trae.creditsPacks', { count })
})

const packKindLabel = (kind?: string): string => {
  switch (kind) {
    case 'checkin':
      return t('admin.accounts.trae.creditsPackKindCheckin')
    case 'plan':
      return t('admin.accounts.trae.creditsPackKindPlan')
    default:
      return t('admin.accounts.trae.creditsPackKindOther')
  }
}

const isoDateOnly = (timestamp: number): string => {
  const d = new Date(timestamp)
  if (Number.isNaN(d.getTime())) return String(timestamp)
  const month = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${month}-${day}`
}

// 悬停剩余积分时展示：前若干条权益包明细（含到期日）+ 更新时间。
const PACKAGES_TOOLTIP_LIMIT = 6
const creditsTooltip = computed(() => {
  const r = resolved.value
  if (!r) return t('admin.accounts.trae.creditsEmpty')
  const lines: string[] = []
  const packages = activePackages.value
  if (packages.length > 0) {
    lines.push(t('admin.accounts.trae.creditsPackagesTitle') + ':')
    for (const pkg of packages.slice(0, PACKAGES_TOOLTIP_LIMIT)) {
      const name = pkg.entitlement_id || packKindLabel(pkg.kind)
      const remain = typeof pkg.remain === 'number' ? formatTraeCredits(pkg.remain) : '-'
      const limit = typeof pkg.limit === 'number' ? formatTraeCredits(pkg.limit) : '-'
      const expiry =
        typeof pkg.expire_at === 'number' && pkg.expire_at > 0
          ? ` (${t('admin.accounts.trae.creditsPackExpiresAt', { date: isoDateOnly(pkg.expire_at) })})`
          : ''
      lines.push(`· ${name}: ${remain} / ${limit}${expiry}`)
    }
    if (packages.length > PACKAGES_TOOLTIP_LIMIT) {
      lines.push(`… +${packages.length - PACKAGES_TOOLTIP_LIMIT}`)
    }
  } else if (Array.isArray(r.packages) && r.packages.length > 0) {
    // 有包但全部已过期：仍给出「已过期」线索，避免 tooltip 空着。
    lines.push(`${t('admin.accounts.trae.creditsPackagesTitle')}: ${t('admin.accounts.trae.creditsPackExpired')}`)
  }
  const fetchedAt = r.fetched_at
  if (typeof fetchedAt === 'number' && fetchedAt > 0) {
    lines.push(
      `${t('admin.accounts.trae.creditsUpdatedAt')}: ${new Date(fetchedAt * 1000).toLocaleString()}`
    )
  }
  return lines.join('\n')
})

// 从 account.extra 读今日签到快照（trae_checkin，仅当日有效）。
const snapshotCheckinStatus = computed(() => {
  const raw = (props.account.extra as Record<string, unknown> | undefined)?.trae_checkin
  if (!raw || typeof raw !== 'object') return ''
  const snap = raw as { date?: string; status?: string }
  if (typeof snap.date !== 'string' || typeof snap.status !== 'string') return ''
  // 上游权威读数（checked_in）缺失时，才以本地日期串粗匹配今日快照。
  if (snap.date !== todayLocalDate()) return ''
  return snap.status
})

const checkinStatus = computed(() => {
  if (localCheckinStatus.value) return localCheckinStatus.value
  const r = resolved.value
  if (r?.checked_in === true) return 'already'
  if (typeof r?.today_checkin_status === 'string' && r.today_checkin_status) {
    return r.today_checkin_status
  }
  return snapshotCheckinStatus.value
})

const isCheckedInToday = computed(() => {
  const status = checkinStatus.value
  return status === 'ok' || status === 'already'
})

// 签到入口可用性：上游 checkable 明确 false 时禁用（缺字段视为可用）。
const checkinUnavailable = computed(() => resolved.value?.checkable === false)

const checkinDisabled = computed(() => isCheckedInToday.value || checkinUnavailable.value)

const checkinStatusLabel = computed(() => {
  switch (checkinStatus.value) {
    case 'ok':
    case 'already':
      return t('admin.accounts.trae.checkinDoneToday')
    case 'fail':
      return t('admin.accounts.trae.checkinFailed')
    case 'skipped':
      return t('admin.accounts.trae.checkinSkipped')
    default:
      return ''
  }
})

const checkinStatusDetail = computed(() => {
  if (checkinStatus.value === 'fail' || checkinStatus.value === 'skipped') {
    return checkinError.value || undefined
  }
  return undefined
})

const checkinStatusClass = computed(() => {
  if (checkinStatus.value === 'fail') return 'text-red-600 dark:text-red-400'
  if (checkinStatus.value === 'skipped') return 'text-gray-400'
  return 'text-emerald-600 dark:text-emerald-400'
})

const checkinButtonTooltip = computed(() => {
  if (isCheckedInToday.value) return t('admin.accounts.trae.checkinDoneToday')
  if (checkinUnavailable.value) return t('admin.accounts.trae.creditsCheckinUnavailableTooltip')
  return checkinError.value && checkinStatus.value === 'skipped'
    ? checkinError.value
    : t('admin.accounts.trae.checkinTooltip')
})

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

const truncatedCheckinError = computed(() => {
  if (!checkinError.value) return ''
  return checkinError.value.length > 80
    ? `${checkinError.value.slice(0, 80)}...`
    : checkinError.value
})

// 挂载时：先用持久化快照渲染；快照缺失或过期再自动探测一次（模块级去抖）。
onMounted(() => {
  if (!visible.value) return
  data.value = snapshotData.value
  const fetchedAt = snapshotData.value?.fetched_at
  const fresh = typeof fetchedAt === 'number' && Date.now() - fetchedAt * 1000 <= SNAPSHOT_STALE_MS
  if (fresh) return
  const last = lastAutoProbeAt.get(props.account.id) ?? 0
  if (Date.now() - last < AUTO_PROBE_DEBOUNCE_MS) return
  lastAutoProbeAt.set(props.account.id, Date.now())
  handleProbe()
})

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    const result: TraeCreditsResult = await adminAPI.trae.queryTraeCredits(props.account.id)
    // 失败时保留已渲染的快照（仅显示错误行），成功才覆盖。
    if (result.success) {
      data.value = result
      // 上游权威读数（checked_in / today_checkin_status）随结果一并刷新展示。
      if (result.today_checkin_status && !localCheckinStatus.value) {
        localCheckinStatus.value = result.today_checkin_status
      }
    } else {
      error.value = result.error || t('common.error')
    }
  } catch (e) {
    error.value = extractErrorMessage(e)
  } finally {
    loading.value = false
  }
}

const handleCheckin = async () => {
  if (checkinLoading.value) return
  checkinLoading.value = true
  checkinError.value = null
  try {
    const result = await adminAPI.trae.checkinTraeAccount(props.account.id)
    localCheckinStatus.value = result.status
    if (result.status === 'fail' || result.status === 'skipped') {
      checkinError.value = result.detail || t('common.error')
    }
    // 签到成功（ok/already）后用回执余额刷新显示，缺额度则回退查一次。
    // 必须以 has_credits=true 为准：后端在回查 status 失败时 credits 恒为 0，
    // 直接写入会把「剩余 2203.5」误显示成 0（与事实相反的读数）。
    if (result.status === 'ok' || result.status === 'already') {
      if (result.has_credits === true && typeof result.credits === 'number' && data.value) {
        data.value = {
          ...data.value,
          remain: result.credits,
          credits: result.credits,
          checked_in: true
        }
      } else if (data.value) {
        data.value = { ...data.value, checked_in: true }
      } else {
        await handleProbe()
      }
    }
  } catch (e) {
    localCheckinStatus.value = 'fail'
    checkinError.value = extractErrorMessage(e)
  } finally {
    checkinLoading.value = false
  }
}

// 列表复用组件：切换账号必须复位全部本地态，否则串号。
watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    checkinError.value = null
    loading.value = false
    checkinLoading.value = false
    localCheckinStatus.value = ''
  }
)
</script>
